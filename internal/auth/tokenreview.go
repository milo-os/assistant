package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Defaults for [TokenReviewConfig], mirroring the SAR side.
const (
	// DefaultTokenReviewTimeout bounds a single TokenReview round-trip. A hung
	// control plane must fail closed (reject the token), never stall a request.
	DefaultTokenReviewTimeout = 5 * time.Second

	// DefaultTokenReviewCacheTTL is how long a successful identity resolution is
	// reused before the next control-plane round-trip. Kept short: a revoked
	// token keeps working for at most this window (acceptable staleness).
	DefaultTokenReviewCacheTTL = 60 * time.Second

	// maxTokenReviewCacheEntries bounds cache memory. On overflow, expired
	// entries are swept first, then one live entry is evicted to make room.
	maxTokenReviewCacheEntries = 4096

	// tokenReviewPath is the cluster-scoped TokenReview endpoint.
	tokenReviewPath = "/apis/authentication.k8s.io/v1/tokenreviews"
)

// TokenReview is the minimal authentication.k8s.io/v1 TokenReview wire shape the
// assistant POSTs and reads back. Only the fields the assistant sets or inspects
// are modeled — the apiserver ignores unknown request fields and we ignore
// unknown response fields — so this stays a small typed contract with no
// k8s.io/client-go dependency (mirroring [SubjectAccessReview]).
type TokenReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Spec       TokenReviewSpec    `json:"spec"`
	Status     *TokenReviewStatus `json:"status,omitempty"`
}

// TokenReviewSpec is the review request: the opaque bearer token to authenticate.
type TokenReviewSpec struct {
	Token string `json:"token,omitempty"`
}

