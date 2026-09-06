// Package gapreport stores capability-gap reports: durable records the
// assistant writes when a provider service let a user down — either no tool
// existed for what they needed, or a tool ran and handed back something
// thin, misleading, or unactionable — so the provider's own team, not the
// consumer project the conversation happened in, can act on it. Reports are
// keyed by providerProject (see [Report].ProviderProject), resolved from the
// composed capability document's spec.reportingProject, never from the
// conversation's project. This is an append-only log: unlike
// internal/memory, reports are written once via the report_capability_gap
// capability tool and never edited by the model.
package gapreport

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrCapabilityTooLong is returned by Insert when capability exceeds
// MaxCapabilityLen.
var ErrCapabilityTooLong = errors.New("gapreport: capability exceeds MaxCapabilityLen")

// ErrCapabilityKeyTooLong is returned by Insert when capabilityKey exceeds
// MaxCapabilityKeyLen.
var ErrCapabilityKeyTooLong = errors.New("gapreport: capability key exceeds MaxCapabilityKeyLen")

// ErrInvalidCapabilityKey is returned by Insert for a key outside the
// [CapabilityKey] grammar. Junk is rejected rather than stored: the key's
// only job is to be recognized and reused by the next conversation, and a
// key nobody will type the same way twice is worse than none at all.
var ErrInvalidCapabilityKey = errors.New("gapreport: capability key must be lowercase alphanumeric words joined by single dashes")

// ErrSummaryTooLong is returned by Insert when summary exceeds
// MaxSummaryLen.
var ErrSummaryTooLong = errors.New("gapreport: summary exceeds MaxSummaryLen")

// ErrEvidenceTooLong is returned by Insert when an evidence field exceeds
// its bound (MaxEvidenceToolLen or MaxEvidenceTextLen).
var ErrEvidenceTooLong = errors.New("gapreport: evidence field exceeds its maximum length")

// ErrUnknownKind is returned by Insert (via [ParseKind]) for a kind outside
// [Kinds]. Unlike a missing kind, which has an obvious default, an
// unrecognized one carries no salvageable meaning — storing it would put a
// value in the provider's feed that no reader can interpret.
var ErrUnknownKind = errors.New("gapreport: unknown kind")

// ErrProjectFull is returned by Insert when a provider project already
// holds MaxReportsPerProject reports. Losing a gap report silently would
// defeat the point of the feature, so this rejects instead of evicting —
// the calling tool surfaces the error to the model.
var ErrProjectFull = errors.New("gapreport: provider project exceeds MaxReportsPerProject")

// MaxCapabilityLen caps the short capability description in bytes.
const MaxCapabilityLen = 200

// MaxCapabilityKeyLen caps the capability key in bytes. 63 is the DNS label
// bound, which is what a key must fit anyway: it is the name of a
// CapabilityGap object in the API.
const MaxCapabilityKeyLen = 63

// MaxSummaryLen caps the summary in bytes.
const MaxSummaryLen = 1000

// MaxEvidenceToolLen caps Evidence.Tool in bytes — it holds a tool name, not
// prose.
const MaxEvidenceToolLen = 200

// MaxEvidenceTextLen caps Evidence.Observed and Evidence.ContradictedBy in
// bytes. Evidence is meant to be a quoted fragment of tool output, not the
// whole response; a bound this size keeps a provider's feed readable and
// limits how much can be dumped across the project boundary at once.
const MaxEvidenceTextLen = 500

// MaxReportsPerProject caps how many reports a single provider project
// accumulates.
const MaxReportsPerProject = 500

// Kind classifies what kind of shortfall a report describes. A gap is not
// only an absent tool: a tool that answers with too little, answers
// misleadingly, or tells the user to do something impossible is a defect the
// provider's team can act on, and one they cannot see from their side.
type Kind string

