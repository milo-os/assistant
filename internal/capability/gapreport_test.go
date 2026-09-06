package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/gapreport"
)

func gapReportDoc(reportingProject string) CapabilityDocument {
	return streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.ReportingProject = reportingProject
	})
}

func TestGapReportComposesToolPerDocumentWithReportingProject(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
		ContextID:       "ctx-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	name := GapReportToolName("streamco")
	if _, ok := composed.Tools[name]; !ok {
		keys := make([]string, 0, len(composed.Tools))
		for k := range composed.Tools {
			keys = append(keys, k)
		}
		t.Fatalf("expected %s to be composed, got tools: %v", name, keys)
	}
}

func TestGapReportNoReportingProjectMeansNoTool(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()
	if _, ok := composed.Tools[GapReportToolName("streamco")]; ok {
		t.Fatal("a document with no reportingProject must not get a gap-report tool")
	}
}

func TestGapReportNoStoreMeansNoTool(t *testing.T) {
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()
	if _, ok := composed.Tools[GapReportToolName("streamco")]; ok {
		t.Fatal("with GapReports nil the feature must be entirely off")
	}
}

func TestGapReportWritesToProviderProjectNotConsumerProject(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project", // the CONSUMER project
		ContextID:       "ctx-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tool := composed.Tools[GapReportToolName("streamco")]
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"capability":"list pipelines for StreamCo","summary":"user needed a pipeline id"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "streaming.streamco.example") {
		t.Fatalf("unexpected result: %q", out)
	}

	// The report must land under the PROVIDER project, never the consumer's.
	reports, err := store.List(context.Background(), "streamco-platform")
	if err != nil || len(reports) != 1 {
		t.Fatalf("List(providerProject) = %v, %v; want 1 report", reports, err)
	}
	r := reports[0]
	if r.Capability != "list pipelines for StreamCo" || r.ConsumerProject != "demo-project" || r.ContextID != "ctx-1" {
		t.Fatalf("unexpected report: %+v", r)
	}

	consumerSide, err := store.List(context.Background(), "demo-project")
	if err != nil || len(consumerSide) != 0 {
		t.Fatalf("List(consumerProject) = %v, %v; want empty — report must not land under the consumer project", consumerSide, err)
	}
}

func TestGapReportBoundsSurfaceAsToolErrors(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tool := composed.Tools[GapReportToolName("streamco")]
	tooLong := strings.Repeat("x", gapreport.MaxCapabilityLen+1)
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"capability":"`+tooLong+`","summary":"s"}`))
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("want a too-long error, got %v", err)
	}
}

// TestGapReportDescriptionWarnsAgainstQuotingUserContent pins the one
// mitigation available for the free-text summary field: since nothing in
// this codebase scrubs it, the tool's own description must instruct the
// model to describe the gap abstractly and never quote/paraphrase the
// user's actual message content. This can only check the instruction is
// present, not that a model obeys it.
func TestGapReportDescriptionWarnsAgainstQuotingUserContent(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tool := composed.Tools[GapReportToolName("streamco")]
	desc := tool.Definition().Description
	for _, want := range []string{"ABSTRACTLY", "Do NOT quote", "Bad:", "Good:"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("tool description missing privacy guidance %q; got: %s", want, desc)
		}
	}
}

// TestGapReportDescriptionDrawsTheEvidenceLine pins the narrower privacy rule
// evidence introduces. Quoting a provider's own tool output back to that
// provider is defensible; quoting the user is not, and the difference has to
// be stated in the prompt because nothing downstream can tell them apart.
func TestGapReportDescriptionDrawsTheEvidenceLine(t *testing.T) {
	desc := gapReportToolDescription("streaming.streamco.example")
	for _, want := range []string{
		"evidence.observed and evidence.contradictedBy are the ONE narrow exception",
		"streaming.streamco.example's OWN tool output",
		"data going home to the team that produced it",
		"NOT permission to quote the user",
		"never carry a value into evidence just because a tool echoed back something the user typed",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("tool description missing evidence privacy guidance %q; got: %s", want, desc)
		}
	}
}

// TestGapReportDescriptionDiscriminatesEveryKind: the description is the only
// thing steering the model's choice of kind, so every kind must appear with a
// discriminator, and MisleadingOutput must carry the retraction trigger the
// provider-side runbook guidance is written against — the two texts are read
// together in the same prompt.
func TestGapReportDescriptionDiscriminatesEveryKind(t *testing.T) {
	desc := gapReportToolDescription("streaming.streamco.example")
	for _, kind := range gapreport.Kinds {
		if !strings.Contains(desc, string(kind)) {
			t.Errorf("tool description never names kind %q", kind)
		}
	}
	for _, want := range []string{
		"the signal is your own retraction",
		"file it at the moment you take it back",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("tool description missing the MisleadingOutput retraction trigger %q", want)
		}
	}
}

// The schema is what constrains the model to the enum; a missing or widened
// enum would let an unknown kind through to a store rejection instead.
func TestGapReportSchemaOffersKindEnumAndEvidence(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	var schema struct {
		Properties struct {
			Kind struct {
				Enum []string `json:"enum"`
			} `json:"kind"`
			Evidence struct {
				Properties map[string]any `json:"properties"`
			} `json:"evidence"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(composed.Tools[GapReportToolName("streamco")].Definition().InputSchema, &schema); err != nil {
		t.Fatal(err)
	}

	if len(schema.Properties.Kind.Enum) != len(gapreport.Kinds) {
		t.Fatalf("kind enum = %v; want every kind in gapreport.Kinds", schema.Properties.Kind.Enum)
	}
	for i, kind := range gapreport.Kinds {
		if schema.Properties.Kind.Enum[i] != string(kind) {
			t.Errorf("kind enum[%d] = %q; want %q", i, schema.Properties.Kind.Enum[i], kind)
		}
	}
	for _, field := range []string{"tool", "observed", "contradictedBy"} {
		if _, ok := schema.Properties.Evidence.Properties[field]; !ok {
			t.Errorf("evidence schema missing %q", field)
		}
	}
	// kind and evidence must stay optional so every existing provider and
	// stored row keeps working.
	for _, req := range schema.Required {
		if req == "kind" || req == "evidence" {
			t.Errorf("%q must not be required", req)
		}
	}
}

