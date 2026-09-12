package basetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/milo-os/assistant/internal/projectapi"
)

// defaultNamespace is where a project's own resources live unless the person
// says otherwise. Kinds that are not held in a namespace ignore it.
const defaultNamespace = "default"

// maxListed caps one listing. A project with more of a kind than this gets the
// first page and is told so, which is a better answer than a reply too large
// for the conversation to carry.
const maxListed = 200

// kindArgs are the three things every read needs to name a kind, plus where to
// look. Deliberately no project field: the project is the conversation's, and
// a tool that accepted one would be a way to ask about somebody else's.
type kindArgs struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
}

// kindProperties is the shared schema fragment for naming a kind.
func kindProperties() map[string]any {
	return map[string]any{
		"group": str("API group of the kind, e.g. \"compute.datumapis.com\". Leave empty for core " +
			"Kubernetes kinds such as ConfigMap or Secret."),
		"version": str("API version of the kind, e.g. \"v1alpha1\"."),
		"kind": str("The kind, e.g. \"Workload\". The plural resource name (\"workloads\") is " +
			"accepted too."),
		"namespace": str("Namespace to look in. Defaults to \"" + defaultNamespace + "\", which is " +
			"where a project's own resources normally are. Ignored for kinds that are not held in a namespace."),
	}
}

// ResourceSummary is one resource in a listing.
type ResourceSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// CreatedAt is when the resource was first accepted, in RFC 3339.
	CreatedAt string `json:"createdAt,omitempty"`
	// Conditions is what the platform is currently reporting about it.
	Conditions []Condition `json:"conditions,omitempty"`
	// Labels are carried through because a placement, a selector or a
	// provider's own tool is usually keyed on one.
	Labels map[string]string `json:"labels,omitempty"`
}

// ResourcesListOutput is every resource of one kind in the project.
type ResourcesListOutput struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Namespace  string            `json:"namespace,omitempty"`
	Resources  []ResourceSummary `json:"resources"`
	// Note says what was left out, when anything was.
	Note string `json:"note,omitempty"`
}

func resourcesList(opts Options) *tool {
	return newTool(
		ResourcesListToolName,
		"List this project's resources of one kind, whichever service owns them, with what the "+
			"platform is currently reporting about each. Name the kind by its API group, version and "+
			"kind — schema_get describes what a kind's fields mean, and a provider's own tools often "+
			"explain a status better than the raw conditions here do. Reads only this project, as the "+
			"person you are talking to, so anything they are not entitled to simply does not come back. "+
			"An empty list means the project has none of that kind, which is a real answer. Read-only.",
		object(kindProperties(), "version", "kind"),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			var args kindArgs
			if err := decode(ResourcesListToolName, input, &args); err != nil {
				return "", err
			}

			resource, err := opts.Project.Resolve(ctx, args.Group, args.Version, args.Kind)
			if err != nil {
				return "", kindError(err)
			}

			namespace := ""
			if resource.Namespaced {
				namespace = args.Namespace
				if namespace == "" {
					namespace = defaultNamespace
				}
			}

			objects, err := opts.Project.List(ctx, resource, namespace)
			if err != nil {
				return "", describe(fmt.Sprintf("%s resources", resource.Kind), err)
			}

			out := ResourcesListOutput{
				Kind:       resource.Kind,
				APIVersion: resource.GroupVersion(),
				Namespace:  namespace,
				Resources:  make([]ResourceSummary, 0, len(objects)),
			}
			if len(objects) > maxListed {
				out.Note = fmt.Sprintf(
					"This project has %d of these; the first %d are shown, by name. Narrow the question "+
						"if the one being looked for is not here.", len(objects), maxListed)
				objects = objects[:maxListed]
			}
			for _, obj := range objects {
				out.Resources = append(out.Resources, summarize(obj))
			}
			sort.Slice(out.Resources, func(i, j int) bool {
				return out.Resources[i].Name < out.Resources[j].Name
			})
			return result(out)
		},
	)
}

