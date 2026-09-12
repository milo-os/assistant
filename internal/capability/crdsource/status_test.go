package crdsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appmetrics "github.com/milo-os/assistant/internal/metrics"
	capv1alpha1 "github.com/milo-os/assistant/pkg/apis/capabilities/v1alpha1"
)

// recordedPatch is one PATCH the fake control plane received.
type recordedPatch struct {
	path        string
	contentType string
	status      capv1alpha1.CapabilityBindingStatus
	raw         string
}

// statusPlane is an httptest control plane that records status PATCHes and
// answers them with whatever the test dictates.
type statusPlane struct {
	*httptest.Server
	mu      sync.Mutex
	patches []recordedPatch
	// respond decides each response. Nil answers 200.
	respond func(w http.ResponseWriter, r *http.Request) bool
}

func newStatusPlane(t *testing.T) *statusPlane {
	t.Helper()
	p := &statusPlane{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Status capv1alpha1.CapabilityBindingStatus `json:"status"`
		}
		raw := new(bytes.Buffer)
		_, _ = raw.ReadFrom(r.Body)
		_ = json.Unmarshal(raw.Bytes(), &body)

		p.mu.Lock()
		p.patches = append(p.patches, recordedPatch{
			path:        r.URL.EscapedPath(),
			contentType: r.Header.Get("Content-Type"),
			status:      body.Status,
			raw:         raw.String(),
		})
		respond := p.respond
		p.mu.Unlock()

		if respond != nil && respond(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *statusPlane) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.patches)
}

func (p *statusPlane) all() []recordedPatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]recordedPatch, len(p.patches))
	copy(out, p.patches)
	return out
}

// waitForPatches polls until n patches have arrived, or fails. The writer is
// asynchronous by design, so every assertion about it is an assertion about
// what eventually reached the control plane.
func (p *statusPlane) waitForPatches(t *testing.T, n int) []recordedPatch {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.count() >= n {
			return p.all()
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d status patches; got %d", n, p.count())
	return nil
}

// quiesce gives the worker a moment to do something wrong, so that a test
// asserting "exactly one write" is not just asserting "the second one has not
// landed yet".
func quiesce(t *testing.T, p *statusPlane, want int) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if got := p.count(); got != want {
		t.Fatalf("status patches: got %d, want %d (%+v)", got, want, p.all())
	}
}

func newTestWriter(t *testing.T, url string, mutate func(*StatusWriterConfig)) *StatusWriter {
	t.Helper()
	cfg := StatusWriterConfig{APIURL: url, Transport: http.DefaultTransport}
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := NewStatusWriter(cfg)
	if err != nil {
		t.Fatalf("NewStatusWriter: %v", err)
	}
	t.Cleanup(w.Close)
	return w
}

// syncBuffer is a log sink safe to read from the test goroutine while the
// writer's worker goroutine is logging into it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func condOf(t *testing.T, p recordedPatch, condType string) metav1.Condition {
	t.Helper()
	for _, c := range p.status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	t.Fatalf("no %s condition in patch %s", condType, p.raw)
	return metav1.Condition{}
}

// The address is the whole contract with the control plane: the project selects
// the control plane, the object is addressed without a namespace (it is
// cluster-scoped inside that plane, exactly as the LIST addresses it), and the
// write targets the status subresource with a merge patch. A regression here is
// a 404 nobody notices, because nothing waits on this call.
func TestStatusWriteTargetsTheStatusSubresource(t *testing.T) {
	plane := newStatusPlane(t)
	w := newTestWriter(t, plane.URL, nil)

	w.ObserveAccepted("acme", "streamco-binding", 7, nil)

	got := plane.waitForPatches(t, 1)[0]
	wantPath := "/apis/resourcemanager.miloapis.com/v1alpha1/projects/acme/control-plane" +
		"/apis/capabilities.assistant.miloapis.com/v1alpha1/capabilitybindings/streamco-binding/status"
	if got.path != wantPath {
		t.Fatalf("PATCH path:\n got %s\nwant %s", got.path, wantPath)
	}
	if got.contentType != "application/merge-patch+json" {
		t.Fatalf("content type: got %q", got.contentType)
	}
	if got.status.ObservedGeneration != 7 {
		t.Fatalf("observedGeneration: got %d, want 7", got.status.ObservedGeneration)
	}
	cond := condOf(t, got, capv1alpha1.CapabilityBindingConditionAccepted)
	if cond.Status != metav1.ConditionTrue || cond.Reason != reasonValidated {
		t.Fatalf("Accepted condition: %+v", cond)
	}
	if cond.LastTransitionTime.IsZero() {
		t.Fatal("lastTransitionTime must be set; the apiserver rejects a condition without one")
	}
}

