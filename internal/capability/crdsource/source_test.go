package crdsource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	capv1alpha1 "github.com/milo-os/assistant/pkg/apis/capabilities/v1alpha1"
)

// binding builds a minimally-valid CapabilityBinding in namespace ns.
// binding builds one CapabilityBinding as a project control plane would return
// it: a name and NO namespace. The kind is cluster-scoped inside its project's
// plane, so a namespace here would be a shape the real server never produces.
func binding(name, service string) capv1alpha1.CapabilityBinding {
	return capv1alpha1.CapabilityBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: capv1alpha1.CapabilityBindingSpec{
			ServiceRef:           capv1alpha1.Ref{Name: "streamco"},
			ServiceName:          service,
			ServiceAgentRef:      capv1alpha1.Ref{Name: "streamco-agent"},
			ConfigurationVersion: "v1",
		},
	}
}

func listBody(t *testing.T, items ...capv1alpha1.CapabilityBinding) string {
	t.Helper()
	list := capv1alpha1.CapabilityBindingList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: capv1alpha1.SchemeGroupVersion.String(),
			Kind:       "CapabilityBindingList",
		},
		Items: items,
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	return string(raw)
}

// fakeControlPlane is an httptest server standing in for a project control
// plane, recording the paths it was asked for.
type fakeControlPlane struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	calls  atomic.Int64
	handle func(w http.ResponseWriter, r *http.Request)
}

func newFakeControlPlane(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *fakeControlPlane {
	t.Helper()
	f := &fakeControlPlane{handle: handle}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.EscapedPath())
		f.mu.Unlock()
		f.calls.Add(1)
		f.handle(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeControlPlane) lastPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) == 0 {
		return ""
	}
	return f.paths[len(f.paths)-1]
}

// clock is a manually advanced test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newSource(t *testing.T, url string, clk *clock, mutate func(*Config)) *Source {
	t.Helper()
	cfg := Config{
		APIURL:    url,
		Transport: http.DefaultTransport,
		now:       clk.now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// The URL is the whole tenancy story, and it states the project exactly once:
// in the control-plane prefix, which selects the virtual plane (and its etcd key
// prefix) the collection is read from. There is no namespace segment — the kind
// is cluster-scoped. A regression here reads as "this project has no
// capabilities", never as an error.
func TestDocumentsAddressesTheProjectControlPlane(t *testing.T) {
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, listBody(t, binding("b", "streaming.streamco.example")))
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	docs, err := s.Documents(context.Background(), "acme corp")
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 document, got %d", len(docs))
	}
	// metadata.name is the part of the envelope that must survive — it is the
	// key the status writer PATCHes back on. There is no namespace to keep.
	if docs[0].Metadata == nil || docs[0].Metadata.Name != "b" {
		t.Fatalf("document lost its metadata envelope: %+v", docs[0].Metadata)
	}
	want := "/apis/resourcemanager.miloapis.com/v1alpha1/projects/acme%20corp/control-plane" +
		"/apis/capabilities.assistant.miloapis.com/v1alpha1/capabilitybindings"
	if got := fake.lastPath(); got != want {
		t.Fatalf("LIST path = %q, want %q", got, want)
	}
}

func TestDocumentsCachesWithinTTLAndRefetchesAfter(t *testing.T) {
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, listBody(t, binding("b", "streaming.streamco.example")))
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	for i := range 3 {
		if _, err := s.Documents(context.Background(), "acme"); err != nil {
			t.Fatalf("Documents %d: %v", i, err)
		}
		clk.advance(20 * time.Second)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("within the 60s TTL, want 1 LIST, got %d", got)
	}
	clk.advance(time.Minute)
	if _, err := s.Documents(context.Background(), "acme"); err != nil {
		t.Fatalf("Documents after expiry: %v", err)
	}
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("after the TTL, want a second LIST, got %d total", got)
	}
}

// The point of the cache: a control-plane outage degrades to STALE, not to
// "entitled to nothing".
func TestDocumentsServesStaleWhenRefreshFails(t *testing.T) {
	var fail atomic.Bool
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","code":500}`)
			return
		}
		fmt.Fprint(w, listBody(t, binding("b", "streaming.streamco.example")))
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	if _, err := s.Documents(context.Background(), "acme"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	fail.Store(true)
	clk.advance(2 * time.Minute)

	docs, err := s.Documents(context.Background(), "acme")
	if err != nil {
		t.Fatalf("degraded fetch returned an error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want the stale document served, got %d", len(docs))
	}
}

