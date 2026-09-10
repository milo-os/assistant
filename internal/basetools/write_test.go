package basetools_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/basetools"
)

const (
	networkDiscovery = `{"resources":[{"name":"networks","kind":"Network","namespaced":true}]}`

	workloadManifest = `apiVersion: compute.datumapis.com/v1alpha1
kind: Workload
metadata:
  name: api
spec:
  replicas: 2
  networkRef:
    name: default
`
	networkManifest = `apiVersion: compute.datumapis.com/v1alpha1
kind: Network
metadata:
  name: default
spec:
  mode: Auto
`
)

// writePlatform serves a project where nothing exists yet and every write is
// accepted.
func writePlatform(t *testing.T) *platform {
	return newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery+"")).
		routeFor(http.MethodGet, "/workloads/api", status(404, `workloads "api" not found`)).
		routeFor(http.MethodPost, "/workloads", ok(`{"metadata":{"name":"api","resourceVersion":"1"}}`))
}

func planOf(t *testing.T, tools agentcore.ToolSet, manifests ...string) basetools.PlanOutput {
	t.Helper()
	return decodeInto[basetools.PlanOutput](t, call(t, tools, basetools.ResourcesPlanToolName,
		manifestsInput(manifests...)))
}

func manifestsInput(manifests ...string) string {
	raw, _ := json.Marshal(map[string]any{"manifests": manifests})
	return string(raw)
}

func applyInput(token string, manifests ...string) string {
	raw, _ := json.Marshal(map[string]any{"manifests": manifests, "planToken": token})
	return string(raw)
}

// ------------------------------------------------------------ the change path

// A service that cannot check a plan token must not issue something that looks
// like one, so the change path is absent rather than broken.
func TestTheChangePathNeedsAKey(t *testing.T) {
	readOnly := newPlatform(t).tools(t, "demo", "tok")
	for _, name := range basetools.MutatingToolNames() {
		if _, exists := readOnly[name]; exists {
			t.Fatalf("%s was composed with no key to bind a plan with", name)
		}
	}
	if _, exists := readOnly[basetools.ResourcesValidateToolName]; exists {
		t.Fatal("resources_validate was composed with no key")
	}

	withKey := newPlatform(t).writeTools(t, "demo", "tok")
	for _, name := range append(basetools.MutatingToolNames(), basetools.ResourcesValidateToolName) {
		if _, exists := withKey[name]; !exists {
			t.Fatalf("%s is missing", name)
		}
	}
}

// ------------------------------------------------------------------ validate

// The whole promise of validate is that it keeps nothing.
func TestValidateWritesNothing(t *testing.T) {
	p := writePlatform(t)

	out := decodeInto[basetools.ValidateOutput](t, call(t, p.writeTools(t, "demo", "tok"),
		basetools.ResourcesValidateToolName, manifestsInput(workloadManifest)))

	if !out.Valid || len(out.Results) != 1 || out.Results[0].Action != "create" {
		t.Fatalf("out = %+v", out)
	}
	assertNothingPersisted(t, p)
}

func TestValidateReportsTheFieldPathThePlatformNamed(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery)).
		routeFor(http.MethodGet, "/workloads/api", status(404, "not found")).
		routeFor(http.MethodPost, "/workloads", reply{code: 422, body: `{"kind":"Status","code":422,
			"message":"Workload \"api\" is invalid",
			"details":{"causes":[{"field":"spec.replicas","message":"must be at least 1"}]}}`})

	out := decodeInto[basetools.ValidateOutput](t, call(t, p.writeTools(t, "demo", "tok"),
		basetools.ResourcesValidateToolName, manifestsInput(workloadManifest)))

	if out.Valid {
		t.Fatal("a rejected manifest was reported as valid")
	}
	if len(out.Results[0].Errors) == 0 || out.Results[0].Errors[0].Field != "spec.replicas" {
		t.Fatalf("errors = %+v", out.Results[0].Errors)
	}
	assertNothingPersisted(t, p)
}

func TestValidateRejectsAManifestThatDoesNotSayWhatItIs(t *testing.T) {
	p := writePlatform(t)

	out := decodeInto[basetools.ValidateOutput](t, call(t, p.writeTools(t, "demo", "tok"),
		basetools.ResourcesValidateToolName, manifestsInput("metadata:\n  name: api\n")))

	if out.Valid {
		t.Fatal("a manifest with no apiVersion was accepted")
	}
	if got := out.Results[0].Errors[0].Field; got != "apiVersion" {
		t.Fatalf("field = %q", got)
	}
}

