package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/mcptool"
	"github.com/milo-os/assistant/internal/basetools"
	"github.com/milo-os/assistant/internal/gapreport"
	"github.com/milo-os/assistant/internal/memory"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	"github.com/milo-os/assistant/internal/projectapi"
)

// Tool composition defaults (Tier 2).
const (
	// DefaultMCPConnectTimeout bounds a single MCP server connect.
	DefaultMCPConnectTimeout = 5 * time.Second
	// ToolNamespaceSeparator joins the server shortname and the provider tool
	// name into the model-facing tool name.
	ToolNamespaceSeparator = "__"
	// defaultMCPClientName is announced to MCP servers during initialization.
	defaultMCPClientName = "datum-assistant-service"
	// gapKeyLookupTimeout bounds the per-service capability-key read Compose
	// does on every turn. The key list only improves de-duplication in a
	// provider's feed, so a slow store must never hold up the user's answer.
	gapKeyLookupTimeout = 2 * time.Second
)

// ProviderToolInvocation is reported once per provider-tool execution (the
// metering hook). It identifies the provider service and both the raw and
// namespaced tool names.
type ProviderToolInvocation struct {
	// ServiceName is the reverse-DNS provider service name from the document.
	ServiceName string
	// ServerName is the mcpServers[].name shortname the tool is namespaced under.
	ServerName string
	// ToolName is the original (un-namespaced) provider tool name.
	ToolName string
	// NamespacedToolName is the name the model sees, "<server>__<tool>".
	NamespacedToolName string
}

// mcpSession is the subset of a connected MCP session that composition needs.
// *[mcptool.Session] satisfies it; tests inject a fake.
type mcpSession interface {
	Tools(ctx context.Context) (map[string]agentcore.Tool, error)
	Close() error
}

// mcpConnector opens a session to the MCP server at endpoint, sending headers
// on every request of that session. It is injectable so tests can compose
// without a live server.
type mcpConnector func(ctx context.Context, endpoint string, headers map[string]string) (mcpSession, error)

