package basetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/milo-os/assistant/internal/projectapi"
)

// Where a service is offered is a platform record, not a provider's opinion.
// Two kinds hold it between them: an availability record says a named service
// is deployed and validated at a named location, and the location itself says
// where that is and whether it is serving.
const (
	availabilityGroup   = "services.miloapis.com"
	availabilityVersion = "v1alpha1"
	availabilityKind    = "ServiceAvailability"

	locationsGroup   = "locations.miloapis.com"
	locationsVersion = "v1alpha1"
	locationKind     = "Location"

	// availableCondition is what an availability record reports when the
	// service is actually offered there.
	availableCondition = "Available"
	// readyCondition is what a location reports when it is serving.
	readyCondition = "Ready"
)

// LocationView is one place a service is offered to this project.
type LocationView struct {
	Name string `json:"name"`
	// Topology is everything the location declares about where it is — its
	// city, its region, and whatever else the platform publishes. A selector
	// that places by region is matched against exactly these keys.
	Topology map[string]string `json:"topology,omitempty"`
	// Ready reports whether the location is serving. A location can be offered
	// yet not ready; the two are worth telling apart before promising anything
	// will start there.
	Ready bool `json:"ready"`
}

// LocationsListOutput is where one service is offered.
type LocationsListOutput struct {
	Service   string         `json:"service"`
	Locations []LocationView `json:"locations"`
	// Note explains an empty list, so it is never read as a failure.
	Note string `json:"note,omitempty"`
}

func locationsList(opts Options) *tool {
	return newTool(
		LocationsListToolName,
		"List the places a named service is offered to this project, each with what it declares "+
			"about where it is and whether it is serving. Take location names from here verbatim when "+
			"writing a manifest: a name that is not on this list can never be satisfied, and the "+
			"request sits unplaced rather than failing loudly. An empty list is a real answer — the "+
			"service is offered nowhere this project can use — and says so. Read-only.",
		object(map[string]any{
			"service": str("The service to ask about, as the platform names it, e.g. \"compute\"."),
		}, "service"),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Service string `json:"service"`
			}
			if err := decode(LocationsListToolName, input, &args); err != nil {
				return "", err
			}
			service := strings.TrimSpace(args.Service)
			if service == "" {
				return "", fmt.Errorf("%s: a service name is required, e.g. \"compute\"", LocationsListToolName)
			}

			offered, err := offeredLocations(ctx, opts.Project, service)
			if err != nil {
				return "", err
			}

			resource, err := opts.Project.Resolve(ctx, locationsGroup, locationsVersion, locationKind)
			if err != nil {
				return "", notServed(locationKind, err)
			}
			all, err := opts.Project.List(ctx, resource, "")
			if err != nil {
				return "", describe("where services are offered", err)
			}

			out := LocationsListOutput{Service: service, Locations: []LocationView{}}
			for _, obj := range all {
				name, _ := projectapi.NestedString(obj, "metadata", "name")
				if !offered[name] {
					continue
				}
				out.Locations = append(out.Locations, LocationView{
					Name:     name,
					Topology: stringMap(obj, "spec", "topology"),
					Ready:    projectapi.ConditionTrue(obj, readyCondition),
				})
			}
			sort.Slice(out.Locations, func(i, j int) bool {
				return out.Locations[i].Name < out.Locations[j].Name
			})

			if len(out.Locations) == 0 {
				out.Note = fmt.Sprintf(
					"%q is not offered anywhere this project can use today. That is the platform's "+
						"current answer, not a failure to look it up.", service)
			}
			return result(out)
		},
	)
}

// offeredLocations returns the names of the locations where service is offered
// to this project.
func offeredLocations(ctx context.Context, project *projectapi.Project, service string) (map[string]bool, error) {
	resource, err := project.Resolve(ctx, availabilityGroup, availabilityVersion, availabilityKind)
	if err != nil {
		return nil, notServed(availabilityKind, err)
	}
	records, err := project.List(ctx, resource, "")
	if err != nil {
		return nil, describe("where services are offered", err)
	}

	offered := map[string]bool{}
	for _, obj := range records {
		named, _ := projectapi.NestedString(obj, "spec", "serviceRef", "name")
		if !strings.EqualFold(named, service) {
			continue
		}
		if !projectapi.ConditionTrue(obj, availableCondition) {
			continue
		}
		if location, ok := projectapi.NestedString(obj, "spec", "locationRef", "name"); ok && location != "" {
			offered[location] = true
		}
	}
	return offered, nil
}

// notServed is the loud failure this tool owes a customer when one of the two
// records it joins is missing from the project.
//
// It deliberately does NOT degrade to an empty list. An empty list is a real
// answer — the service is offered nowhere this project may use — and returning
// it when nothing actually looked tells a person their project has no
// locations when the truth is that this deployment cannot see them. The two
// call for opposite actions: one waits for the service to arrive somewhere, the
// other is a deployment that needs fixing, and they must never read the same.
func notServed(kind string, err error) error {
	if errors.Is(err, projectapi.ErrKindNotServed) {
		return fmt.Errorf(
			"where services are offered cannot be read from this project: it does not carry the %s "+
				"records that answer it, so the question was not asked rather than answered with "+
				"nothing. The person who asked did nothing wrong — whoever operates this deployment "+
				"needs to fix it", kind)
	}
	if projectapi.IsForbidden(err) {
		return notEntitled("where services are offered")
	}
	return fmt.Errorf("where services are offered could not be read: %w", err)
}

// stringMap reads a map of strings at a path, dropping anything that is not one.
func stringMap(obj projectapi.Object, path ...string) map[string]string {
	raw, ok := projectapi.NestedMap(obj, path...)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for _, key := range sortedKeys(raw) {
		if v, ok := raw[key].(string); ok {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