// A manifest that reads badly is one item's problem, not the whole call's.
func TestValidateReportsEachManifestSeparately(t *testing.T) {
	p := writePlatform(t)

	out := decodeInto[basetools.ValidateOutput](t, call(t, p.writeTools(t, "demo", "tok"),
		basetools.ResourcesValidateToolName, manifestsInput(workloadManifest, "kind: Workload\n")))

	if len(out.Results) != 2 {
		t.Fatalf("results = %+v", out.Results)
	}
	if !out.Results[0].Valid || out.Results[1].Valid {
		t.Fatalf("verdicts = %v, %v", out.Results[0].Valid, out.Results[1].Valid)
	}
}

// -------------------------------------------------------------------- plan

func TestPlanWritesNothingAndReturnsAToken(t *testing.T) {
	p := writePlatform(t)

	out := planOf(t, p.writeTools(t, "demo", "tok"), workloadManifest)

	if !out.Valid || out.PlanToken == "" {
		t.Fatalf("out = %+v", out)
	}
	if out.Results[0].Manifest == "" {
		t.Fatal("a plan has to return the manifest the token covers")
	}
	expiry, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt = %q: %v", out.ExpiresAt, err)
	}
	if time.Until(expiry) > 16*time.Minute {
		t.Fatalf("a plan is good for too long: %s", out.ExpiresAt)
	}
	assertNothingPersisted(t, p)
}

func TestPlanMintsNoTokenForSomethingThatWasRejected(t *testing.T) {
	p := writePlatform(t)

	out := planOf(t, p.writeTools(t, "demo", "tok"), "kind: Workload\n")

	if out.Valid || out.PlanToken != "" {
		t.Fatalf("a rejected plan was given a token: %+v", out)
	}
	if !strings.Contains(out.Note, "nothing can be applied") {
		t.Fatalf("note = %q", out.Note)
	}
}

// A manifest that refers to another by name goes after it, so the thing it
// points at exists by the time it is applied.
func TestPlanOrdersAManifestAfterWhatItRefersTo(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1",
			ok(`{"resources":[{"name":"workloads","kind":"Workload","namespaced":true},
				{"name":"networks","kind":"Network","namespaced":true}]}`)).
		routeFor(http.MethodGet, "/workloads/api", status(404, "not found")).
		routeFor(http.MethodGet, "/networks/default", status(404, "not found")).
		routeFor(http.MethodPost, "/workloads", ok(`{"metadata":{"name":"api"}}`)).
		routeFor(http.MethodPost, "/networks", ok(`{"metadata":{"name":"default"}}`))

	// Given the wrong way round on purpose.
	out := planOf(t, p.writeTools(t, "demo", "tok"), workloadManifest, networkManifest)

	if len(out.Results) != 2 {
		t.Fatalf("results = %+v", out.Results)
	}
	if out.Results[0].Kind != "Network" || out.Results[1].Kind != "Workload" {
		t.Fatalf("order = %q, %q; the network has to come first",
			out.Results[0].Kind, out.Results[1].Kind)
	}
	if !strings.Contains(out.Note, "in the order shown") {
		t.Fatalf("note = %q; the order is part of what was agreed to", out.Note)
	}
}

// Nothing referring to anything else keeps the order it was given in.
func TestPlanKeepsTheGivenOrderWhenNothingRefersToAnything(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1",
			ok(`{"resources":[{"name":"networks","kind":"Network","namespaced":true}]}`)).
		routeFor(http.MethodGet, "/networks/", status(404, "not found")).
		routeFor(http.MethodPost, "/networks", ok(`{"metadata":{"name":"n"}}`))

	first := strings.Replace(networkManifest, "name: default", "name: beta", 1)
	second := strings.Replace(networkManifest, "name: default", "name: alpha", 1)

	out := planOf(t, p.writeTools(t, "demo", "tok"), first, second)
	if out.Results[0].Name != "beta" || out.Results[1].Name != "alpha" {
		t.Fatalf("order = %q, %q; the given order should stand",
			out.Results[0].Name, out.Results[1].Name)
	}
}

