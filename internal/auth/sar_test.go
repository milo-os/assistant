package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReviewer is an injectable [SubjectAccessReviewer] for the SAR authorizer
// tests — no live cluster needed. It records the last review it saw and the
// call count, and can block until ctx is canceled to exercise the timeout path.
type fakeReviewer struct {
	mu     sync.Mutex
	calls  int
	last   *SubjectAccessReview
	status *SubjectAccessReviewStatus
	err    error
	block  bool // if set, wait for ctx cancellation and return ctx.Err()
}

func (f *fakeReviewer) Review(ctx context.Context, review *SubjectAccessReview) (*SubjectAccessReviewStatus, error) {
	f.mu.Lock()
	f.calls++
	f.last = review
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.status, f.err
}

func (f *fakeReviewer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newSAR(t *testing.T, cfg SARConfig) Authorizer {
	t.Helper()
	az, err := NewSubjectAccessReviewAuthorizer(cfg)
	if err != nil {
		t.Fatalf("NewSubjectAccessReviewAuthorizer: %v", err)
	}
	return az
}

// allow → permit, and the emitted SAR carries the expected shape.
func TestSAR_AllowPermits(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true}}
	az := newSAR(t, SARConfig{Reviewer: fr})

	if err := az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"); err != nil {
		t.Fatalf("allow should permit: %v", err)
	}

	// The self-asserted grants are irrelevant — only the SAR decides. A
	// principal with NO grants is still permitted when the control plane allows.
	ra := fr.last.Spec.ResourceAttributes
	if fr.last.Spec.User != "alice" {
		t.Errorf("SAR user = %q, want alice", fr.last.Spec.User)
	}
	if ra == nil || ra.Namespace != "projA" || ra.Verb != DefaultSARVerb ||
		ra.Group != DefaultSARGroup || ra.Resource != DefaultSARResource {
		t.Errorf("resourceAttributes = %+v", ra)
	}
	if fr.last.APIVersion != "authorization.k8s.io/v1" || fr.last.Kind != "SubjectAccessReview" {
		t.Errorf("SAR envelope = %+v", fr.last)
	}
}

// deny (allowed=false) → 403, even for a wildcard-granting principal.
func TestSAR_DenyForbids(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: false}}
	az := newSAR(t, SARConfig{Reviewer: fr})

	err := az.AuthorizeProject(context.Background(), PrincipalFromProjects("alice", []string{"*"}), "projA")
	assertStatus(t, err, 403) // self-asserted wildcard does not override the SAR
}

// An explicit denied=true fails closed even if allowed is somehow also true.
func TestSAR_ExplicitDeniedForbids(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true, Denied: true}}
	az := newSAR(t, SARConfig{Reviewer: fr})
	assertStatus(t, az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"), 403)
}

// A nil status (malformed decision) fails closed.
func TestSAR_NilStatusForbids(t *testing.T) {
	fr := &fakeReviewer{status: nil}
	az := newSAR(t, SARConfig{Reviewer: fr})
	assertStatus(t, az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"), 403)
}

// A reviewer error fails closed (deny), never open.
func TestSAR_ErrorFailsClosed(t *testing.T) {
	fr := &fakeReviewer{err: io.ErrUnexpectedEOF}
	az := newSAR(t, SARConfig{Reviewer: fr})
	assertStatus(t, az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"), 403)
}

// A hung control plane hits the SAR timeout and fails closed.
func TestSAR_TimeoutFailsClosed(t *testing.T) {
	fr := &fakeReviewer{block: true}
	az := newSAR(t, SARConfig{Reviewer: fr, Timeout: 20 * time.Millisecond})

	start := time.Now()
	err := az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA")
	assertStatus(t, err, 403)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout not enforced, took %v", elapsed)
	}
}

// An empty subject cannot be reviewed and is denied without a round-trip.
func TestSAR_EmptySubjectForbids(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true}}
	az := newSAR(t, SARConfig{Reviewer: fr})
	assertStatus(t, az.AuthorizeProject(context.Background(), Principal{}, "projA"), 403)
	if fr.count() != 0 {
		t.Errorf("empty subject should not round-trip, calls = %d", fr.count())
	}
}

