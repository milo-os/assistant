package auth

import (
	"testing"
)

// ── ExtractBearerToken ────────────────────────────────────────

func TestExtractBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer abc":   "abc", // case-insensitive scheme
		"Bearer  abc ": "abc", // trimmed
		"":             "",
		"Basic abc":    "",
		"abc":          "",
	}
	for in, want := range cases {
		if got := ExtractBearerToken(in); got != want {
			t.Errorf("ExtractBearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Dev authenticator ─────────────────────────────────────────

func mustErr[T any](_ T, err error) error { return err }

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	authErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is not *auth.Error: %v", err)
	}
	if authErr.Status != want {
		t.Errorf("status = %d, want %d (msg: %s)", authErr.Status, want, authErr.Message)
	}
}