// ComposeOptions configures [Compose]. All fields are optional.
type ComposeOptions struct {
	// HTTPClient fetches knowledge sources. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// KnowledgeTimeout, KnowledgeMaxBytesPerSource, KnowledgeMaxSourcesPerService
	// override the Tier-1 defaults when > 0.
	KnowledgeTimeout              time.Duration
	KnowledgeMaxBytesPerSource    int
	KnowledgeMaxSourcesPerService int
	// MCPConnectTimeout overrides [DefaultMCPConnectTimeout] when > 0.
	MCPConnectTimeout time.Duration
	// SkillTimeout and SkillMaxBytes override the skill-body fetch defaults
	// when > 0 (see DefaultSkillTimeout / DefaultSkillMaxBytes).
	SkillTimeout  time.Duration
	SkillMaxBytes int
	// connect is the MCP connector seam. Nil uses the real mcptool client.
	connect mcpConnector
	// OnToolInvocation, if set, fires once at the start of every provider-tool
	// execution (wired to usage metering by the caller).
	OnToolInvocation func(ProviderToolInvocation)
	// OnDocumentComposed, if set, fires exactly ONCE PER SCOPED DOCUMENT at the
	// end of composition — successes included — with a nil err when that
	// document's declared capabilities came up and a non-nil one naming what did
	// not. It is the seam the CRD source's status writer hangs the Composed
	// condition off (crdsource.BindingObserver.ObserveComposed), so that a
	// provider whose MCP endpoint is unreachable learns it from
	// `kubectl describe capabilitybinding` instead of from a warn line inside
	// someone else's service.
	//
	// Successes are reported on purpose. A condition that is only ever written
	// False is a trap: it would pin a binding to "degraded" forever after one bad
	// turn, and the operator who fixed the endpoint would have no signal that
	// they had. The status writer coalesces on transition, so a steady outcome —
	// True or False — costs one write, not one per turn.
	//
	// # What counts as "not composed"
	//
	// The condition means: everything this binding DECLARED is actually usable on
	// this turn. Aggregated into a non-nil err:
	//
	//   - an MCP endpoint refused by the operator's dial allow-list
	//     (capability.mcp.endpoint_not_sanctioned) — the binding names a host the
	//     platform will not dial, so its tools are unreachable by construction;
	//   - an MCP server that failed to connect or timed out;
	//   - a server that connected but could not list its tools;
	//   - a declared include-list tool the server does not offer;
	//   - a tool name already registered by another binding in this project, so
	//     this document's tool is not the one the model can call;
	//   - TOTAL knowledge loss: every one of a document's knowledge sources
	//     failed (see renderServiceKnowledge for why total rather than any).
	//
	// Deliberately NOT aggregated, because each leaves a working binding and
	// would only make the condition noisier than the thing it reports:
	//
	//   - a knowledge source that failed while its siblings succeeded, and a body
	//     truncated at the byte cap — the knowledge arrived, just less of it;
	//   - a skill that could not be fetched. Skill bodies are fetched lazily by
	//     the load_skill tool DURING the turn, not during composition, so no
	//     verdict about them exists at this point. Indexing a skill cannot fail.
	//
	// A document with no tools and no knowledge sources — knowledge concepts
	// only, or skills only — trivially composes and is reported with a nil err.
	// There is nothing it promised that could have failed.
	//
	// It runs on the request goroutine after the last session is opened, so an
	// implementation MUST NOT block. A panicking callback is recovered and logged
	// rather than allowed to take down the turn: this is a reporting hook, and no
	// bug in reporting is worth failing a user's chat over.
	OnDocumentComposed func(doc CapabilityDocument, err error)
	// Logger receives composition warnings. Nil discards them.
	Logger *slog.Logger
	// AllowPrivateNetworks disables the SSRF IP guard's private/loopback/
	// link-local block for the knowledge, skill, and MCP fetches. It is the
	// dev/cluster escape hatch: local overlays address services over loopback
	// and in-cluster (private) IPs, which the guard blocks by default. It MUST
	// stay false in production. Default false = guard on (safe).
	AllowPrivateNetworks bool
	// AllowedHosts and AllowedCIDRs, when either is non-empty, switch the SSRF
	// guard from its default "block private" IP-policy into an allow-list posture
	// for UNTRUSTED providers: a capability-document URL (knowledge source, skill
	// source, MCP endpoint) is permitted only if its host matches an AllowedHosts
	// entry or resolves into an AllowedCIDRs range. AllowedHosts entries match a
	// host exactly and as a domain suffix ("example.com" permits "example.com"
	// and "api.example.com"); AllowedCIDRs is the reviewed gateway range(s).
	//
	// The always-blocked set (link-local/IMDS, unspecified, multicast) still
	// holds in allow-list mode — an allow-listed host that resolves to metadata
	// is refused. When both are empty the guard keeps its IP-policy behavior
	// (backward compatible). The integrator populates these from config.
	AllowedHosts []string
	AllowedCIDRs []string
	// Caller is the calling user's own credential, forwarded (with
	// ExpectedProject) only to endpoints in IdentityForwardHosts so a provider
	// can read as the caller instead of holding standing access of its own.
	Caller CallerIdentity
	// IdentityForwardHosts is the OPERATOR-sanctioned set of MCP endpoint hosts
	// that may receive Caller — matched exactly and as a domain suffix. Kept
	// separate from the SSRF allow-list on purpose: that one answers "may we
	// connect at all", this the far narrower "may we hand this endpoint the
	// user's credential". Empty (the default) forwards to nobody. See identity.go.
	IdentityForwardHosts []string
	// AllowedMCPEndpointHosts is the OPERATOR-sanctioned set of hosts an MCP
	// endpoint may be DIALED at — matched exactly and as a domain suffix, the
	// same shape as IdentityForwardHosts. In this platform that list is the AI
	// gateway, because every provider tool call is supposed to traverse it.
	//
	// It exists because the component that rewrites a provider endpoint to the
	// gateway's MCPRoute URL (the catalog projection controller) is no longer the
	// component that dials it (this service), and the two can drift. A controller
	// regression that published a raw provider endpoint would otherwise be dialed
	// faithfully, and three things would break at once without any of them
	// looking like a projection bug: the gateway-side allow-list — the copy a
	// compromised assistant cannot bypass — stops being consulted, provider tool
	// calls stop being metered, and caller-identity forwarding fails closed
	// because the raw host is not in IdentityForwardHosts, so read-as-the-caller
	// tools start failing as if the provider were broken.
	//
	// Deliberately NOT the SSRF allow-list above: that one governs all three
	// provider-URL sinks, and knowledge sources and skill bodies legitimately
	// point at a provider's own documentation hosts, which pinning it to the
	// gateway would break. Deliberately NOT IdentityForwardHosts either, even
	// though production will hold the same value in both: "may be dialed at all"
	// and "may receive the user's credential" are different predicates, and
	// sharing one list means widening the reachable set silently widens the set
	// that gets handed a bearer token.
	//
	// Enforced in connectTools before the connector touches the network, not at
	// CRD admission — the writer being defended against is the controller that
	// holds write access to these objects. A non-matching endpoint is skipped and
	// logged (capability.mcp.endpoint_not_sanctioned) like any other unreachable
	// server; it never fails the whole composition. Empty (the default) disables
	// the check entirely, which keeps fixtures, e2e, and dev overlays working.
	AllowedMCPEndpointHosts []string
	// ExpectedProject, when set, is the Milo project of the calling request. It
	// is the tenant-isolation check on the capability Source: any document that
	// NAMES a namespace disagreeing with it is dropped and logged rather than
	// trusted. A document with no namespace is kept — that is every
	// CapabilityBinding, which is cluster-scoped inside its project's own
	// control plane, where the plane itself is the isolation boundary. See
	// [ScopeDocuments].
	ExpectedProject string
	// Memory, when non-nil, enables the memory_remember / memory_forget
	// built-in tools (see internal/capability/memory.go) scoped to
	// ExpectedProject, plus a "Project memory:" addendum section listing the
	// project's current facts. Nil disables the feature entirely — no tools,
	// no addendum section. Also requires ExpectedProject to be set, since the
	// tools need a project to scope reads/writes to.
	Memory memory.Store
	// ContextID, when set, is the conversation this composition is running
	// for. It is threaded into gap reports as provenance only (see
	// GapReports) — it plays no role in tool scoping or tenant isolation.
	ContextID string
	// GapReports, when non-nil, enables one report_capability_gap__<service>
	// tool per composed document that declares spec.reportingProject (see
	// internal/capability/gapreport.go): the model can flag that a provider
	// service is missing a tool or lookup a user needed, and the report is
	// written to that PROVIDER's own project — never ExpectedProject, the
	// consumer's project — so the team that owns the missing capability sees
	// it without the user having to report it themselves. A document with
	// tools but no reportingProject simply gets no gap-report tool. Nil
	// disables the feature entirely.
	GapReports gapreport.Store
	// PlatformAPI, when non-nil, adds the base platform tools (see
	// internal/basetools) to every project's composition: read this project's
	// resources of any kind, describe a kind, list where a service is offered,
	// report what the allowance has left. They are not one provider's
	// contribution, so they are not namespaced under a service and are not
	// allow-listed by a capability document — they are what every project has.
	//
	// They run as the CALLER and only as the caller: the client is bound to
	// Caller.BearerToken and ExpectedProject here, once, and the tools receive
	// a view that carries no other identity. With either missing there is
	// nobody to act as, so nothing is composed. Nil disables the feature.
	PlatformAPI *projectapi.Client
	// PlanTokenKey binds plans for the base tools' change path
	// (resources_validate, resources_plan, resources_apply). Empty leaves the
	// change path out: a service that cannot check a token must not issue one.
	// Ignored when PlatformAPI is nil. See internal/plantoken.
	PlanTokenKey []byte
	// Metrics, when non-nil, records assistant_gap_report_total for every
	// report_capability_gap tool call this composition creates (see
	// internal/metrics). Nil disables recording only — GapReports still
	// governs whether the tool is composed at all.
	Metrics *appmetrics.Metrics
	// resolver is the DNS seam for the SSRF guard. Nil uses net.DefaultResolver.
	resolver ipResolver
}

