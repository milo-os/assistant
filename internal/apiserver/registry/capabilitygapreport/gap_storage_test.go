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

func gapList(t *testing.T, store gapreport.Store, ns string) []assistant.CapabilityGap {
	t.Helper()
	obj, err := NewCapabilityGapREST(store).List(nsCtx(ns), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return obj.(*assistant.CapabilityGapList).Items
}

// The point of the resource: three conversations that described one gap three
// ways are one row with a count, not three rows a human has to recognise as
// the same thing.
func TestCapabilityGapListCollapsesOccurrencesToOneEntry(t *testing.T) {
	store := gapreport.NewMemoryStore()
	prose := []string{
		"time-series CPU/memory utilization metrics",
		"Instance/Workload resource usage metrics (CPU, memory) over a time window",
		"CPU/memory usage metrics for workloads over a time window",
	}
	for i, capability := range prose {
		if _, err := store.Insert(context.Background(), gapreport.InsertParams{
			ProviderProject: "streamco-platform", ServiceName: "streaming.streamco.example",
			ConsumerProject: "consumer-" + string(rune('a'+i)), ContextID: "ctx-" + string(rune('1'+i)),
			CapabilityKey: "workload-metrics", Capability: capability, Summary: "user was diagnosing lag",
		}); err != nil {
			t.Fatal(err)
		}
	}

	items := gapList(t, store, "streamco-platform")
	if len(items) != 1 {
		t.Fatalf("got %d gaps, want 1 — three reports of one gap", len(items))
	}
	got := items[0]
	if got.Name != "workload-metrics" || got.Namespace != "streamco-platform" {
		t.Errorf("name/namespace = %q/%q", got.Name, got.Namespace)
	}
	if got.Status.Conversations != 3 || got.Status.Occurrences != 3 {
		t.Errorf("Conversations/Occurrences = %d/%d; want 3/3", got.Status.Conversations, got.Status.Occurrences)
	}
	if got.Status.CapabilityKey != "workload-metrics" || got.Status.ServiceName != "streaming.streamco.example" {
		t.Errorf("Status = %+v", got.Status)
	}
	if got.Status.Capability != prose[len(prose)-1] {
		t.Errorf("Capability = %q; want the most recent wording", got.Status.Capability)
	}
	if got.Status.FirstSeen.IsZero() || got.Status.LastSeen.IsZero() {
		t.Errorf("FirstSeen/LastSeen not projected: %+v", got.Status)
	}
	if got.Status.Kind != string(gapreport.KindMissingCapability) {
		t.Errorf("Kind = %q", got.Status.Kind)
	}

	// The occurrences are still there behind it — that is where the
	// per-occurrence evidence a provider needs to diagnose actually lives.
	obj, err := NewCapabilityGapReportREST(store).List(nsCtx("streamco-platform"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reports := obj.(*assistant.CapabilityGapReportList).Items; len(reports) != 3 {
		t.Fatalf("got %d reports behind the aggregate, want 3", len(reports))
	}
}

// The aggregate must not become a report of which customers are hitting a
// gap. The count is the prioritisation signal; the consumer identity is a
// separate question this view does not answer.
func TestCapabilityGapCarriesNoConsumerIdentity(t *testing.T) {
	store := gapreport.NewMemoryStore()
	if _, err := store.Insert(context.Background(), gapreport.InsertParams{
		ProviderProject: "streamco-platform", ServiceName: "svc",
		ConsumerProject: "bigco-secret-project", ContextID: "ctx-1",
		CapabilityKey: "workload-metrics", Capability: "cap", Summary: "s",
	}); err != nil {
		t.Fatal(err)
	}
	got := gapList(t, store, "streamco-platform")[0]
	for _, field := range []string{got.Name, got.Status.Capability, got.Status.CapabilityKey, got.Status.ServiceName} {
		if field == "bigco-secret-project" || field == "ctx-1" {
			t.Errorf("consumer identity leaked into the aggregate: %+v", got)
		}
	}
}

// The four rows already in staging predate keys. They must still reach the
// provider, and each must stand alone rather than being merged on the
// strength of prose that cannot establish they are the same gap.
func TestCapabilityGapKeylessReportsStillAppear(t *testing.T) {
	store := gapreport.NewMemoryStore()
	for i, capability := range []string{"list pipelines", "workload stall duration"} {
		if _, err := store.Insert(context.Background(), gapreport.InsertParams{
			ProviderProject: "streamco-platform", ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-" + string(rune('1'+i)),
			Capability: capability, Summary: "s",
		}); err != nil {
			t.Fatal(err)
		}
	}
	items := gapList(t, store, "streamco-platform")
	if len(items) != 2 {
		t.Fatalf("got %d gaps, want 2 unmerged keyless reports", len(items))
	}
	for _, g := range items {
		if g.Status.CapabilityKey != "" {
			t.Errorf("CapabilityKey = %q; want empty for a pre-key report", g.Status.CapabilityKey)
		}
		if g.Name == "" {
			t.Error("a keyless gap must still be named — by its own report id")
		}
		if g.Status.Conversations != 1 || g.Status.Occurrences != 1 {
			t.Errorf("Conversations/Occurrences = %d/%d; want 1/1", g.Status.Conversations, g.Status.Occurrences)
		}
	}
}

// Same namespace rule as capabilitygapreports: the aggregate belongs to the
// PROVIDER, and the consumer project the conversations ran in sees nothing.
func TestCapabilityGapListScopedToProviderProject(t *testing.T) {
	store := gapreport.NewMemoryStore()
	if _, err := store.Insert(context.Background(), gapreport.InsertParams{
		ProviderProject: "streamco-platform", ServiceName: "svc",
		ConsumerProject: "demo-project", ContextID: "ctx-1",
		CapabilityKey: "workload-metrics", Capability: "cap", Summary: "s",
	}); err != nil {
		t.Fatal(err)
	}
	if items := gapList(t, store, "demo-project"); len(items) != 0 {
		t.Fatalf("consumer project sees %d gaps, want 0", len(items))
	}
}

func TestCapabilityGapListMissingNamespaceIsBadRequest(t *testing.T) {
	rest := NewCapabilityGapREST(gapreport.NewMemoryStore())
	if _, err := rest.List(context.Background(), &metainternalversion.ListOptions{}); !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}

func TestCapabilityGapProjectIdentityMismatchIsForbidden(t *testing.T) {
	ctx := request.WithUser(nsCtx("streamco-platform"), &user.DefaultInfo{
		Name: "alice",
		Extra: map[string][]string{
			tenant.ExtraParentType: {"Project"},
			tenant.ExtraParentName: {"other"},
		},
	})
	rest := NewCapabilityGapREST(gapreport.NewMemoryStore())
	if _, err := rest.List(ctx, &metainternalversion.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
}

// One conversation filing the same gap twice is one conversation that hit it.
func TestCapabilityGapCountsConversationsNotRows(t *testing.T) {
	store := gapreport.NewMemoryStore()
	for range 2 {
		if _, err := store.Insert(context.Background(), gapreport.InsertParams{
			ProviderProject: "streamco-platform", ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-same",
			CapabilityKey: "workload-metrics", Capability: "cap", Summary: "s",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := gapList(t, store, "streamco-platform")[0]
	if got.Status.Conversations != 1 || got.Status.Occurrences != 2 {
		t.Fatalf("Conversations/Occurrences = %d/%d; want 1/2", got.Status.Conversations, got.Status.Occurrences)
	}
}

// The occurrence rows carry the key too, so a reader can walk from an
// aggregate entry to the reports that make it up.
func TestCapabilityGapReportProjectsCapabilityKey(t *testing.T) {
	store := gapreport.NewMemoryStore()
	if _, err := store.Insert(context.Background(), gapreport.InsertParams{
		ProviderProject: "streamco-platform", ServiceName: "svc",
		ConsumerProject: "demo-project", ContextID: "ctx-1",
		CapabilityKey: "workload-metrics", Capability: "cap", Summary: "s",
	}); err != nil {
		t.Fatal(err)
	}
	obj, err := NewCapabilityGapReportREST(store).List(nsCtx("streamco-platform"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	items := obj.(*assistant.CapabilityGapReportList).Items
	if len(items) != 1 || items[0].Status.CapabilityKey != "workload-metrics" {
		t.Fatalf("Status = %+v; want capabilityKey projected", items[0].Status)
	}
}