// ResourcesGetOutput is one resource, whole.
type ResourcesGetOutput struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	// Manifest is the resource as YAML, with everything the platform owns
	// removed — so it can be edited and handed straight to resources_plan.
	Manifest string `json:"manifest"`
	// Conditions is what the platform is currently reporting about it. Kept
	// out of the manifest above, which is the desired state rather than the
	// observed one.
	Conditions []Condition `json:"conditions,omitempty"`
}

func resourcesGet(opts Options) *tool {
	properties := kindProperties()
	properties["name"] = str("Name of the resource to read.")

	return newTool(
		ResourcesGetToolName,
		"Read one of this project's resources whole: the manifest as it would be written, with "+
			"everything the platform fills in removed, plus what the platform is currently reporting "+
			"about it. The manifest that comes back is the one to edit and hand to resources_plan when "+
			"the person wants it changed. Reads as the person you are talking to, in their project "+
			"only. Read-only.",
		object(properties, "version", "kind", "name"),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				kindArgs
				Name string `json:"name"`
			}
			if err := decode(ResourcesGetToolName, input, &args); err != nil {
				return "", err
			}
			if args.Name == "" {
				return "", fmt.Errorf("%s: a name is required", ResourcesGetToolName)
			}

			resource, err := opts.Project.Resolve(ctx, args.Group, args.Version, args.Kind)
			if err != nil {
				return "", kindError(err)
			}

			namespace := ""
			if resource.Namespaced {
				namespace = args.Namespace
				if namespace == "" {
					namespace = defaultNamespace
				}
			}

			obj, err := opts.Project.Get(ctx, resource, namespace, args.Name)
			if err != nil {
				if projectapi.IsNotFound(err) {
					return "", fmt.Errorf(
						"this project has no %s called %q%s. resources_list shows the ones it does have",
						resource.Kind, args.Name, inNamespace(namespace))
				}
				return "", describe(fmt.Sprintf("the %s %q", resource.Kind, args.Name), err)
			}

			conditions := conditionsOf(obj)
			manifest, err := toYAML(normalize(obj, namespace))
			if err != nil {
				return "", err
			}
			return result(ResourcesGetOutput{
				Kind:       resource.Kind,
				APIVersion: resource.GroupVersion(),
				Name:       args.Name,
				Namespace:  namespace,
				Manifest:   manifest,
				Conditions: conditions,
			})
		},
	)
}

// summarize reduces one resource to the fields a listing shows.
func summarize(obj projectapi.Object) ResourceSummary {
	summary := ResourceSummary{}
	summary.Name, _ = projectapi.NestedString(obj, "metadata", "name")
	summary.Namespace, _ = projectapi.NestedString(obj, "metadata", "namespace")
	summary.CreatedAt, _ = projectapi.NestedString(obj, "metadata", "creationTimestamp")
	summary.Conditions = conditionsOf(obj)
	if labels, ok := projectapi.NestedMap(obj, "metadata", "labels"); ok && len(labels) > 0 {
		summary.Labels = make(map[string]string, len(labels))
		for _, key := range sortedKeys(labels) {
			if v, ok := labels[key].(string); ok {
				summary.Labels[key] = v
			}
		}
	}
	return summary
}

// kindError rewrites "no such kind" into the two things a person can do about
// it, and leaves everything else alone.
func kindError(err error) error {
	if errors.Is(err, projectapi.ErrKindNotServed) {
		return fmt.Errorf(
			"%w. Check the group, version and kind — they are case-sensitive apart from the kind "+
				"itself — or ask about a service this project is entitled to", err)
	}
	if projectapi.IsForbidden(err) {
		return notEntitled("that kind of resource")
	}
	return err
}

func inNamespace(namespace string) string {
	if namespace == "" {
		return ""
	}
	return fmt.Sprintf(" in %q", namespace)
}