func TestGapReportToolStoresKindAndEvidence(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
		ContextID:       "ctx-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tool := composed.Tools[GapReportToolName("streamco")]
	_, err = tool.Execute(context.Background(), json.RawMessage(`{
		"capability":"workload stall duration",
		"summary":"user was diagnosing a stalled workload",
		"kind":"MisleadingOutput",
		"evidence":{"tool":"workloads_list","observed":"actionability: transient","contradictedBy":"instance unchanged for 9d"}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	reports, err := store.List(context.Background(), "streamco-platform")
	if err != nil || len(reports) != 1 {
		t.Fatalf("List = %v, %v; want 1", reports, err)
	}
	r := reports[0]
	if r.Kind != gapreport.KindMisleadingOutput {
		t.Errorf("Kind = %q; want %q", r.Kind, gapreport.KindMisleadingOutput)
	}
	want := gapreport.Evidence{
		Tool:           "workloads_list",
		Observed:       "actionability: transient",
		ContradictedBy: "instance unchanged for 9d",
	}
	if r.Evidence != want {
		t.Errorf("Evidence = %+v; want %+v", r.Evidence, want)
	}
}

// An omitted kind must keep working exactly as it did before kinds existed —
// this is the shape every provider currently in production sends.
func TestGapReportToolOmittedKindDefaultsAndDoesNotWarn(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	out, err := composed.Tools[GapReportToolName("streamco")].Execute(context.Background(),
		json.RawMessage(`{"capability":"list pipelines","summary":"user needed a pipeline id"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "filed without evidence") {
		t.Errorf("a MissingCapability report has nothing to quote and must not be nagged: %q", out)
	}

	reports, _ := store.List(context.Background(), "streamco-platform")
	if len(reports) != 1 || reports[0].Kind != gapreport.KindMissingCapability {
		t.Fatalf("reports = %+v; want one MissingCapability report", reports)
	}
}

// The decision on missing evidence: stored, not rejected, with the nudge
// returned to the model — the only party that can still supply it.
func TestGapReportToolWarnsButStoresWhenEvidenceIsMissing(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	out, err := composed.Tools[GapReportToolName("streamco")].Execute(context.Background(),
		json.RawMessage(`{"capability":"workload stall duration","summary":"diagnosing a stalled workload","kind":"MisleadingOutput"}`))
	if err != nil {
		t.Fatalf("Execute = %v; a report with no evidence must still be filed", err)
	}
	if !strings.Contains(out, "filed without evidence") {
		t.Errorf("result must nudge the model for evidence; got %q", out)
	}

	reports, _ := store.List(context.Background(), "streamco-platform")
	if len(reports) != 1 || reports[0].Kind != gapreport.KindMisleadingOutput {
		t.Fatalf("reports = %+v; want the report stored as MisleadingOutput despite the warning", reports)
	}
}

func TestGapReportToolRejectsUnknownKind(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	_, err = composed.Tools[GapReportToolName("streamco")].Execute(context.Background(),
		json.RawMessage(`{"capability":"cap","summary":"s","kind":"SlightlyOff"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("Execute = %v; want an unknown-kind error", err)
	}
	// The error names the valid kinds so the model can correct itself.
	if !strings.Contains(err.Error(), string(gapreport.KindMisleadingOutput)) {
		t.Errorf("error must list the valid kinds; got %v", err)
	}
	if reports, _ := store.List(context.Background(), "streamco-platform"); len(reports) != 0 {
		t.Fatalf("a rejected report must not be stored; got %+v", reports)
	}
}

func TestGapReportEvidenceBoundsSurfaceAsToolErrors(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tooLong := strings.Repeat("x", gapreport.MaxEvidenceTextLen+1)
	_, err = composed.Tools[GapReportToolName("streamco")].Execute(context.Background(),
		json.RawMessage(`{"capability":"cap","summary":"s","kind":"MisleadingOutput","evidence":{"observed":"`+tooLong+`"}}`))
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("want a too-long error, got %v", err)
	}
}

func TestGapReportMultipleProvidersGetDistinctTools(t *testing.T) {
	store := gapreport.NewMemoryStore()
	other := gapReportDoc("streamco-platform")
	other.Spec.ServiceRef = Ref{Name: "other-provider"}
	other.Spec.ServiceName = "other.example"
	other.Spec.ReportingProject = "other-provider-project"

	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform"), other}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	if _, ok := composed.Tools[GapReportToolName("streamco")]; !ok {
		t.Fatal("missing streamco gap-report tool")
	}
	if _, ok := composed.Tools[GapReportToolName("other-provider")]; !ok {
		t.Fatal("missing other-provider gap-report tool")
	}
}