// knownCapabilityKeys reads the capability keys already filed against one
// service, for injection into that service's gap-report tool schema. It
// DEGRADES to nil on any failure, like every other store read in composition: a
// conversation must not fail because a bookkeeping lookup did.
//
// Only bare keys cross this boundary. The result lands in one consumer's
// conversation and the keys were coined in others' — a key is a bounded slug
// naming the PROVIDER's own capability, whereas the report prose beside it is
// text about someone else's session and has no business travelling.
func knownCapabilityKeys(ctx context.Context, store gapreport.Store, doc CapabilityDocument, logger *slog.Logger) []string {
	if doc.Spec.ReportingProject == "" || doc.Spec.ServiceName == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gapKeyLookupTimeout)
	defer cancel()
	keys, err := store.CapabilityKeys(ctx, doc.Spec.ReportingProject, doc.Spec.ServiceName, MaxInjectedCapabilityKeys)
	if err != nil {
		logger.Warn("capability.gapreport.keys_failed",
			"service", doc.Spec.ServiceName, "provider_project", doc.Spec.ReportingProject, "error", err.Error())
		return nil
	}
	return keys
}

// Composed is the result of [Compose]: the knowledge addendum for the system
// prompt and the allow-listed, namespaced provider tools ready to drive the
// loop. Close tears down the per-request MCP sessions.
type Composed struct {
	// SystemPromptAddendum is "" when no document contributed knowledge.
	SystemPromptAddendum string
	// Tools holds the allow-listed provider tools, keyed and named
	// "<server>__<tool>", together with the platform's own built-ins.
	Tools agentcore.ToolSet
	// Mutating names the composed tools on a change path: those a capability
	// document flagged in mcpServers[].mutating, plus the base tools' change
	// path. It answers what this project's assistant can change. Sorted, so
	// two compositions of the same project read alike.
	Mutating []string
	close    func() error
}

