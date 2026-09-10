package basetools_test

import (
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/basetools"
)

const workloadDiscovery = `{"resources":[{"name":"workloads","kind":"Workload","namespaced":true}]}`

func TestResourcesListReportsWhatThePlatformIsSaying(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery)).
		route("/workloads", ok(`{"items":[
			{"metadata":{"name":"web","namespace":"default","creationTimestamp":"2026-01-02T03:04:05Z",
			  "labels":{"tier":"front"}},
			 "status":{"conditions":[{"type":"Available","status":"False","reason":"ImageUnavailable",
			   "message":"the image could not be pulled"}]}},
			{"metadata":{"name":"api","namespace":"default"}}
		]}`))

	out := decodeInto[basetools.ResourcesListOutput](t, call(t, p.tools(t, "demo", "tok"),
		basetools.ResourcesListToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`))

	if len(out.Resources) != 2 {
		t.Fatalf("got %d resources, want 2", len(out.Resources))
	}
	// Sorted by name so two calls in one conversation read the same way.
	if out.Resources[0].Name != "api" || out.Resources[1].Name != "web" {
		t.Fatalf("order = %q, %q", out.Resources[0].Name, out.Resources[1].Name)
	}
	web := out.Resources[1]
	if len(web.Conditions) != 1 || web.Conditions[0].Reason != "ImageUnavailable" {
		t.Fatalf("conditions = %+v", web.Conditions)
	}
	if web.Labels["tier"] != "front" {
		t.Fatalf("labels = %+v", web.Labels)
	}
}

// A kind held in a namespace is read in the one a project's own resources are
// in, unless the person names another.
func TestResourcesListDefaultsToTheProjectsOwnNamespace(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery)).
		route("/workloads", ok(`{"items":[]}`))
	tools := p.tools(t, "demo", "tok")

	call(t, tools, basetools.ResourcesListToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`)
	if last := p.seen()[len(p.seen())-1]; !strings.HasSuffix(last.path, "/namespaces/default/workloads") {
		t.Fatalf("path = %q, want the default namespace", last.path)
	}

	call(t, tools, basetools.ResourcesListToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload","namespace":"team-a"}`)
	if last := p.seen()[len(p.seen())-1]; !strings.HasSuffix(last.path, "/namespaces/team-a/workloads") {
		t.Fatalf("path = %q, want the named namespace", last.path)
	}
}

func TestResourcesListSaysWhatToDoAboutAnUnknownKind(t *testing.T) {
	p := newPlatform(t).route("/apis/nope.example/v1", ok(`{"resources":[]}`))

	refusal := callErr(t, p.tools(t, "demo", "tok"), basetools.ResourcesListToolName,
		`{"group":"nope.example","version":"v1","kind":"Widget"}`)
	if !strings.Contains(refusal, "does not offer") {
		t.Fatalf("refusal = %q", refusal)
	}
	if !strings.Contains(refusal, "case-sensitive") {
		t.Fatalf("refusal should say what to check: %q", refusal)
	}
}

func TestResourcesListSaysWhoCanGrantAccess(t *testing.T) {
	p := newPlatform(t).route("/apis/compute.datumapis.com/v1alpha1", status(403, "forbidden"))

	refusal := callErr(t, p.tools(t, "demo", "tok"), basetools.ResourcesListToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`)
	if !strings.Contains(refusal, "administers the project") {
		t.Fatalf("refusal = %q", refusal)
	}
}

// A manifest read back is one that can be edited and planned: the platform's
// own bookkeeping is gone, and the observed state is reported beside it rather
// than inside it.
func TestResourcesGetReturnsAnEditableManifest(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery)).
		route("/workloads/web", ok(`{
			"apiVersion":"compute.datumapis.com/v1alpha1","kind":"Workload",
			"metadata":{"name":"web","namespace":"default","resourceVersion":"991","uid":"abc",
			  "generation":4,"creationTimestamp":"2026-01-02T03:04:05Z","managedFields":[{"manager":"x"}],
			  "annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}","team":"platform"}},
			"spec":{"replicas":2},
			"status":{"conditions":[{"type":"Ready","status":"True"}]}}`))

	out := decodeInto[basetools.ResourcesGetOutput](t, call(t, p.tools(t, "demo", "tok"),
		basetools.ResourcesGetToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload","name":"web"}`))

	for _, gone := range []string{"resourceVersion", "uid:", "managedFields", "generation", "creationTimestamp", "status:", "last-applied"} {
		if strings.Contains(out.Manifest, gone) {
			t.Fatalf("manifest still carries %q:\n%s", gone, out.Manifest)
		}
	}
	for _, kept := range []string{"replicas: 2", "team: platform", "name: web", "namespace: default"} {
		if !strings.Contains(out.Manifest, kept) {
			t.Fatalf("manifest lost %q:\n%s", kept, out.Manifest)
		}
	}
	if len(out.Conditions) != 1 || out.Conditions[0].Type != "Ready" {
		t.Fatalf("conditions = %+v", out.Conditions)
	}
}

func TestResourcesGetSaysWhereToLookWhenThereIsNoSuchResource(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery)).
		route("/workloads/web", status(404, `workloads.compute.datumapis.com "web" not found`))

	refusal := callErr(t, p.tools(t, "demo", "tok"), basetools.ResourcesGetToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload","name":"web"}`)
	if !strings.Contains(refusal, "resources_list") {
		t.Fatalf("refusal should point somewhere useful: %q", refusal)
	}
}

func TestResourcesGetNeedsAName(t *testing.T) {
	p := newPlatform(t)
	refusal := callErr(t, p.tools(t, "demo", "tok"), basetools.ResourcesGetToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`)
	if !strings.Contains(refusal, "a name is required") {
		t.Fatalf("refusal = %q", refusal)
	}
	if len(p.seen()) != 0 {
		t.Fatal("a malformed call should not reach the platform")
	}
}
