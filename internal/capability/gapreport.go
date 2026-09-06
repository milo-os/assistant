package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	kinds := make([]string, 0, len(gapreport.Kinds))
	for _, k := range gapreport.Kinds {
		kinds = append(kinds, string(k))
	}
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"capability": map[string]any{
				"type":        "string",
				"description": "A short description of the capability at fault, e.g. \"list pipelines for StreamCo\".",
			},
			"summary": map[string]any{
				"type": "string",
				"description": "What the user was trying to do when the gap was hit, described ABSTRACTLY — " +
					"never quote or paraphrase the user's actual message content, names, identifiers, credentials, " +
					"or other sensitive/personal details.",
			},
			"kind": map[string]any{
				"type": "string",
				"enum": kinds,
				"description": "What kind of shortfall this was. Omit for a genuinely absent capability " +
					"(defaults to " + string(gapreport.KindMissingCapability) + ").",
			},
			"evidence": map[string]any{
				"type": "object",
				"description": "The tool output this report is about. Expected whenever kind is not " +
					string(gapreport.KindMissingCapability) + ". TOOL OUTPUT AND OBJECT STATE ONLY — never the user's message text.",
				"properties": map[string]any{
					"tool": map[string]any{
						"type":        "string",
						"description": "The tool whose output was at fault, e.g. \"workloads_list\".",
					},
					"observed": map[string]any{
						"type":        "string",
						"description": "What that tool returned, quoted or closely paraphrased, e.g. \"actionability: transient\".",
					},
					"contradictedBy": map[string]any{
						"type":        "string",
						"description": "The fact that makes it wrong, thin, or impossible to act on, e.g. \"instance unchanged for 9d\".",
					},
				},
			},
		},
		"required": []string{"capability", "summary"},
	})
	return agentcore.ToolDefinition{
		Name:        t.name,
		Description: gapReportToolDescription(t.serviceName),
		InputSchema: schema,
	}
}

