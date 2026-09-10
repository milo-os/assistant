// Package basetools is the set of tools every project's Patch has, whoever the
// providers are.
//
// A provider publishes what only it knows: what its resources mean, how to
// build one, what to check when one is unwell. Everything underneath that —
// list what is there, read one, describe a kind's fields — is the same work for
// every service on the platform, and a platform that makes each provider build
// it again gets a different answer from each of them.
//
// What is deliberately NOT here is a platform record another service keeps.
// Where a service is offered, and how much of a project's allowance is left,
// are read from the services that own those records, through tools of their
// own, so there is one answer rather than this package's copy of one. See
// docs/capability-reference.md ("Platform capabilities").
//
// Two properties hold for every tool here:
//
//   - It runs as the person who asked. The tools are handed a
//     [projectapi.Project] already bound to the caller's own credential; there
//     is no credential of the service's own anywhere on this path, so a tool
//     call can read nothing the person could not read themselves.
//   - The project comes from the conversation, never from an argument. None of
//     the input schemas has a project field, and none of the handlers looks for
//     one, so a message that talks a model into naming another project has
//     nothing to talk to.
//
// The names are un-namespaced, like the platform's other built-in tools
// (load_skill, memory_remember, memory_forget) and unlike a provider's, which
// are namespaced "<service>__<tool>". A base tool is not one service's
// contribution, so putting a service's name on it would be a lie; and because
// every provider name carries the separator, a provider can never shadow one of
// these.
package basetools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/projectapi"
)

// The model-facing names of the read tools.
const (
	// ResourcesListToolName lists a project's resources of one kind.
	ResourcesListToolName = "resources_list"
	// ResourcesGetToolName reads one resource whole.
	ResourcesGetToolName = "resources_get"
	// SchemaGetToolName describes the fields a kind accepts.
	SchemaGetToolName = "schema_get"
)

// Options configures one project's base tools for one turn.
type Options struct {
	// Project reads and writes as the caller. Required — with no project view
	// there is no identity to act as, and the tools are not built.
	Project *projectapi.Project
	// Logger receives operational warnings. Nil discards them.
	Logger *slog.Logger
}

// Tools returns the base tool set, or nil when there is nothing to bind it to.
func Tools(opts Options) agentcore.ToolSet {
	if opts.Project == nil {
		return nil
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return agentcore.ToolSet{
		ResourcesListToolName: resourcesList(opts),
		ResourcesGetToolName:  resourcesGet(opts),
		SchemaGetToolName:     schemaGet(opts),
	}
}

// PromptSection is what the system prompt says about these tools. It is the
// same shape as the skills index: a short section, added only when the tools
// are actually there, so a turn without them never advertises them.
func PromptSection() string {
	return strings.TrimSpace(`
Platform tools: this project's own resources are reachable with resources_list and resources_get, whatever service owns them — pass the group, version and kind. schema_get describes what a kind's fields are and which are required; read it before writing a manifest rather than guessing. These read as the person you are talking to, in their project only, so a result they are not entitled to cannot come back. The platform's own record of where a service is offered, and of how much of the project's allowance is left, is not among them: it comes as tools from the services that keep those records, and those are what to call before promising a place or a size — never guess either. Prefer a provider's own tool when one covers the question: it knows what the fields mean, where these only report them.`)
}

// ----------------------------------------------------------------- plumbing

// tool adapts a handler function to [agentcore.Tool].
type tool struct {
	def agentcore.ToolDefinition
	run func(ctx context.Context, input json.RawMessage) (string, error)
}

func (t *tool) Definition() agentcore.ToolDefinition { return t.def }

func (t *tool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	return t.run(ctx, input)
}

// newTool builds one tool from a name, description and input schema.
func newTool(name, description string, schema map[string]any, run func(context.Context, json.RawMessage) (string, error)) *tool {
	raw, err := json.Marshal(schema)
	if err != nil {
		// A schema literal in this package that will not marshal is a
		// programming error, not a runtime condition; an empty object keeps
		// the tool callable rather than panicking a live conversation.
		raw = []byte(`{"type":"object"}`)
	}
	return &tool{
		def: agentcore.ToolDefinition{Name: name, Description: description, InputSchema: raw},
		run: run,
	}
}

// object is a JSON-schema object literal, spelled once.
func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// result renders a tool's output. Indented, because the model reads it and a
// wall of one-line JSON is harder for it to quote back accurately.
func result(v any) (string, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("the answer could not be assembled: %w", err)
	}
	return string(raw), nil
}

// decode reads a tool's arguments, naming the tool in the rejection so the
// model can tell which call was malformed.
func decode(name string, input json.RawMessage, into any) error {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if err := json.Unmarshal(input, into); err != nil {
		return fmt.Errorf("%s: those arguments could not be read: %v", name, err)
	}
	return nil
}

// ------------------------------------------------------------ shared errors

// notEntitled rewrites a refusal from the platform into something the person
// reading it can act on. They asked a fair question and were told no; what they
// need to know is who can change that, not which check said it.
func notEntitled(what string) error {
	return fmt.Errorf(
		"you do not have access to %s in this project, so nothing was read. "+
			"Whoever administers the project can grant it", what)
}

// describe turns a failure from the platform into a customer-readable one.
func describe(what string, err error) error {
	if projectapi.IsForbidden(err) {
		return notEntitled(what)
	}
	return fmt.Errorf("%s could not be read: %w", what, err)
}

// ------------------------------------------------------------ status digest

// Condition is one thing the platform reports about a resource.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// conditionsOf returns what the platform is reporting about one resource. This
// is the whole of the status the read tools surface: a provider's own tool is
// where a status gets interpreted, and a base tool that guessed at meaning
// would be competing with it.
func conditionsOf(obj projectapi.Object) []Condition {
	raw, ok := projectapi.NestedSlice(obj, "status", "conditions")
	if !ok {
		return nil
	}
	out := make([]Condition, 0, len(raw))
	for _, item := range raw {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		condition := Condition{}
		condition.Type, _ = c["type"].(string)
		condition.Status, _ = c["status"].(string)
		condition.Reason, _ = c["reason"].(string)
		condition.Message, _ = c["message"].(string)
		if condition.Type != "" {
			out = append(out, condition)
		}
	}
	return out
}

// sortedKeys returns a map's keys in a stable order, so two calls in one
// conversation read the same way.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
