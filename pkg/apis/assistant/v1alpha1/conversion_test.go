package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// The conversions here are hand-written field copies, so a field added to one
// side and forgotten on the other compiles cleanly and silently disappears
// from what a provider sees. These round-trips are the only thing that catches
// that.
func TestCapabilityGapConversionRoundTrips(t *testing.T) {
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	want := CapabilityGap{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-metrics", Namespace: "streamco-platform"},
		Status: CapabilityGapStatus{
			ServiceName:   "streaming.streamco.example",
			CapabilityKey: "workload-metrics",
			Capability:    "CPU/memory usage metrics for workloads over a time window",
			Kind:          CapabilityGapKindInsufficientDetail,
			Conversations: 7,
			Occurrences:   9,
			FirstSeen:     now,
			LastSeen:      now,
		},
	}

	var internal assistant.CapabilityGap
	if err := convert_v1alpha1_CapabilityGap_To_assistant(&want, &internal); err != nil {
		t.Fatal(err)
	}
	var got CapabilityGap
	if err := convert_assistant_CapabilityGap_To_v1alpha1(&internal, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != want.Status {
		t.Errorf("Status round-trip = %+v; want %+v", got.Status, want.Status)
	}
	if got.Name != want.Name || got.Namespace != want.Namespace {
		t.Errorf("ObjectMeta round-trip = %q/%q", got.Namespace, got.Name)
	}
}

func TestCapabilityGapReportCarriesCapabilityKeyThroughConversion(t *testing.T) {
	in := CapabilityGapReport{Status: CapabilityGapReportStatus{
		CapabilityKey: "workload-metrics", Capability: "cap", Kind: CapabilityGapKindMissingCapability,
	}}
	var internal assistant.CapabilityGapReport
	if err := convert_v1alpha1_CapabilityGapReport_To_assistant(&in, &internal); err != nil {
		t.Fatal(err)
	}
	if internal.Status.CapabilityKey != "workload-metrics" {
		t.Fatalf("internal CapabilityKey = %q", internal.Status.CapabilityKey)
	}
	var out CapabilityGapReport
	if err := convert_assistant_CapabilityGapReport_To_v1alpha1(&internal, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status.CapabilityKey != "workload-metrics" {
		t.Fatalf("round-tripped CapabilityKey = %q", out.Status.CapabilityKey)
	}
}

// spec.archived and status.archivedAt are the conversation's lifecycle state;
// dropping either in conversion would make an archive silently not stick (the
// update path decodes v1alpha1 into the internal type) or not show.
func TestConversationConversionRoundTripsArchiveState(t *testing.T) {
	at := metav1.NewTime(time.Now().Truncate(time.Second))
	want := Conversation{
		ObjectMeta: metav1.ObjectMeta{Name: "ctx-1", Namespace: "demo"},
		Spec:       ConversationSpec{Archived: true},
		Status:     ConversationStatus{Title: "t", Name: "n", MessageCount: 2, ArchivedAt: &at},
	}
	var internal assistant.Conversation
	if err := convert_v1alpha1_Conversation_To_assistant(&want, &internal); err != nil {
		t.Fatal(err)
	}
	if !internal.Spec.Archived || internal.Status.ArchivedAt == nil || !internal.Status.ArchivedAt.Equal(&at) {
		t.Fatalf("to internal lost archive state: %+v", internal)
	}
	var got Conversation
	if err := convert_assistant_Conversation_To_v1alpha1(&internal, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Archived || got.Status.ArchivedAt == nil || !got.Status.ArchivedAt.Equal(&at) {
		t.Fatalf("round trip lost archive state: %+v", got)
	}
	// The pointer is copied, not shared, so mutating one side cannot reach
	// the other.
	if got.Status.ArchivedAt == want.Status.ArchivedAt {
		t.Fatal("archivedAt pointer aliased across conversion")
	}
}

// The generic list handler converts field selector labels through the scheme
// before the storage sees them, and rejects any label with no registered
// conversion. Without this, spec.archived=true would be a 400 from the handler
// no matter what the storage supports.
func TestConversationFieldLabelAdmitsSpecArchived(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := SchemeGroupVersion.WithKind("Conversation")
	if label, value, err := scheme.ConvertFieldLabel(gvk, "spec.archived", "true"); err != nil || label != "spec.archived" || value != "true" {
		t.Fatalf("spec.archived: %q=%q, %v", label, value, err)
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "metadata.name", "x"); err != nil {
		t.Fatalf("metadata.name should still convert: %v", err)
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "status.title", "x"); err == nil {
		t.Fatal("status.title should not be a known field selector")
	}
}