// gapReportToolDescription is the only thing steering the model's choice of
// kind and its handling of evidence, and it sits in every turn's prompt for
// an entitled service — so it is written once, here, rather than assembled
// inline.
func gapReportToolDescription(service string) string {
	return fmt.Sprintf(
		"Report to %[1]s's own team that this service fell short for a user: either no tool covered what they "+
			"needed, or a tool ran and handed back something too thin, misleading, or impossible to act on. Use "+
			"this when the shortfall is %[1]s's — not for user mistakes, not for another provider's service, and "+
			"not for gaps unrelated to any single provider. This does not help the current user answer their "+
			"question; it only helps %[1]s fix its tooling. Still answer the user as best you can (e.g. point them "+
			"to a manual workaround) in addition to filing this report.\n"+
			"KIND — name what actually went wrong; omit it only for a genuinely absent capability (default %[2]s):\n"+
			"- %[2]s: no tool exists for what was needed. e.g. needed to list active pipelines to diagnose lag — "+
			"no list-pipelines tool was available.\n"+
			"- %[3]s: a tool answered, but left out a field the answer needed. e.g. workloads_list returned state "+
			"but no duration, so a 9-day outage looked identical to a 9-second one.\n"+
			"- %[4]s: a tool answered, and its output pointed at the wrong conclusion. You will rarely catch this "+
			"in the moment — the signal is your own retraction: if you told the user something was normal, "+
			"expected, or not worth acting on because a tool said so, and you later take that back, the output "+
			"misled you; file it at the moment you take it back. e.g. a 9-day stall was labelled actionability "+
			"\"transient\" with remediation \"Wait.\", so the user was told to wait on a workload that was never "+
			"going to recover.\n"+
			"- %[5]s: a tool told the user to do something they cannot do. e.g. remediation said \"pull container "+
			"logs\", but those nodes serve HTTPS on the log port, so there are no logs to pull.\n"+
			"EVIDENCE — supply it for every kind except %[2]s, which has no output to quote: evidence.tool (the "+
			"tool at fault), evidence.observed (what it returned), evidence.contradictedBy (the fact that makes "+
			"that wrong). Without it %[1]s's team gets a complaint they cannot check.\n"+
			"PRIVACY: %[1]s's own team will read this report and has no access to this conversation — what you "+
			"write here is the only context they get. Describe the gap ABSTRACTLY: what was missing, thin, wrong, "+
			"or unactionable, and why it mattered. Do NOT quote, paraphrase, or otherwise include the user's actual "+
			"message text, names, account or record identifiers, credentials, or any other sensitive or personal "+
			"detail from the conversation. Bad: quoting the user's literal pasted text (e.g. \"user pasted 'acct "+
			"#48213, need the Q3 churn number for jane.doe@bigco.com'\") — this leaks the user's real content into "+
			"another team's project. Good: \"user needed to list active pipelines for their account to diagnose "+
			"lag — no list-pipelines tool was available.\"\n"+
			"evidence.observed and evidence.contradictedBy are the ONE narrow exception, and only because of where "+
			"the data came from: quote %[1]s's OWN tool output and the state of the objects it returned — that is "+
			"%[1]s's data going home to the team that produced it. It is NOT permission to quote the user. Never "+
			"put the user's message text in evidence, and never carry a value into evidence just because a tool "+
			"echoed back something the user typed.",
		service,
		gapreport.KindMissingCapability,
		gapreport.KindInsufficientDetail,
		gapreport.KindMisleadingOutput,
		gapreport.KindUnactionableGuidance,
	)
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
		Kind       string `json:"kind"`
		Evidence   struct {
			Tool           string `json:"tool"`
			Observed       string `json:"observed"`
			ContradictedBy string `json:"contradictedBy"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.Capability == "" || args.Summary == "" {
		return "", fmt.Errorf("%s: input must be {\"capability\": \"...\", \"summary\": \"...\"}", t.name)
	}

	kind, err := gapreport.ParseKind(args.Kind)
	if err != nil {
		return "", fmt.Errorf("%s: unknown kind %q — use one of %s", t.name, args.Kind, strings.Join(kindNames(), ", "))
	}
	evidence := gapreport.Evidence{
		Tool:           args.Evidence.Tool,
		Observed:       args.Evidence.Observed,
		ContradictedBy: args.Evidence.ContradictedBy,
	}

	if _, err := t.store.Insert(ctx, gapreport.InsertParams{
		ProviderProject: t.providerProject,
		ServiceName:     t.serviceName,
		ConsumerProject: t.consumerProject,
		ContextID:       t.contextID,
		Capability:      args.Capability,
		Summary:         args.Summary,
		Kind:            kind,
		Evidence:        evidence,
	}); err != nil {
		if errors.Is(err, gapreport.ErrCapabilityTooLong) {
			return "", fmt.Errorf("%s: that capability description is too long — try a shorter one", t.name)
		}
		if errors.Is(err, gapreport.ErrSummaryTooLong) {
			return "", fmt.Errorf("%s: that summary is too long — try a shorter one", t.name)
		}
		if errors.Is(err, gapreport.ErrEvidenceTooLong) {
			return "", fmt.Errorf("%s: that evidence is too long — quote the smallest fragment of tool output that shows the problem", t.name)
		}
		if errors.Is(err, gapreport.ErrProjectFull) {
			return "", fmt.Errorf("%s: gap reporting is temporarily at capacity for %s", t.name, t.serviceName)
		}
		return "", fmt.Errorf("%s: gap reporting is temporarily unavailable", t.name)
	}

	// A kind that wants evidence but arrived without it is still stored —
	// dropping the report would lose the signal entirely — so the nudge goes
	// back to the model, which is the only party that can still fix it.
	msg := fmt.Sprintf("Reported to %s (%s): %s", t.serviceName, kind, args.Capability)
	if gapreport.NeedsEvidence(kind) && evidence.IsZero() {
		msg += fmt.Sprintf(" — filed without evidence; a %s report is hard for %s's team to act on without "+
			"evidence.tool/observed/contradictedBy. Include them next time.", kind, t.serviceName)
	}
	return msg, nil
}

// kindNames renders the valid kinds for an error message aimed at the model.
func kindNames() []string {
	out := make([]string, 0, len(gapreport.Kinds))
	for _, k := range gapreport.Kinds {
		out = append(out, string(k))
	}
	return out
}
