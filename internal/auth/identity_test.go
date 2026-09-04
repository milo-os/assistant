package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSARCarriesFullIdentity is the regression test for a staging 403 that
// looked exactly like a missing IAM grant. The PolicyBindings reconciled Ready
// and Milo answered `allowed: true` to a SelfSubjectAccessReview — where Milo
// resolves the whole identity itself — but the assistant's SubjectAccessReview
// carried only user.username, and Milo binds policy to the user's ID. The grant
// was real; the review just did not describe the subject well enough to match
// it.
func TestSARCarriesFullIdentity(t *testing.T) {
	var got SubjectAccessReview
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		got.Status = &SubjectAccessReviewStatus{Allowed: true}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(got)
	}))
	defer srv.Close()

	az, err := NewSubjectAccessReviewAuthorizer(SARConfig{
		APIURL:   srv.URL,
		Group:    DefaultSARGroup,
		Resource: DefaultSARResource,
		Verb:     DefaultSARVerb,
	})
	if err != nil {
		t.Fatalf("NewSubjectAccessReviewAuthorizer: %v", err)
	}

	principal := Principal{
		Subject: "swells@datum.net",
		UID:     "327293583252002829",
		Groups:  []string{"system:authenticated"},
	}
	if err := az.AuthorizeProject(context.Background(), principal, "datum-cloud"); err != nil {
		t.Fatalf("AuthorizeProject: %v", err)
	}

	if got.Spec.User != principal.Subject {
		t.Errorf("spec.user = %q, want %q", got.Spec.User, principal.Subject)
	}
	if got.Spec.UID != principal.UID {
		t.Errorf("spec.uid = %q, want %q", got.Spec.UID, principal.UID)
	}
	if len(got.Spec.Groups) != 1 || got.Spec.Groups[0] != "system:authenticated" {
		t.Errorf("spec.groups = %v, want [system:authenticated]", got.Spec.Groups)
	}
}

// TestAuthorizationCacheKeyedOnUID: two principals sharing a username but not a
// UID must not share a cached allow.
func TestAuthorizationCacheKeyedOnUID(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in SubjectAccessReview
		_ = json.NewDecoder(r.Body).Decode(&in)
		calls++
		// Only the first UID is entitled.
		in.Status = &SubjectAccessReviewStatus{Allowed: in.Spec.UID == "uid-1"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(in)
	}))
	defer srv.Close()

	az, err := NewSubjectAccessReviewAuthorizer(SARConfig{
		APIURL: srv.URL, Group: DefaultSARGroup,
		Resource: DefaultSARResource, Verb: DefaultSARVerb,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := az.AuthorizeProject(ctx, Principal{Subject: "same", UID: "uid-1"}, "p"); err != nil {
		t.Fatalf("uid-1 should be allowed: %v", err)
	}
	if err := az.AuthorizeProject(ctx, Principal{Subject: "same", UID: "uid-2"}, "p"); err == nil {
		t.Fatal("uid-2 must not inherit uid-1's cached allow")
	}
	if calls != 2 {
		t.Fatalf("reviewer called %d times, want 2 (no cache collision)", calls)
	}
}
