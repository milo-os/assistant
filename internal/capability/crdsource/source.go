// Package crdsource implements the assistant's production [capability.Source]:
// a short-TTL cached, per-project LIST of CapabilityBinding objects against
// that project's Milo control plane.
//
// It lives beside internal/capability rather than inside it on purpose. That
// package's doc comment promises types that "carry no control-plane client",
// and the promise is load-bearing: the schema is what providers conform to, so
// it must stay importable by anything (fixtures, tests, the CLI) without
// dragging in TLS material, an API client, or a cache. The client and the
// KRM→document conversion belong on this side of that line; the schema stays
// on the other.
//
// Two facts about the topology drive the whole design, and both are easy to get
// wrong from Kubernetes habit:
//
//   - A Milo project is a VIRTUAL CONTROL PLANE, not a namespace in the
//     assistant's own cluster. A cluster-wide informer against
//     kubernetes.default.svc would connect, report its cache synced, and
//     observe zero objects forever, for every project. Every read here is
//     addressed to /apis/resourcemanager.miloapis.com/v1alpha1/projects/{p}/control-plane,
//     the same way every SubjectAccessReview already is.
//   - The credential is a CLIENT CERTIFICATE, not a service-account token. Milo
//     validates service-account tokens only against its own issuer, so a
//     workload-cluster token is rejected with a 401 before the body is read.
//     See internal/auth/transport.go; the transport is reused from there rather
//     than rebuilt, so there is one place that decision lives.
package crdsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/capability"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	capv1alpha1 "github.com/milo-os/assistant/pkg/apis/capabilities/v1alpha1"
)

const (
	// DefaultCacheTTL is how long a project's fetched bindings are reused
	// before the next LIST. It matches auth.DefaultSARCacheTTL deliberately.
	//
	// The argument is consistency of risk appetite, not convenience. The
	// platform already accepts that a REVOKED USER keeps access for up to 60
	// seconds, because the SAR authorizer caches ALLOW for that long.
	// Entitlement is the weaker claim: a revoked entitlement that lingers for
	// the same window means a project can call a provider tool it no longer has
	// a binding for — and that call still has to pass the AI gateway's own
	// allow-list and the provider's own authorization on the way. A tighter TTL
	// here would assert that stale entitlement is more dangerous than stale
	// authorization, which is not true; a looser one would introduce a larger
	// staleness window with no argument behind it.
	//
	// Revocation latency is therefore the TTL, not ~0. That is the honest cost
	// of reading instead of watching, and it is why the catalog must DELETE a
	// binding on revocation rather than merely stop serving it.
	DefaultCacheTTL = 60 * time.Second

	// DefaultFetchTimeout bounds one LIST, matching auth.DefaultSARTimeout. A
	// cache miss is on a chat request's latency path, so a hung control plane
	// must degrade (to stale, then to empty) within a budget a user can wait
	// out, never stall the turn.
	DefaultFetchTimeout = 5 * time.Second

	// maxCacheEntries bounds cache memory, copying auth's maxSARCacheEntries
	// rather than inventing a second policy: on overflow, expired entries are
	// swept first, then one live entry is evicted. An operator who has reasoned
	// about the SAR cache's behavior should not have to reason separately about
	// this one.
	//
	// Memory scales with ACTIVE PROJECTS, not with the catalog's object count —
	// a few kilobytes per entry, dominated by knowledge URLs and tool
	// allow-lists — so 4096 entries is single-digit megabytes and the eviction
	// path is only exercised by a deployment with more than 4096 concurrently
	// active projects, which is a good problem.
	maxCacheEntries = 4096

	// maxResponseBytes caps the LIST body read. A control plane is trusted, but
	// "trusted" is not "permitted to exhaust this process's memory on a chat
	// request path".
	maxResponseBytes = 8 << 20

	// listPathTemplate is appended to the project's control-plane prefix.
	// %[1]s group, %[2]s version.
	//
	// NO namespace segment: CapabilityBinding is cluster-scoped, and the
	// project's control-plane prefix in front of this path is what scopes the
	// result (Milo partitions one apiserver by the etcd key prefix
	// "/projects/<project>"). A collection read through a project's path is
	// therefore already exactly "everything this project is entitled to". A
	// namespaced path here would address a namespace that nothing creates and
	// return a permanently empty list.
	listPathTemplate = "/apis/%[1]s/%[2]s/capabilitybindings"
)

