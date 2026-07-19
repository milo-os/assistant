package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/mcptool"
	"github.com/milo-os/assistant/internal/gapreport"
	"github.com/milo-os/assistant/internal/memory"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
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

// mcpConnector opens a session to the MCP server at endpoint. It is injectable
// so tests can compose without a live server.
type mcpConnector func(ctx context.Context, endpoint string) (mcpSession, error)

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
	// ExpectedProject, when set, is the namespace/project of the calling request.
	// It is a defense-in-depth tenant-isolation check on the capability Source:
	// the Source is responsible for returning only the calling project's
	// documents, but any document whose Metadata.Namespace disagrees with
	// ExpectedProject is dropped and logged rather than trusted. Documents that
	// carry no namespace are passed through — for those the Source remains the
	// scoping authority (the CRD projection has no spec-level project field to
	// cross-check; if one is added later, extend scopeDocuments to verify it).
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
	// Metrics, when non-nil, records assistant_gap_report_total for every
	// report_capability_gap tool call this composition creates (see
	// internal/metrics). Nil disables recording only — GapReports still
	// governs whether the tool is composed at all.
	Metrics *appmetrics.Metrics
	// resolver is the DNS seam for the SSRF guard. Nil uses net.DefaultResolver.
	resolver ipResolver
}

// Composed is the result of [Compose]: the knowledge addendum for the system
// prompt and the allow-listed, namespaced provider tools ready to drive the
// loop. Close tears down the per-request MCP sessions.
type Composed struct {
	// SystemPromptAddendum is "" when no document contributed knowledge.
	SystemPromptAddendum string
	// Tools holds exactly the allow-listed provider tools, keyed and named
	// "<server>__<tool>".
	Tools agentcore.ToolSet
	close func() error
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

	// Tenant-isolation depth: drop any document the Source mis-scoped before it
	// contributes knowledge or tools (no-op unless ExpectedProject is set).
	docs = scopeDocuments(docs, opts.ExpectedProject, logger)

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

	addendum := buildKnowledgeAddendum(ctx, docs, knowledgeOptions{
		httpClient:           httpClient,
		guard:                guard,
		timeout:              opts.KnowledgeTimeout,
		maxBytesPerSource:    opts.KnowledgeMaxBytesPerSource,
		maxSourcesPerService: opts.KnowledgeMaxSourcesPerService,
		logger:               logger,
	})

	tools, sessions := connectTools(ctx, docs, opts, guard, logger)

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
				metrics:         opts.Metrics,
			}
		}
	}

	closed := false
	return &Composed{
		SystemPromptAddendum: addendum,
		Tools:                tools,
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

// scopeDocuments is the defense-in-depth tenant-isolation seam. The capability
// Source is trusted to return only the calling project's documents; this guards
// against a Source bug (or a compromised fan-out) leaking another tenant's
// document by dropping any whose Metadata.Namespace names a different project.
// It fails closed only on a positive mismatch: a document with no namespace is
// kept, because the schema carries no other project handle to cross-check and
// the Source stays the scoping authority there. With no ExpectedProject the
// check is disabled and docs pass through unchanged (backward compatible).
func scopeDocuments(docs []CapabilityDocument, expectedProject string, logger *slog.Logger) []CapabilityDocument {
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
			continue
		}
		kept = append(kept, doc)
	}
	return kept
}

func connectTools(ctx context.Context, docs []CapabilityDocument, opts ComposeOptions, guard *ipGuard, logger *slog.Logger) (agentcore.ToolSet, []mcpSession) {
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

	for _, doc := range docs {
		if doc.Spec.Tools == nil {
			continue
		}
		serviceName := doc.Spec.ServiceName
		for _, server := range doc.Spec.Tools.MCPServers {
			session, err := connectWithTimeout(ctx, connect, server.Endpoint, timeout)
			if err != nil {
				logger.Warn("capability.mcp.connect_failed",
					"service", serviceName, "server", server.Name, "endpoint", server.Endpoint, "error", err.Error())
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
				continue
			}

			for _, toolName := range server.ToolSelector.Include {
				providerTool, ok := serverTools[toolName]
				if !ok {
					logger.Warn("capability.mcp.tool_missing",
						"service", serviceName, "server", server.Name, "tool", toolName)
					continue
				}
				namespaced := NamespaceToolName(server.Name, toolName)
				if _, exists := tools[namespaced]; exists {
					logger.Warn("capability.mcp.tool_collision",
						"service", serviceName, "server", server.Name, "tool", namespaced)
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
	}

	return tools, sessions
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
	sanitize := func(v string) string { return toolNameSanitizer.ReplaceAllString(v, "-") }
	return sanitize(serverName) + ToolNamespaceSeparator + sanitize(toolName)
}

// guardedConnector wraps a connector with the SSRF guard: it refuses to connect
// to an endpoint whose scheme is disallowed or that resolves to a non-routable
// address (loopback, private, link-local incl. cloud IMDS) before the inner
// connector touches the network. Test connectors injected via ComposeOptions
// bypass this — they never reach the real network.
func guardedConnector(inner mcpConnector, guard *ipGuard) mcpConnector {
	return func(ctx context.Context, endpoint string) (mcpSession, error) {
		if err := guard.checkURL(ctx, endpoint); err != nil {
			return nil, err
		}
		return inner(ctx, endpoint)
	}
}

// defaultConnector opens real MCP sessions via the mcptool client.
func defaultConnector() mcpConnector {
	return func(ctx context.Context, endpoint string) (mcpSession, error) {
		return mcptool.Connect(ctx, mcptool.Options{Endpoint: endpoint, ClientName: defaultMCPClientName})
	}
}

// connectWithTimeout races a connect against a deadline. If the deadline wins,
// it returns an error but still closes a session that arrives late, so a slow
// server never leaks a connection.
func connectWithTimeout(ctx context.Context, connect mcpConnector, endpoint string, timeout time.Duration) (mcpSession, error) {
	type result struct {
		session mcpSession
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		session, err := connect(ctx, endpoint)
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
