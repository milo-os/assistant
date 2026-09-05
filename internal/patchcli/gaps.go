// `patch gaps` — a provider service's own read view of capability-gap
// reports (see internal/gapreport, docs/capability-gap-reporting-design.md):
// records the assistant writes when it told a user it lacked a tool, lookup,
// or piece of knowledge a provider service should have supplied. --project
// here is the PROVIDER's own project (spec.reportingProject on its
// capability document), never the project the conversation that hit the gap
// ran in — the capabilitygapreports resource is namespaced by provider, so a
// caller only ever sees reports attributed to a provider they have access
// to. Same read-path shape as `conversations`: shells out to kubectl using
// the caller's normal k8s identity, no PATCH_TOKEN.
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

// runGapsList prints a table of a provider project's capability-gap reports
// (service, capability, summary, consumer project, age), newest first.
func runGapsList(ctx context.Context, inv Invocation, io Io) int {
	out, err := kubectlJSON(ctx, inv.Kubeconfig,
		"get", "capabilitygapreports", "-n", inv.Project, "-o", "json")
	if err != nil {
		return failKubectl(io, err, out)
	}

	var list assistantv1alpha1.CapabilityGapReportList
	if err := json.Unmarshal(out, &list); err != nil {
		io.Err("patch: could not parse gap reports response: " + err.Error() + "\n")
		return 1
	}

	if inv.JSON {
		io.Out(string(out))
		if !strings.HasSuffix(string(out), "\n") {
			io.Out("\n")
		}
		return 0
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
	fmt.Fprintln(tw, "AGE\tSERVICE\tCAPABILITY\tCONSUMER-PROJECT\tSUMMARY")
	for _, r := range list.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ago(r.CreationTimestamp.Time),
			r.Status.ServiceName,
			r.Status.Capability,
			r.Status.ConsumerProject,
			previewText(r.Status.Summary, 60),
		)
	}
	_ = tw.Flush()
	io.Out(b.String())
	return 0
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
