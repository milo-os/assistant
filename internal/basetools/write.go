package basetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/milo-os/assistant/internal/plantoken"
	"github.com/milo-os/assistant/internal/projectapi"
)

// The write path is three tools, of which exactly one can change anything.
//
// resources_validate asks the platform for its verdict on some manifests
// without keeping any of them, so it is safe to run as often as it takes to get
// them right. resources_plan settles what would happen — create or change, in
// what order, changing what — and returns the manifests it settled on together
// with a token that is a hash of them. resources_apply takes those manifests
// and that token, re-derives the hash from what it was actually handed, and
// refuses anything that does not match.
//
// So the only thing that can reach the platform is the change the model already
// put in front of the person who asked. A change nobody was shown has no token
// and cannot be applied at all. See internal/plantoken for what the token
// covers and why.
const (
	// ResourcesValidateToolName checks manifests without keeping them.
	ResourcesValidateToolName = "resources_validate"
	// ResourcesPlanToolName settles what would happen and authorizes it.
	ResourcesPlanToolName = "resources_plan"
	// ResourcesApplyToolName carries out a plan that was agreed to.
	ResourcesApplyToolName = "resources_apply"
)

const (
	actionCreate = "create"
	actionUpdate = "update"

	// fieldManifest is what a rejection names when a manifest could not be
	// read at all, rather than when the platform named a path inside it.
	fieldManifest = "manifest"

	// maxManifests bounds one call. A change to more than this at once is not
	// something a person can read and agree to, which is the only thing the
	// plan token can protect.
	maxManifests = 20

	// maxDiffLines bounds what one change reports. Past it the person is
	// reading a wall rather than a change.
	maxDiffLines = 60
)

// MutatingToolNames are the base tools on the change path.
//
// resources_apply is the only one that writes; resources_plan is on the list
// because it is the only thing that can authorize a write, and an operator
// asking "what can this project's assistant change" wants both answers.
func MutatingToolNames() []string {
	return []string{ResourcesPlanToolName, ResourcesApplyToolName}
}

// writeTools returns the change path, or nil when there is no key to bind a
// plan with. A service that cannot check a token must not issue something that
// looks like one, so the write tools are absent rather than broken.
func writeTools(opts Options) map[string]*tool {
	if len(opts.PlanTokenKey) == 0 {
		return nil
	}
	return map[string]*tool{
		ResourcesValidateToolName: resourcesValidate(opts),
		ResourcesPlanToolName:     resourcesPlan(opts),
		ResourcesApplyToolName:    resourcesApply(opts),
	}
}

// ------------------------------------------------------------------- output

// ManifestResult is what one manifest would do, and whether it can.
type ManifestResult struct {
	// Index is where this manifest was in the list that was passed in.
	Index int `json:"index"`
	// Kind, Name and Namespace identify what it describes, when they could be
	// read from it.
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Valid is false when the platform rejected it. Nothing can be planned or
	// applied until it is fixed.
	Valid bool `json:"valid"`
	// Errors are the rejections, each with the field path the platform named.
	Errors []projectapi.FieldError `json:"errors,omitempty"`
	// Exists reports whether a resource of this name is already there, which
	// is what decides between creating one and changing one.
	Exists bool `json:"exists"`
	// Action is "create" or "update".
	Action string `json:"action,omitempty"`
	// Diff is what applying this would change about the existing resource,
	// taken from what the platform said it would become. Empty for a create,
	// and empty for a change that changes nothing.
	Diff []string `json:"diff,omitempty"`
	// Manifest is the canonical form this covers. Present in a plan, where it
	// is what the token covers and what apply has to be handed back.
	Manifest string `json:"manifest,omitempty"`
}

// ValidateOutput is the platform's verdict on each manifest.
type ValidateOutput struct {
	Valid   bool             `json:"valid"`
	Results []ManifestResult `json:"results"`
}

