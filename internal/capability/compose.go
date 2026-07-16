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

	addendum := buildKnowledgeAddendum(ctx, docs, knowledgeOptions{
		httpClient:           opts.HTTPClient,
		timeout:              opts.KnowledgeTimeout,
		maxBytesPerSource:    opts.KnowledgeMaxBytesPerSource,
		maxSourcesPerService: opts.KnowledgeMaxSourcesPerService,
		logger:               logger,
	})

	tools, sessions := connectTools(ctx, docs, opts, logger)

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
			tools[LoadSkillToolName] = newLoadSkillTool(skills, opts, logger)
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

func connectTools(ctx context.Context, docs []CapabilityDocument, opts ComposeOptions, logger *slog.Logger) (agentcore.ToolSet, []mcpSession) {
	connect := opts.connect
	if connect == nil {
		connect = defaultConnector()
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

			serverTools, err := session.Tools(ctx)
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
			if r := <-ch; r.session != nil {
				_ = r.session.Close()
			}
		}()
		return nil, fmt.Errorf("timed out after %s", timeout)
	}
}

// compile-time check that the real session type satisfies the seam.
var _ mcpSession = (*mcptool.Session)(nil)
