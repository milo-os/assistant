// Package projectapi is how Patch reads and writes one project's resources —
// always as the person who asked, never as itself.
//
// The service holds no credential for a customer's project. It has a
// service-account identity for asking the platform who a caller is and whether
// they may act on a project (see internal/auth), and that identity is
// deliberately good for nothing else. So every request this package makes
// carries the caller's own bearer token, taken from the authenticated request,
// and the project comes from the request the platform already authorized. A
// [Client] has no token field at all: there is nothing here to fall back to
// when a caller has none, which is what makes "act as the caller" a property of
// the type rather than a rule someone has to remember.
//
// The wire format is plain JSON over the project's own API prefix, decoded into
// map[string]any rather than typed structs. That is not laziness: the whole
// point of the base tools is to work with any kind a project serves, including
// kinds this repository has never heard of, so a typed client would be a list
// of the kinds we happened to compile in.
package projectapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// projectPrefix is the path a project's own API is served under. Everything
// below it belongs to that one project, which is why the project name is
// applied here — once, from the authorized request — and never taken from a
// caller's arguments further down.
const projectPrefix = "/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane"

const (
	// DefaultTimeout bounds one request. These run inside a chat turn, so a
	// slow answer is worse than a fast failure the model can report.
	DefaultTimeout = 20 * time.Second
	// maxResponseBytes caps a single response body. A list of every object of
	// a kind is the biggest thing read here; a schema document is the biggest
	// thing read anywhere, hence the generous ceiling.
	maxResponseBytes = 16 << 20
)

// Object is one resource, as it travels on the wire.
type Object = map[string]any

// Config builds a [Client].
type Config struct {
	// BaseURL is the platform API this project's resources are served from.
	BaseURL string
	// CACert verifies the server. Empty uses the system roots.
	CACert []byte
	// Timeout overrides [DefaultTimeout] when > 0.
	Timeout time.Duration
	// Transport is the test seam. Nil builds one from CACert.
	Transport http.RoundTripper
}

// Client reaches the platform API. It is safe for concurrent use and holds no
// credential: see [Client.As].
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a [Client]. It fails only on a configuration that could never
// work — no base URL, or a CA bundle that is not PEM — so a deployment mistake
// surfaces at boot rather than as an unexplained failure mid-conversation.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("projectapi: BaseURL is required")
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("projectapi: BaseURL %q is not a URL: %w", cfg.BaseURL, err)
	}

	transport := cfg.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		if len(cfg.CACert) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(cfg.CACert) {
				return nil, errors.New("projectapi: CACert is not valid PEM")
			}
			t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
		}
		transport = t
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{baseURL: base, http: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

// As returns a view of one project that acts as the holder of bearerToken.
//
// Both arguments come from the authenticated, authorized request and from
// nowhere else. A [Project] built with an empty token can still be used, and
// every call it makes is refused by the platform — which is the correct
// outcome, and a far better one than quietly reading as the service.
func (c *Client) As(project, bearerToken string) *Project {
	return &Project{client: c, project: project, token: bearerToken}
}

// Project reads and writes one project's resources as one caller.
type Project struct {
	client  *Client
	project string
	token   string
}

// Name is the project every request this view makes is scoped to.
func (p *Project) Name() string { return p.project }

// Resource identifies one kind as the project serves it.
type Resource struct {
	Group      string
	Version    string
	Resource   string
	Kind       string
	Namespaced bool
}

// GroupVersion renders "group/version", or just "version" for the core group.
func (r Resource) GroupVersion() string {
	if r.Group == "" {
		return r.Version
	}
	return r.Group + "/" + r.Version
}

// ErrKindNotServed reports that the project does not serve a kind at all —
// distinct from serving it and holding none of it.
//
// The two call for opposite actions and must never arrive as the same answer:
// an empty list means there is nothing there yet, and a kind nobody is serving
// means nothing looked. Matched with errors.Is.
var ErrKindNotServed = errors.New("this project does not offer that kind of resource")

// APIError is a rejection from the platform, kept whole. The status carries
// the field paths a rejection names, which are the part a person acts on.
type APIError struct {
	// Code is the HTTP status.
	Code int
	// Status is the platform's own reply, when it sent a structured one.
	Status *metav1.Status
	// Body is the raw reply, for the case where it did not.
	Body string
}

func (e *APIError) Error() string {
	if e.Status != nil && e.Status.Message != "" {
		return e.Status.Message
	}
	if e.Body != "" {
		return fmt.Sprintf("request failed (%d): %s", e.Code, e.Body)
	}
	return fmt.Sprintf("request failed (%d)", e.Code)
}

// IsNotFound reports whether the object asked for is not there.
func (e *APIError) IsNotFound() bool { return e.Code == http.StatusNotFound }

// IsForbidden reports whether the caller may not do this.
func (e *APIError) IsForbidden() bool {
	return e.Code == http.StatusForbidden || e.Code == http.StatusUnauthorized
}

// IsNotFound reports whether err is a "no such object" rejection.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

// IsForbidden reports whether err is a "you may not" rejection.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsForbidden()
}

