// What a tool call reported back, as resources rather than as text.
//
// A turn's answer names resources the user never typed — "which workloads are
// failing?" is answered out of a listing the user has no other way to point
// at. Clients that let the user reference resources (the CLI's "@" picker)
// want those names, so the run loop extracts them from each tool result and
// they travel out beside the tool-activity events the client already gets.
//
// Extraction is deliberately narrow and cheap. A tool result is provider text:
// it may be JSON, YAML, a table, an apology, or a megabyte of logs. Only
// Kubernetes-shaped JSON is recognized — an object with a "kind" and a
// "metadata.name", which is what every listing and every get returns — and
// anything else yields nothing at all rather than guesses. Nothing here parses
// prose: a name that only ever appeared in a sentence is not a reference.
package agent

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Extraction limits. The content is provider-controlled, so the walk is bounded
// on every axis that could otherwise be driven by it: how much text is parsed
// at all, how deep nesting goes, how many values are visited, and how many
// refs come back.
const (
	maxToolResultBytes = 1 << 20 // 1 MiB of result text is parsed; past that, none
	maxResourceDepth   = 12
	maxResourceNodes   = 20_000
	maxToolResources   = 30
)

// ResourceRef is one resource a tool reported. Kind is the lowercased kind
// ("workload"), which is both what a user types after "@" and what
// [Mention].Kind carries, so a reported resource and a typed one are the same
// shape by the time a client sees them.
type ResourceRef struct {
	Kind     string
	Name     string
	APIGroup string
}

// resourcesInToolResult extracts the resources a tool result names, in the
// order they appear and without duplicates. Content that is not JSON, or that
// holds nothing Kubernetes-shaped, yields nil.
func resourcesInToolResult(content string) []ResourceRef {
	doc, ok := decodeToolResult(content)
	if !ok {
		return nil
	}
	w := &resourceWalk{seen: map[string]bool{}, nodes: maxResourceNodes}
	w.walk(doc, 0, "", "")
	return w.out
}

// decodeToolResult parses a tool result as JSON. A result is often JSON with a
// sentence in front of it ("Found 3 workloads:\n{…}"), so a failed parse is
// retried over the span between the first opening bracket and the last closing
// one — still one parse, of a substring of text already in memory.
func decodeToolResult(content string) (any, bool) {
	if len(content) > maxToolResultBytes {
		return nil, false
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, false
	}
	var doc any
	if json.Unmarshal([]byte(trimmed), &doc) == nil {
		return doc, true
	}
	start := strings.IndexAny(trimmed, "{[")
	end := strings.LastIndexAny(trimmed, "}]")
	if start < 0 || end <= start {
		return nil, false
	}
	if json.Unmarshal([]byte(trimmed[start:end+1]), &doc) != nil {
		return nil, false
	}
	return doc, true
}

// resourceWalk is one extraction in progress: what has been collected, what has
// already been seen, and how much walking is left in the budget.
type resourceWalk struct {
	out   []ResourceRef
	seen  map[string]bool
	nodes int
}

// walk descends one JSON value. kind/group are what an enclosing list has said
// its items are, for the listing shape where only the list itself is typed:
// {"kind":"WorkloadList","items":[{"metadata":{"name":"web"}}]}.
func (w *resourceWalk) walk(v any, depth int, kind, group string) {
	if w.nodes <= 0 || depth > maxResourceDepth || len(w.out) >= maxToolResources {
		return
	}
	w.nodes--

	switch val := v.(type) {
	case []any:
		for _, item := range val {
			w.walk(item, depth+1, kind, group)
		}
	case map[string]any:
		objKind, objGroup := kind, group
		if k, ok := val["kind"].(string); ok && k != "" {
			objKind, objGroup = k, groupOf(val)
		}
		if name := metadataName(val); name != "" && objKind != "" {
			w.add(objKind, name, objGroup)
		}
		// A list types its own items: "WorkloadList" holds Workloads, and the
		// items themselves usually carry no kind of their own.
		itemKind, itemGroup := kind, group
		if base, isList := strings.CutSuffix(objKind, "List"); isList {
			itemKind, itemGroup = base, objGroup
		}
		// Keys in sorted order, never map order: the client keeps these in the
		// order they arrive, so the same result must always produce the same
		// order.
		for _, key := range slices.Sorted(maps.Keys(val)) {
			if key == "metadata" {
				continue // owner references and labels are not this object
			}
			w.walk(val[key], depth+1, itemKind, itemGroup)
		}
	}
}

// add records one resource, lowercasing the kind into the token a user would
// type and dropping repeats.
func (w *resourceWalk) add(kind, name, group string) {
	ref := ResourceRef{Kind: strings.ToLower(kind), Name: name, APIGroup: group}
	key := ref.Kind + "/" + ref.Name
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	w.out = append(w.out, ref)
}

// metadataName is an object's metadata.name, or "" when it has none.
func metadataName(obj map[string]any) string {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := meta["name"].(string)
	return name
}

// groupOf is the API group of an object's apiVersion ("compute.datumapis.com"
// from "compute.datumapis.com/v1alpha1"). Core-group objects ("v1") have none.
func groupOf(obj map[string]any) string {
	av, _ := obj["apiVersion"].(string)
	group, _, ok := strings.Cut(av, "/")
	if !ok {
		return ""
	}
	return group
}
