package basetools_test

import (
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/basetools"
)

const (
	quotaDiscovery = `{"resources":[
		{"name":"allowancebuckets","kind":"AllowanceBucket","namespaced":true},
		{"name":"resourceregistrations","kind":"ResourceRegistration","namespaced":true}
	]}`

	buckets = `{"items":[
		{"metadata":{"name":"vcpus","labels":{"quota.miloapis.com/consumer-kind":"Project"}},
		 "spec":{"resourceType":"compute.datumapis.com/vcpus"},
		 "status":{"limit":8000,"allocated":2500,"available":5500}},
		{"metadata":{"name":"workloads","labels":{"quota.miloapis.com/consumer-kind":"Project"}},
		 "spec":{"resourceType":"compute.datumapis.com/workloads"},
		 "status":{"limit":10,"allocated":3,"available":7}},
		{"metadata":{"name":"zones","labels":{"quota.miloapis.com/consumer-kind":"Project"}},
		 "spec":{"resourceType":"dns.datumapis.com/zones"},
		 "status":{"limit":5,"allocated":1,"available":4}},
		{"metadata":{"name":"someone-else","labels":{"quota.miloapis.com/consumer-kind":"Organization"}},
		 "spec":{"resourceType":"compute.datumapis.com/vcpus"},
		 "status":{"limit":99999,"allocated":0,"available":99999}}
	]}`

	registrations = `{"items":[
		{"spec":{"resourceType":"compute.datumapis.com/vcpus","baseUnit":"millicore",
		  "displayUnit":"vCPUs","unitConversionFactor":1000}},
		{"spec":{"resourceType":"compute.datumapis.com/workloads","baseUnit":"workload",
		  "displayUnit":"1","unitConversionFactor":1}},
		{"spec":{"resourceType":"dns.datumapis.com/zones","baseUnit":"zone",
		  "displayUnit":"zones","unitConversionFactor":1}}
	]}`
)

func quotaPlatform(t *testing.T) *platform {
	return newPlatform(t).
		route("/resourceregistrations", ok(registrations)).
		route("/allowancebuckets", ok(buckets)).
		route("/apis/quota.miloapis.com/v1alpha1", ok(quotaDiscovery))
}

func rowFor(t *testing.T, out basetools.QuotaGetOutput, resourceType string) basetools.QuotaRow {
	t.Helper()
	for _, row := range out.Resources {
		if row.ResourceType == resourceType {
			return row
		}
	}
	t.Fatalf("no row for %q in %+v", resourceType, out.Resources)
	return basetools.QuotaRow{}
}

// The numbers a person reads are in the published unit, not the stored one:
// 8000 millicores is 8 vCPUs, and only one of those means anything to them.
func TestQuotaGetConvertsToThePublishedUnit(t *testing.T) {
	out := decodeInto[basetools.QuotaGetOutput](t, call(t,
		quotaPlatform(t).tools(t, "demo", "tok"), basetools.QuotaGetToolName, `{}`))

	vcpus := rowFor(t, out, "compute.datumapis.com/vcpus")
	if vcpus.Unit != "vCPUs" {
		t.Fatalf("unit = %q, want vCPUs", vcpus.Unit)
	}
	if vcpus.Limit != 8 || vcpus.Used != 2 || vcpus.Available != 5 {
		t.Fatalf("limit/used/available = %d/%d/%d, want 8/2/5", vcpus.Limit, vcpus.Used, vcpus.Available)
	}
}

// A published unit of "1" tells a reader nothing, so it becomes the generic
// word rather than being repeated back at them.
func TestQuotaGetDoesNotEchoAPlaceholderUnit(t *testing.T) {
	out := decodeInto[basetools.QuotaGetOutput](t, call(t,
		quotaPlatform(t).tools(t, "demo", "tok"), basetools.QuotaGetToolName, `{}`))

	workloads := rowFor(t, out, "compute.datumapis.com/workloads")
	if workloads.Unit != "units" {
		t.Fatalf("unit = %q, want the generic word", workloads.Unit)
	}
	if workloads.Limit != 10 || workloads.Used != 3 || workloads.Available != 7 {
		t.Fatalf("row = %+v", workloads)
	}
}

