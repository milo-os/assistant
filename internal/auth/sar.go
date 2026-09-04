package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults for [SARConfig]. The resourceAttributes triple models the question
// "may this subject start assistant work in this project?" — a create on the
// assistant's conversations resource, scoped to the project's namespace. All
// three are overridable so a deployment can retune the check without a code
// change (the service-shell workstream sets them from config).
const (
	DefaultSARGroup    = "assistant.miloapis.com"
	DefaultSARResource = "conversations"
	DefaultSARVerb     = "create"

	// DefaultSARTimeout bounds a single SAR round-trip. A hung control plane
	// must fail closed (deny), never stall a request indefinitely.
	DefaultSARTimeout = 5 * time.Second

	// DefaultSARCacheTTL is how long an ALLOW decision is reused before the
	// next control-plane round-trip. Kept short: a revoked user keeps access
	// for at most this window (acceptable staleness), while a JUST-granted user
	// is never locked out — denies are never cached, so the very next request
	// re-checks and permits immediately.
	DefaultSARCacheTTL = 60 * time.Second

	// maxSARCacheEntries bounds cache memory. On overflow, expired entries are
	// swept first, then one live entry is evicted to make room.
	maxSARCacheEntries = 4096

	// sarPath is the SubjectAccessReview endpoint, relative to the project
	// control plane it is addressed to.
	sarPath = "/apis/authorization.k8s.io/v1/subjectaccessreviews"

	// projectControlPlanePath is the prefix that scopes a request to one
	// project's control plane. Milo decides access to project-scoped resources
	// THERE, not at the core control plane: asking the core plane about
	// conversations in a project returns an explicit `denied: true` no matter
	// what has been granted, because the resource does not live at that scope.
	// %s is the project name.
	projectControlPlanePath = "/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane"
)

// sarEndpoint returns the URL a review for projectName must be POSTed to.
//
// Always project-scoped: the assistant only ever asks whether a subject may act
// in a specific project, and Milo decides that at the project's control plane.
func sarEndpoint(baseURL, projectName string) string {
	return strings.TrimRight(baseURL, "/") +
		fmt.Sprintf(projectControlPlanePath, url.PathEscape(projectName)) +
		sarPath
}

// SubjectAccessReview is the minimal authorization.k8s.io/v1 SubjectAccessReview
// wire shape the assistant POSTs and reads back. Only the fields the assistant
// sets or inspects are modeled — the apiserver ignores unknown request fields
// and we ignore unknown response fields — so this stays a small typed contract
// with no k8s.io/client-go dependency.
type SubjectAccessReview struct {
	APIVersion string                     `json:"apiVersion"`
	Kind       string                     `json:"kind"`
	Spec       SubjectAccessReviewSpec    `json:"spec"`
	Status     *SubjectAccessReviewStatus `json:"status,omitempty"`
}

// SubjectAccessReviewSpec is the review request: who (User) may do what
// (ResourceAttributes).
type SubjectAccessReviewSpec struct {
	ResourceAttributes *ResourceAttributes `json:"resourceAttributes,omitempty"`
	// User is the subject under review — the authenticated principal, NOT the
	// assistant's own identity (that is the client certificate on the HTTP
	// call).
	User string `json:"user,omitempty"`
	// UID and Groups complete the subject's identity. Milo binds policy to a
	// user's ID rather than to the username string, so omitting UID makes an
	// otherwise-valid grant evaluate as "not allowed".
	UID    string   `json:"uid,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// ResourceAttributes scopes the access check to a specific verb/group/resource
// in a namespace (the project's scope).
type ResourceAttributes struct {
	Namespace string `json:"namespace,omitempty"`
	Verb      string `json:"verb,omitempty"`
	Group     string `json:"group,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Name      string `json:"name,omitempty"`
}

// SubjectAccessReviewStatus is the decision the apiserver (via Milo's
// OpenFGA-backed authorization webhook) returns.
type SubjectAccessReviewStatus struct {
	Allowed         bool   `json:"allowed"`
	Denied          bool   `json:"denied,omitempty"`
	Reason          string `json:"reason,omitempty"`
	EvaluationError string `json:"evaluationError,omitempty"`
}