// FieldErrors returns the field/message pairs a rejection named, plus the
// platform's own summary. The summary is included on purpose: it is the
// wording a person quotes when they escalate.
func (e *APIError) FieldErrors() []FieldError {
	var out []FieldError
	if e.Status != nil && e.Status.Details != nil {
		for _, cause := range e.Status.Details.Causes {
			out = append(out, FieldError{Field: cause.Field, Message: cause.Message})
		}
	}
	return append(out, FieldError{Message: e.Error()})
}

// FieldError is one rejection and the field it names.
type FieldError struct {
	// Field is the path the platform named, e.g. "spec.replicas". Empty when
	// the rejection is about the request as a whole.
	Field string `json:"field,omitempty"`
	// Message is the platform's own wording, kept verbatim so it can be quoted.
	Message string `json:"message"`
}

// ---------------------------------------------------------------- discovery

// Resolve maps a group, version and a kind (or a plural resource name) onto
// the resource the project actually serves, and reports whether it is held in
// a namespace.
//
// Both spellings are accepted because both turn up: a person says "Workload"
// and a manifest says "workloads", and asking a model to know which one this
// tool wants is a needless way to fail.
func (p *Project) Resolve(ctx context.Context, group, version, kindOrResource string) (Resource, error) {
	group = strings.TrimSpace(group)
	version = strings.TrimSpace(version)
	name := strings.TrimSpace(kindOrResource)
	if version == "" {
		return Resource{}, fmt.Errorf("a version is required, e.g. \"v1alpha1\"")
	}
	if name == "" {
		return Resource{}, fmt.Errorf("a kind is required, e.g. \"Workload\"")
	}

	path := "/apis/" + group + "/" + version
	if group == "" {
		path = "/api/" + version
	}
	body, err := p.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		if IsNotFound(err) {
			return Resource{}, fmt.Errorf("%w: nothing is served under %q", ErrKindNotServed, groupVersion(group, version))
		}
		return Resource{}, err
	}

	var list struct {
		Resources []struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			Namespaced bool   `json:"namespaced"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return Resource{}, fmt.Errorf("the reply listing %q could not be read: %w", groupVersion(group, version), err)
	}

	for _, r := range list.Resources {
		// Subresources ("workloads/status") are not kinds a caller works with.
		if strings.Contains(r.Name, "/") {
			continue
		}
		if strings.EqualFold(r.Kind, name) || strings.EqualFold(r.Name, name) {
			return Resource{
				Group: group, Version: version,
				Resource: r.Name, Kind: r.Kind, Namespaced: r.Namespaced,
			}, nil
		}
	}
	return Resource{}, fmt.Errorf("%w: %q is not served under %q", ErrKindNotServed, name, groupVersion(group, version))
}

func groupVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// ------------------------------------------------------------------- reads

// List returns every object of the kind. An empty namespace reads across all
// of them, which is also the only correct call for a kind held in none.
func (p *Project) List(ctx context.Context, r Resource, namespace string) ([]Object, error) {
	body, err := p.do(ctx, http.MethodGet, p.path(r, namespace, ""), nil, nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []Object `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("the list of %s could not be read: %w", r.Kind, err)
	}
	if list.Items == nil {
		list.Items = []Object{}
	}
	return list.Items, nil
}

// Get returns one object. A missing one comes back as an [*APIError] that
// [IsNotFound] recognizes, so callers can tell "not there" from "went wrong".
func (p *Project) Get(ctx context.Context, r Resource, namespace, name string) (Object, error) {
	body, err := p.do(ctx, http.MethodGet, p.path(r, namespace, name), nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeObject(body, r.Kind)
}

// ------------------------------------------------------------------ writes

// Create creates the object, or — with dryRun — asks the platform whether it
// would accept one without keeping anything.
func (p *Project) Create(ctx context.Context, r Resource, obj Object, dryRun bool) (Object, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("the %s could not be encoded: %w", r.Kind, err)
	}
	body, err := p.do(ctx, http.MethodPost, p.path(r, namespaceOf(obj), ""), dryRunQuery(dryRun), raw)
	if err != nil {
		return nil, err
	}
	return decodeObject(body, r.Kind)
}

