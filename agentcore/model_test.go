package agentcore

import (
	"fmt"
	"sort"
	"testing"
)

// Tools lead the prompt, so their order is part of the cached prefix. A set
// that comes back in a different order on each call invalidates the prompt
// cache on every turn, so Definitions must be deterministic.
func TestToolSetDefinitionsSortedByName(t *testing.T) {
	ts := ToolSet{}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("tool-%02d", (i*7)%20)
		ts[name] = funcTool{name: name}
	}

	for attempt := 0; attempt < 50; attempt++ {
		defs := ts.Definitions()
		if len(defs) != len(ts) {
			t.Fatalf("got %d definitions, want %d", len(defs), len(ts))
		}
		if !sort.SliceIsSorted(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name }) {
			names := make([]string, len(defs))
			for i, d := range defs {
				names[i] = d.Name
			}
			t.Fatalf("attempt %d: definitions not sorted by name: %v", attempt, names)
		}
	}
}
