package capability

import "sort"

// ServiceEntitlement is one entitled provider service, derived STRICTLY from
// that project's capability documents — nothing global, nothing inferred, and
// nothing learned from a live MCP connection.
type ServiceEntitlement struct {
	// ID is the sanitized serviceRef: the stable identifier a card advertises,
	// run through [SanitizeName] so a provider-supplied name cannot put an
	// arbitrary string in a field consumers treat as an ID.
	ID                string
	ServiceRef        string   // spec.serviceRef.name, verbatim — "streamco"
	ServiceName       string   // spec.serviceName — "streaming.streamco.example"
	ToolNames         []string // namespaced "<server>__<tool>", from ToolSelector.Include ONLY
	MCPEndpoints      []string // spec.tools.mcpServers[].endpoint
	SkillNames        []string // namespaced "<serviceRef>__<skill>"
	SkillDescriptions []string
	KnowledgeTitles   []string // knowledge source titles (or URLs when untitled)
}

// Entitlements derives one entry per entitled service from ALREADY-SCOPED
// documents. Callers MUST pass the output of [ScopeDocuments]: a Source may
// return more than the calling project's documents (FixtureSource ignores the
// project entirely), and that gate is the only thing standing between a
// project's card and another project's services.
//
// It reads documents, never a [Composed], so Patch's own built-ins (load_skill,
// memory_remember, memory_forget, report_capability_gap__*) can never leak into
// what is advertised as provider surface. It performs no MCP connect either:
// the declared ToolSelector.Include list is what a project is entitled to, which
// is a claim about entitlement, not about health.
func Entitlements(docs []CapabilityDocument) []ServiceEntitlement {
	// Namespaced names are global in Compose (see connectTools/collectSkills),
	// so collisions resolve the same way here: first registration wins.
	seenTools := map[string]bool{}
	seenSkills := map[string]bool{}

	byID := make(map[string]*ServiceEntitlement, len(docs))
	out := make([]*ServiceEntitlement, 0, len(docs))
	for _, doc := range docs {
		spec := doc.Spec
		if !advertisable(spec) {
			continue // nothing to put on a card
		}

		// Documents sharing a serviceRef MERGE rather than the later ones being
		// dropped: Compose registers every one of their servers and skills, so
		// dropping here would advertise less than the next turn will actually
		// run — a router would conclude a tool is unavailable when it is not.
		id := SanitizeName(spec.ServiceRef.Name)
		ent := byID[id]
		if ent == nil {
			ent = &ServiceEntitlement{ID: id, ServiceRef: spec.ServiceRef.Name, ServiceName: spec.ServiceName}
			byID[id] = ent
			out = append(out, ent)
		}
		if spec.Tools != nil {
			for _, server := range spec.Tools.MCPServers {
				ent.MCPEndpoints = append(ent.MCPEndpoints, server.Endpoint)
				for _, toolName := range server.ToolSelector.Include {
					namespaced := NamespaceToolName(server.Name, toolName)
					if seenTools[namespaced] {
						continue
					}
					seenTools[namespaced] = true
					ent.ToolNames = append(ent.ToolNames, namespaced)
				}
			}
		}
		for _, sk := range spec.Skills {
			namespaced := NamespaceToolName(spec.ServiceRef.Name, sk.Name)
			if seenSkills[namespaced] {
				continue
			}
			seenSkills[namespaced] = true
			ent.SkillNames = append(ent.SkillNames, namespaced)
			ent.SkillDescriptions = append(ent.SkillDescriptions, sk.Description)
		}
		if spec.Knowledge != nil {
			for _, src := range spec.Knowledge.Sources {
				title := src.Title
				if title == "" {
					title = src.URL
				}
				ent.KnowledgeTitles = append(ent.KnowledgeTitles, title)
			}
		}
	}

	// Sorted so the card is stable regardless of the order the Source returned.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	flat := make([]ServiceEntitlement, 0, len(out))
	for _, ent := range out {
		flat = append(flat, *ent)
	}
	return flat
}

// advertisable reports whether a document contributes anything a card could
// name.
func advertisable(spec CapabilitySpec) bool {
	if spec.Tools != nil && len(spec.Tools.MCPServers) > 0 {
		return true
	}
	if len(spec.Skills) > 0 {
		return true
	}
	return spec.Knowledge != nil && (len(spec.Knowledge.Sources) > 0 || len(spec.Knowledge.Concepts) > 0)
}
