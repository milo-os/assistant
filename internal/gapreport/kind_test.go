package gapreport

import (
	"errors"
	"testing"
)

// TestParseKindDefaultsToMissingCapability pins the compatibility rule the
// whole feature rests on: kinds were added after the tool shipped, so an
// omitted kind must keep meaning what every report meant before they existed.
func TestParseKindDefaultsToMissingCapability(t *testing.T) {
	got, err := ParseKind("")
	if err != nil {
		t.Fatalf("ParseKind(\"\") = %v", err)
	}
	if got != KindMissingCapability {
		t.Fatalf("ParseKind(\"\") = %q; want %q", got, KindMissingCapability)
	}
}

func TestParseKindAcceptsEveryDeclaredKind(t *testing.T) {
	for _, k := range Kinds {
		got, err := ParseKind(string(k))
		if err != nil || got != k {
			t.Fatalf("ParseKind(%q) = %q, %v; want %q, nil", k, got, err, k)
		}
	}
}

// An unrecognized kind carries no salvageable meaning, so unlike an omitted
// one it is rejected rather than defaulted — a value no reader can interpret
// must not reach the provider's feed.
func TestParseKindRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"missingcapability", "Missing Capability", "Wrong", "MissingCapability "} {
		if _, err := ParseKind(bad); !errors.Is(err, ErrUnknownKind) {
			t.Fatalf("ParseKind(%q) = %v; want ErrUnknownKind", bad, err)
		}
	}
}

func TestNeedsEvidence(t *testing.T) {
	for kind, want := range map[Kind]bool{
		"":                       false, // an unset kind is MissingCapability
		KindMissingCapability:    false,
		KindInsufficientDetail:   true,
		KindMisleadingOutput:     true,
		KindUnactionableGuidance: true,
	} {
		if got := NeedsEvidence(kind); got != want {
			t.Errorf("NeedsEvidence(%q) = %v; want %v", kind, got, want)
		}
	}
}

func TestEvidenceIsZero(t *testing.T) {
	if !(Evidence{}).IsZero() {
		t.Error("empty Evidence must report IsZero")
	}
	if (Evidence{Tool: "workloads_list"}).IsZero() {
		t.Error("partially filled Evidence must not report IsZero")
	}
}
