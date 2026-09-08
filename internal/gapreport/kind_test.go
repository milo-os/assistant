package gapreport

import (
	"errors"
	"strings"
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

// TestValidateCapabilityKey pins the grammar. The key's whole job is to be
// reproduced character-for-character by a later conversation, so anything the
// model might write two ways — capitals, spaces, underscores — is junk and is
// rejected rather than stored under a spelling nobody will match.
func TestValidateCapabilityKey(t *testing.T) {
	valid := []string{
		"",                 // optional: no key is not an invalid key
		"workload-metrics", // the canonical shape
		"metrics",
		"cpu-memory-usage-over-time",
		"s3",
		"2fa-status", // a leading digit is a real key, not junk
		strings.Repeat("a", MaxCapabilityKeyLen),
	}
	for _, k := range valid {
		if err := ValidateCapabilityKey(k); err != nil {
			t.Errorf("ValidateCapabilityKey(%q) = %v; want nil", k, err)
		}
	}

	junk := []string{
		"Workload-Metrics",            // capitals
		"workload metrics",            // spaces
		"workload_metrics",            // underscores
		"workload--metrics",           // doubled dash
		"-workload",                   // leading dash
		"workload-",                   // trailing dash
		"workload/metrics",            // path-ish
		"time-series CPU/mem métrics", // prose with punctuation and accents
	}
	for _, k := range junk {
		if err := ValidateCapabilityKey(k); !errors.Is(err, ErrInvalidCapabilityKey) {
			t.Errorf("ValidateCapabilityKey(%q) = %v; want ErrInvalidCapabilityKey", k, err)
		}
	}

	tooLong := strings.Repeat("a", MaxCapabilityKeyLen+1)
	if err := ValidateCapabilityKey(tooLong); !errors.Is(err, ErrCapabilityKeyTooLong) {
		t.Errorf("ValidateCapabilityKey(%d chars) = %v; want ErrCapabilityKeyTooLong", len(tooLong), err)
	}
}