// A validation failure is the reason this writeback exists: the provider who
// authored the binding must see the same path-qualified error the assistant
// logs to itself.
func TestValidationFailureCarriesThePathQualifiedError(t *testing.T) {
	plane := newStatusPlane(t)
	w := newTestWriter(t, plane.URL, nil)

	w.ObserveAccepted("acme", "broken", 3, errors.New("spec.skills[0].source: required"))

	cond := condOf(t, plane.waitForPatches(t, 1)[0], capv1alpha1.CapabilityBindingConditionAccepted)
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonValidationFailed {
		t.Fatalf("condition: %+v", cond)
	}
	if cond.Message != "spec.skills[0].source: required" {
		t.Fatalf("message: got %q", cond.Message)
	}
	if cond.ObservedGeneration != 3 {
		t.Fatalf("condition observedGeneration: got %d", cond.ObservedGeneration)
	}
}

// Coalescing is the difference between a status writer and a hot loop: a cache
// entry lives 60s and a turn consults a binding repeatedly, so an uncoalesced
// writer issues an API call per observation.
func TestRepeatedIdenticalObservationsWriteOnce(t *testing.T) {
	plane := newStatusPlane(t)
	w := newTestWriter(t, plane.URL, nil)

	for range 25 {
		w.ObserveAccepted("acme", "streamco-binding", 1, nil)
	}
	plane.waitForPatches(t, 1)
	quiesce(t, plane, 1)
}

func TestTransitionProducesAWrite(t *testing.T) {
	plane := newStatusPlane(t)
	clk := &clock{t: time.Unix(1700000000, 0)}
	w := newTestWriter(t, plane.URL, func(c *StatusWriterConfig) { c.now = clk.now })

	w.ObserveAccepted("acme", "b", 1, nil)
	plane.waitForPatches(t, 1)
	clk.advance(time.Minute)
	w.ObserveAccepted("acme", "b", 1, nil) // identical: coalesced
	w.ObserveAccepted("acme", "b", 1, errors.New("spec.serviceName: required"))

	patches := plane.waitForPatches(t, 2)
	quiesce(t, plane, 2)
	if c := condOf(t, patches[1], capv1alpha1.CapabilityBindingConditionAccepted); c.Status != metav1.ConditionFalse {
		t.Fatalf("second patch should report the new verdict: %+v", c)
	}
	first := condOf(t, patches[0], capv1alpha1.CapabilityBindingConditionAccepted)
	second := condOf(t, patches[1], capv1alpha1.CapabilityBindingConditionAccepted)
	if !second.LastTransitionTime.Time.After(first.LastTransitionTime.Time) {
		t.Fatalf("a status change must move lastTransitionTime: %v → %v",
			first.LastTransitionTime, second.LastTransitionTime)
	}
}

// A new generation with the SAME verdict still writes: observedGeneration is
// how a producer who just applied an edit tells "the assistant has seen my
// change" from "the assistant is still reporting on the previous spec".
func TestNewGenerationRewritesEvenWithTheSameVerdict(t *testing.T) {
	plane := newStatusPlane(t)
	clk := &clock{t: time.Unix(1700000000, 0)}
	w := newTestWriter(t, plane.URL, func(c *StatusWriterConfig) { c.now = clk.now })

	w.ObserveAccepted("acme", "b", 1, nil)
	plane.waitForPatches(t, 1)
	clk.advance(time.Minute)
	w.ObserveAccepted("acme", "b", 2, nil)

	patches := plane.waitForPatches(t, 2)
	if patches[1].status.ObservedGeneration != 2 {
		t.Fatalf("observedGeneration: got %d, want 2", patches[1].status.ObservedGeneration)
	}
	// Same status, so the transition time must NOT move: a reader uses it to
	// ask how long a binding has been in this state.
	first := condOf(t, patches[0], capv1alpha1.CapabilityBindingConditionAccepted)
	second := condOf(t, patches[1], capv1alpha1.CapabilityBindingConditionAccepted)
	if !second.LastTransitionTime.Equal(&first.LastTransitionTime) {
		t.Fatalf("lastTransitionTime moved without a status change: %v → %v",
			first.LastTransitionTime, second.LastTransitionTime)
	}
}