// What applying would change is taken from what the platform says the resource
// would become, so the differences are real changes rather than every value the
// platform fills in on its own.
func TestPlanReportsWhatWouldChange(t *testing.T) {
	existing := `{"apiVersion":"compute.datumapis.com/v1alpha1","kind":"Workload",
		"metadata":{"name":"api","namespace":"default","resourceVersion":"991"},
		"spec":{"replicas":1,"networkRef":{"name":"default"},"image":"api:1.0"}}`
	would := `{"apiVersion":"compute.datumapis.com/v1alpha1","kind":"Workload",
		"metadata":{"name":"api","namespace":"default","resourceVersion":"991"},
		"spec":{"replicas":2,"networkRef":{"name":"default"}}}`

	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery)).
		routeFor(http.MethodGet, "/workloads/api", ok(existing)).
		routeFor(http.MethodPut, "/workloads/api", ok(would))

	out := planOf(t, p.writeTools(t, "demo", "tok"), workloadManifest)

	if out.Results[0].Action != "update" || !out.Results[0].Exists {
		t.Fatalf("result = %+v", out.Results[0])
	}
	joined := strings.Join(out.Results[0].Diff, "\n")
	if !strings.Contains(joined, "spec.replicas: 1 → 2") {
		t.Fatalf("diff = %q", joined)
	}
	if !strings.Contains(joined, "spec.image: api:1.0 → (removed)") {
		t.Fatalf("diff should report the removal: %q", joined)
	}
}

// ------------------------------------------------------------------- apply

func TestApplyCarriesOutThePlanItWasAgreedTo(t *testing.T) {
	p := writePlatform(t)
	tools := p.writeTools(t, "demo", "tok")

	plan := planOf(t, tools, workloadManifest)
	out := decodeInto[basetools.ApplyOutput](t, call(t, tools, basetools.ResourcesApplyToolName,
		applyInput(plan.PlanToken, plan.Results[0].Manifest)))

	if len(out.Applied) != 1 || out.Applied[0].Action != "create" || out.Applied[0].Name != "api" {
		t.Fatalf("applied = %+v", out.Applied)
	}
	if !strings.Contains(out.Next, "not the same as anything running") {
		t.Fatalf("next = %q; accepted is not running", out.Next)
	}
	if writes := persistedWrites(p); len(writes) != 1 {
		t.Fatalf("writes = %+v, want exactly one", writes)
	}
}

// One changed character. This is the whole point.
func TestApplyRefusesAnEditedManifest(t *testing.T) {
	p := writePlatform(t)
	tools := p.writeTools(t, "demo", "tok")

	plan := planOf(t, tools, workloadManifest)
	edited := strings.Replace(plan.Results[0].Manifest, "replicas: 2", "replicas: 20", 1)
	if edited == plan.Results[0].Manifest {
		t.Fatal("the test did not actually edit the manifest")
	}

	refusal := callErr(t, tools, basetools.ResourcesApplyToolName, applyInput(plan.PlanToken, edited))
	assertRefusedAndUnchanged(t, p, refusal)
}

func TestApplyRefusesAReorderedPlan(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1",
			ok(`{"resources":[{"name":"workloads","kind":"Workload","namespaced":true},
				{"name":"networks","kind":"Network","namespaced":true}]}`)).
		routeFor(http.MethodGet, "/workloads/api", status(404, "not found")).
		routeFor(http.MethodGet, "/networks/default", status(404, "not found")).
		routeFor(http.MethodPost, "/workloads", ok(`{"metadata":{"name":"api"}}`)).
		routeFor(http.MethodPost, "/networks", ok(`{"metadata":{"name":"default"}}`))
	tools := p.writeTools(t, "demo", "tok")

	plan := planOf(t, tools, networkManifest, workloadManifest)

	refusal := callErr(t, tools, basetools.ResourcesApplyToolName,
		applyInput(plan.PlanToken, plan.Results[1].Manifest, plan.Results[0].Manifest))
	assertRefusedAndUnchanged(t, p, refusal)
}

// A resource somebody else changed between the plan and the apply is refused:
// what the person agreed to is no longer what would happen.
func TestApplyRefusesAResourceSomebodyElseChanged(t *testing.T) {
	existing := func(version string) string {
		return fmt.Sprintf(`{"apiVersion":"compute.datumapis.com/v1alpha1","kind":"Workload",
			"metadata":{"name":"api","namespace":"default","resourceVersion":%q},
			"spec":{"replicas":1,"networkRef":{"name":"default"}}}`, version)
	}
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(workloadDiscovery)).
		routeFor(http.MethodGet, "/workloads/api", ok(existing("991"))).
		routeFor(http.MethodPut, "/workloads/api", ok(existing("991")))
	tools := p.writeTools(t, "demo", "tok")

	plan := planOf(t, tools, workloadManifest)

	// Somebody else changes it in the meantime.
	p.override(http.MethodGet, "/workloads/api", ok(existing("992")))
	moved := p.writeTools(t, "demo", "tok")

	refusal := callErr(t, moved, basetools.ResourcesApplyToolName,
		applyInput(plan.PlanToken, plan.Results[0].Manifest))
	if !strings.Contains(refusal, "changed by somebody else") {
		t.Fatalf("refusal should name this case: %q", refusal)
	}
	assertRefusedAndUnchanged(t, p, refusal)
}

