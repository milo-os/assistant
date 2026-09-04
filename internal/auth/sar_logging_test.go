package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestDeniedReviewIsLogged: a fail-closed authorizer must say why. Without this
// a missing grant, an unmatched subject, and an authorizer error are
// indistinguishable to an operator — which is exactly how a staging 403 stayed
// unexplained across three deploys.
func TestDeniedReviewIsLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	az, err := NewSubjectAccessReviewAuthorizer(SARConfig{
		Logger:   logger,
		Group:    DefaultSARGroup,
		Resource: DefaultSARResource,
		Verb:     DefaultSARVerb,
		Reviewer: &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: false, Reason: "no binding matched"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	principal := Principal{Subject: "swells@datum.net", UID: "327293583252002829"}
	if err := az.AuthorizeProject(context.Background(), principal, "datum-cloud"); err == nil {
		t.Fatal("expected denial")
	}

	line := buf.String()
	if !strings.Contains(line, "authz.sar.denied") {
		t.Fatalf("no denial log emitted: %s", line)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	for k, want := range map[string]string{
		"subject":  "swells@datum.net",
		"uid":      "327293583252002829",
		"project":  "datum-cloud",
		"resource": DefaultSARResource,
		"reason":   "no binding matched",
	} {
		if got, _ := rec[k].(string); got != want {
			t.Errorf("log[%q] = %q, want %q", k, got, want)
		}
	}
}

// A nil logger must not panic — tests and embedders construct without one.
func TestDeniedReviewWithoutLogger(t *testing.T) {
	az, err := NewSubjectAccessReviewAuthorizer(SARConfig{
		Group: DefaultSARGroup, Resource: DefaultSARResource, Verb: DefaultSARVerb,
		Reviewer: &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := az.AuthorizeProject(context.Background(), Principal{Subject: "x"}, "p"); err == nil {
		t.Fatal("expected denial")
	}
}