const (
	// KindMissingCapability: no tool covered what the user needed. The
	// zero value and the default for any report that does not say otherwise.
	KindMissingCapability Kind = "MissingCapability"
	// KindInsufficientDetail: a tool answered, but omitted a field the
	// answer needed to be actionable.
	KindInsufficientDetail Kind = "InsufficientDetail"
	// KindMisleadingOutput: a tool answered, and its output pointed at a
	// wrong conclusion.
	KindMisleadingOutput Kind = "MisleadingOutput"
	// KindUnactionableGuidance: a tool told the user to do something they
	// cannot do.
	KindUnactionableGuidance Kind = "UnactionableGuidance"
)

// Kinds lists every valid [Kind], in the order the report_capability_gap
// tool schema presents them.
var Kinds = []Kind{KindMissingCapability, KindInsufficientDetail, KindMisleadingOutput, KindUnactionableGuidance}

// ParseKind maps tool input to a [Kind]. The empty string is
// KindMissingCapability: kinds were added after the fact, so every provider
// that never sets one and every row stored before they existed keeps meaning
// exactly what it already meant.
func ParseKind(s string) (Kind, error) {
	if s == "" {
		return KindMissingCapability, nil
	}
	for _, k := range Kinds {
		if Kind(s) == k {
			return k, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownKind, s)
}

// NeedsEvidence reports whether a kind is one whose report is only
// actionable with evidence attached. A MissingCapability report has no tool
// output to quote; every other kind is an accusation about output that
// exists, and without the quote the receiving team has nothing to check.
// Evidence is still not *enforced* — see [Store].Insert.
func NeedsEvidence(k Kind) bool { return k != "" && k != KindMissingCapability }

// capabilityKeyPattern is the whole grammar: lowercase alphanumeric words
// joined by single dashes. Deliberately the DNS-label shape — a key names a
// CapabilityGap object in the API, and it is also the vocabulary the model is
// shown and asked to reuse, so it has to be a form the model reproduces
// character-for-character rather than approximately.
var capabilityKeyPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidateCapabilityKey checks a key against the grammar and
// MaxCapabilityKeyLen. The empty string is valid: a key is optional, and
// every row written before keys existed has none.
func ValidateCapabilityKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > MaxCapabilityKeyLen {
		return ErrCapabilityKeyTooLong
	}
	if !capabilityKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidCapabilityKey, key)
	}
	return nil
}

// Evidence quotes the tool output a non-MissingCapability report is about.
// Its fields hold TOOL OUTPUT and OBJECT STATE only — never text from the
// user's message; see the report_capability_gap tool description for why
// that line matters and where it is drawn.
type Evidence struct {
	// Tool is the tool whose output is at fault, e.g. "workloads_list".
	Tool string
	// Observed is what that tool returned, e.g. "actionability: transient".
	Observed string
	// ContradictedBy is the fact that makes Observed wrong, thin, or
	// impossible to act on, e.g. "instance unchanged for 9d".
	ContradictedBy string
}

// IsZero reports whether no evidence was supplied at all.
func (e Evidence) IsZero() bool { return e == Evidence{} }

// Report is one capability-gap report, attributed to the provider whose
// service fell short — not the consumer project the conversation ran in,
// which is carried only as provenance.
type Report struct {
	ID string
	// ProviderProject is the write key: the provider's own project
	// (spec.reportingProject on the capability document), where the
	// provider's team reviews reports.
	ProviderProject string
	// ServiceName is the provider service the gap belongs to (spec.serviceName).
	ServiceName string
	// ConsumerProject is the project the conversation happened in — provenance only.
	ConsumerProject string
	// ContextID is the conversation the gap arose in — provenance only.
	ContextID string
	// CapabilityKey names the gap in a form two conversations can agree on,
	// e.g. "workload-metrics". Capability is prose the model writes fresh
	// each time and no two writings of the same gap match; the key is what
	// makes occurrences of one gap group. Empty on rows written before keys
	// existed, and on any report filed without one.
	//
	// The model will eventually coin a synonym for a key it already has, so
	// merging has to stay possible. It is: because the key lives per
	// occurrence and grouping happens at read time, merging two keys is one
	// UPDATE ... SET capability_key over the losing key, and the next
	// Aggregate re-derives every count from the rows. Nothing has to be
	// reconciled, because nothing is precomputed — which is the second reason
	// this is not an upsert onto a counter.
	CapabilityKey string
	// Capability is a short description of the capability at fault, e.g.
	// "list pipelines for StreamCo".
	Capability string
	// Summary is what the user was trying to do.
	Summary string
	// Kind classifies the shortfall. Always set on a report returned by a
	// [Store]; an unset stored value reads back as KindMissingCapability.
	Kind Kind
	// Evidence quotes the offending tool output. Zero for most
	// MissingCapability reports, which have nothing to quote.
	Evidence  Evidence
	CreatedAt time.Time
}

