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
