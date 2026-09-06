package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