// Aggregate is one distinct capability gap: every report filed against the
// same [Report].CapabilityKey for one service, collapsed into a single entry
// with a count of how many conversations hit it. It is the view a provider's
// team prioritises from; the occurrence rows behind it stay available through
// [Store].List and carry the per-occurrence evidence, which is the material
// that makes a quality defect diagnosable.
//
// It deliberately carries NO consumer identity — not ConsumerProject, not
// ContextID, not a per-customer breakdown. "How many conversations" is the
// prioritisation signal; "which of your customers" is a distinct, more
// legible cross-tenant profile that prioritisation does not need. The
// occurrence rows already show a provider the consumer project per report;
// this view simply declines to hand them the same thing pre-tabulated.
type Aggregate struct {
	// Key is the group's identity, and the name of the CapabilityGap object
	// the API projects from it. It is CapabilityKey when there is one, and
	// otherwise the single report's own ID — see [Store].Aggregate for why
	// keyless occurrences are never merged with each other.
	Key string
	// CapabilityKey is the model-coined key, empty for a group of one
	// keyless (pre-key, or filed-without-one) report.
	CapabilityKey string
	// ServiceName is the provider service the gap belongs to. Keys are
	// per-service: the same key filed against two services is two gaps.
	ServiceName string
	// Capability and Kind are taken from the most recent occurrence — the
	// freshest description of a gap that has been re-filed several times.
	Capability string
	Kind       Kind
	// Conversations counts DISTINCT ContextIDs, which is literally "how many
	// conversations hit this". Robust to one conversation filing twice.
	Conversations int
	// Occurrences counts reports, which can exceed Conversations.
	Occurrences int
	FirstSeen   time.Time
	LastSeen    time.Time
}

// InsertParams is the input to [Store].Insert. It is a struct rather than a
// positional argument list because every field is a string: providerProject,
// consumerProject, and contextID are mutually swappable at a call site with
// no compiler complaint, and a swap silently files a report into the wrong
// team's project.
type InsertParams struct {
	ProviderProject string
	ServiceName     string
	ConsumerProject string
	ContextID       string
	// CapabilityKey is optional but is what makes de-duplication work; see
	// [Report].CapabilityKey. Validated against [ValidateCapabilityKey].
	CapabilityKey string
	Capability    string
	Summary       string
	// Kind is optional; empty means KindMissingCapability.
	Kind Kind
	// Evidence is optional. Expected for every kind but MissingCapability,
	// though not required — see [Store].Insert.
	Evidence Evidence
}

