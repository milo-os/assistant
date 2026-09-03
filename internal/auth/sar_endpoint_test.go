package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSAREndpointIsProjectScoped pins the URL shape. This is the regression
// test for a staging failure that looked exactly like a missing grant: the
// PolicyBindings were Ready and Milo answered `allowed: true` when asked at the
// project's control plane, but the assistant asked the CORE control plane,
// which answers `denied: true` for a project-scoped resource regardless of what
// has been granted. Fail-closed authorization turned that into a 403 on every
// request with nothing wrong in the logs.
func TestSAREndpointIsProjectScoped(t *testing.T) {
	const base = "https://milo-apiserver.datum-system.svc.cluster.local:6443"
	got := sarEndpoint(base, "datum-cloud")
	want := base +
		"/apis/resourcemanager.miloapis.com/v1alpha1/projects/datum-cloud/control-plane" +
		"/apis/authorization.k8s.io/v1/subjectaccessreviews"
	if got != want {
		t.Fatalf("sarEndpoint =\n  %s\nwant\n  %s", got, want)
	}
}

func TestSAREndpointTrimsAndEscapes(t *testing.T) {
	// A trailing slash on the configured URL must not double up.
	if got := sarEndpoint("https://milo:6443/", "p"); strings.Contains(got, "6443//") {
		t.Fatalf("double slash in %q", got)
	}
	// A project name is a path segment: it must be escaped, never allowed to
	// alter the path structure.
	got := sarEndpoint("https://milo:6443", "we/ird")
	if strings.Contains(got, "projects/we/ird") {
		t.Fatalf("project name was not escaped: %q", got)
	}
	if !strings.Contains(got, "projects/we%2Fird/control-plane") {
		t.Fatalf("unexpected escaping in %q", got)
	}
}

// TestReviewPostsToProjectPath drives the reviewer end to end and asserts the
// path the server actually receives, so the wiring between Review and
// sarEndpoint cannot silently regress.
func TestReviewPostsToProjectPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":{"allowed":true}}`))
	}))
	defer srv.Close()

	reviewer, err := newHTTPReviewer(SARConfig{APIURL: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPReviewer: %v", err)
	}
	review := &SubjectAccessReview{
		Spec: SubjectAccessReviewSpec{
			User: "someone@example.com",
			ResourceAttributes: &ResourceAttributes{
				Namespace: "datum-cloud",
				Group:     DefaultSARGroup,
				Resource:  DefaultSARResource,
				Verb:      DefaultSARVerb,
			},
		},
	}
	status, err := reviewer.Review(context.Background(), review)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !status.Allowed {
		t.Fatal("status.Allowed = false, want true")
	}
	want := "/apis/resourcemanager.miloapis.com/v1alpha1/projects/datum-cloud/control-plane" +
		"/apis/authorization.k8s.io/v1/subjectaccessreviews"
	if gotPath != want {
		t.Fatalf("server saw path\n  %s\nwant\n  %s", gotPath, want)
	}
}
