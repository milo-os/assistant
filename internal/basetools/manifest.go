package basetools

import (
	"fmt"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/milo-os/assistant/internal/projectapi"
)

// Manifests in and out of these tools are YAML, because that is what a person
// reads back and what every other way of writing to this platform already
// takes. Inside, they are ordinary maps: the base tools work with kinds this
// repository has never compiled in, so there is nothing to unmarshal into.

// serverOwnedMetadata are the metadata fields the platform fills in. They are
// removed on the way out so the manifest a person is shown is the one they
// would write, and removed on the way in so two spellings of the same desired
// state cannot hash differently.
var serverOwnedMetadata = []string{
	"resourceVersion",
	"uid",
	"generation",
	"creationTimestamp",
	"deletionTimestamp",
	"deletionGracePeriodSeconds",
	"managedFields",
	"selfLink",
}

// lastAppliedAnnotation is written by other clients and is a copy of the whole
// object, so leaving it in doubles the size of every manifest for nothing.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// normalize returns obj as a manifest: the platform's own bookkeeping removed,
// the observed state removed, and the namespace forced to the one this request
// reaches.
//
// The namespace is applied here rather than trusted from the object for the
// same reason the project is: a manifest naming a different one would be asking
// to write somewhere this conversation does not reach.
func normalize(obj projectapi.Object, namespace string) projectapi.Object {
	out := deepCopy(obj)
	delete(out, "status")

	metadata, ok := out["metadata"].(map[string]any)
	if !ok {
		metadata = map[string]any{}
		out["metadata"] = metadata
	}
	for _, field := range serverOwnedMetadata {
		delete(metadata, field)
	}
	if annotations, ok := metadata["annotations"].(map[string]any); ok {
		delete(annotations, lastAppliedAnnotation)
		if len(annotations) == 0 {
			delete(metadata, "annotations")
		}
	}
	if namespace == "" {
		delete(metadata, "namespace")
	} else {
		metadata["namespace"] = namespace
	}
	return out
}

// toYAML renders a manifest for a person to read.
func toYAML(obj projectapi.Object) (string, error) {
	raw, err := sigsyaml.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("the manifest could not be written out: %w", err)
	}
	return string(raw), nil
}

// deepCopy copies a decoded object so trimming one never mutates what a caller
// still holds.
func deepCopy(value projectapi.Object) projectapi.Object {
	copied, _ := deepCopyValue(value).(projectapi.Object)
	if copied == nil {
		return projectapi.Object{}
	}
	return copied
}

func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			out[k] = deepCopyValue(v)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = deepCopyValue(v)
		}
		return out
	default:
		return value
	}
}