// scheme registers the CapabilityBinding kinds so the LIST response is decoded
// through the same type registry the CRD is generated from. Decoding via the
// scheme rather than a bare json.Unmarshal buys one thing worth having: a
// response that is NOT a CapabilityBindingList — a Status object from a control
// plane that answered 200 with an error body, say — fails to decode instead of
// silently yielding zero items, which is the failure mode this whole change
// exists to eliminate.
var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	utilruntime.Must(capv1alpha1.AddToScheme(scheme))
}

// Config configures [New]. The control-plane coordinates are deliberately the
// SAME ones the SAR authorizer is configured with (AUTHZ_SAR_*): two ways to
// name one control plane is two ways to point half the service at the wrong
// one.
type Config struct {
	// APIURL is the control-plane API base URL (e.g. https://milo-apiserver...).
	// Required. Project addressing is appended to it; it is never read directly.
	APIURL string
	// BearerToken is the assistant's own service-account token. Carried for a
	// plain Kubernetes apiserver (kind, dev, e2e); on Milo the client
	// certificate below is the credential that works. See internal/auth.
	BearerToken string
	// CACert verifies the SERVER. ClientCert/ClientKey prove who WE are.
	CACert     []byte
	ClientCert []byte
	ClientKey  []byte

	// Timeout bounds one LIST. Zero uses [DefaultFetchTimeout].
	Timeout time.Duration
	// CacheTTL bounds how long a fetched entry is served fresh. Zero uses
	// [DefaultCacheTTL]; a negative value disables caching entirely (every call
	// round-trips and nothing is retained to serve stale).
	CacheTTL time.Duration
	// MaxCacheEntries overrides [maxCacheEntries] when > 0 (tests).
	MaxCacheEntries int

	// Observer, when non-nil, receives one Accepted verdict per binding per
	// LIST — the feedback loop to whoever authored the binding (defect #3).
	// *[StatusWriter] is the production implementation; it writes
	// status.conditions back onto the object, off the request goroutine.
	//
	// It hangs here, rather than the source reporting conditions itself,
	// because convert is the ONE place that sees both the typed KRM item (and
	// therefore metadata.generation, which the CapabilityDocument does not
	// carry) and the per-document validation error. It is also naturally
	// rate-limited: convert runs on a cache MISS, so a project's bindings are
	// observed at most once per [DefaultCacheTTL] no matter how many turns
	// consult them.
	//
	// Nil disables reporting entirely; the source is otherwise unchanged.
	Observer BindingObserver

	// Logger records degradations. Nil discards them.
	Logger *slog.Logger
	// Metrics records the assistant_capability_* series. Nil disables recording.
	Metrics *appmetrics.Metrics

	// Transport overrides the control-plane transport (tests point it at an
	// httptest server). Nil builds the real one from the credentials above.
	Transport http.RoundTripper
	// now overrides the clock (tests). Nil uses time.Now.
	now func() time.Time
}

// Source is the CRD-backed [capability.Source].
type Source struct {
	baseURL  string
	token    string
	client   *http.Client
	ttl      time.Duration
	logger   *slog.Logger
	metrics  *appmetrics.Metrics
	observer BindingObserver
	cache    *documentCache
	now      func() time.Time
}