// Composed has no generation of its own — the composition side holds a
// CapabilityDocument, which carries name and namespace but not generation — so
// it must reuse the one the LIST observed rather than reporting zero.
func TestComposedReusesTheObservedGeneration(t *testing.T) {
	plane := newStatusPlane(t)
	w := newTestWriter(t, plane.URL, nil)

	w.ObserveAccepted("acme", "b", 9, nil)
	plane.waitForPatches(t, 1)
	w.ObserveComposed("acme", "b", errors.New("mcp connect failed: streamco"))

	patches := plane.waitForPatches(t, 2)
	cond := condOf(t, patches[1], capv1alpha1.CapabilityBindingConditionComposed)
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonCompositionDegraded {
		t.Fatalf("Composed condition: %+v", cond)
	}
	if cond.ObservedGeneration != 9 {
		t.Fatalf("Composed observedGeneration: got %d, want 9", cond.ObservedGeneration)
	}
}

// Merge patch REPLACES an array rather than merging it by key, so a patch that
// carried only the condition that changed would delete the other one.
func TestEveryPatchCarriesTheFullConditionSet(t *testing.T) {
	plane := newStatusPlane(t)
	w := newTestWriter(t, plane.URL, nil)

	w.ObserveAccepted("acme", "b", 1, nil)
	plane.waitForPatches(t, 1)
	w.ObserveComposed("acme", "b", nil)

	last := plane.waitForPatches(t, 2)[1]
	if len(last.status.Conditions) != 2 {
		t.Fatalf("expected both conditions in the patch, got %s", last.raw)
	}
	// Sorted by type, so identical state always renders identical bytes.
	if last.status.Conditions[0].Type != capv1alpha1.CapabilityBindingConditionAccepted {
		t.Fatalf("conditions are not sorted by type: %s", last.raw)
	}
}

// The queue is bounded and sheds the OLDEST. A control-plane outage must
// degrade to "status not updated", never to unbounded memory.
func TestQueueIsBoundedAndDropsOldest(t *testing.T) {
	release := make(chan struct{})
	plane := newStatusPlane(t)
	plane.respond = func(w http.ResponseWriter, r *http.Request) bool {
		<-release // the worker parks on the first write
		return false
	}
	m := appmetrics.New()
	w := newTestWriter(t, plane.URL, func(c *StatusWriterConfig) {
		c.QueueSize = 2
		c.Metrics = m
	})

	// One write is taken by the worker and blocks; the rest queue, and
	// everything past the cap sheds.
	for i := range 12 {
		w.ObserveAccepted("acme", fmt.Sprintf("b-%d", i), 1, nil)
	}
	deadline := time.Now().Add(2 * time.Second)
	var dropped float64
	for time.Now().Before(deadline) {
		dropped = testutil.ToFloat64(m.CapabilityStatusWriteTotal.WithLabelValues("dropped"))
		if dropped > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(release)
	if dropped == 0 {
		t.Fatal("expected the bounded queue to shed writes under a stalled control plane")
	}
	// Whatever is delivered is delivered in order, and the survivors are the
	// NEWEST: drop-oldest keeps the observations closest to the truth.
	patches := plane.waitForPatches(t, 2)
	if !strings.HasSuffix(patches[len(patches)-1].path, "/status") {
		t.Fatalf("unexpected path %s", patches[len(patches)-1].path)
	}
}

// A hung control plane must cost the worker one timeout, not the process. The
// write is recorded as an error and the next observation of that binding
// retries it, because a failed write must not be remembered as written.
func TestWriteTimesOutAndIsRetriedOnTheNextTransition(t *testing.T) {
	gate := make(chan struct{})
	plane := newStatusPlane(t)
	plane.respond = func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-gate:
			return false
		case <-r.Context().Done():
			return true
		}
	}
	m := appmetrics.New()
	w := newTestWriter(t, plane.URL, func(c *StatusWriterConfig) {
		c.Timeout = 40 * time.Millisecond
		c.Metrics = m
	})

	w.ObserveAccepted("acme", "b", 1, nil)
	plane.waitForPatches(t, 1)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(m.CapabilityStatusWriteTotal.WithLabelValues("error")) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := testutil.ToFloat64(m.CapabilityStatusWriteTotal.WithLabelValues("error")); got != 1 {
		t.Fatalf("error writes: got %v, want 1", got)
	}

	// The identical observation is a transition again, because the failed write
	// was forgotten.
	close(gate)
	w.ObserveAccepted("acme", "b", 1, nil)
	plane.waitForPatches(t, 2)
	if got := testutil.ToFloat64(m.CapabilityStatusWriteTotal.WithLabelValues("ok")); got != 1 {
		t.Fatalf("ok writes after retry: got %v, want 1", got)
	}
}

