// API discovery for "@" mentions: what kinds of Datum resource exist in the
// caller's project, and what is actually there under one of them.
//
// Both reads go through [ReadView], so they carry the same identity and the
// same project scoping as the conversation views (see readview.go). Discovery
// is fetched once per session and instances lazily per kind, because a project
// can carry a lot of both and the picker only ever shows six rows.
//
// Kubernetes offers two discovery shapes. Aggregated discovery (one request for
// every group and its resources) is asked for first via a custom Accept; an
// apiserver too old for it — or the kubectl transport, which cannot send a
// header at all — answers with the classic APIGroupList instead, which is
// recognized by its kind and walked group by group. Everything here is best
// effort: a failure becomes one line in the picker, never an error in the chat.
package patchcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// aggregatedDiscoveryAccept asks for Kubernetes' v2 aggregated discovery, which
// answers "/apis" with every group's resources instead of only their names.
const aggregatedDiscoveryAccept = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList"

// maxMentionInstances caps how many objects of one kind are held for the
// picker. Names are cheap, but a project with thousands of instances should not
// turn a keystroke into a megabyte of retained strings.
const maxMentionInstances = 500

// maxDiscoveryGroups caps the classic walk's fan-out (see walkClassicDiscovery).
const maxDiscoveryGroups = 40

// resourceKind is one mentionable kind: what the user types after "@", and
// enough of its GroupVersionResource to list it.
type resourceKind struct {
	token   string // the mention token — the singular resource name ("workload")
	plural  string // the collection path segment ("workloads")
	kind    string // the API kind ("Workload"), shown as the row's description
	group   string
	version string
}

// listPath is the collection path for this kind, across every namespace: a
// mention names a resource in the project, and the project — not a namespace
// inside it — is what [ReadView] already scopes every request to.
func (k resourceKind) listPath() string {
	return fmt.Sprintf("/apis/%s/%s/%s?limit=%d", k.group, k.version, k.plural, maxMentionInstances)
}

// excludedGroups are the API groups whose contents are Kubernetes' own plumbing
// rather than anything a user would point at mid-sentence. The core group ("" —
// served under /api, not /apis) is out for the same reason plus one of its own:
// enumerating a project's Secrets to fill a picker is not something to do
// casually.
var excludedGroups = map[string]bool{
	"":                             true,
	"admissionregistration.k8s.io": true,
	"apiextensions.k8s.io":         true,
	"apiregistration.k8s.io":       true,
	"apps":                         true,
	"authentication.k8s.io":        true,
	"authorization.k8s.io":         true,
	"autoscaling":                  true,
	"batch":                        true,
	"certificates.k8s.io":          true,
	"coordination.k8s.io":          true,
	"discovery.k8s.io":             true,
	"events.k8s.io":                true,
	"extensions":                   true,
	"flowcontrol.apiserver.k8s.io": true,
	"internal.apiserver.k8s.io":    true,
	"node.k8s.io":                  true,
	"policy":                       true,
	"rbac.authorization.k8s.io":    true,
	"scheduling.k8s.io":            true,
	"storage.k8s.io":               true,
}

// discoverResourceKinds returns every kind that can be mentioned in a project,
// sorted by token. It asks for aggregated discovery and falls back to the
// classic two-level walk whenever the answer is not the aggregated document —
// an older apiserver, or the kubectl transport, which sends no Accept.
func discoverResourceKinds(ctx context.Context, view ReadView, project string) ([]resourceKind, error) {
	body, err := view.getAccept(ctx, project, "/apis", aggregatedDiscoveryAccept)
	if err != nil {
		return nil, errors.New(readViewErrorText(view, err))
	}
	if kinds, ok := decodeAggregatedDiscovery(body); ok {
		return sortKinds(kinds), nil
	}
	kinds, err := walkClassicDiscovery(ctx, view, project, body)
	if err != nil {
		return nil, err
	}
	return sortKinds(kinds), nil
}

// apiGroupDiscoveryList is the shape of Kubernetes' v2 aggregated discovery.
type apiGroupDiscoveryList struct {
	Kind  string `json:"kind"`
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Versions []struct {
			Version   string `json:"version"`
			Resources []struct {
				Resource         string `json:"resource"`
				SingularResource string `json:"singularResource"`
				ResponseKind     struct {
					Kind string `json:"kind"`
				} `json:"responseKind"`
				Verbs []string `json:"verbs"`
			} `json:"resources"`
		} `json:"versions"`
	} `json:"items"`
}

