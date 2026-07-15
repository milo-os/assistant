package usage

import (
	"strconv"
	"time"
)

// UsageTokens carries the per-run token counts surfaced by the model adapter.
// Any axis at or below zero is skipped (never emitted as a zero-valued event).
type UsageTokens struct {
	InputTokens              int64
	OutputTokens             int64
	CachedInputTokens        int64
	CacheCreationInputTokens int64
	// Messages is the per-request message count. nil defaults to 1, matching
	// the TS `tokens.messages ?? 1`.
	Messages *int64
}

// BuildUsageInput is the input to [BuildUsageEvents].
type BuildUsageInput struct {
	ProjectName     string
	ConversationID  string
	ConversationUID string
	Model           string // Anthropic model id, e.g. claude-sonnet-4-6
	Namespace       string // empty defaults to "default"
	Tokens          UsageTokens
	// NowMillis is the emit time in unix milliseconds; 0 uses time.Now().
	NowMillis int64
}

// BuildUsageEvents builds one usage [Event] per non-zero token axis plus the
// messages meter, each dimensioned by model. It returns an empty slice when no
// axis has a positive count. Ported verbatim from the TS assistant-events.ts.
func BuildUsageEvents(in BuildUsageInput) []Event {
	now := in.NowMillis
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	namespace := in.Namespace
	if namespace == "" {
		namespace = "default"
	}
	timestamp := isoMillis(now)
	projectRef := ProjectRef{Name: in.ProjectName}
	dimensions := map[string]string{"model": in.Model}
	labels := map[string]string{"model": in.Model}
	resource := EventResource{
		Ref: ResourceRef{
			ProjectRef: projectRef,
			Group:      ResourceGroup,
			Kind:       ResourceKind,
			Namespace:  namespace,
			Name:       in.ConversationID,
			UID:        in.ConversationUID,
		},
		Labels: labels,
	}

	var events []Event
	push := func(meter string, count int64) {
		if count <= 0 {
			return
		}
		events = append(events, Event{
			EventID:    NewULID(now),
			MeterName:  meter,
			Timestamp:  timestamp,
			ProjectRef: projectRef,
			Value:      strconv.FormatInt(count, 10),
			Dimensions: dimensions,
			Resource:   resource,
		})
	}

	messages := int64(1)
	if in.Tokens.Messages != nil {
		messages = *in.Tokens.Messages
	}

	push(MeterInputTokens, in.Tokens.InputTokens)
	push(MeterOutputTokens, in.Tokens.OutputTokens)
	push(MeterCacheReadTokens, in.Tokens.CachedInputTokens)
	push(MeterCacheWriteTokens, in.Tokens.CacheCreationInputTokens)
	push(MeterMessages, messages)

	return events
}

// BuildToolInvocationInput is the input to [BuildToolInvocationEvent].
type BuildToolInvocationInput struct {
	ProjectName     string
	ConversationID  string
	ConversationUID string
	// ServiceName is the reverse-DNS provider service name (AgentBinding
	// spec.serviceName), e.g. streaming.streamco.example. Emitted as the
	// `service` dimension so billing can price per provider.
	ServiceName string
	Namespace   string // empty defaults to "default"
	NowMillis   int64  // 0 uses time.Now()
}

// BuildToolInvocationEvent builds the single usage event for one provider-tool
// invocation: value always "1" (a Delta counter aggregated downstream),
// dimensioned by the provider service. Ported verbatim from the TS builder.
func BuildToolInvocationEvent(in BuildToolInvocationInput) Event {
	now := in.NowMillis
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	namespace := in.Namespace
	if namespace == "" {
		namespace = "default"
	}
	projectRef := ProjectRef{Name: in.ProjectName}
	return Event{
		EventID:    NewULID(now),
		MeterName:  MeterToolInvocations,
		Timestamp:  isoMillis(now),
		ProjectRef: projectRef,
		Value:      "1",
		Dimensions: map[string]string{"service": in.ServiceName},
		Resource: EventResource{
			Ref: ResourceRef{
				ProjectRef: projectRef,
				Group:      ResourceGroup,
				Kind:       ResourceKind,
				Namespace:  namespace,
				Name:       in.ConversationID,
				UID:        in.ConversationUID,
			},
			Labels: map[string]string{"service": in.ServiceName},
		},
	}
}

// isoMillis formats a unix-millisecond timestamp exactly like JavaScript's
// Date.prototype.toISOString(): UTC, always three-digit milliseconds, Z suffix.
func isoMillis(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("2006-01-02T15:04:05.000") + "Z"
}