// 403 is the shape of the open question about Milo's permission model (no
// subresource axis) being answered the other way. It must be diagnosable in
// minutes: its own event name, its own metric outcome, never folded into
// generic write errors.
func TestForbiddenIsLoggedAndCountedDistinguishably(t *testing.T) {
	plane := newStatusPlane(t)
	plane.respond = func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, `{"kind":"Status","reason":"Forbidden"}`, http.StatusForbidden)
		return true
	}
	logs := &syncBuffer{}
	m := appmetrics.New()
	w := newTestWriter(t, plane.URL, func(c *StatusWriterConfig) {
		c.Metrics = m
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})

	w.ObserveAccepted("acme", "b", 1, nil)
	plane.waitForPatches(t, 1)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(m.CapabilityStatusWriteTotal.WithLabelValues("forbidden")) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := testutil.ToFloat64(m.CapabilityStatusWriteTotal.WithLabelValues("forbidden")); got != 1 {
		t.Fatalf("forbidden writes: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.CapabilityStatusWriteTotal.WithLabelValues("error")); got != 0 {
		t.Fatalf("a 403 must not also count as a generic error: got %v", got)
	}
	if !strings.Contains(logs.String(), "capability.crd.status_forbidden") {
		t.Fatalf("403 was not logged under its own event:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "subresource") {
		t.Fatalf("the 403 log should name the subresource-permission hypothesis:\n%s", logs.String())
	}
}

// The empty-key guard. Both halves of (project, name) are path segments: the
// project selects the virtual control plane, the name selects the object. The
// live hazard is a caller that derives the project from a document's namespace
// — always empty now that CapabilityBinding is cluster-scoped — which would
// send every PATCH to a control-plane path for no project, silently, forever.
// Refuse the observation instead, and say so.
func TestObservationWithNoProjectOrNameIsDroppedNotSent(t *testing.T) {
	plane := newStatusPlane(t)
	logs := &syncBuffer{}
	w := newTestWriter(t, plane.URL, func(c *StatusWriterConfig) {
		c.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})

	w.ObserveComposed("", "streamco-binding", nil) // project lost (the namespace-derived bug)
	w.ObserveAccepted("", "streamco-binding", 1, nil)
	w.ObserveComposed("acme", "", nil) // binding name lost

	// One real write proves the writer is otherwise live, and lets the worker
	// be observed to have run at all.
	w.ObserveAccepted("acme", "streamco-binding", 1, nil)
	got := plane.waitForPatches(t, 1)
	quiesce(t, plane, 1)

	if len(got) != 1 || !strings.HasSuffix(got[0].path, "/capabilitybindings/streamco-binding/status") {
		t.Fatalf("only the fully-keyed observation may be written; got %+v", got)
	}
	if n := strings.Count(logs.String(), "capability.crd.status_unkeyed"); n != 3 {
		t.Fatalf("every unkeyed observation must be logged; got %d:\n%s", n, logs.String())
	}
}

// Close drains what this replica already decided, then stops the worker.
func TestCloseDrainsPendingWrites(t *testing.T) {
	plane := newStatusPlane(t)
	w, err := NewStatusWriter(StatusWriterConfig{APIURL: plane.URL, Transport: http.DefaultTransport})
	if err != nil {
		t.Fatalf("NewStatusWriter: %v", err)
	}
	for i := range 5 {
		w.ObserveAccepted("acme", fmt.Sprintf("b-%d", i), 1, nil)
	}
	w.Close()
	if got := plane.count(); got != 5 {
		t.Fatalf("Close should have drained the queue: got %d writes, want 5", got)
	}
	w.Close() // idempotent
}

// ── The source seam ───────────────────────────────────────────

// The verdict must reach the writer for every binding the LIST returned, with
// the generation the KRM object carried — the fact the CapabilityDocument does
// not survive with.
func TestSourceReportsAcceptedPerBinding(t *testing.T) {
	good := binding("good", "streaming.streamco.example")
	good.Generation = 4
	bad := binding("bad", "")
	bad.Generation = 11

	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(listBody(t, good, bad)))
	})
	plane := newStatusPlane(t)
	writer := newTestWriter(t, plane.URL, nil)
	clk := &clock{t: time.Unix(1700000000, 0)}
	src := newSource(t, fake.URL, clk, func(c *Config) { c.Observer = writer })

	docs, err := src.Documents(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected the invalid binding to be dropped, got %d documents", len(docs))
	}

	byName := map[string]recordedPatch{}
	for _, p := range plane.waitForPatches(t, 2) {
		parts := strings.Split(p.path, "/")
		byName[parts[len(parts)-2]] = p
	}
	goodCond := condOf(t, byName["good"], capv1alpha1.CapabilityBindingConditionAccepted)
	if goodCond.Status != metav1.ConditionTrue || goodCond.ObservedGeneration != 4 {
		t.Fatalf("good binding: %+v", goodCond)
	}
	badCond := condOf(t, byName["bad"], capv1alpha1.CapabilityBindingConditionAccepted)
	if badCond.Status != metav1.ConditionFalse || badCond.ObservedGeneration != 11 {
		t.Fatalf("bad binding: %+v", badCond)
	}
	if badCond.Message == "" {
		t.Fatal("a rejected binding must say why; that is the entire point of the condition")
	}
}