// decodeAggregatedDiscovery reads the aggregated document, reporting false when
// the body is something else (the classic APIGroupList, most often, because the
// server ignored the Accept).
func decodeAggregatedDiscovery(body []byte) ([]resourceKind, bool) {
	var doc apiGroupDiscoveryList
	if err := json.Unmarshal(body, &doc); err != nil || doc.Kind != "APIGroupDiscoveryList" {
		return nil, false
	}
	var out []resourceKind
	for _, g := range doc.Items {
		group := g.Metadata.Name
		if excludedGroups[group] || len(g.Versions) == 0 {
			continue
		}
		// The first version is the server's preferred one; the rest describe
		// the same resources at an older revision.
		v := g.Versions[0]
		for _, r := range v.Resources {
			if k, ok := mentionableKind(group, v.Version, r.Resource, r.SingularResource, r.ResponseKind.Kind, r.Verbs); ok {
				out = append(out, k)
			}
		}
	}
	return out, true
}

// walkClassicDiscovery is the fallback: the /apis group list already in hand,
// then one request per group for its preferred version's resources.
func walkClassicDiscovery(ctx context.Context, view ReadView, project string, groupList []byte) ([]resourceKind, error) {
	var doc struct {
		Groups []struct {
			Name             string `json:"name"`
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
				Version      string `json:"version"`
			} `json:"preferredVersion"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(groupList, &doc); err != nil {
		return nil, fmt.Errorf("decode api groups: %w", err)
	}

	var out []resourceKind
	walked := 0
	for _, g := range doc.Groups {
		if excludedGroups[g.Name] || g.PreferredVersion.GroupVersion == "" {
			continue
		}
		// One request per group, and on the kubectl transport that is one
		// process per group — worth a ceiling before it becomes a stall.
		if walked == maxDiscoveryGroups {
			break
		}
		walked++
		body, err := view.get(ctx, project, "/apis/"+g.PreferredVersion.GroupVersion)
		if err != nil {
			// One unreadable group is not worth losing every other group over.
			continue
		}
		var list struct {
			Resources []struct {
				Name         string   `json:"name"`
				SingularName string   `json:"singularName"`
				Kind         string   `json:"kind"`
				Verbs        []string `json:"verbs"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			continue
		}
		for _, r := range list.Resources {
			if k, ok := mentionableKind(g.Name, g.PreferredVersion.Version, r.Name, r.SingularName, r.Kind, r.Verbs); ok {
				out = append(out, k)
			}
		}
	}
	return out, nil
}

// mentionableKind builds a [resourceKind] from one discovery entry, rejecting
// what cannot be mentioned: subresources ("workloads/status"), and anything
// that cannot be listed — which is also what drops the create-only *Review
// resources without naming each of them.
func mentionableKind(group, version, plural, singular, kind string, verbs []string) (resourceKind, bool) {
	if group == "" || version == "" || plural == "" || strings.Contains(plural, "/") {
		return resourceKind{}, false
	}
	if !slices.Contains(verbs, "list") {
		return resourceKind{}, false
	}
	token := singular
	if token == "" {
		token = strings.ToLower(kind)
	}
	if token == "" {
		return resourceKind{}, false
	}
	return resourceKind{token: token, plural: plural, kind: kind, group: group, version: version}, true
}

// sortKinds orders kinds by token (then group, so the order is total) and drops
// later duplicates of a token. Two groups can both call their singular
// "policy"; which one wins is arbitrary, but it has to be stable or the
// picker's rows would reshuffle between keystrokes.
func sortKinds(kinds []resourceKind) []resourceKind {
	sort.SliceStable(kinds, func(i, j int) bool {
		if kinds[i].token != kinds[j].token {
			return kinds[i].token < kinds[j].token
		}
		return kinds[i].group < kinds[j].group
	})
	out := make([]resourceKind, 0, len(kinds))
	last := ""
	for _, k := range kinds {
		if k.token == last {
			continue
		}
		last = k.token
		out = append(out, k)
	}
	return out
}

// listResourceNames lists one kind's objects and returns their names,
// newest-first (ties broken by name) so the things just created — the ones most
// likely to be asked about — are the first rows in the picker.
func listResourceNames(ctx context.Context, view ReadView, project string, k resourceKind) ([]string, error) {
	body, err := view.get(ctx, project, k.listPath())
	if err != nil {
		return nil, errors.New(readViewErrorText(view, err))
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode %s: %w", k.plural, err)
	}
	items := list.Items
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].Metadata, items[j].Metadata
		if !a.CreationTimestamp.Equal(b.CreationTimestamp) {
			return a.CreationTimestamp.After(b.CreationTimestamp)
		}
		return a.Name < b.Name
	})
	names := make([]string, 0, len(items))
	for _, it := range items {
		if it.Metadata.Name == "" {
			continue
		}
		names = append(names, it.Metadata.Name)
		if len(names) == maxMentionInstances {
			break
		}
	}
	return names, nil
}
