package basetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/milo-os/assistant/internal/projectapi"
)

const (
	// refPrefix is how one part of a description points at another.
	refPrefix = "#/components/schemas/"
	// maxRefsResolved and maxRefDepth bound how much of a description is
	// followed through. A kind's description refers to other descriptions,
	// which refer to more; following all of them for a large kind returns
	// more than a conversation can carry and buries the fields that matter.
	maxRefsResolved = 60
	maxRefDepth     = 8
	// maxSchemaBytes is the ceiling on one answer. Past it the references are
	// left unfollowed and the model is told to ask about one part at a time.
	maxSchemaBytes = 48 << 10
)

// SchemaGetOutput describes one kind's fields.
type SchemaGetOutput struct {
	Group   string `json:"group,omitempty"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	// Path is the part of the kind this describes, when one was asked for.
	Path string `json:"path,omitempty"`
	// Schema is the description itself: the fields, their types, which are
	// required, and what each is for.
	Schema map[string]any `json:"schema"`
	// Note says what was left out, when anything was.
	Note string `json:"note,omitempty"`
}

func schemaGet(opts Options) *tool {
	return newTool(
		SchemaGetToolName,
		"Describe what a kind's fields are, which are required, and what each one is for, as this "+
			"project itself defines them. Read this before writing a manifest for a kind you have not "+
			"written before, rather than guessing at field names: a guessed field is rejected, and the "+
			"rejection arrives after the person has been told the manifest was written. For a large "+
			"kind, ask about one part at a time with path, e.g. \"spec.template\". Read-only.",
		object(map[string]any{
			"group": str("API group of the kind, e.g. \"compute.datumapis.com\". Leave empty for core " +
				"Kubernetes kinds."),
			"version": str("API version of the kind, e.g. \"v1alpha1\"."),
			"kind":    str("The kind, e.g. \"Workload\"."),
			"path": str("Optional dotted path to describe just one part, e.g. \"spec.placements\". " +
				"Use it when the whole kind comes back truncated."),
		}, "version", "kind"),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Group   string `json:"group"`
				Version string `json:"version"`
				Kind    string `json:"kind"`
				Path    string `json:"path"`
			}
			if err := decode(SchemaGetToolName, input, &args); err != nil {
				return "", err
			}
			args.Group = strings.TrimSpace(args.Group)
			args.Version = strings.TrimSpace(args.Version)
			args.Kind = strings.TrimSpace(args.Kind)
			if args.Version == "" || args.Kind == "" {
				return "", fmt.Errorf("%s: a version and a kind are required", SchemaGetToolName)
			}

			doc, err := opts.Project.OpenAPI(ctx, args.Group, args.Version)
			if err != nil {
				if errors.Is(err, projectapi.ErrKindNotServed) {
					return "", fmt.Errorf(
						"%w. Check the group and version, or ask about a service this project is "+
							"entitled to", err)
				}
				return "", describe(fmt.Sprintf("the description of %s", args.Kind), err)
			}

			schemas, ok := projectapi.NestedMap(doc, "components", "schemas")
			if !ok || len(schemas) == 0 {
				return "", fmt.Errorf(
					"this project does not publish a description of %s, so its fields cannot be listed "+
						"here. resources_get on an existing one shows the shape instead", args.Kind)
			}

			root, found := findKindSchema(schemas, args.Group, args.Version, args.Kind)
			if !found {
				return "", fmt.Errorf(
					"nothing called %q is described under %q. Check the kind — it is spelled with a "+
						"capital, as in \"Workload\" — or list what the project has with resources_list",
					args.Kind, groupVersionLabel(args.Group, args.Version))
			}

			selected := root
			if path := strings.TrimSpace(args.Path); path != "" {
				selected, err = walkPath(root, schemas, path)
				if err != nil {
					return "", err
				}
			}

			out := SchemaGetOutput{
				Group:   args.Group,
				Version: args.Version,
				Kind:    args.Kind,
				Path:    strings.TrimSpace(args.Path),
			}

			budget := maxRefsResolved
			out.Schema = resolve(selected, schemas, 0, &budget)
			if raw, err := json.Marshal(out.Schema); err == nil && len(raw) > maxSchemaBytes {
				// Too much came back to be useful. Hand over the top level with
				// its references unfollowed and say how to go deeper, rather
				// than a truncated document that reads as complete.
				zero := 0
				out.Schema = resolve(selected, schemas, 0, &zero)
				out.Note = "This kind is too large to describe in full. The fields are listed with " +
					"their nested parts left unexpanded; call schema_get again with path set to one " +
					"of them, e.g. \"spec\", to see inside."
			} else if budget <= 0 {
				out.Note = "Some nested parts were left unexpanded. Call schema_get again with path " +
					"set to the one you need."
			}
			return result(out)
		},
	)
}

// findKindSchema locates a kind's description.
//
// The keys are reverse-DNS and there is no reliable way to build one from a
// group name, so the group/version/kind the platform stamps on each
// description is what is matched; the name suffix is the fallback for a
// description that carries no stamp.
func findKindSchema(schemas map[string]any, group, version, kind string) (map[string]any, bool) {
	suffix := "." + version + "." + kind
	var fallback map[string]any
	var fallbackFound bool

	for _, name := range sortedKeys(schemas) {
		schema, ok := schemas[name].(map[string]any)
		if !ok {
			continue
		}
		if matchesGVK(schema, group, version, kind) {
			return schema, true
		}
		if !fallbackFound && strings.HasSuffix(name, suffix) {
			fallback, fallbackFound = schema, true
		}
	}
	return fallback, fallbackFound
}

func matchesGVK(schema map[string]any, group, version, kind string) bool {
	stamps, ok := schema["x-kubernetes-group-version-kind"].([]any)
	if !ok {
		return false
	}
	for _, raw := range stamps {
		gvk, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		g, _ := gvk["group"].(string)
		v, _ := gvk["version"].(string)
		k, _ := gvk["kind"].(string)
		if g == group && v == version && strings.EqualFold(k, kind) {
			return true
		}
	}
	return false
}

// walkPath narrows a description to one dotted path, following references and
// stepping through lists as it goes.
func walkPath(root map[string]any, schemas map[string]any, path string) (map[string]any, error) {
	current := root
	var walked []string
	for _, segment := range strings.Split(path, ".") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		current = follow(current, schemas)
		// A list of things: the path names the thing, not the list.
		if items, ok := current["items"].(map[string]any); ok {
			if _, hasProps := current["properties"]; !hasProps {
				current = follow(items, schemas)
			}
		}
		properties, ok := current["properties"].(map[string]any)
		if !ok {
			return nil, pathError(walked, segment, nil)
		}
		next, ok := properties[segment].(map[string]any)
		if !ok {
			return nil, pathError(walked, segment, properties)
		}
		walked = append(walked, segment)
		current = next
	}
	return follow(current, schemas), nil
}

func pathError(walked []string, segment string, properties map[string]any) error {
	where := "the top level"
	if len(walked) > 0 {
		where = strings.Join(walked, ".")
	}
	if len(properties) == 0 {
		return fmt.Errorf("%s has no fields inside it, so %q cannot be described", where, segment)
	}
	names := sortedKeys(properties)
	if len(names) > 25 {
		names = names[:25]
	}
	return fmt.Errorf("%s has no field called %q. It has: %s", where, segment, strings.Join(names, ", "))
}

// follow replaces a reference with what it points at, once.
func follow(schema map[string]any, schemas map[string]any) map[string]any {
	ref, ok := schema["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, refPrefix) {
		return schema
	}
	target, ok := schemas[strings.TrimPrefix(ref, refPrefix)].(map[string]any)
	if !ok {
		return schema
	}
	return target
}

// resolve follows references through a description until the budget or the
// depth runs out, leaving anything past that as the reference it was.
func resolve(schema map[string]any, schemas map[string]any, depth int, budget *int) map[string]any {
	out := make(map[string]any, len(schema))
	for key, value := range schema {
		if key == "$ref" {
			ref, ok := value.(string)
			if !ok || !strings.HasPrefix(ref, refPrefix) {
				out[key] = value
				continue
			}
			target, ok := schemas[strings.TrimPrefix(ref, refPrefix)].(map[string]any)
			if !ok || depth >= maxRefDepth || *budget <= 0 {
				out[key] = value
				continue
			}
			*budget--
			for k, v := range resolve(target, schemas, depth+1, budget) {
				out[k] = v
			}
			continue
		}
		out[key] = resolveValue(value, schemas, depth, budget)
	}
	return out
}

func resolveValue(value any, schemas map[string]any, depth int, budget *int) any {
	switch typed := value.(type) {
	case map[string]any:
		return resolve(typed, schemas, depth, budget)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = resolveValue(item, schemas, depth, budget)
		}
		return out
	default:
		return value
	}
}

func groupVersionLabel(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}