// IsMutating reports whether a composed tool is on a change path.
func (c *Composed) IsMutating(name string) bool {
	for _, mutating := range c.Mutating {
		if mutating == name {
			return true
		}
	}
	return false
}

// Close closes every MCP session opened during composition. It is safe to call
// more than once.
func (c *Composed) Close() error {
	if c.close == nil {
		return nil
	}
	return c.close()
}

// Compose turns a project's capability documents into composed capabilities:
// it fetches knowledge into a provenance-labelled addendum and connects each
// document's MCP servers, exposing only the allow-listed tools, namespaced and
// de-collided. A server that cannot be reached (or a tool that is missing)
// contributes nothing and is logged — it never fails the whole composition.
func Compose(ctx context.Context, docs []CapabilityDocument, opts ComposeOptions) (*Composed, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// Tenant isolation: drop any document the Source mis-scoped before it
	// contributes knowledge or tools (no-op unless ExpectedProject is set).
	docs = ScopeDocuments(docs, opts.ExpectedProject, logger, ScopeMetrics(opts.Metrics))

	// One SSRF guard drives all three provider-URL sinks (knowledge, skills,
	// MCP). The knowledge/skill fetches share a guarded HTTP client; the MCP
	// connect path re-checks the endpoint host before connecting.
	guard := newIPGuard(opts.AllowPrivateNetworks, opts.resolver)
	allow, err := parseHostAllowList(opts.AllowedHosts, opts.AllowedCIDRs)
	if err != nil {
		return nil, err
	}
	guard.allow = allow
	httpClient := guard.wrapClient(opts.HTTPClient)

	// Sanctioned identity-forwarding hosts. A malformed entry fails composition
	// closed, exactly as a malformed SSRF allow-list entry does.
	sanctioned, err := parseHostAllowList(opts.IdentityForwardHosts, nil)
	if err != nil {
		return nil, err
	}

	addendum, knowledgeErrs := buildKnowledgeAddendum(ctx, docs, knowledgeOptions{
		httpClient:           httpClient,
		guard:                guard,
		timeout:              opts.KnowledgeTimeout,
		maxBytesPerSource:    opts.KnowledgeMaxBytesPerSource,
		maxSourcesPerService: opts.KnowledgeMaxSourcesPerService,
		logger:               logger,
	})

	// Sanctioned MCP endpoint hosts (the gateway). Same fail-closed parse as the
	// two allow-lists above: a mis-typed entry must not silently leave the dial
	// site unguarded.
	if err := validateEndpointHosts(opts.AllowedMCPEndpointHosts); err != nil {
		return nil, err
	}
	dialable, err := parseHostAllowList(opts.AllowedMCPEndpointHosts, nil)
	if err != nil {
		return nil, err
	}

	tools, sessions, toolErrs := connectTools(ctx, docs, opts, guard, sanctioned, dialable, logger)

	// One verdict per scoped document, successes included. Reported here rather
	// than inside connectTools because a document's outcome spans both tiers,
	// and because a caller must get exactly one callback per document however
	// many servers or sources it declared.
	reportComposed(docs, knowledgeErrs, toolErrs, opts.OnDocumentComposed, logger)

	// What the document declared as mutating, namespaced like its tools, kept
	// only where the tool was actually composed.
	mutating := map[string]bool{}
	for _, doc := range docs {
		if doc.Spec.Tools == nil {
			continue
		}
		for _, server := range doc.Spec.Tools.MCPServers {
			for _, toolName := range server.Mutating {
				namespaced := NamespaceToolName(server.Name, toolName)
				if _, composed := tools[namespaced]; composed {
					mutating[namespaced] = true
				}
			}
		}
	}

	// Skills: descriptions into the prompt, bodies behind the built-in
	// load_skill tool (progressive disclosure).
	skills := collectSkills(docs, logger)
	if len(skills) > 0 {
		index := buildSkillsIndex(skills)
		if addendum == "" {
			addendum = index
		} else {
			addendum = addendum + "\n\n" + index
		}
		if _, exists := tools[LoadSkillToolName]; !exists {
			tools[LoadSkillToolName] = newLoadSkillTool(skills, opts, httpClient, guard, logger)
		}
	}

	// Project memory: registers memory_remember/memory_forget and folds the
	// project's current facts into the addendum, exactly like the skills
	// index above. A List failure degrades to "no facts shown" rather than
	// failing composition — the tools still register, so the model can still
	// remember things even if reading the current snapshot failed.
	if opts.Memory != nil && opts.ExpectedProject != "" {
		facts, err := opts.Memory.List(ctx, opts.ExpectedProject)
		if err != nil {
			logger.Warn("capability.memory.list_failed", "project", opts.ExpectedProject, "error", err.Error())
		}
		if index := buildMemoryIndex(facts); index != "" {
			if addendum == "" {
				addendum = index
			} else {
				addendum = addendum + "\n\n" + index
			}
		}
		if _, exists := tools[RememberMemoryToolName]; !exists {
			tools[RememberMemoryToolName] = &rememberMemoryTool{store: opts.Memory, project: opts.ExpectedProject}
		}
		if _, exists := tools[ForgetMemoryToolName]; !exists {
			tools[ForgetMemoryToolName] = &forgetMemoryTool{store: opts.Memory, project: opts.ExpectedProject}
		}
	}

	// Base platform tools: always present, never namespaced, always the
	// caller's own identity. Registered before the gap-report tools and after
	// the provider ones, which cannot collide with them: every provider name
	// carries the "<server>__" prefix.
	if opts.PlatformAPI != nil && opts.ExpectedProject != "" && opts.Caller.BearerToken != "" {
		base := basetools.Tools(basetools.Options{
			Project:      opts.PlatformAPI.As(opts.ExpectedProject, opts.Caller.BearerToken),
			PlanTokenKey: opts.PlanTokenKey,
			Logger:       logger,
		})
		for name, t := range base {
			if _, exists := tools[name]; exists {
				continue // first registration wins, deterministically
			}
			tools[name] = t
		}
		if len(base) > 0 {
			writePath := false
			for _, name := range basetools.MutatingToolNames() {
				if _, composed := base[name]; composed {
					mutating[name] = true
					writePath = true
				}
			}
			if section := basetools.PromptSection(writePath); section != "" {
				if addendum == "" {
					addendum = section
				} else {
					addendum = addendum + "\n\n" + section
				}
			}
		}
	}

	// Capability-gap reporting: one tool instance per document that declares
	// a ReportingProject, closed over THAT document's own ServiceName and
	// ReportingProject. The model's tool input never carries a project or
	// service — it can only pick which of these pre-scoped tool instances to
	// call — so a report can never be misdirected to a provider it wasn't
	// actually composed from.
	if opts.GapReports != nil {
		for _, doc := range docs {
			if doc.Spec.ReportingProject == "" {
				continue
			}
			name := GapReportToolName(doc.Spec.ServiceRef.Name)
			if _, exists := tools[name]; exists {
				continue // first registration wins, deterministically
			}
			tools[name] = &reportCapabilityGapTool{
				store:           opts.GapReports,
				name:            name,
				serviceName:     doc.Spec.ServiceName,
				providerProject: doc.Spec.ReportingProject,
				consumerProject: opts.ExpectedProject,
				contextID:       opts.ContextID,
				knownKeys:       knownCapabilityKeys(ctx, opts.GapReports, doc, logger),
				metrics:         opts.Metrics,
			}
		}
	}

	mutatingNames := make([]string, 0, len(mutating))
	for name := range mutating {
		mutatingNames = append(mutatingNames, name)
	}
	sort.Strings(mutatingNames)

	closed := false
	return &Composed{
		SystemPromptAddendum: addendum,
		Tools:                tools,
		Mutating:             mutatingNames,
		close: func() error {
			if closed {
				return nil
			}
			closed = true
			for _, s := range sessions {
				_ = s.Close()
			}
			return nil
		},
	}, nil
}

