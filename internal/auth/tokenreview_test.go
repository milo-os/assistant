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

// fakeTokenReviewer is an injectable [TokenReviewer] for the TokenReview
// authenticator tests — no live cluster needed. It records the last review it
// saw and the call count, and can block until ctx is canceled to exercise the
// timeout path (mirrors sar_test.go's fakeReviewer).
type fakeTokenReviewer struct {
	mu     sync.Mutex
	calls  int
	last   *TokenReview
	status *TokenReviewStatus
	err    error
	block  bool // if set, wait for ctx cancellation and return ctx.Err()
}

func (f *fakeTokenReviewer) Review(ctx context.Context, review *TokenReview) (*TokenReviewStatus, error) {
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

func (f *fakeTokenReviewer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTR(t *testing.T, cfg TokenReviewConfig) Authenticator {
	t.Helper()
	an, err := NewTokenReviewAuthenticator(cfg)
	if err != nil {
		t.Fatalf("NewTokenReviewAuthenticator: %v", err)
	}
	return an
}

// authenticated + username → a Principal carrying that Subject, and the emitted
// TokenReview carries the expected shape.
func TestTR_AuthenticatedResolves(t *testing.T) {
	fr := &fakeTokenReviewer{status: &TokenReviewStatus{Authenticated: true, User: UserInfo{Username: "alice"}}}
	an := newTR(t, TokenReviewConfig{Reviewer: fr})

	p, err := an.Authenticate(context.Background(), "tok-abc")
	if err != nil {
		t.Fatalf("authenticated token should resolve: %v", err)
	}
	if p.Subject != "alice" {
		t.Errorf("Subject = %q, want alice", p.Subject)
	}
	// Identity only — project grants are decided downstream by the Authorizer.
	if p.GrantAll || len(p.Projects) != 0 {
		t.Errorf("TokenReview must not carry grants: %+v", p)
	}
	if fr.last.Spec.Token != "tok-abc" {
		t.Errorf("review token = %q, want tok-abc", fr.last.Spec.Token)
	}
	if fr.last.APIVersion != "authentication.k8s.io/v1" || fr.last.Kind != "TokenReview" {
		t.Errorf("TokenReview envelope = %+v", fr.last)
	}
}

// authenticated=false → 401.
func TestTR_NotAuthenticatedRejected(t *testing.T) {
	fr := &fakeTokenReviewer{status: &TokenReviewStatus{Authenticated: false}}
	an := newTR(t, TokenReviewConfig{Reviewer: fr})
	_, err := an.Authenticate(context.Background(), "tok-abc")
	assertStatus(t, err, 401)
}

// A reviewer error fails closed (401), never open.
func TestTR_ErrorFailsClosed(t *testing.T) {
	fr := &fakeTokenReviewer{err: io.ErrUnexpectedEOF}
	an := newTR(t, TokenReviewConfig{Reviewer: fr})
	_, err := an.Authenticate(context.Background(), "tok-abc")
	assertStatus(t, err, 401)
}

// A hung control plane hits the timeout and fails closed.
func TestTR_TimeoutFailsClosed(t *testing.T) {
	fr := &fakeTokenReviewer{block: true}
	an := newTR(t, TokenReviewConfig{Reviewer: fr, Timeout: 20 * time.Millisecond})

	start := time.Now()
	_, err := an.Authenticate(context.Background(), "tok-abc")
	assertStatus(t, err, 401)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout not enforced, took %v", elapsed)
	}
}

// An authenticated response with no username is malformed → 401 (don't trust it).
func TestTR_AuthenticatedNoUsernameRejected(t *testing.T) {
	fr := &fakeTokenReviewer{status: &TokenReviewStatus{Authenticated: true, User: UserInfo{Username: "  "}}}
	an := newTR(t, TokenReviewConfig{Reviewer: fr})
	_, err := an.Authenticate(context.Background(), "tok-abc")
	assertStatus(t, err, 401)
}

// A nil status (malformed decision) fails closed.
func TestTR_NilStatusRejected(t *testing.T) {
	fr := &fakeTokenReviewer{status: nil}
	an := newTR(t, TokenReviewConfig{Reviewer: fr})
	_, err := an.Authenticate(context.Background(), "tok-abc")
	assertStatus(t, err, 401)
}

// An empty token cannot be reviewed and is rejected without a round-trip.
func TestTR_EmptyTokenRejected(t *testing.T) {
	fr := &fakeTokenReviewer{status: &TokenReviewStatus{Authenticated: true, User: UserInfo{Username: "alice"}}}
	an := newTR(t, TokenReviewConfig{Reviewer: fr})
	_, err := an.Authenticate(context.Background(), "   ")
	assertStatus(t, err, 401)
	if fr.count() != 0 {
		t.Errorf("empty token should not round-trip, calls = %d", fr.count())
	}
}

// A successful resolution is cached within TTL (no second round-trip) and
// re-checked past it. A movable clock drives expiry deterministically.
func TestTR_CacheHitAvoidsSecondCall(t *testing.T) {
	fr := &fakeTokenReviewer{status: &TokenReviewStatus{Authenticated: true, User: UserInfo{Username: "alice"}}}
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	an := newTR(t, TokenReviewConfig{Reviewer: fr, CacheTTL: 60 * time.Second, now: clock})
	ctx := context.Background()

	if _, err := an.Authenticate(ctx, "tok-abc"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 1 {
		t.Fatalf("calls after first = %d, want 1", fr.count())
	}
	// Within TTL → served from cache.
	now = now.Add(59 * time.Second)
	if _, err := an.Authenticate(ctx, "tok-abc"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 1 {
		t.Fatalf("calls within TTL = %d, want 1 (cached)", fr.count())
	}
	// A different token is a distinct key → round-trips.
	if _, err := an.Authenticate(ctx, "tok-other"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 2 {
		t.Fatalf("distinct token calls = %d, want 2", fr.count())
	}
	// Past TTL → re-checks.
	now = now.Add(2 * time.Second) // tok-abc entry now 61s old
	if _, err := an.Authenticate(ctx, "tok-abc"); err != nil {
		t.Fatal(err)
	}
	if fr.count() != 3 {
		t.Fatalf("calls past TTL = %d, want 3 (re-checked)", fr.count())
	}
}

// Rejections are never cached: a just-provisioned token is authenticated on the
// very next request rather than being stuck failing for a TTL.
func TestTR_RejectionNotCached(t *testing.T) {
	fr := &fakeTokenReviewer{status: &TokenReviewStatus{Authenticated: false}}
	an := newTR(t, TokenReviewConfig{Reviewer: fr})
	ctx := context.Background()

	if _, err := an.Authenticate(ctx, "tok-abc"); err == nil {
		t.Fatal("expected rejection")
	}
	fr.status = &TokenReviewStatus{Authenticated: true, User: UserInfo{Username: "alice"}}
	if _, err := an.Authenticate(ctx, "tok-abc"); err != nil {
		t.Fatalf("just-provisioned token should authenticate immediately: %v", err)
	}
	if fr.count() != 2 {
		t.Errorf("both requests should round-trip (no rejection cache), calls = %d", fr.count())
	}
}

// The constructor rejects a config with neither a Reviewer nor an APIURL.
func TestTR_ConstructorRequiresTarget(t *testing.T) {
	if _, err := NewTokenReviewAuthenticator(TokenReviewConfig{}); err == nil {
		t.Fatal("expected error when neither Reviewer nor APIURL is set")
	}
}

// An invalid CA bundle is a construction-time error, not a silent fail-open.
func TestTR_InvalidCARejected(t *testing.T) {
	_, err := NewTokenReviewAuthenticator(TokenReviewConfig{APIURL: "https://milo", CACert: []byte("not-pem")})
	if err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("expected PEM error, got %v", err)
	}
}

// ── Default HTTP reviewer against a stub apiserver ─────────────

// The default HTTP reviewer POSTs the TokenReview to the k8s tokenreviews
// endpoint with the assistant's bearer token, and reads back the decided status.
func TestTR_HTTPReviewer_Authenticated(t *testing.T) {
	var gotPath, gotAuth, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var in TokenReview
		_ = json.Unmarshal(body, &in)
		gotToken = in.Spec.Token

		in.Status = &TokenReviewStatus{Authenticated: true, User: UserInfo{Username: "alice"}}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(in)
	}))
	defer srv.Close()

	an := newTR(t, TokenReviewConfig{APIURL: srv.URL, BearerToken: "svc-token"})
	p, err := an.Authenticate(context.Background(), "tok-abc")
	if err != nil {
		t.Fatalf("HTTP authenticated should resolve: %v", err)
	}
	if p.Subject != "alice" {
		t.Errorf("Subject = %q", p.Subject)
	}
	if gotPath != tokenReviewPath {
		t.Errorf("path = %q, want %q", gotPath, tokenReviewPath)
	}
	if gotAuth != "Bearer svc-token" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotToken != "tok-abc" {
		t.Errorf("reviewed token = %q", gotToken)
	}
}

// A non-2xx from the apiserver fails closed (401).
func TestTR_HTTPReviewer_NonOKFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	an := newTR(t, TokenReviewConfig{APIURL: srv.URL})
	_, err := an.Authenticate(context.Background(), "tok-abc")
	assertStatus(t, err, 401)
}
