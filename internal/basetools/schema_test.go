package basetools_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/basetools"
)

// The description keys are reverse-DNS and cannot be derived from a group
// name, so the group/version/kind stamped on each one is what is matched.
const workloadSchema = `{"components":{"schemas":{
	"com.datumapis.compute.v1alpha1.Workload":{
		"type":"object",
		"x-kubernetes-group-version-kind":[{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}],
		"required":["spec"],
		"properties":{
			"spec":{"$ref":"#/components/schemas/com.datumapis.compute.v1alpha1.WorkloadSpec"}
		}},
	"com.datumapis.compute.v1alpha1.WorkloadSpec":{
		"type":"object",
		"required":["replicas"],
		"properties":{
			"replicas":{"type":"integer","description":"How many instances to run."},
			"template":{"$ref":"#/components/schemas/com.datumapis.compute.v1alpha1.Template"}
		}},
	"com.datumapis.compute.v1alpha1.Template":{
		"type":"object",
		"properties":{"image":{"type":"string","description":"The container image."}}}
}}}`

func schemaPlatform(t *testing.T) *platform {
	return newPlatform(t).
		route("/openapi/v3/apis/compute.datumapis.com/v1alpha1", ok(workloadSchema))
}

func TestSchemaGetDescribesTheKindAndFollowsItsReferences(t *testing.T) {
	out := decodeInto[basetools.SchemaGetOutput](t, call(t, schemaPlatform(t).tools(t, "demo", "tok"),
		basetools.SchemaGetToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`))

	raw, _ := json.Marshal(out.Schema)
	for _, want := range []string{"replicas", "How many instances to run.", "The container image."} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("description is missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "$ref") {
		t.Fatalf("a reference was left unfollowed with budget to spare:\n%s", raw)
	}
}

func TestSchemaGetNarrowsToOnePart(t *testing.T) {
	out := decodeInto[basetools.SchemaGetOutput](t, call(t, schemaPlatform(t).tools(t, "demo", "tok"),
		basetools.SchemaGetToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload","path":"spec.template"}`))

	raw, _ := json.Marshal(out.Schema)
	if !strings.Contains(string(raw), "The container image.") {
		t.Fatalf("the named part is missing:\n%s", raw)
	}
	if strings.Contains(string(raw), "How many instances") {
		t.Fatalf("the whole kind came back rather than the named part:\n%s", raw)
	}
	if out.Path != "spec.template" {
		t.Fatalf("path = %q", out.Path)
	}
}

// A path that is not there lists what is, which is the answer a model needs to
// correct itself in one step rather than three.
func TestSchemaGetListsTheFieldsWhenThePathIsWrong(t *testing.T) {
	refusal := callErr(t, schemaPlatform(t).tools(t, "demo", "tok"), basetools.SchemaGetToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload","path":"spec.replicaz"}`)

	if !strings.Contains(refusal, "replicaz") || !strings.Contains(refusal, "replicas") {
		t.Fatalf("refusal should name what was asked for and what is there: %q", refusal)
	}
}

func TestSchemaGetSaysWhenTheKindIsNotDescribed(t *testing.T) {
	refusal := callErr(t, schemaPlatform(t).tools(t, "demo", "tok"), basetools.SchemaGetToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Widget"}`)
	if !strings.Contains(refusal, "Widget") {
		t.Fatalf("refusal = %q", refusal)
	}
}

func TestSchemaGetSaysWhenTheGroupIsNotDescribed(t *testing.T) {
	p := newPlatform(t) // every path 404s
	refusal := callErr(t, p.tools(t, "demo", "tok"), basetools.SchemaGetToolName,
		`{"group":"nope.example","version":"v1","kind":"Widget"}`)
	if !strings.Contains(refusal, "no description of") {
		t.Fatalf("refusal = %q", refusal)
	}
}

// A kind too large to describe in one answer comes back with its references
// unfollowed and a way to go deeper, rather than a truncation that reads as
// the whole thing.
func TestSchemaGetTellsYouHowToGoDeeperWhenTheKindIsHuge(t *testing.T) {
	p := newPlatform(t).route("/openapi/v3/apis/big.example/v1", ok(hugeSchema()))

	out := decodeInto[basetools.SchemaGetOutput](t, call(t, p.tools(t, "demo", "tok"),
		basetools.SchemaGetToolName, `{"group":"big.example","version":"v1","kind":"Big"}`))

	if !strings.Contains(out.Note, "path") {
		t.Fatalf("note = %q; it should say how to ask for less", out.Note)
	}
	raw, _ := json.Marshal(out.Schema)
	if len(raw) > 48<<10 {
		t.Fatalf("the answer is %d bytes; it was meant to be capped", len(raw))
	}
}

// hugeSchema builds a description far too large to return whole.
func hugeSchema() string {
	filler := strings.Repeat("x", 512)
	var parts []string
	properties := []string{}
	for i := range 200 {
		name := fmt.Sprintf("part%d", i)
		parts = append(parts, fmt.Sprintf(
			`"big.example.v1.%s":{"type":"object","properties":{"f":{"type":"string","description":%q}}}`,
			name, filler))
		properties = append(properties, fmt.Sprintf(
			`%q:{"$ref":"#/components/schemas/big.example.v1.%s"}`, name, name))
	}
	root := fmt.Sprintf(
		`"big.example.v1.Big":{"type":"object",
		 "x-kubernetes-group-version-kind":[{"group":"big.example","version":"v1","kind":"Big"}],
		 "properties":{%s}}`, strings.Join(properties, ","))
	return `{"components":{"schemas":{` + root + "," + strings.Join(parts, ",") + `}}}`
}
