package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/gapreport"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
)

// GapReportToolBaseName is the un-namespaced model-facing tool name for
// capability-gap reporting. Each provider service that declares a
// ReportingProject gets its own namespaced instance — see
// [GapReportToolName] — so the model can only report a gap against a
// service actually present in the current conversation, never an
// arbitrary provider it names in free text.
const GapReportToolBaseName = "report_capability_gap"

// GapReportToolName renders the model-facing name for one provider's gap-
// report tool, "report_capability_gap__<serviceRef>", sanitized the same
// way provider MCP tools are namespaced (see [NamespaceToolName]).
func GapReportToolName(serviceRefName string) string {
	sanitize := func(v string) string { return toolNameSanitizer.ReplaceAllString(v, "-") }
	return GapReportToolBaseName + ToolNamespaceSeparator + sanitize(serviceRefName)
}

// reportCapabilityGapTool is the built-in [agentcore.Tool] that records a
// capability gap for one specific provider. It is closed over that
// provider's own identity (ServiceName, ProviderProject) at registration
// time in [Compose] — the model's input never names a project or service,
// so it cannot misdirect a report into the wrong provider's project. It
// fires no tool-invocation metering, matching rememberMemoryTool — this is
// platform bookkeeping, not a billable provider call.
type reportCapabilityGapTool struct {
	store gapreport.Store
	name  string
	// serviceName identifies the provider in reports and in the tool's own
	// description; providerProject is the write key (spec.reportingProject).
	serviceName     string
	providerProject string
	// consumerProject and contextID are provenance only: where and in which
	// conversation the gap was hit, not where the report is written to.
	consumerProject string
	contextID       string
	// metrics records assistant_gap_report_total. Nil (e.g. in tests that
	// don't set ComposeOptions.Metrics) is a safe no-op — see
	// [appmetrics.Metrics]'s nil-receiver methods.
	metrics *appmetrics.Metrics
}

func (t *reportCapabilityGapTool) Definition() agentcore.ToolDefinition {
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"capability": map[string]any{
				"type":        "string",
				"description": "A short description of the missing capability, e.g. \"list pipelines for StreamCo\".",
			},
			"summary": map[string]any{
				"type": "string",
				"description": "What the user was trying to do when the gap was hit, described ABSTRACTLY — " +
					"never quote or paraphrase the user's actual message content, names, identifiers, credentials, " +
					"or other sensitive/personal details.",
			},
		},
		"required": []string{"capability", "summary"},
	})
	return agentcore.ToolDefinition{
		Name: t.name,
		Description: fmt.Sprintf(
			"Report to %s's own team that this service is missing a tool, lookup, or piece of knowledge a user "+
				"needed. Use this when a request cannot be fulfilled because %s's capabilities don't cover it — not "+
				"for user mistakes, not for gaps in a different provider's service, and not for gaps unrelated to any "+
				"single provider. This does not help the current user answer their question; it only helps %s improve "+
				"its tooling. Still answer the user as best you can (e.g. point them to a manual workaround) in "+
				"addition to filing this report. "+
				"PRIVACY: %s's own team will read this report and has no access to this conversation — the summary "+
				"is the only context they get. Describe the gap ABSTRACTLY: what kind of tool, lookup, or data was "+
				"missing, and why it mattered. Do NOT quote, paraphrase, or otherwise include the user's actual "+
				"message text, names, account or record identifiers, credentials, or any other sensitive or "+
				"personal detail from the conversation. Bad: quoting the user's literal pasted text (e.g. "+
				"\"user pasted 'acct #48213, need the Q3 churn number for jane.doe@bigco.com'\") — this leaks the "+
				"user's real content into another team's project. Good: \"user needed to list active pipelines for "+
				"their account to diagnose lag — no list-pipelines tool was available.\"",
			t.serviceName, t.serviceName, t.serviceName, t.serviceName),
		InputSchema: schema,
	}
}

func (t *reportCapabilityGapTool) Execute(ctx context.Context, input json.RawMessage) (result string, err error) {
	// Every return path below — malformed input, a store bound violation, or
	// a genuine store failure, as well as a clean success — is one recorded
	// outcome of assistant_gap_report_total; the defer covers them uniformly
	// rather than duplicating a RecordGapReport call at each return.
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		t.metrics.RecordGapReport(outcome)
	}()

	var args struct {
		Capability string `json:"capability"`
		Summary    string `json:"summary"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.Capability == "" || args.Summary == "" {
		return "", fmt.Errorf("%s: input must be {\"capability\": \"...\", \"summary\": \"...\"}", t.name)
	}

	if _, err := t.store.Insert(ctx, t.providerProject, t.serviceName, t.consumerProject, t.contextID, args.Capability, args.Summary); err != nil {
		if errors.Is(err, gapreport.ErrCapabilityTooLong) {
			return "", fmt.Errorf("%s: that capability description is too long — try a shorter one", t.name)
		}
		if errors.Is(err, gapreport.ErrSummaryTooLong) {
			return "", fmt.Errorf("%s: that summary is too long — try a shorter one", t.name)
		}
		if errors.Is(err, gapreport.ErrProjectFull) {
			return "", fmt.Errorf("%s: gap reporting is temporarily at capacity for %s", t.name, t.serviceName)
		}
		return "", fmt.Errorf("%s: gap reporting is temporarily unavailable", t.name)
	}
	return fmt.Sprintf("Reported to %s: %s", t.serviceName, args.Capability), nil
}