// ScopeOption tunes [ScopeDocuments]. It is variadic rather than a parameter so
// the call sites that only want the gate — tests, the card path — are not forced
// to name observability wiring they do not have.
type ScopeOption func(*scopePolicy)

type scopePolicy struct {
	metrics *appmetrics.Metrics
}

// ScopeMetrics records every dropped document as
// assistant_capability_scope_dropped_total, labeled by reason. Tenant isolation
// is the one boundary whose enforcement must not be visible only by grepping a
// log file: without this, an operator cannot alert on a Source that has started
// offering another project's documents, and the first sign of it is a support
// ticket. Nil is tolerated (the counter is simply not recorded).
func ScopeMetrics(m *appmetrics.Metrics) ScopeOption { return func(p *scopePolicy) { p.metrics = m } }

// ScopeDocuments is the tenant-isolation gate for documents that CARRY their
// own namespace: it drops any document whose Metadata.Namespace names a project
// other than expectedProject, so that a Source bug — or a compromised fan-out —
// cannot spend another tenant's knowledge, MCP endpoints, or tools inside this
// project's turn. A drop is logged as capability.scope.rejected and counted as
// assistant_capability_scope_dropped_total{reason="mismatch"}.
//
// A document with NO namespace is kept, and under the production source that is
// the normal case rather than a tolerated legacy one. Tenant isolation for the
// CapabilityBinding CRD is STRUCTURAL, not a field comparison: a Milo project is
// a virtual control plane over one apiserver partitioned by an etcd key prefix,
// the LIST is addressed to that project's control-plane path, and so the
// response can only ever contain that project's objects. The objects are
// cluster-scoped inside their plane and carry no namespace at all — there is
// nothing here left to check, and checking would mean dropping every document.
//
// This function therefore guards the sources whose payload does carry a
// namespace — the fixture export and the HTTP provider endpoint, both of which
// serve JSON that could name any project — where it remains defense in depth
// behind an already project-scoped Source.
//
// A previous revision offered a fail-closed "require a namespace" posture for
// the CRD source, written when the design assumed a cluster-wide informer over
// namespace-per-project objects. Both halves of that assumption are false, so
// the option is gone rather than merely defaulted off: an option that can only
// ever reject 100% of production traffic is a loaded gun, not a safety.
//
// With no expectedProject there is nothing to compare against and the check is
// disabled entirely — documents pass through unchanged.
func ScopeDocuments(docs []CapabilityDocument, expectedProject string, logger *slog.Logger, opts ...ScopeOption) []CapabilityDocument {
	var policy scopePolicy
	for _, opt := range opts {
		opt(&policy)
	}
	if expectedProject == "" {
		return docs
	}
	kept := make([]CapabilityDocument, 0, len(docs))
	for _, doc := range docs {
		ns := ""
		if doc.Metadata != nil {
			ns = doc.Metadata.Namespace
		}
		if ns != "" && ns != expectedProject {
			logger.Warn("capability.scope.rejected",
				"service", doc.Spec.ServiceName, "documentNamespace", ns, "expectedProject", expectedProject)
			policy.metrics.RecordCapabilityScopeDropped("mismatch")
			continue
		}
		kept = append(kept, doc)
	}
	return kept
}

