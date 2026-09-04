package auth

import (
	"context"
	"testing"
)

// TestSARCarriesProjectParentContext is the regression test for the staging
// 403 that survived three other fixes. Milo's OpenFGA authorizer decides the
// SCOPE of a review from the subject's extra — not from the request path and
// not from resourceAttributes.namespace. Without all three parent keys its
// extractParentContext returns nil and the review is answered at CLUSTER scope,
// where a project-scoped resource is denied however it has been granted. The
// webhook logged exactly that: scope="cluster" decision="denied", while the
// same question asked with project context came back allowed.
func TestSARCarriesProjectParentContext(t *testing.T) {
	fake := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true}}
	az, err := NewSubjectAccessReviewAuthorizer(SARConfig{
		Reviewer: fake, Group: DefaultSARGroup,
		Resource: DefaultSARResource, Verb: DefaultSARVerb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := az.AuthorizeProject(context.Background(), Principal{Subject: "u", UID: "1"}, "datum-cloud"); err != nil {
		t.Fatalf("AuthorizeProject: %v", err)
	}

	extra := fake.last.Spec.Extra
	for key, want := range map[string]string{
		parentAPIGroupExtraKey: projectAPIGroup,
		parentKindExtraKey:     projectKind,
		parentNameExtraKey:     "datum-cloud",
	} {
		got, ok := extra[key]
		if !ok {
			t.Errorf("extra[%q] missing — review would be evaluated at cluster scope", key)
			continue
		}
		// extractParentContext requires exactly one value per key.
		if len(got) != 1 || got[0] != want {
			t.Errorf("extra[%q] = %v, want exactly [%q]", key, got, want)
		}
	}
}

// The caller's own extra must survive; the parent keys describe the question,
// not the caller, so they win on collision.
func TestPrincipalExtraIsPreserved(t *testing.T) {
	fake := &fakeReviewer{status: &SubjectAccessReviewStatus{Allowed: true}}
	az, err := NewSubjectAccessReviewAuthorizer(SARConfig{
		Reviewer: fake, Group: DefaultSARGroup,
		Resource: DefaultSARResource, Verb: DefaultSARVerb,
	})
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Subject: "u", UID: "1",
		Extra: map[string][]string{
			"authentication.kubernetes.io/credential-id": {"abc"},
			parentNameExtraKey:                           {"some-other-project"},
		},
	}
	if err := az.AuthorizeProject(context.Background(), principal, "datum-cloud"); err != nil {
		t.Fatal(err)
	}
	extra := fake.last.Spec.Extra
	if v := extra["authentication.kubernetes.io/credential-id"]; len(v) != 1 || v[0] != "abc" {
		t.Errorf("caller extra dropped: %v", extra)
	}
	if v := extra[parentNameExtraKey]; len(v) != 1 || v[0] != "datum-cloud" {
		t.Errorf("parent name = %v, want the project under review", v)
	}
	// Mutating the returned map must not corrupt the caller's principal.
	if v := principal.Extra[parentNameExtraKey]; v[0] != "some-other-project" {
		t.Errorf("principal.Extra was mutated: %v", v)
	}
}
