package patchcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// canned aggregated discovery: two Datum groups worth mentioning, one
// Kubernetes plumbing group that is not, a subresource, and a create-only
// resource that cannot be listed.
const aggregatedDiscoveryJSON = `{
  "kind": "APIGroupDiscoveryList",
  "apiVersion": "apidiscovery.k8s.io/v2",
  "items": [
    {"metadata": {"name": "compute.datumapis.com"},
     "versions": [{"version": "v1alpha1", "resources": [
       {"resource": "workloads", "singularResource": "workload",
        "responseKind": {"kind": "Workload"}, "scope": "Namespaced",
        "verbs": ["get", "list", "watch"]},
       {"resource": "workloads/status", "singularResource": "",
        "responseKind": {"kind": "Workload"}, "verbs": ["get", "list"]},
       {"resource": "instances", "singularResource": "instance",
        "responseKind": {"kind": "Instance"}, "verbs": ["get", "list"]}]}]},
    {"metadata": {"name": "networking.datumapis.com"},
     "versions": [{"version": "v1alpha1", "resources": [
       {"resource": "httpproxies", "singularResource": "httpproxy",
        "responseKind": {"kind": "HTTPProxy"}, "verbs": ["get", "list"]}]}]},
    {"metadata": {"name": "authorization.k8s.io"},
     "versions": [{"version": "v1", "resources": [
       {"resource": "subjectaccessreviews", "singularResource": "subjectaccessreview",
        "responseKind": {"kind": "SubjectAccessReview"}, "verbs": ["create"]}]}]},
    {"metadata": {"name": "events.k8s.io"},
     "versions": [{"version": "v1", "resources": [
       {"resource": "events", "singularResource": "event",
        "responseKind": {"kind": "Event"}, "verbs": ["get", "list"]}]}]}
  ]
}`

// the same server seen through the classic two-level walk
const classicGroupListJSON = `{
  "kind": "APIGroupList",
  "groups": [
    {"name": "compute.datumapis.com",
     "versions": [{"groupVersion": "compute.datumapis.com/v1alpha1", "version": "v1alpha1"}],
     "preferredVersion": {"groupVersion": "compute.datumapis.com/v1alpha1", "version": "v1alpha1"}},
    {"name": "events.k8s.io",
     "versions": [{"groupVersion": "events.k8s.io/v1", "version": "v1"}],
     "preferredVersion": {"groupVersion": "events.k8s.io/v1", "version": "v1"}}
  ]
}`

const classicResourceListJSON = `{
  "kind": "APIResourceList",
  "groupVersion": "compute.datumapis.com/v1alpha1",
  "resources": [
    {"name": "workloads", "singularName": "workload", "namespaced": true,
     "kind": "Workload", "verbs": ["get", "list", "watch"]},
    {"name": "workloads/status", "singularName": "", "kind": "Workload", "verbs": ["get"]}
  ]
}`

const workloadListJSON = `{
  "kind": "WorkloadList",
  "items": [
    {"metadata": {"name": "api-backend", "namespace": "default",
                  "creationTimestamp": "2026-01-02T00:00:00Z"}},
    {"metadata": {"name": "web-frontend", "namespace": "default",
                  "creationTimestamp": "2026-03-04T00:00:00Z"}},
    {"metadata": {"name": "batch-runner", "namespace": "other",
                  "creationTimestamp": "2025-12-01T00:00:00Z"}}
  ]
}`

// discoveryServer answers the aggregated-discovery request only when the client
// asked for it, so one server can stand in for both an apiserver that speaks it
// and one that does not.
func discoveryServer(t *testing.T, aggregated bool) (ReadView, func()) {
	t.Helper()
	const prefix = "/apis/resourcemanager.miloapis.com/v1alpha1/projects/demo-project/control-plane"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		switch {
		case path == "/apis" && aggregated && strings.Contains(r.Header.Get("Accept"), "APIGroupDiscoveryList"):
			_, _ = w.Write([]byte(aggregatedDiscoveryJSON))
		case path == "/apis":
			_, _ = w.Write([]byte(classicGroupListJSON))
		case path == "/apis/compute.datumapis.com/v1alpha1":
			_, _ = w.Write([]byte(classicResourceListJSON))
		case path == "/apis/compute.datumapis.com/v1alpha1/workloads":
			_, _ = w.Write([]byte(workloadListJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"the server could not find ` + path + `"}`))
		}
	}))
	view := ReadView{apiHost: srv.URL, token: func() (string, error) { return "tok", nil }}
	return view, srv.Close
}

func tokensOf(kinds []resourceKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k.token)
	}
	return out
}

func TestDiscoverResourceKindsAggregated(t *testing.T) {
	view, done := discoveryServer(t, true)
	defer done()

	kinds, err := discoverResourceKinds(context.Background(), view, "demo-project")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	got := strings.Join(tokensOf(kinds), ",")
	// Sorted by token; no subresource, no create-only review, no events group.
	if want := "httpproxy,instance,workload"; got != want {
		t.Fatalf("tokens = %s, want %s", got, want)
	}
	for _, k := range kinds {
		if k.token == "httpproxy" && (k.group != "networking.datumapis.com" || k.kind != "HTTPProxy") {
			t.Errorf("httpproxy decoded as %+v", k)
		}
	}
}

// An apiserver that ignores the Accept answers with the classic APIGroupList;
// the walk has to notice that and follow each group instead of failing.
func TestDiscoverResourceKindsFallsBackToClassicWalk(t *testing.T) {
	view, done := discoveryServer(t, false)
	defer done()

	kinds, err := discoverResourceKinds(context.Background(), view, "demo-project")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got := strings.Join(tokensOf(kinds), ","); got != "workload" {
		t.Fatalf("tokens = %s, want workload", got)
	}
}

func TestDiscoverResourceKindsSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"User \"u\" cannot list resources"}`))
	}))
	defer srv.Close()

	view := ReadView{apiHost: srv.URL, token: func() (string, error) { return "t", nil }}
	_, err := discoverResourceKinds(context.Background(), view, "demo-project")
	if err == nil || !strings.Contains(err.Error(), "cannot list resources") {
		t.Fatalf("want the apiserver's own message, got: %v", err)
	}
}

// Instances come back newest-first, across every namespace: the project, not a
// namespace inside it, is the scope a mention names.
func TestListResourceNamesNewestFirst(t *testing.T) {
	view, done := discoveryServer(t, true)
	defer done()

	k := resourceKind{token: "workload", plural: "workloads", kind: "Workload",
		group: "compute.datumapis.com", version: "v1alpha1"}
	names, err := listResourceNames(context.Background(), view, "demo-project", k)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := strings.Join(names, ","); got != "web-frontend,api-backend,batch-runner" {
		t.Fatalf("names = %s, want newest-first across namespaces", got)
	}
}

// Two groups can each own a singular called "policy"; the picker must offer one
// row for it, and the same one on every keystroke.
func TestSortKindsDedupesTokensStably(t *testing.T) {
	kinds := sortKinds([]resourceKind{
		{token: "policy", group: "z.example.com"},
		{token: "workload", group: "compute.datumapis.com"},
		{token: "policy", group: "a.example.com"},
	})
	if got := strings.Join(tokensOf(kinds), ","); got != "policy,workload" {
		t.Fatalf("tokens = %s, want policy,workload", got)
	}
	if kinds[0].group != "a.example.com" {
		t.Errorf("duplicate token resolved to %s, want the first group alphabetically", kinds[0].group)
	}
}