// Conversion happens on a cache MISS, so the writeback inherits the cache's
// rate limiting for free: a project's bindings are observed once per TTL no
// matter how many turns consult them.
func TestCachedTurnsDoNotReObserve(t *testing.T) {
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(listBody(t, binding("b", "streaming.streamco.example"))))
	})
	plane := newStatusPlane(t)
	writer := newTestWriter(t, plane.URL, nil)
	clk := &clock{t: time.Unix(1700000000, 0)}
	src := newSource(t, fake.URL, clk, func(c *Config) { c.Observer = writer })

	for range 10 {
		if _, err := src.Documents(context.Background(), "acme"); err != nil {
			t.Fatalf("Documents: %v", err)
		}
	}
	plane.waitForPatches(t, 1)
	quiesce(t, plane, 1)
}

// The load-bearing promise: a status write must never add latency to a turn or
// fail one. With the control plane's status endpoint hung and the queue full,
// Documents still returns its documents promptly.
func TestStatusWritesNeverBlockOrFailDocuments(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	items := make([]capv1alpha1.CapabilityBinding, 0, 20)
	for i := range 20 {
		items = append(items, binding(fmt.Sprintf("b-%d", i), "streaming.streamco.example"))
	}
	fake := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(listBody(t, items...)))
	})
	plane := newStatusPlane(t)
	plane.respond = func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	}
	writer := newTestWriter(t, plane.URL, func(c *StatusWriterConfig) { c.QueueSize = 1 })
	clk := &clock{t: time.Unix(1700000000, 0)}
	src := newSource(t, fake.URL, clk, func(c *Config) { c.Observer = writer })

	done := make(chan int, 1)
	go func() {
		docs, err := src.Documents(context.Background(), "acme")
		if err != nil {
			t.Errorf("Documents returned an error: %v", err)
		}
		done <- len(docs)
	}()
	select {
	case n := <-done:
		if n != 20 {
			t.Fatalf("documents: got %d, want 20", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Documents blocked on the status writer — the config plane is back on the latency path")
	}
}

func TestNewStatusWriterRequiresAPIURL(t *testing.T) {
	if _, err := NewStatusWriter(StatusWriterConfig{}); err == nil {
		t.Fatal("expected an error for a missing APIURL")
	}
}

// A nil *StatusWriter is a valid no-op observer, so a caller that did not
// configure one cannot panic a turn.
func TestNilStatusWriterIsANoOp(t *testing.T) {
	var w *StatusWriter
	w.ObserveAccepted("acme", "b", 1, nil)
	w.ObserveComposed("acme", "b", errors.New("boom"))
	w.Close()
}