// A plan made in one project cannot be spent in another.
func TestApplyRefusesATokenFromAnotherProject(t *testing.T) {
	p := writePlatform(t)
	plan := planOf(t, p.writeTools(t, "one-project", "tok"), workloadManifest)

	refusal := callErr(t, p.writeTools(t, "another-project", "tok"),
		basetools.ResourcesApplyToolName, applyInput(plan.PlanToken, plan.Results[0].Manifest))
	assertRefusedAndUnchanged(t, p, refusal)
}

func TestApplyRefusesAnExpiredPlan(t *testing.T) {
	p := writePlatform(t)
	past := func() time.Time { return time.Now().Add(-time.Hour) }

	stale := basetools.Tools(basetools.Options{
		Project: p.view(t, "demo", "tok"), PlanTokenKey: testPlanKey, Now: past,
	})
	plan := planOf(t, stale, workloadManifest)

	refusal := callErr(t, p.writeTools(t, "demo", "tok"), basetools.ResourcesApplyToolName,
		applyInput(plan.PlanToken, plan.Results[0].Manifest))
	if !strings.Contains(refusal, "expired") {
		t.Fatalf("refusal = %q", refusal)
	}
	assertRefusedAndUnchanged(t, p, refusal)
}

func TestApplyRefusesACallWithNoTokenAtAll(t *testing.T) {
	p := writePlatform(t)

	refusal := callErr(t, p.writeTools(t, "demo", "tok"), basetools.ResourcesApplyToolName,
		manifestsInput(workloadManifest))
	if !strings.Contains(refusal, "no plan token") {
		t.Fatalf("refusal = %q", refusal)
	}
	assertNothingPersisted(t, p)
}

// The order the plan settled on is the order things are applied in.
func TestApplyFollowsThePlanOrder(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1",
			ok(`{"resources":[{"name":"workloads","kind":"Workload","namespaced":true},
				{"name":"networks","kind":"Network","namespaced":true}]}`)).
		routeFor(http.MethodGet, "/workloads/api", status(404, "not found")).
		routeFor(http.MethodGet, "/networks/default", status(404, "not found")).
		routeFor(http.MethodPost, "/workloads", ok(`{"metadata":{"name":"api"}}`)).
		routeFor(http.MethodPost, "/networks", ok(`{"metadata":{"name":"default"}}`))
	tools := p.writeTools(t, "demo", "tok")

	plan := planOf(t, tools, workloadManifest, networkManifest)
	out := decodeInto[basetools.ApplyOutput](t, call(t, tools, basetools.ResourcesApplyToolName,
		applyInput(plan.PlanToken, plan.Results[0].Manifest, plan.Results[1].Manifest)))

	if len(out.Applied) != 2 {
		t.Fatalf("applied = %+v", out.Applied)
	}
	if out.Applied[0].Kind != "Network" || out.Applied[1].Kind != "Workload" {
		t.Fatalf("applied in the wrong order: %+v", out.Applied)
	}

	writes := persistedWrites(p)
	if len(writes) != 2 {
		t.Fatalf("writes = %+v", writes)
	}
	if !strings.HasSuffix(writes[0].path, "/networks") || !strings.HasSuffix(writes[1].path, "/workloads") {
		t.Fatalf("the platform saw them in the wrong order: %+v", writes)
	}
}

// A plan is checked again against the live platform just before it is applied,
// because the things it depends on move underneath it.
func TestApplyChecksAgainBeforeWriting(t *testing.T) {
	p := writePlatform(t)
	tools := p.writeTools(t, "demo", "tok")

	plan := planOf(t, tools, workloadManifest)

	// The platform changes its mind between the plan and the apply.
	p.override(http.MethodPost, "/workloads", reply{code: 422, body: `{"kind":"Status","code":422,
		"message":"the project is out of allowance"}`})

	refusal := callErr(t, p.writeTools(t, "demo", "tok"), basetools.ResourcesApplyToolName,
		applyInput(plan.PlanToken, plan.Results[0].Manifest))
	if !strings.Contains(refusal, "nothing was changed") {
		t.Fatalf("refusal = %q", refusal)
	}
	if !strings.Contains(refusal, "out of allowance") {
		t.Fatalf("the refusal should quote the platform: %q", refusal)
	}
	assertNothingPersisted(t, p)
}