// Store persists and lists capability-gap reports. Implementations must be
// safe for concurrent use.
type Store interface {
	// List returns a provider project's reports, newest first. An unknown
	// project yields nil, nil. This is the occurrence view: one row per
	// report filed, each with its own evidence.
	List(ctx context.Context, providerProject string) ([]Report, error)
	// Aggregate returns a provider project's distinct gaps, one entry per
	// (service, capability key), most-hit first — ordered by Conversations
	// descending, then LastSeen descending, then Key ascending so the result
	// is deterministic. An unknown project yields nil, nil.
	//
	// Reports with no capability key are NOT merged with each other: each
	// becomes its own single-occurrence entry keyed by its report ID. Free
	// prose is exactly what cannot tell us whether two of them are the same
	// gap — that is the problem keys exist to solve — so merging them would
	// be a guess presented as a count. They still appear, unmerged, so
	// nothing already filed drops out of the provider's view.
	Aggregate(ctx context.Context, providerProject string) ([]Aggregate, error)
	// CapabilityKeys returns the keys already filed against one service in
	// one provider project, most-hit first (same ordering as Aggregate),
	// capped at limit (<= 0 means no cap). Reports with no key contribute
	// nothing.
	//
	// It returns bare keys and nothing else, on purpose: its caller is
	// [github.com/milo-os/assistant/internal/capability.Compose], which
	// injects the result into a prompt running in SOME OTHER consumer's
	// conversation. A key is a bounded, charset-constrained slug naming the
	// provider's own capability; the report prose around it is not, and has
	// no business crossing into a different tenant's turn. The signature is
	// where that boundary is enforced.
	CapabilityKeys(ctx context.Context, providerProject, serviceName string, limit int) ([]string, error)
	// Insert records a new report, assigning ID and CreatedAt, and
	// defaulting an empty Kind to KindMissingCapability. It returns
	// ErrCapabilityTooLong, ErrSummaryTooLong, ErrCapabilityKeyTooLong,
	// ErrInvalidCapabilityKey, ErrEvidenceTooLong, ErrUnknownKind, or
	// ErrProjectFull if the input is invalid or a bound is violated; the
	// store is left unchanged.
	//
	// A malformed capability key is REJECTED, unlike missing evidence, which
	// is accepted: a key is mechanically fixable by the caller on the spot,
	// so the error round-trips into a corrected retry rather than losing the
	// report. An omitted key is fine — it just does not group.
	//
	// A kind that [NeedsEvidence] with no evidence is ACCEPTED, not
	// rejected: an under-evidenced report still tells the provider's team
	// which tool to look at, whereas rejecting it drops the signal entirely
	// and pushes the caller toward relabelling it MissingCapability — a
	// wrong classification is worse for the reader than a thin one. The
	// nudge belongs in the tool result, where the model can act on it.
	Insert(ctx context.Context, params InsertParams) (Report, error)
}

// normalize applies the shared input contract — kind defaulting and every
// length bound — so both stores accept and reject exactly the same inputs.
func normalize(p InsertParams) (InsertParams, error) {
	if len(p.Capability) > MaxCapabilityLen {
		return InsertParams{}, ErrCapabilityTooLong
	}
	if len(p.Summary) > MaxSummaryLen {
		return InsertParams{}, ErrSummaryTooLong
	}
	if err := ValidateCapabilityKey(p.CapabilityKey); err != nil {
		return InsertParams{}, err
	}
	if len(p.Evidence.Tool) > MaxEvidenceToolLen ||
		len(p.Evidence.Observed) > MaxEvidenceTextLen ||
		len(p.Evidence.ContradictedBy) > MaxEvidenceTextLen {
		return InsertParams{}, ErrEvidenceTooLong
	}
	kind, err := ParseKind(string(p.Kind))
	if err != nil {
		return InsertParams{}, err
	}
	p.Kind = kind
	return p, nil
}

// newReportID generates a report identifier, shared by MemoryStore and
// PostgresStore so both assign IDs the same way.
func newReportID() string { return uuid.NewString() }

// MemoryStore is an in-process [Store]. Reports live for the lifetime of
// the service process; [PostgresStore] is the durable equivalent behind
// the same interface.
type MemoryStore struct {
	mu      sync.Mutex
	reports map[string][]Report // providerProject -> reports
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{reports: make(map[string][]Report)}
}