// TokenReviewStatus is the identity decision the apiserver returns.
type TokenReviewStatus struct {
	Authenticated bool     `json:"authenticated"`
	User          UserInfo `json:"user,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// UserInfo is the resolved identity. Only Username is consumed downstream (it
// becomes [Principal.Subject]); the rest are modeled for completeness/logging.
type UserInfo struct {
	Username string              `json:"username,omitempty"`
	UID      string              `json:"uid,omitempty"`
	Groups   []string            `json:"groups,omitempty"`
	Extra    map[string][]string `json:"extra,omitempty"`
}

// TokenReviewer issues one TokenReview and returns its status. The default
// implementation POSTs to the control plane; tests inject a fake so no live
// cluster is needed. A non-nil error means the review could NOT be decided
// (transport, timeout, non-2xx, decode) — the authenticator treats that as a
// rejection (fail closed).
type TokenReviewer interface {
	Review(ctx context.Context, review *TokenReview) (*TokenReviewStatus, error)
}

// TokenReviewConfig configures [NewTokenReviewAuthenticator]. In production the
// service shell builds it from the in-cluster rest config (APIURL from
// KUBERNETES_SERVICE_HOST/PORT, BearerToken and CACert from the mounted
// service-account); tests inject a Reviewer and leave the rest zero.
type TokenReviewConfig struct {
	// APIURL is the control-plane API base URL the TokenReview is POSTed to
	// (e.g. https://kubernetes.default.svc). Required unless Reviewer is set.
	APIURL string
	// BearerToken is the ASSISTANT's own service-account token, sent as the
	// Authorization header so the apiserver authenticates the caller. This is
	// distinct from the token in the TokenReview body (the token under review).
	// Ignored when Reviewer is injected.
	BearerToken string
	// CACert is the PEM-encoded apiserver CA bundle. Empty falls back to the
	// system roots. Ignored when Reviewer is injected.
	CACert []byte
	// ClientCert/ClientKey are the PEM-encoded client certificate the assistant
	// presents to identify ITSELF to the control plane, as an alternative to
	// BearerToken. Milo accepts service-account tokens only from its own
	// issuer, so a workload-cluster token is rejected and mTLS is the path that
	// works; see newControlPlaneTransport. Both or neither. Ignored when
	// Reviewer is injected.
	ClientCert []byte
	ClientKey  []byte

	// Timeout bounds a single TokenReview round-trip. Zero uses the default.
	Timeout time.Duration
	// CacheTTL bounds how long a successful resolution is reused. Zero uses the
	// default; a negative value disables the cache (every request round-trips).
	CacheTTL time.Duration

	// Reviewer overrides the default HTTP reviewer — tests inject a fake.
	Reviewer TokenReviewer
	// now overrides the clock (tests). Nil uses time.Now.
	now func() time.Time
}

// tokenReviewAuthenticator is the production [Authenticator] for a Kubernetes-
// style bearer token: it POSTs a TokenReview to the control plane and trusts
// only an explicitly authenticated response carrying a username. It FAILS CLOSED
// — any error, timeout, non-authenticated status, or empty username yields a 401.
type tokenReviewAuthenticator struct {
	reviewer TokenReviewer
	timeout  time.Duration
	cache    *principalCache
	now      func() time.Time
}

// NewTokenReviewAuthenticator builds the production TokenReview-based
// [Authenticator]. It returns an error only on misconfiguration (no Reviewer and
// no APIURL, or an invalid CA bundle); once constructed it never fails open —
// Authenticate rejects on any review failure.
func NewTokenReviewAuthenticator(cfg TokenReviewConfig) (Authenticator, error) {
	reviewer := cfg.Reviewer
	if reviewer == nil {
		if strings.TrimSpace(cfg.APIURL) == "" {
			return nil, errors.New("auth: TokenReviewConfig requires APIURL (or an injected Reviewer)")
		}
		r, err := newHTTPTokenReviewer(cfg)
		if err != nil {
			return nil, err
		}
		reviewer = r
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTokenReviewTimeout
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = DefaultTokenReviewCacheTTL
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	return &tokenReviewAuthenticator{
		reviewer: reviewer,
		timeout:  timeout,
		cache:    newPrincipalCache(ttl, maxTokenReviewCacheEntries),
		now:      now,
	}, nil
}

// Authenticate issues (or reuses a cached) TokenReview for bearerToken and
// resolves it to a [Principal] carrying only the Subject (username). It returns
// a 401 [*Error] on any failure mode — empty token, review error, timeout,
// non-authenticated status, or an authenticated response with no username — so
// the control plane is authoritative and the assistant never trusts on doubt.
// GrantAll/Projects are deliberately left zero: per-project access is decided
// downstream by the [Authorizer] (SubjectAccessReview), not carried here.
func (a *tokenReviewAuthenticator) Authenticate(ctx context.Context, bearerToken string) (Principal, error) {
	if strings.TrimSpace(bearerToken) == "" {
		return Principal{}, Unauthenticated("TokenReview requires a bearer token; none provided")
	}

	if p, ok := a.cache.get(bearerToken, a.now()); ok {
		return p, nil
	}

	review := &TokenReview{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       TokenReviewSpec{Token: bearerToken},
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	status, err := a.reviewer.Review(ctx, review)
	if err != nil {
		// Transport, timeout, non-2xx, or decode failure — identity is unknown,
		// so reject. Never fail open.
		return Principal{}, Unauthenticated(fmt.Sprintf("TokenReview failed: %v", err))
	}
	// Trust ONLY an explicitly authenticated response with a username. A nil
	// status, authenticated=false, or an empty username all fail closed.
	if status == nil || !status.Authenticated {
		return Principal{}, Unauthenticated("TokenReview did not authenticate the token")
	}
	username := strings.TrimSpace(status.User.Username)
	if username == "" {
		return Principal{}, Unauthenticated("TokenReview authenticated the token but returned no username")
	}

	principal := Principal{Subject: username}
	a.cache.store(bearerToken, principal, a.now())
	return principal, nil
}

// ── Default HTTP reviewer ─────────────────────────────────────

// httpTokenReviewer POSTs the TokenReview to the apiserver's tokenreviews
// endpoint (mirrors sar.go's httpReviewer).
type httpTokenReviewer struct {
	endpoint string
	token    string
	client   *http.Client
}

// newHTTPTokenReviewer builds the reviewer from cfg's control-plane coordinates.
func newHTTPTokenReviewer(cfg TokenReviewConfig) (*httpTokenReviewer, error) {
	transport, err := newControlPlaneTransport(cfg.CACert, cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTokenReviewTimeout
	}
	return &httpTokenReviewer{
		endpoint: strings.TrimRight(cfg.APIURL, "/") + tokenReviewPath,
		token:    cfg.BearerToken,
		client:   &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

// Review POSTs review and returns the apiserver's decided status. Any transport
// error, non-2xx status, or undecodable body is returned as an error so the
// caller fails closed.
func (r *httpTokenReviewer) Review(ctx context.Context, review *TokenReview) (*TokenReviewStatus, error) {
	body, err := json.Marshal(review)
	if err != nil {
		return nil, fmt.Errorf("marshal TokenReview: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build TokenReview request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read TokenReview response: %w", err)
	}
	// The apiserver returns 201 Created for a TokenReview POST (200 tolerated too).
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TokenReview returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out TokenReview
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode TokenReview response: %w", err)
	}
	if out.Status == nil {
		return nil, errors.New("TokenReview response carried no status")
	}
	return out.Status, nil
}

// ── Principal cache ───────────────────────────────────────────

// principalCache is a bounded, TTL'd map of successful token→Principal
// resolutions. Only successes are stored — a rejected token is re-reviewed on
// its next presentation, so a just-granted token is never stuck failing for a
// TTL (mirrors sar.go's allowCache, keyed on the token instead of allow/deny).
type principalCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]principalEntry
}

type principalEntry struct {
	principal Principal
	expiry    time.Time
}

func newPrincipalCache(ttl time.Duration, max int) *principalCache {
	return &principalCache{ttl: ttl, max: max, entries: make(map[string]principalEntry)}
}

// get returns the cached Principal for key when present and unexpired at now.
func (c *principalCache) get(key string, now time.Time) (Principal, bool) {
	if c == nil || c.ttl <= 0 {
		return Principal{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return Principal{}, false
	}
	if !now.Before(e.expiry) {
		delete(c.entries, key)
		return Principal{}, false
	}
	return e.principal, true
}

// store records principal for key expiring at now+ttl, evicting to stay bounded.
func (c *principalCache) store(key string, principal Principal, now time.Time) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		for k, e := range c.entries {
			if !now.Before(e.expiry) {
				delete(c.entries, k)
			}
		}
		// Still full of live entries: drop one to make room (bounds memory).
		if len(c.entries) >= c.max {
			for k := range c.entries {
				delete(c.entries, k)
				break
			}
		}
	}
	c.entries[key] = principalEntry{principal: principal, expiry: now.Add(c.ttl)}
}