// validateEndpointHosts rejects an AllowedMCPEndpointHosts entry that is not a
// bare host. parseHostAllowList only hard-errors on a malformed CIDR, and this
// knob takes no CIDRs, so without this a mis-typed entry — a full URL
// ("https://gateway.example/mcp"), a host:port, a CIDR — would normalize into a
// string that can never match u.Hostname() and the allow-list would quietly
// sanction nothing. That failure mode is the worst one available here: every
// endpoint gets skipped and provider tools disappear cluster-wide with no
// configuration error to point at. Fail closed at startup instead.
func validateEndpointHosts(entries []string) error {
	for _, e := range entries {
		trimmed := strings.TrimSpace(e)
		if trimmed == "" {
			continue // blank entries are dropped, as elsewhere
		}
		if strings.ContainsAny(trimmed, "/ \t") {
			return fmt.Errorf("invalid MCP endpoint host %q: expected a bare host, not a URL, host:port, or CIDR", e)
		}
	}
	return nil
}

// sanctionedEndpointHost reports whether endpoint may be dialed under the
// operator's MCP endpoint allow-list, and if not, why — the reason is logged so
// an operator reading it sees the offending host without having to go find the
// document. An empty list disables the check (every endpoint is dialable).
//
// Matching is on the NAME only, never on resolved addresses, for the same
// reason identityHeaders does so: an operator sanctions a name, and someone who
// can steer DNS must not be able to talk their way onto the list.
func sanctionedEndpointHost(endpoint string, dialable *hostAllowList) (string, bool) {
	if dialable.empty() {
		return "", true
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "endpoint is not a parseable URL", false
	}
	host := u.Hostname()
	if host == "" {
		return "endpoint has no host", false
	}
	if !dialable.permits(host, nil) {
		return "host is not an operator-sanctioned MCP endpoint host", false
	}
	return "", true
}

// reportComposed fires OnDocumentComposed once per document, folding that
// document's knowledge and tool verdicts into a single provider-facing error.
//
// Both input slices are index-aligned with docs and carry one entry per
// document (nil meaning "nothing wrong"), which is what makes "exactly one
// callback per document, successes included" a property of the shape rather
// than of the control flow above.
func reportComposed(docs []CapabilityDocument, knowledgeErrs, toolErrs []error, fn func(CapabilityDocument, error), logger *slog.Logger) {
	if fn == nil {
		return
	}
	for i, doc := range docs {
		var knowledgeErr, toolErr error
		if i < len(knowledgeErrs) {
			knowledgeErr = knowledgeErrs[i]
		}
		if i < len(toolErrs) {
			toolErr = toolErrs[i]
		}
		notifyComposed(fn, doc, errors.Join(toolErr, knowledgeErr), logger)
	}
}

