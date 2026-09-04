package usage

// Canonical meter and resource identifiers for the AI assistant, ported
// verbatim from the TS service's src/usage/meters.ts.
//
// These MUST stay in sync with the assistant ServiceConfiguration that the
// services-operator fans out into billing.miloapis.com/MeterDefinition and
// MonitoredResourceType objects in Milo. metric names, kind, unit and the
// monitored-resource type/gvk are immutable once Published — treat the values
// below as wire constants and version any breaking change as a new metric name.

// ServiceName is the reverse-DNS service identifier for the assistant.
const ServiceName = "assistant.miloapis.com"

// Group/Kind that emits assistant usage events.
const (
	ResourceGroup = "assistant.miloapis.com"
	ResourceKind  = "Conversation"
)

// Canonical meter names (reverse-DNS paths under the service name).
const (
	MeterInputTokens      = "assistant.miloapis.com/conversation/input-tokens"
	MeterOutputTokens     = "assistant.miloapis.com/conversation/output-tokens"
	MeterCacheReadTokens  = "assistant.miloapis.com/conversation/cache-read-tokens"
	MeterCacheWriteTokens = "assistant.miloapis.com/conversation/cache-write-tokens"
	MeterMessages         = "assistant.miloapis.com/conversation/messages"
	// MeterToolInvocations is a Delta counter: one event per provider-tool
	// invocation, dimensioned by the provider's reverse-DNS service name.
	MeterToolInvocations = "assistant.miloapis.com/conversation/tool-invocations"
)