// Allow decisions are cached within TTL and re-checked past it. Denies are
// never cached. A movable clock drives expiry deterministically.
func TestSAR_CacheBehavior(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true}}
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	az := newSAR(t, SARConfig{Reviewer: fr, CacheTTL: 60 * time.Second, now: clock})
	ctx := context.Background()

	// First allow → one round-trip.
	if err := az.AuthorizeProject(ctx, Principal{Subject: "alice"}, "projA"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 1 {
		t.Fatalf("calls after first = %d, want 1", fr.count())
	}
	// Within TTL → served from cache, no new round-trip.
	now = now.Add(59 * time.Second)
	if err := az.AuthorizeProject(ctx, Principal{Subject: "alice"}, "projA"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 1 {
		t.Fatalf("calls within TTL = %d, want 1 (cached)", fr.count())
	}
	// A different project for the same subject is a distinct key → round-trips.
	if err := az.AuthorizeProject(ctx, Principal{Subject: "alice"}, "projB"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 2 {
		t.Fatalf("distinct project calls = %d, want 2", fr.count())
	}
	// Past TTL → cache entry expired, re-checks.
	now = now.Add(2 * time.Second) // projA entry now 61s old
	if err := az.AuthorizeProject(ctx, Principal{Subject: "alice"}, "projA"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 3 {
		t.Fatalf("calls past TTL = %d, want 3 (re-checked)", fr.count())
	}
}

// Denies are never cached: a just-granted subject is permitted on the very next
// request rather than being locked out for a TTL.
func TestSAR_DenyNotCached(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: false}}
	az := newSAR(t, SARConfig{Reviewer: fr})
	ctx := context.Background()

	assertStatus(t, az.AuthorizeProject(ctx, Principal{Subject: "alice"}, "projA"), 403)

	// Grant just landed in the control plane; the next check must permit.
	fr.status = &SubjectAccessReviewStatus{Allowed: true}
	if err := az.AuthorizeProject(ctx, Principal{Subject: "alice"}, "projA"); err != nil {
		t.Fatalf("just-granted subject should permit immediately: %v", err)
	}
	if fr.count() != 2 {
		t.Errorf("both requests should round-trip (no deny cache), calls = %d", fr.count())
	}
}

// A negative CacheTTL disables the cache: every request round-trips.
func TestSAR_CacheDisabled(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true}}
	az := newSAR(t, SARConfig{Reviewer: fr, CacheTTL: -1})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := az.AuthorizeProject(ctx, Principal{Subject: "alice"}, "projA"); err != nil {
			t.Fatal(err)
		}
	}
	if fr.count() != 3 {
		t.Errorf("cache-disabled calls = %d, want 3", fr.count())
	}
}

// Overridable resourceAttributes flow into the emitted SAR.
func TestSAR_CustomResourceAttributes(t *testing.T) {
	fr := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true}}
	az := newSAR(t, SARConfig{
		Reviewer: fr, Group: "resourcemanager.miloapis.com", Resource: "projects", Verb: "get",
	})
	if err := az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"); err != nil {
		t.Fatal(err)
	}
	ra := fr.last.Spec.ResourceAttributes
	if ra.Group != "resourcemanager.miloapis.com" || ra.Resource != "projects" || ra.Verb != "get" {
		t.Errorf("custom resourceAttributes = %+v", ra)
	}
}

// The constructor rejects a config with neither a Reviewer nor an APIURL.
func TestSAR_ConstructorRequiresTarget(t *testing.T) {
	if _, err := NewSubjectAccessReviewAuthorizer(SARConfig{}); err == nil {
		t.Fatal("expected error when neither Reviewer nor APIURL is set")
	}
}

// ── Default HTTP reviewer against a stub apiserver ─────────────

// The default HTTP reviewer POSTs the SAR to the k8s SAR endpoint with the
// assistant's bearer token, and reads back the decided status (201 Created).
func TestSAR_HTTPReviewer_Allow(t *testing.T) {
	var gotPath, gotAuth, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var in SubjectAccessReview
		_ = json.Unmarshal(body, &in)
		gotUser = in.Spec.User

		in.Status = &SubjectAccessReviewStatus{Allowed: true, Reason: "granted"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(in)
	}))
	defer srv.Close()

	az := newSAR(t, SARConfig{APIURL: srv.URL, BearerToken: "svc-token"})
	if err := az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"); err != nil {
		t.Fatalf("HTTP allow should permit: %v", err)
	}
	if gotPath != sarPath {
		t.Errorf("path = %q, want %q", gotPath, sarPath)
	}
	if gotAuth != "Bearer svc-token" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotUser != "alice" {
		t.Errorf("SAR user = %q", gotUser)
	}
}

// A non-2xx from the apiserver fails closed (deny).
func TestSAR_HTTPReviewer_NonOKFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	az := newSAR(t, SARConfig{APIURL: srv.URL})
	assertStatus(t, az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"), 403)
}

// A denied status over HTTP forbids.
func TestSAR_HTTPReviewer_Deny(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in SubjectAccessReview
		_ = json.Unmarshal(body, &in)
		in.Status = &SubjectAccessReviewStatus{Allowed: false, Reason: "no grant"}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(in)
	}))
	defer srv.Close()

	az := newSAR(t, SARConfig{APIURL: srv.URL})
	assertStatus(t, az.AuthorizeProject(context.Background(), Principal{Subject: "alice"}, "projA"), 403)
}

// An invalid CA bundle is a construction-time error, not a silent fail-open.
func TestSAR_InvalidCARejected(t *testing.T) {
	_, err := NewSubjectAccessReviewAuthorizer(SARConfig{APIURL: "https://milo", CACert: []byte("not-pem")})
	if err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("expected PEM error, got %v", err)
	}
}