// notifyComposed makes one callback survivable. The hook is a REPORTING seam
// owned by the caller (today: a status-condition writer), and a panic in it
// would otherwise unwind Compose and fail a user's chat turn over a defect in
// bookkeeping — the exact inversion of priorities this whole feature is written
// against. Recovered, logged, and the next document is still reported.
func notifyComposed(fn func(CapabilityDocument, error), doc CapabilityDocument, err error, logger *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("capability.compose.observer_panic",
				"service", doc.Spec.ServiceName, "panic", fmt.Sprint(r),
				"effect", "this document's composition status was not reported; the turn is unaffected")
		}
	}()
	fn(doc, err)
}

// connectTools connects each document's MCP servers and returns the exposed
// tools, the sessions to close, and a per-document verdict index-aligned with
// docs (nil where every declared server and tool came up).
//
// The verdict slice is why this returns three values instead of two: every
// failure below was already a warn line, but a warn line lands in the
// assistant's log, and the person who can fix an unreachable endpoint reads the
// binding object instead. Aggregating per DOCUMENT rather than per server is
// what the Composed condition needs — the condition describes one binding, and
// one binding may declare several servers.
func connectTools(ctx context.Context, docs []CapabilityDocument, opts ComposeOptions, guard *ipGuard, sanctioned, dialable *hostAllowList, logger *slog.Logger) (agentcore.ToolSet, []mcpSession, []error) {
	connect := opts.connect
	if connect == nil {
		connect = guardedConnector(defaultConnector(), guard)
	}
	timeout := opts.MCPConnectTimeout
	if timeout <= 0 {
		timeout = DefaultMCPConnectTimeout
	}

	tools := agentcore.ToolSet{}
	var sessions []mcpSession
	docErrs := make([]error, len(docs))

	for i, doc := range docs {
		if doc.Spec.Tools == nil {
			continue
		}
		serviceName := doc.Spec.ServiceName
		// Per-document accumulator: a document with three servers of which one is
		// down is still degraded, and the condition should name the one that is.
		var failures []error
		for _, server := range doc.Spec.Tools.MCPServers {
			// Before dialing: the endpoint must be one the operator sanctioned.
			// A distinct event from capability.mcp.connect_failed on purpose —
			// "the projection published a non-gateway endpoint" and "the gateway
			// is down" are different incidents with different owners, and an
			// operator must be able to separate them in a log query.
			if reason, ok := sanctionedEndpointHost(server.Endpoint, dialable); !ok {
				logger.Warn("capability.mcp.endpoint_not_sanctioned",
					"service", serviceName, "server", server.Name, "endpoint", server.Endpoint, "reason", reason)
				// Worded so the provider can tell this apart from an outage: the
				// endpoint was never dialed, and no amount of waiting will change
				// that. It is a configuration verdict, not a reachability one.
				failures = append(failures, fmt.Errorf(
					"mcp server %q: endpoint %s was not dialed: %s", server.Name, server.Endpoint, reason))
				continue
			}

			headers := identityHeaders(server.Endpoint, opts.Caller, opts.ExpectedProject, sanctioned)
			session, err := connectWithTimeout(ctx, connect, server.Endpoint, headers, timeout)
			if err != nil {
				logger.Warn("capability.mcp.connect_failed",
					"service", serviceName, "server", server.Name, "endpoint", server.Endpoint, "error", err.Error())
				// Names the server and the endpoint, not the transport error:
				// that string carries resolved addresses and the proxy chain,
				// which is this cluster's topology rather than anything the
				// provider can act on. "connect failed" plus the endpoint is the
				// actionable part; the full error stays in the assistant's log for
				// the operator, one grep away by server name.
				failures = append(failures, fmt.Errorf(
					"mcp server %q: connect to %s failed", server.Name, server.Endpoint))
				continue
			}
			sessions = append(sessions, session)

			// Bound tools/list with the same budget as connect: a provider that
			// connects then hangs on the list call must never stall a chat turn.
			// On timeout we degrade to no tools for this provider.
			listCtx, cancelList := context.WithTimeout(ctx, timeout)
			serverTools, err := session.Tools(listCtx)
			cancelList()
			if err != nil {
				logger.Warn("capability.mcp.list_failed",
					"service", serviceName, "server", server.Name, "error", err.Error())
				failures = append(failures, fmt.Errorf(
					"mcp server %q: connected, but listing tools failed", server.Name))
				continue
			}

			for _, toolName := range server.ToolSelector.Include {
				providerTool, ok := serverTools[toolName]
				if !ok {
					logger.Warn("capability.mcp.tool_missing",
						"service", serviceName, "server", server.Name, "tool", toolName)
					failures = append(failures, fmt.Errorf(
						"mcp server %q: tool %q is not offered by the server", server.Name, toolName))
					continue
				}
				namespaced := NamespaceToolName(server.Name, toolName)
				if _, exists := tools[namespaced]; exists {
					logger.Warn("capability.mcp.tool_collision",
						"service", serviceName, "server", server.Name, "tool", namespaced)
					// Reported as a degradation because THIS document's tool is
					// not the one the model can reach: another binding won the
					// name. Deterministic (first registration wins), so it is a
					// steady condition and one write, and the fix — rename the
					// server — belongs to whoever reads this binding.
					failures = append(failures, fmt.Errorf(
						"tool %q is already provided by another binding in this project", namespaced))
					continue // first registration wins, deterministically
				}
				invocation := ProviderToolInvocation{
					ServiceName:        serviceName,
					ServerName:         server.Name,
					ToolName:           toolName,
					NamespacedToolName: namespaced,
				}
				tools[namespaced] = &meteredTool{
					inner:    providerTool,
					def:      namespacedDefinition(providerTool.Definition(), namespaced),
					onInvoke: opts.OnToolInvocation,
					invoke:   invocation,
				}
			}
		}
		docErrs[i] = errors.Join(failures...)
	}

	return tools, sessions, docErrs
}