// New builds the CRD capability source. It fails only on misconfiguration — a
// missing APIURL, or a client keypair that is not a valid pair — because once
// constructed this source never fails a chat: every runtime error degrades.
func New(cfg Config) (*Source, error) {
	if strings.TrimSpace(cfg.APIURL) == "" {
		return nil, fmt.Errorf("crdsource: APIURL is required (the Milo control-plane base URL)")
	}

	transport := cfg.Transport
	if transport == nil {
		t, err := auth.NewControlPlaneTransport(cfg.CACert, cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("crdsource: %w", err)
		}
		transport = t
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	max := cfg.MaxCacheEntries
	if max <= 0 {
		max = maxCacheEntries
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	return &Source{
		baseURL:  strings.TrimRight(cfg.APIURL, "/"),
		token:    cfg.BearerToken,
		client:   &http.Client{Transport: transport, Timeout: timeout},
		ttl:      ttl,
		logger:   logger,
		metrics:  cfg.Metrics,
		observer: cfg.Observer,
		cache:    newDocumentCache(max),
		now:      now,
	}, nil
}

// Documents returns projectName's capability documents, from cache when fresh
// and from the project's control plane otherwise.
//
// The degrade order is FRESH → STALE → EMPTY, and the middle step is the
// point of the whole design. [capability.HTTPSource] returns nil on any
// failure, which makes "the config plane is down" indistinguishable from "this
// project is entitled to nothing": the user gets a built-ins-only assistant and
// no signal either way. Here, a refresh failure serves the last answer this
// process actually received. Only a project with nothing retained — one whose
// entry has been evicted, or whose first-ever fetch failed — falls back to
// empty.
//
// The error return is always nil, as with every other Source: a capability
// outage degrades a chat, it never fails one.
func (s *Source) Documents(ctx context.Context, projectName string) ([]capability.CapabilityDocument, error) {
	now := s.now()

	if entry, ok := s.cache.get(projectName); ok && entry.fresh(now) {
		s.metrics.RecordCapabilityFetch("hit")
		s.metrics.RecordCapabilityCacheEntryAge(now.Sub(entry.storedAt))
		return clone(entry.docs), nil
	}

	docs, err := s.fetch(ctx, projectName)
	if err != nil {
		s.metrics.RecordCapabilityFetch("refresh_failed")
		// Stale beats empty. An entry that has expired is still the last thing
		// the control plane actually said about this project, and serving it is
		// strictly better than serving a project's users an assistant that
		// behaves as though they bought nothing.
		if entry, ok := s.cache.get(projectName); ok {
			age := now.Sub(entry.storedAt)
			s.metrics.RecordCapabilityFetch("stale_served")
			s.metrics.RecordCapabilityCacheEntryAge(age)
			s.logger.Warn("capability.crd.stale_served",
				"projectName", projectName, "ageSeconds", age.Seconds(),
				"documents", len(entry.docs), "error", err.Error())
			return clone(entry.docs), nil
		}
		s.logger.Warn("capability.crd.fetch_failed",
			"projectName", projectName, "error", err.Error(),
			"effect", "no capability documents for this turn (nothing cached to serve stale)")
		return nil, nil
	}

	if len(docs) == 0 {
		// NEVER cache an empty result — the analogue of the SAR cache's
		// never-cache-a-DENY asymmetry. Empty is what a project with no
		// bindings returns AND what a newly-entitled project returns in the
		// window before the catalog's first write lands; caching it would pin
		// that project to a built-ins-only assistant for a TTL, for no reason
		// the user can see. Not caching it means the cache can only ever hold a
		// positive entitlement set, so its worst failure is a redundant LIST,
		// never a withheld capability.
		//
		// A successful empty LIST does drop any retained entry: this is the
		// revocation path, and a deleted binding must stop being served.
		s.cache.delete(projectName)
		s.metrics.RecordCapabilityFetch("empty")
		s.metrics.SetCapabilityCacheEntries(s.cache.len())
		return nil, nil
	}

	s.cache.store(projectName, docs, now, s.ttl)
	s.metrics.RecordCapabilityFetch("miss")
	s.metrics.RecordCapabilityCacheEntryAge(0)
	s.metrics.SetCapabilityCacheEntries(s.cache.len())
	return clone(docs), nil
}

// listURL is the CapabilityBinding LIST for one project. The project appears
// exactly once, in the control-plane prefix, because that is the only place it
// is expressed: it selects the virtual control plane (and with it the etcd key
// prefix) the collection is read from. There is no second, redundant statement
// of tenancy inside the path for the two to drift apart on.
func (s *Source) listURL(projectName string) string {
	gv := capv1alpha1.SchemeGroupVersion
	return auth.ProjectControlPlaneURL(s.baseURL, projectName) +
		fmt.Sprintf(listPathTemplate, gv.Group, gv.Version)
}

// fetch performs one LIST and converts the result. Unlike [Source.Documents] it
// returns errors, because its caller is the one that decides what to degrade to.
func (s *Source) fetch(ctx context.Context, projectName string) (docs []capability.CapabilityDocument, err error) {
	endpoint := s.listURL(projectName)

	// Own timeout, not the caller's: a turn context may be generous (or
	// cancelled early), and the budget for reading configuration is a property
	// of this source, not of the turn it happens to be serving.
	ctx, cancel := context.WithTimeout(ctx, s.client.Timeout)
	defer cancel()

	start := s.now()
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		s.metrics.RecordCapabilityFetchDuration(outcome, s.now().Sub(start))
	}()

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if reqErr != nil {
		return nil, fmt.Errorf("build CapabilityBinding LIST request: %w", reqErr)
	}
	req.Header.Set("Accept", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, doErr := s.client.Do(req)
	if doErr != nil {
		return nil, fmt.Errorf("LIST %s: %w", endpoint, doErr)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, fmt.Errorf("read CapabilityBinding LIST response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 403 here is the expected shape of the standing-read question being
		// unanswered for a project, so the status and the endpoint both belong
		// in the message: "no capabilities" and "not allowed to look" must not
		// read the same in a log.
		return nil, fmt.Errorf("LIST %s returned status %d: %s",
			endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	list, decErr := decodeList(raw)
	if decErr != nil {
		return nil, decErr
	}
	return s.convert(list, projectName, endpoint)
}

// decodeList decodes a LIST response into the typed CapabilityBindingList.
//
// The expected GVK is supplied as the default so a body that omits apiVersion
// or kind still decodes — some servers elide them — while a body that names a
// DIFFERENT kind still fails, which is the check worth keeping.
func decodeList(raw []byte) (*capv1alpha1.CapabilityBindingList, error) {
	gvk := capv1alpha1.SchemeGroupVersion.WithKind("CapabilityBindingList")
	obj, _, err := codecs.UniversalDeserializer().Decode(raw, &gvk, &capv1alpha1.CapabilityBindingList{})
	if err != nil {
		return nil, fmt.Errorf("decode CapabilityBindingList: %w", err)
	}
	list, ok := obj.(*capv1alpha1.CapabilityBindingList)
	if !ok {
		return nil, fmt.Errorf("decode CapabilityBindingList: control plane returned %T", obj)
	}
	return list, nil
}

// convert turns list items into capability documents by re-marshalling each one
// and running it through [capability.ParseDocuments] — the SAME parser and the
// SAME per-document Validate the fixture and HTTP sources use.
//
// It goes through JSON rather than assigning field by field on purpose. The two
// type sets are byte-identical in their JSON tags (pkg/apis/.../types.go is a
// field-for-field port, pinned by its wire_test), but they are NOT
// assignment-compatible — spec.authority.maxTaskDurationSeconds is *int64 on
// the CRD and *int in internal/capability — and a hand-written converter is a
// place for a field to be quietly forgotten, which shows up as a capability
// missing from a prompt and nothing else. One marshal keeps the tags as the
// single contract.
//
// The apiVersion/kind/metadata envelope is included deliberately: a LIST
// response elides TypeMeta on its items, and metadata.name is the key the
// status writer PATCHes back on. Dropping the envelope would leave every
// composed binding nameless, which silently disables the whole status feedback
// loop. metadata.namespace is always empty here (the object is cluster-scoped);
// the project a document belongs to is the project this LIST was issued for,
// carried in projectName, and never read back off a document.
func (s *Source) convert(list *capv1alpha1.CapabilityBindingList, projectName, endpoint string) ([]capability.CapabilityDocument, error) {
	if len(list.Items) == 0 {
		return nil, nil
	}
	// refs runs parallel to items, not to list.Items: an object that fails to
	// marshal is absent from what ParseDocuments sees, so indexing the original
	// slice by the parser's index would name the wrong binding — in a log line
	// and, worse, on a status condition written to somebody else's object.
	type bindingRef struct {
		name       string
		generation int64
	}
	items := make([]json.RawMessage, 0, len(list.Items))
	refs := make([]bindingRef, 0, len(list.Items))
	for i := range list.Items {
		item := list.Items[i]
		item.APIVersion = capv1alpha1.SchemeGroupVersion.String()
		item.Kind = "CapabilityBinding"
		encoded, err := json.Marshal(item)
		if err != nil {
			// Not fatal: one unencodable object must not cost a project its
			// other bindings.
			s.logger.Warn("capability.crd.entry_skipped",
				"url", endpoint, "projectName", projectName,
				"name", item.Name, "stage", "marshal", "error", err.Error())
			continue
		}
		items = append(items, encoded)
		refs = append(refs, bindingRef{name: item.Name, generation: item.Generation})
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("re-encode CapabilityBinding items: %w", err)
	}

	// Skipped indices are collected rather than reported inline so that the
	// Accepted=True verdict for everything else can be derived from the same
	// pass: ParseDocuments reports only failures, and "accepted" is the
	// complement.
	skipped := make(map[int]error, 0)
	docs, err := capability.ParseDocuments(raw, func(index int, skipErr error) {
		name := ""
		if index >= 0 && index < len(refs) {
			name = refs[index].name
			skipped[index] = skipErr
		}
		// The log stays even though the condition now carries the same fact:
		// the condition is for the binding's author, this line is for whoever
		// is looking at the assistant, and a control plane that rejects the
		// status write leaves this as the only channel.
		s.logger.Warn("capability.crd.entry_skipped",
			"url", endpoint, "projectName", projectName,
			"name", name, "index", index, "stage", "validate", "error", skipErr.Error())
	})
	if err != nil {
		// A parse failure is about the whole batch, not about any one binding,
		// so no per-object verdict is claimed: the caller degrades and the
		// existing conditions stay as they were.
		return nil, err
	}

	if s.observer != nil {
		for i, ref := range refs {
			s.observer.ObserveAccepted(projectName, ref.name, ref.generation, skipped[i])
		}
	}
	return docs, nil
}

// clone returns a copy of the document slice so a caller that appends to (or
// reorders) what it was given cannot reach into the cache. The documents
// themselves are treated as immutable by every consumer.
func clone(docs []capability.CapabilityDocument) []capability.CapabilityDocument {
	if len(docs) == 0 {
		return nil
	}
	out := make([]capability.CapabilityDocument, len(docs))
	copy(out, docs)
	return out
}

// ── Cache ─────────────────────────────────────────────────────

// cacheEntry is one project's last known-good documents. Only non-empty results
// are ever stored (see [Source.Documents]).
type cacheEntry struct {
	docs     []capability.CapabilityDocument
	storedAt time.Time
	expiry   time.Time
}

func (e cacheEntry) fresh(now time.Time) bool { return now.Before(e.expiry) }

// documentCache is a bounded, TTL'd map of project → documents. Expired entries
// are RETAINED rather than deleted on read: expiry decides whether an entry may
// be served without a refresh, not whether it is still worth having. What makes
// it worth having is a control plane that has stopped answering.
type documentCache struct {
	mu      sync.Mutex
	max     int
	entries map[string]cacheEntry
}

func newDocumentCache(max int) *documentCache {
	return &documentCache{max: max, entries: make(map[string]cacheEntry)}
}

func (c *documentCache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok
}

// store records docs for key, evicting to stay bounded: expired entries first
// (they are the cheapest to lose), then one live entry. Copied from auth's
// allowCache rather than re-derived.
func (c *documentCache) store(key string, docs []capability.CapabilityDocument, now time.Time, ttl time.Duration) {
	if ttl <= 0 {
		return // caching disabled: nothing retained, nothing served stale
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.max {
		for k, e := range c.entries {
			if !now.Before(e.expiry) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			for k := range c.entries {
				delete(c.entries, k)
				break
			}
		}
	}
	c.entries[key] = cacheEntry{docs: clone(docs), storedAt: now, expiry: now.Add(ttl)}
}

func (c *documentCache) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *documentCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// compile-time assertion that this really is the seam the agent consumes.
var _ capability.Source = (*Source)(nil)
