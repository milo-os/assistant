package capabilitygapreport

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"github.com/milo-os/assistant/internal/gapreport"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
)

func nsCtx(ns string) context.Context {
	return request.WithNamespace(context.Background(), ns)
}

// TestListScopedToProviderProjectNotConsumerProject is the core guarantee
// this resource exists for: a report filed from a "demo-project" conversation
// is only visible when listing the PROVIDER's own namespace, never the
// consumer's.
func TestListScopedToProviderProjectNotConsumerProject(t *testing.T) {
	store := gapreport.NewMemoryStore()
	if _, err := store.Insert(context.Background(), gapreport.InsertParams{
		ProviderProject: "streamco-platform", ServiceName: "streaming.streamco.example",
		ConsumerProject: "demo-project", ContextID: "ctx-1",
		Capability: "list pipelines", Summary: "user needed a pipeline id",
	}); err != nil {
		t.Fatal(err)
	}
	rest := NewCapabilityGapReportREST(store)

	obj, err := rest.List(nsCtx("streamco-platform"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list := obj.(*assistant.CapabilityGapReportList)
	if len(list.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(list.Items))
	}
	item := list.Items[0]
	if item.Namespace != "streamco-platform" {
		t.Errorf("Namespace = %q, want streamco-platform", item.Namespace)
	}
	if item.Status.ConsumerProject != "demo-project" || item.Status.ContextID != "ctx-1" {
		t.Errorf("Status = %+v", item.Status)
	}
	if item.Status.Capability != "list pipelines" || item.Status.Summary != "user needed a pipeline id" {
		t.Errorf("Status = %+v", item.Status)
	}
	// A report filed without a kind projects as MissingCapability, and
	// projects no evidence block at all rather than an empty one.
	if item.Status.Kind != string(gapreport.KindMissingCapability) {
		t.Errorf("Status.Kind = %q, want %q", item.Status.Kind, gapreport.KindMissingCapability)
	}
	if item.Status.Evidence != nil {
		t.Errorf("Status.Evidence = %+v, want nil for a report with nothing to quote", item.Status.Evidence)
	}

	obj, err = rest.List(nsCtx("demo-project"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := obj.(*assistant.CapabilityGapReportList); len(got.Items) != 0 {
		t.Fatalf("consumer project namespace should see 0 reports, got %d", len(got.Items))
	}
}

func TestListMissingNamespaceIsBadRequest(t *testing.T) {
	rest := NewCapabilityGapReportREST(gapreport.NewMemoryStore())
	_, err := rest.List(context.Background(), &metainternalversion.ListOptions{})
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}

// A project-scoped milo identity may not read a different project's namespace.
func TestProjectIdentityMismatchIsForbidden(t *testing.T) {
	ctx := request.WithUser(nsCtx("streamco-platform"), &user.DefaultInfo{
		Name: "alice",
		Extra: map[string][]string{
			tenant.ExtraParentType: {"Project"},
			tenant.ExtraParentName: {"other"},
		},
	})
	rest := NewCapabilityGapReportREST(gapreport.NewMemoryStore())
	_, err := rest.List(ctx, &metainternalversion.ListOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
}

// TestListProjectsKindAndEvidence pins that a quality defect survives the
// projection intact — this is what `datumctl get capabilitygapreports -o yaml`
// shows the provider's team.
func TestListProjectsKindAndEvidence(t *testing.T) {
	store := gapreport.NewMemoryStore()
	evidence := gapreport.Evidence{
		Tool:           "workloads_list",
		Observed:       "actionability: transient",
		ContradictedBy: "instance unchanged for 9d",
	}
	if _, err := store.Insert(context.Background(), gapreport.InsertParams{
		ProviderProject: "streamco-platform", ServiceName: "streaming.streamco.example",
		ConsumerProject: "demo-project", ContextID: "ctx-1",
		Capability: "workload stall duration", Summary: "user was diagnosing a stalled workload",
		Kind: gapreport.KindMisleadingOutput, Evidence: evidence,
	}); err != nil {
		t.Fatal(err)
	}

	obj, err := NewCapabilityGapReportREST(store).List(nsCtx("streamco-platform"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items := obj.(*assistant.CapabilityGapReportList).Items
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	status := items[0].Status
	if status.Kind != string(gapreport.KindMisleadingOutput) {
		t.Errorf("Kind = %q, want %q", status.Kind, gapreport.KindMisleadingOutput)
	}
	if status.Evidence == nil {
		t.Fatal("Evidence not projected")
	}
	if status.Evidence.Tool != evidence.Tool ||
		status.Evidence.Observed != evidence.Observed ||
		status.Evidence.ContradictedBy != evidence.ContradictedBy {
		t.Errorf("Evidence = %+v, want %+v", status.Evidence, evidence)
	}
}