// meteredTool wraps a provider tool: it presents the namespaced name to the
// model and fires the metering hook at the start of every execution.
type meteredTool struct {
	inner    agentcore.Tool
	def      agentcore.ToolDefinition
	onInvoke func(ProviderToolInvocation)
	invoke   ProviderToolInvocation
}

func (t *meteredTool) Definition() agentcore.ToolDefinition { return t.def }

func (t *meteredTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	if t.onInvoke != nil {
		t.onInvoke(t.invoke)
	}
	return t.inner.Execute(ctx, input)
}

// namespacedDefinition copies a tool definition, replacing the model-facing
// name with its namespaced form.
func namespacedDefinition(def agentcore.ToolDefinition, namespaced string) agentcore.ToolDefinition {
	def.Name = namespaced
	return def
}

var toolNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// NamespaceToolName renders the model-facing name "<server>__<tool>", with
// both parts sanitized to the [a-zA-Z0-9_-] set model providers require.
func NamespaceToolName(serverName, toolName string) string {
	return SanitizeName(serverName) + ToolNamespaceSeparator + SanitizeName(toolName)
}

// SanitizeName reduces a provider-supplied name to the character set tool and
// skill identifiers use. Exported so anything deriving an identifier from a
// document (the agent card's per-service skill IDs) applies the same rule as
// [NamespaceToolName] rather than passing an arbitrary provider string through.
func SanitizeName(v string) string { return toolNameSanitizer.ReplaceAllString(v, "-") }

// guardedConnector wraps a connector with the SSRF guard: it refuses to connect
// to an endpoint whose scheme is disallowed or that resolves to a non-routable
// address (loopback, private, link-local incl. cloud IMDS) before the inner
// connector touches the network. Test connectors injected via ComposeOptions
// bypass this — they never reach the real network.
func guardedConnector(inner mcpConnector, guard *ipGuard) mcpConnector {
	return func(ctx context.Context, endpoint string, headers map[string]string) (mcpSession, error) {
		if err := guard.checkURL(ctx, endpoint); err != nil {
			return nil, err
		}
		return inner(ctx, endpoint, headers)
	}
}

// defaultConnector opens real MCP sessions via the mcptool client.
func defaultConnector() mcpConnector {
	return func(ctx context.Context, endpoint string, headers map[string]string) (mcpSession, error) {
		return mcptool.Connect(ctx, mcptool.Options{
			Endpoint:   endpoint,
			ClientName: defaultMCPClientName,
			Headers:    headers,
		})
	}
}

// connectWithTimeout races a connect against a deadline. If the deadline wins,
// it returns an error but still closes a session that arrives late, so a slow
// server never leaks a connection.
func connectWithTimeout(ctx context.Context, connect mcpConnector, endpoint string, headers map[string]string, timeout time.Duration) (mcpSession, error) {
	type result struct {
		session mcpSession
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		session, err := connect(ctx, endpoint, headers)
		ch <- result{session, err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.session, r.err
	case <-timer.C:
		go func() {
			// Close only a session the connector RETURNED SUCCESSFULLY. On
			// failure the connector returns a typed-nil *Session in a non-nil
			// mcpSession interface (the classic Go nil-interface trap), so a
			// bare `r.session != nil` would be true and Close() would panic on
			// the nil receiver — crashing the process from this goroutine. Gate
			// on err == nil so a slow-then-failed connect degrades quietly.
			if r := <-ch; r.err == nil && r.session != nil {
				_ = r.session.Close()
			}
		}()
		return nil, fmt.Errorf("timed out after %s", timeout)
	}
}

// compile-time check that the real session type satisfies the seam.
var _ mcpSession = (*mcptool.Session)(nil)