// List implements [Store].
func (s *MemoryStore) List(_ context.Context, providerProject string) ([]Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reports := s.reports[providerProject]
	if len(reports) == 0 {
		return nil, nil
	}
	out := make([]Report, len(reports))
	copy(out, reports)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Insert implements [Store].
func (s *MemoryStore) Insert(_ context.Context, params InsertParams) (Report, error) {
	p, err := normalize(params)
	if err != nil {
		return Report{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports[p.ProviderProject]) >= MaxReportsPerProject {
		return Report{}, ErrProjectFull
	}
	r := Report{
		ID:              newReportID(),
		ProviderProject: p.ProviderProject,
		ServiceName:     p.ServiceName,
		ConsumerProject: p.ConsumerProject,
		ContextID:       p.ContextID,
		CapabilityKey:   p.CapabilityKey,
		Capability:      p.Capability,
		Summary:         p.Summary,
		Kind:            p.Kind,
		Evidence:        p.Evidence,
		CreatedAt:       time.Now(),
	}
	s.reports[p.ProviderProject] = append(s.reports[p.ProviderProject], r)
	return r, nil
}

// Aggregate implements [Store].
func (s *MemoryStore) Aggregate(_ context.Context, providerProject string) ([]Aggregate, error) {
	s.mu.Lock()
	reports := make([]Report, len(s.reports[providerProject]))
	copy(reports, s.reports[providerProject])
	s.mu.Unlock()
	return aggregateReports(reports), nil
}

// CapabilityKeys implements [Store].
func (s *MemoryStore) CapabilityKeys(ctx context.Context, providerProject, serviceName string, limit int) ([]string, error) {
	groups, err := s.Aggregate(ctx, providerProject)
	if err != nil {
		return nil, err
	}
	return keysFromAggregates(groups, serviceName, limit), nil
}

// aggregateReports is the in-memory form of the grouping [Store].Aggregate
// specifies; PostgresStore does the same thing in SQL. Keeping it here rather
// than in the test file means the two implementations are held to one written
// definition of the grouping, not two readings of a doc comment.
func aggregateReports(reports []Report) []Aggregate {
	type group struct {
		agg      Aggregate
		contexts map[string]struct{}
	}
	byKey := make(map[string]*group)
	var order []string
	for _, r := range reports {
		// A keyless report groups only with itself: see [Store].Aggregate.
		// Namespacing by service keeps one key from spanning two services.
		id := r.ServiceName + "\x00" + r.CapabilityKey
		if r.CapabilityKey == "" {
			id = r.ServiceName + "\x00\x00" + r.ID
		}
		g, ok := byKey[id]
		if !ok {
			g = &group{
				agg: Aggregate{
					Key:           r.CapabilityKey,
					CapabilityKey: r.CapabilityKey,
					ServiceName:   r.ServiceName,
					FirstSeen:     r.CreatedAt,
				},
				contexts: map[string]struct{}{},
			}
			if r.CapabilityKey == "" {
				g.agg.Key = r.ID
			}
			byKey[id] = g
			order = append(order, id)
		}
		g.contexts[r.ContextID] = struct{}{}
		g.agg.Occurrences++
		if r.CreatedAt.Before(g.agg.FirstSeen) {
			g.agg.FirstSeen = r.CreatedAt
		}
		// Capability and Kind describe the most recent occurrence, so they
		// move only when this report is at least as new as the current one.
		if g.agg.LastSeen.IsZero() || !r.CreatedAt.Before(g.agg.LastSeen) {
			g.agg.LastSeen = r.CreatedAt
			g.agg.Capability = r.Capability
			g.agg.Kind = r.Kind
			if g.agg.Kind == "" {
				g.agg.Kind = KindMissingCapability
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]Aggregate, 0, len(order))
	for _, id := range order {
		g := byKey[id]
		g.agg.Conversations = len(g.contexts)
		out = append(out, g.agg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Conversations != out[j].Conversations {
			return out[i].Conversations > out[j].Conversations
		}
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// keysFromAggregates narrows an aggregate list to one service's keys, in the
// order it was already sorted, dropping keyless groups (they have no key to
// reuse) and applying limit.
func keysFromAggregates(groups []Aggregate, serviceName string, limit int) []string {
	var out []string
	for _, g := range groups {
		if g.CapabilityKey == "" || g.ServiceName != serviceName {
			continue
		}
		out = append(out, g.CapabilityKey)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}