// PlanOutput is everything a person needs to see before agreeing, plus the
// token that binds their agreement to exactly this.
type PlanOutput struct {
	// Valid is false when anything was rejected. There is no token in that
	// case and nothing can be applied.
	Valid bool `json:"valid"`
	// Results are in the order they would be applied in, which is part of what
	// the token covers.
	Results []ManifestResult `json:"results"`
	// PlanToken authorizes applying exactly these manifests, in this order,
	// and nothing else.
	PlanToken string `json:"planToken,omitempty"`
	// ExpiresAt is when the token stops being accepted, in RFC 3339.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// Note says anything about the plan the person should hear.
	Note string `json:"note,omitempty"`
}

// AppliedResource is one thing that was created or changed.
type AppliedResource struct {
	Action    string `json:"action"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// ApplyOutput is what was done.
type ApplyOutput struct {
	Applied []AppliedResource `json:"applied"`
	// Next is the step that turns an accepted request into something that is
	// actually running, which are not the same thing.
	Next string `json:"next"`
}

// ------------------------------------------------------------------- tools

func manifestsProperty(what string) map[string]any {
	return map[string]any{
		"manifests": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": what,
		},
	}
}

func resourcesValidate(opts Options) *tool {
	return newTool(
		ResourcesValidateToolName,
		"Ask the platform whether some manifests would be accepted, without creating or changing "+
			"anything. Returns each rejection with the field path it names, whether a resource of that "+
			"name is already there, and — when it is — what applying would change about it. Every "+
			"rejection reported here is one that would otherwise arrive after the person was told "+
			"their resource was written correctly. Quote the field path and say in plain words what it "+
			"means, fix the manifest, and validate again. Never plan or apply a manifest that failed. "+
			"Writes nothing.",
		object(manifestsProperty(
			"The manifests to check, as YAML or JSON, one resource per entry. A single entry holding "+
				"several documents separated by --- is accepted too."), "manifests"),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			manifests, err := readManifestsArg(ResourcesValidateToolName, input)
			if err != nil {
				return "", err
			}
			items, err := prepare(ctx, opts, manifests)
			if err != nil {
				return "", err
			}
			check(ctx, opts, items)

			out := ValidateOutput{Valid: true, Results: make([]ManifestResult, 0, len(items))}
			for _, item := range items {
				out.Results = append(out.Results, item.result(false))
				if len(item.errors) > 0 {
					out.Valid = false
				}
			}
			return result(out)
		},
	)
}

func resourcesPlan(opts Options) *tool {
	return newTool(
		ResourcesPlanToolName,
		"Settle everything that has to be true before these resources can be created or changed, and "+
			"mint the token that authorizes exactly that. Validates every manifest, works out for each "+
			"whether it creates or changes, puts them in an order that works when one refers to "+
			"another, reports what would change, and returns canonical manifests with a plan token "+
			"that is a hash of them. The manifests in this output are the ones the token covers: show "+
			"them in full, and the differences, to the person who asked, say plainly what will exist "+
			"afterwards, and get an explicit yes before calling "+ResourcesApplyToolName+". A question "+
			"about the plan is not a yes. If anything changes, plan again — a token minted for the "+
			"earlier manifests is refused, and applying it because it was close is exactly what this "+
			"prevents. Tokens are good for "+plantoken.TTL.String()+". Planning by itself creates and "+
			"changes nothing.",
		object(manifestsProperty(
			"The manifests to plan, as YAML or JSON, one resource per entry. Several may be planned "+
				"together when they belong to one change; they are ordered for you."), "manifests"),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			manifests, err := readManifestsArg(ResourcesPlanToolName, input)
			if err != nil {
				return "", err
			}
			items, err := prepare(ctx, opts, manifests)
			if err != nil {
				return "", err
			}
			check(ctx, opts, items)

			out := PlanOutput{Valid: true}
			for _, item := range items {
				if len(item.errors) > 0 {
					out.Valid = false
				}
			}
			if !out.Valid {
				out.Results = make([]ManifestResult, 0, len(items))
				for _, item := range items {
					out.Results = append(out.Results, item.result(false))
				}
				out.Note = "Nothing was planned, so there is no token and nothing can be applied. Fix " +
					"what was rejected and plan again."
				return result(out)
			}

			ordered := inDependencyOrder(items)
			binding := plantoken.Binding{Project: opts.Project.Name()}
			out.Results = make([]ManifestResult, 0, len(ordered))
			for _, item := range ordered {
				canonical, err := canonicalJSON(item.desired)
				if err != nil {
					return "", err
				}
				binding.Manifests = append(binding.Manifests, canonical)
				binding.ResourceVersions = append(binding.ResourceVersions, item.resourceVersion)
				out.Results = append(out.Results, item.result(true))
			}

			expiry := opts.now().Add(plantoken.TTL)
			out.PlanToken = plantoken.Mint(opts.PlanTokenKey, binding, expiry)
			out.ExpiresAt = expiry.UTC().Format(time.RFC3339)
			if len(ordered) > 1 {
				out.Note = "These are applied in the order shown, because a later one refers to an " +
					"earlier one. Hand them back to " + ResourcesApplyToolName + " in exactly this order."
			}
			return result(out)
		},
	)
}

func resourcesApply(opts Options) *tool {
	properties := manifestsProperty(
		"The manifests " + ResourcesPlanToolName + " returned, verbatim and in the order it returned " +
			"them. One changed character, or one swapped pair, and the token stops matching and " +
			"nothing is changed.")
	properties["planToken"] = str(
		"The token " + ResourcesPlanToolName + " returned for exactly these manifests. Call this only " +
			"once the person who asked has seen the plan and said yes.")

	return newTool(
		ResourcesApplyToolName,
		"Create or change exactly what a plan token was minted for, and nothing else. Takes the "+
			"manifests "+ResourcesPlanToolName+" returned and that plan's token: the token is a hash "+
			"of those manifests, their order, the project, and the version of each resource the plan "+
			"saw, so a manifest edited after the plan, a reordered list, a token from another project, "+
			"or a resource somebody else changed in the meantime is refused rather than applied. Call "+
			"this only once the person who asked has been shown the plan and has said yes; if they "+
			"asked for something different instead, go back and plan again. A resource being created "+
			"means the request was accepted, not that anything is running yet — say that plainly "+
			"rather than reporting it as done.",
		object(properties, "manifests", "planToken"),
		func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Manifests []string `json:"manifests"`
				PlanToken string   `json:"planToken"`
			}
			if err := decode(ResourcesApplyToolName, input, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.PlanToken) == "" {
				return "", fmt.Errorf(
					"nothing was changed: this call carried no plan token. Call %s with the manifests "+
						"to apply, show the person who asked what it returns, and use the token from it "+
						"once they have agreed", ResourcesPlanToolName)
			}
			manifests, err := checkManifestsArg(ResourcesApplyToolName, args.Manifests)
			if err != nil {
				return "", err
			}

			items, err := prepare(ctx, opts, manifests)
			if err != nil {
				return "", err
			}
			// A manifest that cannot even be read has no canonical form, so
			// there is nothing to compare a token against.
			for _, item := range items {
				if len(item.errors) > 0 {
					return "", fmt.Errorf(
						"nothing was changed: manifest %d could not be read. %s. Plan again and hand "+
							"back exactly what comes out of it",
						item.index+1, item.errors[0].Message)
				}
			}

			// The token is checked before anything is written and before
			// anything is even offered to the platform: a change nobody agreed
			// to must not reach it at all, not even as a check.
			binding := plantoken.Binding{Project: opts.Project.Name()}
			for _, item := range items {
				canonical, err := canonicalJSON(item.desired)
				if err != nil {
					return "", err
				}
				binding.Manifests = append(binding.Manifests, canonical)
				binding.ResourceVersions = append(binding.ResourceVersions, item.resourceVersion)
			}
			if err := plantoken.Verify(
				opts.PlanTokenKey, args.PlanToken, binding, opts.now(), ResourcesPlanToolName,
			); err != nil {
				return "", err
			}

			// Checked once more against the platform, because the plan may
			// have been made minutes ago and the things it depends on move
			// underneath it. A rejection here changes nothing.
			check(ctx, opts, items)
			for _, item := range items {
				if len(item.errors) > 0 {
					return "", fmt.Errorf(
						"nothing was changed: the platform rejected %s %q when it was checked again "+
							"just before applying. %s. Something moved since the plan was made — fix "+
							"the manifest, plan again, and show the person who asked what comes back",
						item.kind, item.name, item.errors[0].Message)
				}
			}

			out := ApplyOutput{Applied: make([]AppliedResource, 0, len(items))}
			for _, item := range items {
				action := actionCreate
				var err error
				if item.existing != nil {
					action = actionUpdate
					desired := deepCopy(item.desired)
					setResourceVersion(desired, item.resourceVersion)
					_, err = opts.Project.Update(ctx, item.resource, desired, false)
				} else {
					_, err = opts.Project.Create(ctx, item.resource, item.desired, false)
				}
				if err != nil {
					return "", partialFailure(out.Applied, item, err)
				}
				out.Applied = append(out.Applied, AppliedResource{
					Action: action, Kind: item.kind, Name: item.name, Namespace: item.namespace,
				})
			}

			out.Next = "The request was accepted, which is not the same as anything running yet. Read " +
				"the resource back with " + ResourcesGetToolName + ", or use the owning service's own " +
				"tools if it publishes any, before telling the person it is done."
			return result(out)
		},
	)
}

// partialFailure reports a change that stopped part-way. What was already done
// stays done, and saying exactly what that was is the difference between a
// person who can carry on and one who has to guess.
func partialFailure(applied []AppliedResource, failed *manifestItem, err error) error {
	if len(applied) == 0 {
		return fmt.Errorf("nothing was changed: %s %q could not be applied: %w",
			failed.kind, failed.name, err)
	}
	var done []string
	for _, resource := range applied {
		done = append(done, fmt.Sprintf("%s %s (%sd)", resource.Kind, resource.Name, resource.Action))
	}
	return fmt.Errorf(
		"this change stopped part-way. Already done, and still done: %s. Not done: %s %q, because %w. "+
			"Nothing after it was attempted either. Tell the person exactly this, then plan the rest again",
		strings.Join(done, ", "), failed.kind, failed.name, err)
}

// ------------------------------------------------------------------ parsing

// manifestItem is one manifest on its way through validate, plan or apply.
type manifestItem struct {
	// index is where this manifest was in the list that came in.
	index int
	// desired is the manifest, normalized: what the token is computed over.
	desired projectapi.Object
	// existing is what is there today, or nil when nothing is.
	existing projectapi.Object
	// resourceVersion is the version existing was read at, or "" for a create.
	resourceVersion string

	resource  projectapi.Resource
	kind      string
	name      string
	namespace string

	// errors is empty when the platform accepted this manifest.
	errors []projectapi.FieldError
	diff   []string
}

func (m *manifestItem) result(withManifest bool) ManifestResult {
	out := ManifestResult{
		Index:     m.index,
		Kind:      m.kind,
		Name:      m.name,
		Namespace: m.namespace,
		Valid:     len(m.errors) == 0,
		Errors:    m.errors,
		Exists:    m.existing != nil,
		Diff:      m.diff,
	}
	if out.Valid {
		out.Action = actionCreate
		if m.existing != nil {
			out.Action = actionUpdate
		}
	}
	if withManifest && out.Valid {
		if manifest, err := toYAML(m.desired); err == nil {
			out.Manifest = manifest
		}
	}
	return out
}

func readManifestsArg(toolName string, input json.RawMessage) ([]string, error) {
	var args struct {
		Manifests []string `json:"manifests"`
	}
	if err := decode(toolName, input, &args); err != nil {
		return nil, err
	}
	return checkManifestsArg(toolName, args.Manifests)
}

func checkManifestsArg(toolName string, manifests []string) ([]string, error) {
	// One entry may hold several documents; a person pasting a file gets what
	// they expect rather than a rejection about shape.
	var split []string
	for _, manifest := range manifests {
		split = append(split, splitDocuments(manifest)...)
	}
	if len(split) == 0 {
		return nil, fmt.Errorf("%s: at least one manifest is required", toolName)
	}
	if len(split) > maxManifests {
		return nil, fmt.Errorf(
			"%s: %d manifests is more than one change should carry. A person has to be able to read "+
				"and agree to all of it, and that is what the plan exists for — split this into "+
				"changes of at most %d", toolName, len(split), maxManifests)
	}
	return split, nil
}

// splitDocuments breaks a multi-document manifest apart, dropping empty ones.
func splitDocuments(manifest string) []string {
	var out []string
	for _, document := range strings.Split(manifest, "\n---") {
		document = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(document), "---"))
		if document != "" {
			out = append(out, document)
		}
	}
	return out
}

// prepare decodes each manifest, works out what kind it is, and reads what is
// there today. A manifest that cannot be read becomes a rejection on that one
// item rather than a failure of the whole call: the other manifests still have
// verdicts worth reporting.
func prepare(ctx context.Context, opts Options, manifests []string) ([]*manifestItem, error) {
	items := make([]*manifestItem, 0, len(manifests))
	for i, manifest := range manifests {
		item := &manifestItem{index: i}
		items = append(items, item)

		obj, ferr := decodeManifest(manifest)
		if ferr != nil {
			item.errors = []projectapi.FieldError{*ferr}
			continue
		}
		item.kind, _ = projectapi.NestedString(obj, "kind")
		item.name, _ = projectapi.NestedString(obj, "metadata", "name")

		apiVersion, _ := projectapi.NestedString(obj, "apiVersion")
		group, version := splitAPIVersion(apiVersion)
		resource, err := opts.Project.Resolve(ctx, group, version, item.kind)
		if err != nil {
			if errors.Is(err, projectapi.ErrKindNotServed) || projectapi.IsForbidden(err) {
				item.errors = []projectapi.FieldError{{Field: "apiVersion", Message: kindError(err).Error()}}
				continue
			}
			return nil, err
		}
		item.resource = resource

		// The project decides where this goes, never the manifest: one naming
		// somewhere else would be asking to write somewhere this conversation
		// does not reach.
		if resource.Namespaced {
			item.namespace = defaultNamespace
			if named, ok := projectapi.NestedString(obj, "metadata", "namespace"); ok && named != "" {
				item.namespace = named
			}
		}
		item.desired = normalize(obj, item.namespace)

		existing, err := opts.Project.Get(ctx, resource, item.namespace, item.name)
		switch {
		case err == nil:
			item.existing = existing
			item.resourceVersion, _ = projectapi.NestedString(existing, "metadata", "resourceVersion")
		case projectapi.IsNotFound(err):
			// Nothing there: this would create one.
		case projectapi.IsForbidden(err):
			item.errors = []projectapi.FieldError{{
				Message: notEntitled(fmt.Sprintf("%s resources", item.kind)).Error(),
			}}
		default:
			return nil, err
		}
	}
	return items, nil
}

// check asks the platform for its verdict on each item without keeping
// anything. A rejection is a result, not a failure: the field paths are what
// has to be acted on.
func check(ctx context.Context, opts Options, items []*manifestItem) {
	for _, item := range items {
		if item.desired == nil {
			continue // already rejected before it got this far
		}
		item.errors = nil
		item.diff = nil

		attempt := deepCopy(item.desired)
		var (
			would projectapi.Object
			err   error
		)
		if item.existing != nil {
			// Checked at the version that was read, so what is checked is the
			// change that would actually be made.
			setResourceVersion(attempt, item.resourceVersion)
			would, err = opts.Project.Update(ctx, item.resource, attempt, true)
		} else {
			would, err = opts.Project.Create(ctx, item.resource, attempt, true)
		}
		if err != nil {
			item.errors = fieldErrors(err)
			continue
		}
		if item.existing != nil && would != nil {
			// What the platform says it would become, against what is there
			// now — so the differences are real changes rather than every
			// value the platform fills in on its own.
			item.diff = diff(
				normalize(item.existing, item.namespace),
				normalize(would, item.namespace))
		}
	}
}

// decodeManifest reads one manifest and checks it says what it is.
func decodeManifest(manifest string) (projectapi.Object, *projectapi.FieldError) {
	if strings.TrimSpace(manifest) == "" {
		return nil, &projectapi.FieldError{Field: fieldManifest, Message: "a manifest is required"}
	}
	var obj projectapi.Object
	// Strict, so a duplicated field is reported rather than silently dropped
	// and then missing from a resource somebody was told it was on.
	if err := sigsyaml.UnmarshalStrict([]byte(manifest), &obj); err != nil {
		return nil, &projectapi.FieldError{
			Field:   fieldManifest,
			Message: fmt.Sprintf("this is not a readable manifest: %v", err),
		}
	}
	if apiVersion, _ := projectapi.NestedString(obj, "apiVersion"); strings.TrimSpace(apiVersion) == "" {
		return nil, &projectapi.FieldError{
			Field:   "apiVersion",
			Message: "a manifest has to say which apiVersion it is, e.g. \"compute.datumapis.com/v1alpha1\"",
		}
	}
	if kind, _ := projectapi.NestedString(obj, "kind"); strings.TrimSpace(kind) == "" {
		return nil, &projectapi.FieldError{
			Field:   "kind",
			Message: "a manifest has to say which kind it is, e.g. \"Workload\"",
		}
	}
	if name, _ := projectapi.NestedString(obj, "metadata", "name"); strings.TrimSpace(name) == "" {
		return nil, &projectapi.FieldError{
			Field:   "metadata.name",
			Message: "a manifest has to carry a name",
		}
	}
	return obj, nil
}

func splitAPIVersion(apiVersion string) (group, version string) {
	if before, after, found := strings.Cut(apiVersion, "/"); found {
		return before, after
	}
	return "", apiVersion
}

func setResourceVersion(obj projectapi.Object, resourceVersion string) {
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok {
		metadata = map[string]any{}
		obj["metadata"] = metadata
	}
	if resourceVersion == "" {
		delete(metadata, "resourceVersion")
		return
	}
	metadata["resourceVersion"] = resourceVersion
}

// canonicalJSON renders what the plan token is computed over. The manifest is
// normalized first and Go writes map keys in sorted order, so two spellings of
// the same desired state always produce the same bytes.
func canonicalJSON(obj projectapi.Object) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("the manifest could not be put in a settled form: %w", err)
	}
	return raw, nil
}

// fieldErrors turns a rejection into field/message pairs.
func fieldErrors(err error) []projectapi.FieldError {
	var apiErr *projectapi.APIError
	if errors.As(err, &apiErr) {
		return apiErr.FieldErrors()
	}
	return []projectapi.FieldError{{Message: err.Error()}}
}

// -------------------------------------------------------------------- order

// inDependencyOrder puts manifests in an order that works: one that refers to
// another by name goes after it.
//
// A reference is a field whose name ends in "Ref" (or "Refs") carrying a name,
// which is how this platform's kinds point at each other. Anything with no
// reference into the rest of the batch keeps the order it was given in, because
// that is the order the person wrote and there is no reason to disturb it. A
// cycle cannot be ordered at all, so it falls back to the given order and the
// platform rejects whichever half cannot be satisfied.
func inDependencyOrder(items []*manifestItem) []*manifestItem {
	// Index what this batch provides, by kind and by bare name. Kind is
	// checked when the reference declares one, so a reference to a Network
	// called "web" is not satisfied by a Workload called "web".
	provided := map[string][]int{}
	for i, item := range items {
		provided[strings.ToLower(item.name)] = append(provided[strings.ToLower(item.name)], i)
	}

	dependencies := make([]map[int]bool, len(items))
	for i, item := range items {
		dependencies[i] = map[int]bool{}
		for _, ref := range referencesIn(item.desired) {
			for _, j := range provided[strings.ToLower(ref.name)] {
				if j == i {
					continue
				}
				if ref.kind != "" && !strings.EqualFold(ref.kind, items[j].kind) {
					continue
				}
				dependencies[i][j] = true
			}
		}
	}

	ordered := make([]*manifestItem, 0, len(items))
	emitted := make([]bool, len(items))
	for len(ordered) < len(items) {
		next := -1
		for i := range items {
			if emitted[i] {
				continue
			}
			ready := true
			for j := range dependencies[i] {
				if !emitted[j] {
					ready = false
					break
				}
			}
			if ready {
				next = i
				break
			}
		}
		if next == -1 {
			// Everything left refers to everything else. Nothing can be
			// ordered, so keep what was given.
			for i := range items {
				if !emitted[i] {
					next = i
					break
				}
			}
		}
		emitted[next] = true
		ordered = append(ordered, items[next])
	}
	return ordered
}

// reference is one pointer from a manifest to something else.
type reference struct {
	kind string
	name string
}

// referencesIn finds every "<something>Ref" carrying a name.
func referencesIn(obj projectapi.Object) []reference {
	var found []reference
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for _, key := range sortedKeys(typed) {
				child := typed[key]
				if isReferenceKey(key) {
					found = append(found, readReferences(child)...)
				}
				walk(child)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(obj)
	return found
}

func isReferenceKey(key string) bool {
	lower := strings.ToLower(key)
	return strings.HasSuffix(lower, "ref") || strings.HasSuffix(lower, "refs")
}

func readReferences(value any) []reference {
	switch typed := value.(type) {
	case map[string]any:
		name, ok := typed["name"].(string)
		if !ok || name == "" {
			return nil
		}
		kind, _ := typed["kind"].(string)
		return []reference{{kind: kind, name: name}}
	case []any:
		var out []reference
		for _, item := range typed {
			out = append(out, readReferences(item)...)
		}
		return out
	}
	return nil
}

// --------------------------------------------------------------------- diff

// diff reports what changes between two manifests, as one line per field.
//
// Kind-agnostic on purpose: these tools work with kinds this repository has
// never compiled in, so there is nothing to compare field by field. Both sides
// are flattened to paths and values and the paths compared, which reads well
// for exactly the changes a person makes.
func diff(before, after projectapi.Object) []string {
	from := flatten(before)
	to := flatten(after)

	paths := map[string]bool{}
	for path := range from {
		paths[path] = true
	}
	for path := range to {
		paths[path] = true
	}

	var lines []string
	for _, path := range sortedKeys(paths) {
		old, hadOld := from[path]
		current, hasNew := to[path]
		switch {
		case hadOld && hasNew && old != current:
			lines = append(lines, fmt.Sprintf("%s: %s → %s", path, old, current))
		case !hadOld && hasNew:
			lines = append(lines, fmt.Sprintf("%s: (not set) → %s", path, current))
		case hadOld && !hasNew:
			lines = append(lines, fmt.Sprintf("%s: %s → (removed)", path, old))
		}
	}
	if len(lines) > maxDiffLines {
		kept := lines[:maxDiffLines]
		return append(kept, fmt.Sprintf("… and %d more changes", len(lines)-maxDiffLines))
	}
	return lines
}

// flatten reduces a manifest to path/value pairs.
func flatten(obj projectapi.Object) map[string]string {
	out := map[string]string{}
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			if len(typed) == 0 {
				out[prefix] = "{}"
				return
			}
			for _, key := range sortedKeys(typed) {
				child := key
				if prefix != "" {
					child = prefix + "." + key
				}
				walk(child, typed[key])
			}
		case []any:
			if len(typed) == 0 {
				out[prefix] = "[]"
				return
			}
			for i, item := range typed {
				walk(prefix+"["+strconv.Itoa(i)+"]", item)
			}
		default:
			out[prefix] = renderScalar(typed)
		}
	}
	walk("", obj)
	return out
}

func renderScalar(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", typed)
	}
}
