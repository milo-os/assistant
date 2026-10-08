package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func refTokens(refs []ResourceRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Kind+"/"+r.Name)
	}
	return out
}

func TestResourcesInToolResultReadsAListing(t *testing.T) {
	content := `{
	  "apiVersion": "compute.datumapis.com/v1alpha1",
	  "kind": "WorkloadList",
	  "items": [
	    {"metadata": {"name": "web-frontend"}, "status": {"available": true}},
	    {"metadata": {"name": "api-backend"}},
	    {"metadata": {"name": "batch-runner"}}
	  ]
	}`
	got := resourcesInToolResult(content)
	want := []string{"workload/web-frontend", "workload/api-backend", "workload/batch-runner"}
	if fmt.Sprint(refTokens(got)) != fmt.Sprint(want) {
		t.Fatalf("refs = %v, want %v — items typed by the list that holds them", refTokens(got), want)
	}
	for _, r := range got {
		if r.APIGroup != "compute.datumapis.com" {
			t.Fatalf("ref %+v should inherit the list's API group", r)
		}
	}
}

// A single object names itself, and a nested object with a kind of its own is
// not mistaken for its parent's kind.
func TestResourcesInToolResultReadsSingleObjects(t *testing.T) {
	content := `{
	  "apiVersion": "compute.datumapis.com/v1alpha1",
	  "kind": "Workload",
	  "metadata": {"name": "web-frontend", "namespace": "default"},
	  "spec": {"proxy": {
	    "apiVersion": "networking.datumapis.com/v1alpha1",
	    "kind": "HTTPProxy",
	    "metadata": {"name": "edge"}
	  }}
	}`
	got := resourcesInToolResult(content)
	want := []string{"workload/web-frontend", "httpproxy/edge"}
	if fmt.Sprint(refTokens(got)) != fmt.Sprint(want) {
		t.Fatalf("refs = %v, want %v", refTokens(got), want)
	}
	if got[1].APIGroup != "networking.datumapis.com" {
		t.Fatalf("the nested object should carry its own group, got %+v", got[1])
	}
}

// metadata is the object's own; the references and labels inside it are not
// resources this result reported.
func TestResourcesInToolResultIgnoresOwnerReferences(t *testing.T) {
	content := `{
	  "kind": "Workload",
	  "apiVersion": "compute.datumapis.com/v1alpha1",
	  "metadata": {
	    "name": "web-frontend",
	    "ownerReferences": [{"kind": "Deployment", "name": "owner", "metadata": {"name": "owner"}}]
	  }
	}`
	if got := refTokens(resourcesInToolResult(content)); fmt.Sprint(got) != fmt.Sprint([]string{"workload/web-frontend"}) {
		t.Fatalf("refs = %v, want only the object itself", got)
	}
}

// A result is often JSON with a sentence in front of it.
func TestResourcesInToolResultFindsJSONAfterProse(t *testing.T) {
	content := "Found 1 workload:\n{\"kind\":\"Workload\",\"apiVersion\":\"compute.datumapis.com/v1alpha1\",\"metadata\":{\"name\":\"api-backend\"}}"
	if got := refTokens(resourcesInToolResult(content)); fmt.Sprint(got) != fmt.Sprint([]string{"workload/api-backend"}) {
		t.Fatalf("refs = %v, want the embedded object", got)
	}
}

// Anything that is not Kubernetes-shaped JSON yields nothing — no guessing at
// names in prose, tables or logs.
func TestResourcesInToolResultIgnoresEverythingElse(t *testing.T) {
	for _, content := range []string{
		"",
		"   ",
		"the web-frontend workload is fine",
		"NAME           READY\nweb-frontend   3/3",
		`{"ok": true, "count": 3}`,
		`{"kind": "Workload"}`,                      // no name
		`{"metadata": {"name": "web-frontend"}}`,    // no kind
		`{"kind": "Workload", "metadata": "oops"}`,  // metadata is not an object
		`{"kind": 7, "metadata": {"name": "what"}}`, // kind is not a string
		"not json at all { oh dear",
	} {
		if got := resourcesInToolResult(content); len(got) != 0 {
			t.Errorf("content %q yielded %v, want nothing", content, refTokens(got))
		}
	}
}

// The same resource named twice is one reference.
func TestResourcesInToolResultDedupes(t *testing.T) {
	content := `{"kind":"WorkloadList","apiVersion":"compute.datumapis.com/v1alpha1","items":[
	  {"metadata":{"name":"web"}},{"metadata":{"name":"web"}}]}`
	if got := refTokens(resourcesInToolResult(content)); len(got) != 1 {
		t.Fatalf("refs = %v, want one", got)
	}
}

// The content is provider-controlled, so every axis of the walk is bounded.
func TestResourcesInToolResultIsBounded(t *testing.T) {
	// More objects than the cap.
	items := make([]string, 0, maxToolResources*2)
	for i := range maxToolResources * 2 {
		items = append(items, fmt.Sprintf(`{"metadata":{"name":"w-%d"}}`, i))
	}
	listing := `{"kind":"WorkloadList","apiVersion":"g/v1","items":[` + strings.Join(items, ",") + `]}`
	if got := resourcesInToolResult(listing); len(got) != maxToolResources {
		t.Fatalf("got %d refs, want the cap of %d", len(got), maxToolResources)
	}

	// Deeper than the depth budget: the object at the bottom is out of reach.
	deep := `{"kind":"Workload","apiVersion":"g/v1","metadata":{"name":"deep"}}`
	for range maxResourceDepth + 2 {
		deep = `{"a":` + deep + `}`
	}
	if got := resourcesInToolResult(deep); len(got) != 0 {
		t.Fatalf("refs = %v, want nothing past the depth budget", refTokens(got))
	}

	// Bigger than the byte budget: not parsed at all.
	padding, _ := json.Marshal(strings.Repeat("x", maxToolResultBytes))
	huge := `{"kind":"Workload","apiVersion":"g/v1","metadata":{"name":"big"},"pad":` + string(padding) + `}`
	if got := resourcesInToolResult(huge); len(got) != 0 {
		t.Fatalf("refs = %v, want nothing past the byte budget", refTokens(got))
	}
}

// Two runs over the same result must agree: the client keeps these in arrival
// order, and Go's map iteration is deliberately random.
func TestResourcesInToolResultIsDeterministic(t *testing.T) {
	content := `{"alpha":{"kind":"Workload","apiVersion":"g/v1","metadata":{"name":"a"}},
	            "beta":{"kind":"HTTPProxy","apiVersion":"g/v1","metadata":{"name":"b"}},
	            "gamma":{"kind":"Instance","apiVersion":"g/v1","metadata":{"name":"c"}}}`
	first := fmt.Sprint(refTokens(resourcesInToolResult(content)))
	for range 20 {
		if got := fmt.Sprint(refTokens(resourcesInToolResult(content))); got != first {
			t.Fatalf("order changed between runs: %s then %s", first, got)
		}
	}
}