func TestDocumentsReturnsEmptyWhenNothingCachedAndFetchFails(t *testing.T) {
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	docs, err := s.Documents(context.Background(), "acme")
	if err != nil {
		t.Fatalf("want a degrade, got error: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("want no documents, got %d", len(docs))
	}
}

// Empty is the analogue of a DENY: caching it would pin a newly-entitled
// project to built-ins-only for a TTL.
func TestEmptyResultIsNeverCached(t *testing.T) {
	var empty atomic.Bool
	empty.Store(true)
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if empty.Load() {
			fmt.Fprint(w, listBody(t))
			return
		}
		fmt.Fprint(w, listBody(t, binding("b", "streaming.streamco.example")))
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	if docs, _ := s.Documents(context.Background(), "acme"); len(docs) != 0 {
		t.Fatalf("want no documents, got %d", len(docs))
	}
	// No clock advance: a cached empty would be served here.
	empty.Store(false)
	docs, _ := s.Documents(context.Background(), "acme")
	if len(docs) != 1 {
		t.Fatalf("a just-entitled project must see its binding immediately, got %d", len(docs))
	}
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("want a second LIST (empty not cached), got %d", got)
	}
}

// Revocation: a successful empty LIST must drop what was retained, or a deleted
// binding would be served stale forever.
func TestSuccessfulEmptyLISTEvictsTheRetainedEntry(t *testing.T) {
	var revoked atomic.Bool
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if revoked.Load() {
			fmt.Fprint(w, listBody(t))
			return
		}
		fmt.Fprint(w, listBody(t, binding("b", "streaming.streamco.example")))
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	if docs, _ := s.Documents(context.Background(), "acme"); len(docs) != 1 {
		t.Fatalf("warm: want 1 document, got %d", len(docs))
	}
	revoked.Store(true)
	clk.advance(2 * time.Minute)
	if docs, _ := s.Documents(context.Background(), "acme"); len(docs) != 0 {
		t.Fatalf("after revocation, want 0 documents, got %d", len(docs))
	}
	if s.cache.len() != 0 {
		t.Fatalf("revoked project still has a cache entry")
	}
}

// One invalid binding must not cost a project its other bindings.
func TestInvalidDocumentsAreSkipped(t *testing.T) {
	bad := binding("bad", "") // no serviceName ⇒ Validate fails
	good := binding("good", "ok")
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, listBody(t, bad, good))
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	docs, _ := s.Documents(context.Background(), "acme")
	if len(docs) != 1 || docs[0].Spec.ServiceName != "ok" {
		t.Fatalf("want only the valid document, got %+v", docs)
	}
}

// A body that is not a CapabilityBindingList must fail rather than decode to
// zero items — "the CRD is not installed" must not look like "no entitlements".
func TestNonListResponseIsAFailureNotAnEmptyResult(t *testing.T) {
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Failure"}`)
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, nil)

	if _, err := s.fetch(context.Background(), "acme"); err == nil {
		t.Fatal("want a decode error for a non-list body")
	}
}

func TestCacheIsBounded(t *testing.T) {
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, listBody(t, binding("b", "streaming.streamco.example")))
	})
	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, func(c *Config) { c.MaxCacheEntries = 4 })

	for i := range 20 {
		if _, err := s.Documents(context.Background(), fmt.Sprintf("project-%d", i)); err != nil {
			t.Fatalf("Documents: %v", err)
		}
	}
	if got := s.cache.len(); got > 4 {
		t.Fatalf("cache grew past its bound: %d entries", got)
	}
}

// A hung control plane must degrade within the source's own budget rather than
// stall the turn.
func TestFetchTimesOut(t *testing.T) {
	release := make(chan struct{})
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)

	clk := &clock{t: time.Unix(1000, 0)}
	s := newSource(t, fake.URL, clk, func(c *Config) { c.Timeout = 50 * time.Millisecond })

	done := make(chan struct{})
	go func() {
		defer close(done)
		if docs, err := s.Documents(context.Background(), "acme"); err != nil || len(docs) != 0 {
			t.Errorf("timeout must degrade to empty, got %d docs err=%v", len(docs), err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Documents did not return within the fetch timeout")
	}
}

func TestNewRequiresAPIURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want an error when APIURL is unset")
	}
}
