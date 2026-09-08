// `patch gaps` — a provider service's own read view of capability-gap
// reports (see internal/gapreport, docs/capability-gap-reporting-design.md):
// records the assistant writes when it told a user it lacked a tool, lookup,
// or piece of knowledge a provider service should have supplied. --project
// here is the PROVIDER's own project (spec.reportingProject on its
// capability document), never the project the conversation that hit the gap
// ran in — the capabilitygapreports resource is namespaced by provider, so a
// caller only ever sees reports attributed to a provider they have access
// to. Same read path as `conversations`: raw API paths through [ReadView],
// which prefers datumctl's identity and falls back to kubectl (readview.go).
package patchcli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// gapReportsPath builds the group-relative path for a provider project's
// capability-gap reports (the occurrence log).
func gapReportsPath(project string) string {
	return fmt.Sprintf("/apis/assistant.miloapis.com/v1alpha1/namespaces/%s/capabilitygapreports", project)
}

// gapsPath builds the group-relative path for a provider project's distinct
// capability gaps (the aggregate).
func gapsPath(project string) string {
	return fmt.Sprintf("/apis/assistant.miloapis.com/v1alpha1/namespaces/%s/capabilitygaps", project)
}

// runGapsList prints one row per DISTINCT gap, most-hit first, with how many
// conversations hit it. `patch gaps reports` prints the reports behind them.
func runGapsList(ctx context.Context, inv Invocation, io Io) int {
	view := ReadViewFor(inv)
	out, err := view.get(ctx, inv.Project, gapsPath(inv.Project))
	if err != nil {
		io.Err("patch: " + readViewErrorText(view, err) + "\n")
		return 1
	}

	var list assistantv1alpha1.CapabilityGapList
	if err := json.Unmarshal(out, &list); err != nil {
		io.Err("patch: could not parse capability gaps response: " + err.Error() + "\n")
		return 1
	}

	if inv.JSON {
		return emitRaw(out, io)
	}

	if len(list.Items) == 0 {
		io.Err("no capability gaps for provider project " + inv.Project + "\n")
		return 0
	}

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CONVERSATIONS\tOCCURRENCES\tLAST-SEEN\tSERVICE\tKEY\tKIND\tCAPABILITY")
	for _, g := range list.Items {
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			g.Status.Conversations,
			g.Status.Occurrences,
			ago(g.Status.LastSeen.Time),
			g.Status.ServiceName,
			gapKeyText(g.Status.CapabilityKey),
			gapKindText(g.Status.Kind),
			previewText(g.Status.Capability, 60),
		)
	}
	_ = tw.Flush()
	io.Out(b.String())
	return 0
}

// gapKeyText renders a gap's key. A gap filed before keys existed has none, so
// say that rather than leaving a blank that reads as a rendering bug.
func gapKeyText(key string) string {
	if key == "" {
		return "(unkeyed)"
	}
	return key
}

// emitRaw passes an API response through unchanged for --json.
func emitRaw(out []byte, io Io) int {
	io.Out(string(out))
	if !strings.HasSuffix(string(out), "\n") {
		io.Out("\n")
	}
	return 0
}

// runGapReportsList prints the occurrence log: one row per report filed, newest
// first. The drill-down behind `patch gaps list`, and the only view that shows
// which consumer project each report came from.
func runGapReportsList(ctx context.Context, inv Invocation, io Io) int {
	view := ReadViewFor(inv)
	out, err := view.get(ctx, inv.Project, gapReportsPath(inv.Project))
	if err != nil {
		io.Err("patch: " + readViewErrorText(view, err) + "\n")
		return 1
	}

	var list assistantv1alpha1.CapabilityGapReportList
	if err := json.Unmarshal(out, &list); err != nil {
		io.Err("patch: could not parse gap reports response: " + err.Error() + "\n")
		return 1
	}

	if inv.JSON {
		return emitRaw(out, io)
	}

	if len(list.Items) == 0 {
		io.Err("no capability-gap reports for provider project " + inv.Project + "\n")
		return 0
	}

	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].CreationTimestamp.After(list.Items[j].CreationTimestamp.Time)
	})

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "AGE\tSERVICE\tKEY\tKIND\tCAPABILITY\tCONSUMER-PROJECT\tSUMMARY")
	for _, r := range list.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ago(r.CreationTimestamp.Time),
			r.Status.ServiceName,
			gapKeyText(r.Status.CapabilityKey),
			gapKindText(r.Status.Kind),
			r.Status.Capability,
			r.Status.ConsumerProject,
			previewText(r.Status.Summary, 60),
		)
	}
	_ = tw.Flush()
	io.Out(b.String())
	return 0
}

// gapKindText renders a report's kind. Reports stored before kinds existed
// carry none and meant "no tool for this", so say so rather than blank.
func gapKindText(kind assistantv1alpha1.CapabilityGapKind) string {
	if kind == "" {
		return string(assistantv1alpha1.CapabilityGapKindMissingCapability)
	}
	return string(kind)
}

// previewText collapses whitespace and truncates for a compact table cell.
func previewText(s string, maxRunes int) string {
	collapsed := strings.Join(strings.Fields(s), " ")
	runes := []rune(collapsed)
	if len(runes) <= maxRunes {
		return collapsed
	}
	return string(runes[:maxRunes]) + "…"
}
