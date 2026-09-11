package basetools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/milo-os/assistant/internal/projectapi"
)

// A project's allowance is a platform record too. One object per resource type
// holds the limit, what is already taken and what is left; a second kind says
// what the numbers are counted in and how to turn them into something a person
// reads (2000 millicores is 2 vCPUs, and only one of those is worth saying).
const (
	quotaGroup   = "quota.miloapis.com"
	quotaVersion = "v1alpha1"

	allowanceKind    = "AllowanceBucket"
	registrationKind = "ResourceRegistration"

	// quotaNamespace is where a project's allowance objects are held inside
	// its own project.
	quotaNamespace = "milo-system"

	// consumerKindLabel and consumerKindProject select the allowance the
	// project itself holds, rather than any held by something nested under it.
	consumerKindLabel   = "quota.miloapis.com/consumer-kind"
	consumerKindProject = "Project"

	// defaultUnit is what a number is called when the platform does not say.
	defaultUnit = "units"
)

// QuotaRow is one resource type's allowance, in the unit a person reads.
type QuotaRow struct {
	// ResourceType is the platform's identifier, e.g.
	// "compute.datumapis.com/vcpus".
	ResourceType string `json:"resourceType"`
	// Unit is what the numbers below count, e.g. "vCPUs".
	Unit string `json:"unit"`
	// Limit, Used and Available are in Unit, not in whatever the platform
	// stores internally.
	Limit     int64 `json:"limit"`
	Used      int64 `json:"used"`
	Available int64 `json:"available"`
}

// QuotaGetOutput is a project's allowance, one row per resource type.
type QuotaGetOutput struct {
	Service   string     `json:"service,omitempty"`
	Resources []QuotaRow `json:"resources"`
	// Note explains an empty answer.
	Note string `json:"note,omitempty"`
}

func quotaGet(opts Options) *tool {
	return newTool(
		QuotaGetToolName,
		"Report what this project's allowance has left: for each resource type, the limit, how much "+
			"is already taken, and how much is available, in the unit the platform publishes rather "+
			"than the one it stores. Pass service to narrow it to one service's resource types. Call "+
			"it before proposing a size or a count, so what is proposed fits, and call it when "+
			"something is refused for want of allowance to see how much room there actually is. "+
			"Read-only.",
		object(map[string]any{
			"service": str("Optional. Narrow the answer to one service's resource types, given as the " +
				"prefix the platform uses, e.g. \"compute.datumapis.com\". Omit it for everything " +
				"this project has an allowance for."),
		}),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Service string `json:"service"`
			}
			if err := decode(QuotaGetToolName, input, &args); err != nil {
				return "", err
			}
			prefix := strings.TrimSpace(args.Service)

			resource, err := opts.Project.Resolve(ctx, quotaGroup, quotaVersion, allowanceKind)
			if err != nil {
				return "", quotaError(err)
			}
			namespace := ""
			if resource.Namespaced {
				namespace = quotaNamespace
			}
			buckets, err := opts.Project.List(ctx, resource, namespace)
			if err != nil {
				return "", describe("this project's allowance", err)
			}

			byType := map[string]projectapi.Object{}
			for _, bucket := range buckets {
				if !heldByProject(bucket) {
					continue
				}
				resourceType, ok := projectapi.NestedString(bucket, "spec", "resourceType")
				if !ok || resourceType == "" {
					continue
				}
				if prefix != "" && !strings.HasPrefix(resourceType, prefix) {
					continue
				}
				byType[resourceType] = bucket
			}

			display := displayMetadata(ctx, opts, prefix)

			out := QuotaGetOutput{Service: prefix, Resources: make([]QuotaRow, 0, len(byType))}
			for _, resourceType := range sortedKeys(byType) {
				bucket := byType[resourceType]
				meta := display[resourceType]
				limit, _ := projectapi.NestedInt64(bucket, "status", "limit")
				used, _ := projectapi.NestedInt64(bucket, "status", "allocated")
				available, _ := projectapi.NestedInt64(bucket, "status", "available")
				out.Resources = append(out.Resources, QuotaRow{
					ResourceType: resourceType,
					Unit:         meta.unit(),
					Limit:        meta.convert(limit),
					Used:         meta.convert(used),
					Available:    meta.convert(available),
				})
			}
			sort.Slice(out.Resources, func(i, j int) bool {
				return out.Resources[i].ResourceType < out.Resources[j].ResourceType
			})

			if len(out.Resources) == 0 {
				out.Note = emptyNote(prefix)
			}
			return result(out)
		},
	)
}

// unitMeta is how one resource type's stored numbers become readable ones.
type unitMeta struct {
	displayUnit string
	// factor divides the stored value to get the displayed one: 1000 turns
	// millicores into cores, 1073741824 turns bytes into GiB.
	factor int64
}

// unit is what the numbers are called. A platform that publishes no unit, or
// publishes the placeholder "1", tells a reader nothing, so those become the
// generic word rather than being repeated.
func (m unitMeta) unit() string {
	if m.displayUnit == "" || m.displayUnit == "1" {
		return defaultUnit
	}
	return m.displayUnit
}

// convert turns a stored number into a displayed one.
func (m unitMeta) convert(value int64) int64 {
	if m.factor <= 1 {
		return value
	}
	return value / m.factor
}

// displayMetadata reads what each resource type's numbers mean.
//
// Best effort on purpose: this is presentation, and a project that will not
// hand it over should still get its numbers. Without it every row is counted
// in the generic unit, which is worse than the real one and much better than
// no answer at all.
func displayMetadata(ctx context.Context, opts Options, prefix string) map[string]unitMeta {
	display := map[string]unitMeta{}

	resource, err := opts.Project.Resolve(ctx, quotaGroup, quotaVersion, registrationKind)
	if err != nil {
		opts.Logger.Warn("basetools.quota.units_unavailable", "error", err.Error())
		return display
	}
	namespace := ""
	if resource.Namespaced {
		namespace = quotaNamespace
	}
	registrations, err := opts.Project.List(ctx, resource, namespace)
	if err != nil {
		opts.Logger.Warn("basetools.quota.units_unavailable", "error", err.Error())
		return display
	}

	for _, registration := range registrations {
		resourceType, ok := projectapi.NestedString(registration, "spec", "resourceType")
		if !ok || resourceType == "" {
			continue
		}
		if prefix != "" && !strings.HasPrefix(resourceType, prefix) {
			continue
		}
		unit, _ := projectapi.NestedString(registration, "spec", "displayUnit")
		factor, _ := projectapi.NestedInt64(registration, "spec", "unitConversionFactor")
		display[resourceType] = unitMeta{displayUnit: unit, factor: factor}
	}
	return display
}

// heldByProject reports whether an allowance object belongs to the project
// itself rather than to something nested under it.
func heldByProject(bucket projectapi.Object) bool {
	labels, ok := projectapi.NestedMap(bucket, "metadata", "labels")
	if !ok {
		return false
	}
	kind, _ := labels[consumerKindLabel].(string)
	return kind == consumerKindProject
}

func emptyNote(prefix string) string {
	if prefix != "" {
		return fmt.Sprintf(
			"This project has no allowance recorded for %q. Either nothing of that service is "+
				"counted against an allowance, or the prefix is not the one the platform uses — "+
				"call quota_get with no service to see every resource type it does have.", prefix)
	}
	return "This project has no allowance recorded against it, so nothing here is capped by one."
}

func quotaError(err error) error {
	if projectapi.IsForbidden(err) {
		return notEntitled("this project's allowance")
	}
	return fmt.Errorf(
		"this project's allowance could not be read, so how much room is left is unknown: %w", err)
}