// Only the project's own allowance counts. An allowance held by something
// above it is a different number and must not be reported as this project's.
func TestQuotaGetReportsOnlyTheProjectsOwnAllowance(t *testing.T) {
	out := decodeInto[basetools.QuotaGetOutput](t, call(t,
		quotaPlatform(t).tools(t, "demo", "tok"), basetools.QuotaGetToolName, `{}`))

	if got := rowFor(t, out, "compute.datumapis.com/vcpus"); got.Limit == 99 {
		t.Fatalf("an allowance held elsewhere was reported: %+v", got)
	}
	if len(out.Resources) != 3 {
		t.Fatalf("resources = %+v, want the project's three", out.Resources)
	}
}

func TestQuotaGetNarrowsToOneService(t *testing.T) {
	out := decodeInto[basetools.QuotaGetOutput](t, call(t,
		quotaPlatform(t).tools(t, "demo", "tok"),
		basetools.QuotaGetToolName, `{"service":"compute.datumapis.com"}`))

	if len(out.Resources) != 2 {
		t.Fatalf("resources = %+v, want compute's two", out.Resources)
	}
	for _, row := range out.Resources {
		if !strings.HasPrefix(row.ResourceType, "compute.datumapis.com") {
			t.Fatalf("row from another service: %+v", row)
		}
	}
}

// Units are presentation. A project that will not hand them over still gets
// its numbers, counted in the generic word.
func TestQuotaGetStillReportsNumbersWithoutUnits(t *testing.T) {
	p := newPlatform(t).
		route("/resourceregistrations", status(403, "forbidden")).
		route("/allowancebuckets", ok(buckets)).
		route("/apis/quota.miloapis.com/v1alpha1", ok(quotaDiscovery))

	out := decodeInto[basetools.QuotaGetOutput](t, call(t, p.tools(t, "demo", "tok"),
		basetools.QuotaGetToolName, `{}`))

	vcpus := rowFor(t, out, "compute.datumapis.com/vcpus")
	if vcpus.Unit != "units" {
		t.Fatalf("unit = %q", vcpus.Unit)
	}
	// Unconverted, because nothing said what to convert by. Better a raw
	// number in a generic unit than a number scaled by a guess.
	if vcpus.Limit != 8000 {
		t.Fatalf("limit = %d, want the stored value", vcpus.Limit)
	}
}

func TestQuotaGetExplainsAnEmptyAnswer(t *testing.T) {
	p := newPlatform(t).
		route("/resourceregistrations", ok(registrations)).
		route("/allowancebuckets", ok(`{"items":[]}`)).
		route("/apis/quota.miloapis.com/v1alpha1", ok(quotaDiscovery))

	out := decodeInto[basetools.QuotaGetOutput](t, call(t, p.tools(t, "demo", "tok"),
		basetools.QuotaGetToolName, `{}`))
	if !strings.Contains(out.Note, "no allowance recorded") {
		t.Fatalf("note = %q", out.Note)
	}

	out = decodeInto[basetools.QuotaGetOutput](t, call(t, quotaPlatform(t).tools(t, "demo", "tok"),
		basetools.QuotaGetToolName, `{"service":"nope.example"}`))
	if !strings.Contains(out.Note, "quota_get with no service") {
		t.Fatalf("note = %q; it should say how to widen the question", out.Note)
	}
}

// The allowance lives in one place inside a project, and reading it anywhere
// else would quietly report nothing.
func TestQuotaGetReadsTheAllowanceWhereItIsHeld(t *testing.T) {
	p := quotaPlatform(t)
	call(t, p.tools(t, "demo", "tok"), basetools.QuotaGetToolName, `{}`)

	var read bool
	for _, request := range p.seen() {
		if strings.HasSuffix(request.path, "/namespaces/milo-system/allowancebuckets") {
			read = true
		}
	}
	if !read {
		t.Fatalf("the allowance was not read where it is held: %+v", p.seen())
	}
}

func TestQuotaGetSaysHowMuchRoomIsUnknownWhenItCannotLook(t *testing.T) {
	p := newPlatform(t).route("/apis/quota.miloapis.com/v1alpha1", status(404, "not served here"))

	refusal := callErr(t, p.tools(t, "demo", "tok"), basetools.QuotaGetToolName, `{}`)
	if !strings.Contains(refusal, "how much room is left is unknown") {
		t.Fatalf("refusal = %q", refusal)
	}
}