// SubjectAccessReviewer issues one SubjectAccessReview and returns its status.
// The default implementation POSTs to the control plane; tests inject a fake so
// no live cluster is needed. A non-nil error means the review could NOT be
// decided (transport, timeout, non-2xx, decode) — the authorizer treats that as
// a deny (fail closed).
type SubjectAccessReviewer interface {
	Review(ctx context.Context, review *SubjectAccessReview) (*SubjectAccessReviewStatus, error)
}

// SARConfig configures [NewSubjectAccessReviewAuthorizer]. In production the
// service-shell workstream builds it from the in-cluster rest config
// (APIURL from KUBERNETES_SERVICE_HOST/PORT, BearerToken and CACert from the
// mounted service-account); tests inject a Reviewer and leave the rest zero.
type SARConfig struct {
	// APIURL is the control-plane API base URL the SAR is POSTed to
	// (e.g. https://kubernetes.default.svc). Required unless Reviewer is set.
	APIURL string
	// BearerToken is the ASSISTANT's own service-account token, sent as the
	// Authorization header so the apiserver authenticates the caller. This is
	// distinct from the User in the SAR body (the principal under review).
	// Ignored when Reviewer is injected.
	BearerToken string
	// CACert is the PEM-encoded apiserver CA bundle. Empty falls back to the
	// system roots. Ignored when Reviewer is injected.
	CACert []byte
	// ClientCert/ClientKey are the PEM-encoded client certificate the assistant
	// presents to identify ITSELF to the control plane, as an alternative to
	// BearerToken. Both or neither. Ignored when Reviewer is injected.
	ClientCert []byte
	ClientKey  []byte

	// Group, Resource, Verb are the resourceAttributes the SAR asks about.
	// Empty fields fall back to the Default* constants.
	Group    string
	Resource string
	Verb     string

	// Timeout bounds a single SAR round-trip. Zero uses DefaultSARTimeout.
	Timeout time.Duration
	// CacheTTL bounds how long an ALLOW is reused. Zero uses DefaultSARCacheTTL;
	// a negative value disables the cache (every request round-trips).
	CacheTTL time.Duration

	// Reviewer overrides the default HTTP reviewer — tests inject a fake.
	Reviewer SubjectAccessReviewer
	// now overrides the clock (tests). Nil uses time.Now.
	now func() time.Time
}

// sarAuthorizer is the production [Authorizer]: instead of trusting the grants a
// credential carries (that is [ClaimsAuthorizer]), it issues a
// SubjectAccessReview against the Milo control plane and permits only on an
// explicit allow. It FAILS CLOSED — any error, timeout, missing status, or
// non-allow response yields a 403.
type sarAuthorizer struct {
	reviewer SubjectAccessReviewer
	group    string
	resource string
	verb     string
	timeout  time.Duration
	cache    *allowCache
	now      func() time.Time
}

