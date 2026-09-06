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
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrCapabilityTooLong is returned by Insert when capability exceeds
// MaxCapabilityLen.
var ErrCapabilityTooLong = errors.New("gapreport: capability exceeds MaxCapabilityLen")

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
	Capability      string
	Summary         string
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
	// project yields nil, nil.
	List(ctx context.Context, providerProject string) ([]Report, error)
	// Insert records a new report, assigning ID and CreatedAt, and
	// defaulting an empty Kind to KindMissingCapability. It returns
	// ErrCapabilityTooLong, ErrSummaryTooLong, ErrEvidenceTooLong,
	// ErrUnknownKind, or ErrProjectFull if the input is invalid or a bound
	// is violated; the store is left unchanged.
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
		Capability:      p.Capability,
		Summary:         p.Summary,
		Kind:            p.Kind,
		Evidence:        p.Evidence,
		CreatedAt:       time.Now(),
	}
	s.reports[p.ProviderProject] = append(s.reports[p.ProviderProject], r)
	return r, nil
}