// Update replaces the object, or — with dryRun — asks whether the replacement
// would be accepted. The object carries the version it was read at, so a change
// somebody else made in between is refused by the platform rather than
// overwritten.
func (p *Project) Update(ctx context.Context, r Resource, obj Object, dryRun bool) (Object, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("the %s could not be encoded: %w", r.Kind, err)
	}
	name, _ := NestedString(obj, "metadata", "name")
	body, err := p.do(ctx, http.MethodPut, p.path(r, namespaceOf(obj), name), dryRunQuery(dryRun), raw)
	if err != nil {
		return nil, err
	}
	return decodeObject(body, r.Kind)
}

func dryRunQuery(dryRun bool) url.Values {
	if !dryRun {
		return nil
	}
	return url.Values{"dryRun": []string{"All"}}
}

// ----------------------------------------------------------------- schemas

// OpenAPI returns the schema document describing one group and version.
func (p *Project) OpenAPI(ctx context.Context, group, version string) (map[string]any, error) {
	path := "/openapi/v3/apis/" + group + "/" + version
	if group == "" {
		path = "/openapi/v3/api/" + version
	}
	body, err := p.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("%w: there is no description of %q here", ErrKindNotServed, groupVersion(group, version))
		}
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("the description of %q could not be read: %w", groupVersion(group, version), err)
	}
	return doc, nil
}

// ----------------------------------------------------------------- plumbing

// path builds the request path for a kind. The project prefix is added by do,
// so nothing here can address another project.
func (p *Project) path(r Resource, namespace, name string) string {
	var b strings.Builder
	if r.Group == "" {
		b.WriteString("/api/" + r.Version)
	} else {
		b.WriteString("/apis/" + r.Group + "/" + r.Version)
	}
	if r.Namespaced && namespace != "" {
		b.WriteString("/namespaces/" + url.PathEscape(namespace))
	}
	b.WriteString("/" + r.Resource)
	if name != "" {
		b.WriteString("/" + url.PathEscape(name))
	}
	return b.String()
}

// do performs one request as the caller and returns the raw body.
func (p *Project) do(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	endpoint := p.client.baseURL + fmt.Sprintf(projectPrefix, url.PathEscape(p.project)) + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
	}
	// The caller's own credential, and nothing else. There is no service
	// credential on this path to fall back to.
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the platform could not be reached: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("the reply could not be read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp.StatusCode, raw)
	}
	return raw, nil
}

func newAPIError(code int, raw []byte) *APIError {
	apiErr := &APIError{Code: code, Body: strings.TrimSpace(string(raw))}
	var status metav1.Status
	if err := json.Unmarshal(raw, &status); err == nil && status.Kind == "Status" {
		apiErr.Status = &status
	}
	if len(apiErr.Body) > 2048 {
		apiErr.Body = apiErr.Body[:2048] + "…"
	}
	return apiErr
}

func decodeObject(raw []byte, kind string) (Object, error) {
	var obj Object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("the %s could not be read: %w", kind, err)
	}
	return obj, nil
}

func namespaceOf(obj Object) string {
	ns, _ := NestedString(obj, "metadata", "namespace")
	return ns
}

// NestedString reads a string at a path, reporting whether one was there.
func NestedString(obj Object, path ...string) (string, bool) {
	v, ok := Nested(obj, path...)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// NestedInt64 reads a number at a path. JSON numbers decode as float64, which
// is what makes this worth a helper rather than a type assertion at each site.
func NestedInt64(obj Object, path ...string) (int64, bool) {
	v, ok := Nested(obj, path...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// NestedMap reads a nested object at a path.
func NestedMap(obj Object, path ...string) (map[string]any, bool) {
	v, ok := Nested(obj, path...)
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// NestedSlice reads a nested array at a path.
func NestedSlice(obj Object, path ...string) ([]any, bool) {
	v, ok := Nested(obj, path...)
	if !ok {
		return nil, false
	}
	s, ok := v.([]any)
	return s, ok
}

// Nested walks path through nested objects and returns what it finds.
func Nested(obj Object, path ...string) (any, bool) {
	var current any = obj
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// ConditionTrue reports whether the object's status carries condition type
// with status "True".
func ConditionTrue(obj Object, conditionType string) bool {
	conditions, ok := NestedSlice(obj, "status", "conditions")
	if !ok {
		return false
	}
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); !strings.EqualFold(t, conditionType) {
			continue
		}
		s, _ := c["status"].(string)
		return s == string(metav1.ConditionTrue)
	}
	return false
}