// A change that stops part-way says exactly what was done and what was not.
func TestApplyReportsAChangeThatStoppedPartWay(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1",
			ok(`{"resources":[{"name":"workloads","kind":"Workload","namespaced":true},
				{"name":"networks","kind":"Network","namespaced":true}]}`)).
		routeFor(http.MethodGet, "/workloads/api", status(404, "not found")).
		routeFor(http.MethodGet, "/networks/default", status(404, "not found")).
		routeFor(http.MethodPost, "/networks", ok(`{"metadata":{"name":"default"}}`)).
		routeFor(http.MethodPost, "/workloads", ok(`{"metadata":{"name":"api"}}`))
	tools := p.writeTools(t, "demo", "tok")

	plan := planOf(t, tools, workloadManifest, networkManifest)

	// The second write fails after the first one has already happened. The dry
	// run before it still passes, which is exactly how a change gets part-way.
	p.overrideWrite(http.MethodPost, "/workloads",
		reply{code: 500, body: `{"kind":"Status","code":500,"message":"the platform is unwell"}`})

	refusal := callErr(t, p.writeTools(t, "demo", "tok"), basetools.ResourcesApplyToolName,
		applyInput(plan.PlanToken, plan.Results[0].Manifest, plan.Results[1].Manifest))

	if !strings.Contains(refusal, "still done") {
		t.Fatalf("a part-way change must say what stands: %q", refusal)
	}
	if !strings.Contains(refusal, "Network default") {
		t.Fatalf("it must name what was done: %q", refusal)
	}
}

// ---------------------------------------------------------------- guardrails

func TestTheChangePathRefusesMoreThanAPersonCanRead(t *testing.T) {
	p := writePlatform(t)
	many := make([]string, 30)
	for i := range many {
		many[i] = workloadManifest
	}

	refusal := callErr(t, p.writeTools(t, "demo", "tok"), basetools.ResourcesPlanToolName,
		manifestsInput(many...))
	if !strings.Contains(refusal, "read and agree to all of it") {
		t.Fatalf("refusal = %q", refusal)
	}
	assertNothingPersisted(t, p)
}

// A file pasted whole is one entry holding several documents, and that is what
// a person will do.
func TestTheChangePathAcceptsAMultiDocumentManifest(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1",
			ok(`{"resources":[{"name":"workloads","kind":"Workload","namespaced":true},
				{"name":"networks","kind":"Network","namespaced":true}]}`)).
		routeFor(http.MethodGet, "/workloads/api", status(404, "not found")).
		routeFor(http.MethodGet, "/networks/default", status(404, "not found")).
		routeFor(http.MethodPost, "/workloads", ok(`{"metadata":{"name":"api"}}`)).
		routeFor(http.MethodPost, "/networks", ok(`{"metadata":{"name":"default"}}`))

	out := planOf(t, p.writeTools(t, "demo", "tok"), networkManifest+"\n---\n"+workloadManifest)
	if len(out.Results) != 2 {
		t.Fatalf("results = %+v, want both documents", out.Results)
	}
}

// The project decides where a resource goes, never the manifest.
func TestAManifestCannotNameAnotherProject(t *testing.T) {
	p := writePlatform(t)

	call(t, p.writeTools(t, "demo-project", "tok"), basetools.ResourcesValidateToolName,
		manifestsInput(strings.Replace(workloadManifest, "  name: api",
			"  name: api\n  namespace: default", 1)))

	for _, request := range p.seen() {
		if !strings.Contains(request.path, "/projects/demo-project/control-plane") {
			t.Fatalf("a request left the conversation's project: %s", request.path)
		}
	}
}

// ------------------------------------------------------------------ helpers

func persistedWrites(p *platform) []recorded {
	var writes []recorded
	for _, request := range p.seen() {
		if request.persisted() {
			writes = append(writes, request)
		}
	}
	return writes
}

func assertNothingPersisted(t *testing.T, p *platform) {
	t.Helper()
	if writes := persistedWrites(p); len(writes) > 0 {
		t.Fatalf("something was written: %+v", writes)
	}
}

func assertRefusedAndUnchanged(t *testing.T, p *platform, refusal string) {
	t.Helper()
	if !strings.Contains(refusal, "nothing was changed") {
		t.Errorf("the refusal does not say nothing happened: %q", refusal)
	}
	if !strings.Contains(refusal, basetools.ResourcesPlanToolName) {
		t.Errorf("the refusal does not say what to do instead: %q", refusal)
	}
	assertNothingPersisted(t, p)
}