// NewSubjectAccessReviewAuthorizer builds the production SAR-based [Authorizer].
// It returns an error only on misconfiguration (no Reviewer and no APIURL); once
// constructed it never fails open — AuthorizeProject denies on any SAR failure.
func NewSubjectAccessReviewAuthorizer(cfg SARConfig) (Authorizer, error) {
	reviewer := cfg.Reviewer
	if reviewer == nil {
		if strings.TrimSpace(cfg.APIURL) == "" {
			return nil, errors.New("auth: SARConfig requires APIURL (or an injected Reviewer)")
		}
		r, err := newHTTPReviewer(cfg)
		if err != nil {
			return nil, err
		}
		reviewer = r
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultSARTimeout
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = DefaultSARCacheTTL
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	return &sarAuthorizer{
		reviewer: reviewer,
		group:    firstNonEmpty(cfg.Group, DefaultSARGroup),
		resource: firstNonEmpty(cfg.Resource, DefaultSARResource),
		verb:     firstNonEmpty(cfg.Verb, DefaultSARVerb),
		timeout:  timeout,
		cache:    newAllowCache(ttl, maxSARCacheEntries),
		now:      now,
	}, nil
}

// AuthorizeProject issues (or reuses a cached) SubjectAccessReview for principal
// against projectName. It returns nil on an explicit allow and a 403 [*Error]
// on any deny — including every failure mode (empty subject, SAR error,
// timeout, missing/negative status), so the control plane is authoritative and
// the assistant never permits on doubt.
func (a *sarAuthorizer) AuthorizeProject(ctx context.Context, principal Principal, projectName string) error {
	if principal.Subject == "" {
		return Unauthorized("SubjectAccessReview requires a subject; token carried none")
	}

	// UID participates in the key: two principals must never share a cached
	// decision just because they present the same username.
	key := principal.Subject + "\x00" + principal.UID + "\x00" + projectName
	if a.cache.allowed(key, a.now()) {
		return nil
	}

	review := &SubjectAccessReview{
		APIVersion: "authorization.k8s.io/v1",
		Kind:       "SubjectAccessReview",
		Spec: SubjectAccessReviewSpec{
			User:   principal.Subject,
			UID:    principal.UID,
			Groups: principal.Groups,
			ResourceAttributes: &ResourceAttributes{
				Namespace: projectName,
				Verb:      a.verb,
				Group:     a.group,
				Resource:  a.resource,
			},
		},
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	status, err := a.reviewer.Review(ctx, review)
	if err != nil {
		// Transport, timeout, non-2xx, or decode failure — the decision is
		// unknown, so deny. Never fail open.
		return Unauthorized(fmt.Sprintf("SubjectAccessReview failed for project %q: %v", projectName, err))
	}
	// Permit ONLY on an explicit, undenied allow. A nil status, allowed=false,
	// or an explicit denied all fail closed.
	if status == nil || !status.Allowed || status.Denied {
		return Unauthorized(fmt.Sprintf("Subject %q is not allowed to access project %q", principal.Subject, projectName))
	}

	a.cache.store(key, a.now())
	return nil
}

// ── Default HTTP reviewer ─────────────────────────────────────

// httpReviewer POSTs the SAR to the apiserver's subjectaccessreviews endpoint.
//
// baseURL, not a precomputed endpoint: the URL depends on the project under
// review, so it is built per request from the review's own resourceAttributes.
type httpReviewer struct {
	baseURL string
	token   string
	client  *http.Client
}

// newHTTPReviewer builds the reviewer from cfg's control-plane coordinates.
func newHTTPReviewer(cfg SARConfig) (*httpReviewer, error) {
	transport, err := newControlPlaneTransport(cfg.CACert, cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultSARTimeout
	}
	return &httpReviewer{
		baseURL: cfg.APIURL,
		token:   cfg.BearerToken,
		client:  &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

// Review POSTs review and returns the apiserver's decided status. Any transport
// error, non-2xx status, or undecodable body is returned as an error so the
// caller fails closed.
func (r *httpReviewer) Review(ctx context.Context, review *SubjectAccessReview) (*SubjectAccessReviewStatus, error) {
	body, err := json.Marshal(review)
	if err != nil {
		return nil, fmt.Errorf("marshal SubjectAccessReview: %w", err)
	}
	var project string
	if review.Spec.ResourceAttributes != nil {
		project = review.Spec.ResourceAttributes.Namespace
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sarEndpoint(r.baseURL, project), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build SubjectAccessReview request: %w", err)
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
		return nil, fmt.Errorf("read SubjectAccessReview response: %w", err)
	}
	// The apiserver returns 201 Created for a SAR POST (200 tolerated too).
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SubjectAccessReview returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out SubjectAccessReview
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode SubjectAccessReview response: %w", err)
	}
	if out.Status == nil {
		return nil, errors.New("SubjectAccessReview response carried no status")
	}
	return out.Status, nil
}

// ── Allow cache ───────────────────────────────────────────────

// allowCache is a bounded, TTL'd set of ALLOW decisions keyed by
// subject+project. Only allows are ever stored — denies are never cached, so a
// newly granted subject is re-checked (and permitted) on their very next
// request rather than being locked out for a TTL.
type allowCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]time.Time // key → expiry
}

func newAllowCache(ttl time.Duration, max int) *allowCache {
	return &allowCache{ttl: ttl, max: max, entries: make(map[string]time.Time)}
}

// allowed reports whether key has an unexpired allow at now.
func (c *allowCache) allowed(key string, now time.Time) bool {
	if c == nil || c.ttl <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[key]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(c.entries, key)
		return false
	}
	return true
}

// store records an allow for key expiring at now+ttl, evicting to stay bounded.
func (c *allowCache) store(key string, now time.Time) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		for k, exp := range c.entries {
			if !now.Before(exp) {
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
	c.entries[key] = now.Add(c.ttl)
}

func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
