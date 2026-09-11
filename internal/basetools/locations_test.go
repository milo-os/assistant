package basetools_test

import (
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/basetools"
)

const (
	availabilityDiscovery = `{"resources":[{"name":"serviceavailabilities","kind":"ServiceAvailability","namespaced":false}]}`
	locationDiscovery     = `{"resources":[{"name":"locations","kind":"Location","namespaced":false}]}`

	availabilities = `{"items":[
		{"metadata":{"name":"compute-dfw"},
		 "spec":{"serviceRef":{"name":"compute"},"locationRef":{"name":"us-south-dfw-1"}},
		 "status":{"conditions":[{"type":"Available","status":"True"}]}},
		{"metadata":{"name":"compute-lhr"},
		 "spec":{"serviceRef":{"name":"compute"},"locationRef":{"name":"eu-west-lhr-1"}},
		 "status":{"conditions":[{"type":"Available","status":"False"}]}},
		{"metadata":{"name":"dns-dfw"},
		 "spec":{"serviceRef":{"name":"dns"},"locationRef":{"name":"us-south-dfw-1"}},
		 "status":{"conditions":[{"type":"Available","status":"True"}]}}
	]}`

	locationObjects = `{"items":[
		{"metadata":{"name":"us-south-dfw-1"},
		 "spec":{"topology":{"topology.datum.net/city-code":"DFW","topology.datum.net/region":"us-south"}},
		 "status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"eu-west-lhr-1"},
		 "spec":{"topology":{"topology.datum.net/city-code":"LHR"}},
		 "status":{"conditions":[{"type":"Ready","status":"True"}]}}
	]}`
)

func locationsPlatform(t *testing.T) *platform {
	return newPlatform(t).
		route("/apis/services.miloapis.com/v1alpha1", ok(availabilityDiscovery)).
		route("/apis/locations.miloapis.com/v1alpha1", ok(locationDiscovery)).
		route("/serviceavailabilities", ok(availabilities)).
		route("/locations", ok(locationObjects))
}

func TestLocationsListJoinsAvailabilityToTheLocation(t *testing.T) {
	out := decodeInto[basetools.LocationsListOutput](t, call(t,
		locationsPlatform(t).tools(t, "demo", "tok"),
		basetools.LocationsListToolName, `{"service":"compute"}`))

	if len(out.Locations) != 1 {
		t.Fatalf("locations = %+v, want just the one compute is offered at", out.Locations)
	}
	got := out.Locations[0]
	if got.Name != "us-south-dfw-1" {
		t.Fatalf("name = %q", got.Name)
	}
	if got.Topology["topology.datum.net/city-code"] != "DFW" {
		t.Fatalf("topology = %+v", got.Topology)
	}
	if !got.Ready {
		t.Fatal("the location reports Ready and should say so")
	}
}

// A record for another service must not leak into this answer.
func TestLocationsListIsPerService(t *testing.T) {
	out := decodeInto[basetools.LocationsListOutput](t, call(t,
		locationsPlatform(t).tools(t, "demo", "tok"),
		basetools.LocationsListToolName, `{"service":"dns"}`))

	if len(out.Locations) != 1 || out.Locations[0].Name != "us-south-dfw-1" {
		t.Fatalf("locations = %+v", out.Locations)
	}
}

// Offered nowhere is a real answer and comes back as one, with a note so it is
// never read as a failure to look.
func TestLocationsListSaysSoWhenAServiceIsOfferedNowhere(t *testing.T) {
	out := decodeInto[basetools.LocationsListOutput](t, call(t,
		locationsPlatform(t).tools(t, "demo", "tok"),
		basetools.LocationsListToolName, `{"service":"storage"}`))

	if len(out.Locations) != 0 {
		t.Fatalf("locations = %+v, want none", out.Locations)
	}
	if !strings.Contains(out.Note, "not a failure") {
		t.Fatalf("note = %q; an empty list must be explained", out.Note)
	}
}

// The one thing this tool must never do is answer "nowhere" when nothing
// actually looked. A project that does not carry either record fails loudly and
// blames the deployment, not the person.
func TestLocationsListFailsLoudlyWhenTheRecordsAreNotThere(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *platform
		kind  string
	}{
		{
			name: "no availability records",
			kind: "ServiceAvailability",
			build: func(t *testing.T) *platform {
				return newPlatform(t).
					route("/apis/services.miloapis.com/v1alpha1", status(404, "not served here")).
					route("/apis/locations.miloapis.com/v1alpha1", ok(locationDiscovery)).
					route("/locations", ok(locationObjects))
			},
		},
		{
			name: "no locations",
			kind: "Location",
			build: func(t *testing.T) *platform {
				return newPlatform(t).
					route("/apis/services.miloapis.com/v1alpha1", ok(availabilityDiscovery)).
					route("/serviceavailabilities", ok(availabilities)).
					route("/apis/locations.miloapis.com/v1alpha1", status(404, "not served here"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := callErr(t, tc.build(t).tools(t, "demo", "tok"),
				basetools.LocationsListToolName, `{"service":"compute"}`)

			if !strings.Contains(refusal, tc.kind) {
				t.Fatalf("refusal should name what is missing: %q", refusal)
			}
			if !strings.Contains(refusal, "did nothing wrong") {
				t.Fatalf("refusal should blame the deployment: %q", refusal)
			}
			if strings.Contains(strings.ToLower(refusal), "no locations are offered") {
				t.Fatalf("refusal must not read as an empty answer: %q", refusal)
			}
		})
	}
}

func TestLocationsListNeedsAService(t *testing.T) {
	refusal := callErr(t, locationsPlatform(t).tools(t, "demo", "tok"),
		basetools.LocationsListToolName, `{}`)
	if !strings.Contains(refusal, "service name is required") {
		t.Fatalf("refusal = %q", refusal)
	}
}
