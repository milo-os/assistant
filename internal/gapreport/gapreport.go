// Package gapreport stores capability-gap reports: durable records the
// assistant writes when a provider service is missing a tool or lookup a
// user needed, so the provider's own team — not the consumer project the
// conversation happened in — can act on it. Reports are keyed by
// providerProject (see [Report].ProviderProject), resolved from the
// composed capability document's spec.reportingProject, never from the
// conversation's project. This is an append-only log: unlike
// internal/memory, reports are written once via the report_capability_gap
// capability tool and never edited by the model.
package gapreport

import (
	"context"
	"errors"
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

// ErrProjectFull is returned by Insert when a provider project already
// holds MaxReportsPerProject reports. Losing a gap report silently would
// defeat the point of the feature, so this rejects instead of evicting —
// the calling tool surfaces the error to the model.
var ErrProjectFull = errors.New("gapreport: provider project exceeds MaxReportsPerProject")

// MaxCapabilityLen caps the short capability description in bytes.
const MaxCapabilityLen = 200

// MaxSummaryLen caps the summary in bytes.
const MaxSummaryLen = 1000

// MaxReportsPerProject caps how many reports a single provider project
// accumulates.
const MaxReportsPerProject = 500

// Report is one capability-gap report, attributed to the provider whose
// service was missing something — not the consumer project the
// conversation ran in, which is carried only as provenance.
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
	// Capability is a short description of what was missing, e.g.
	// "list pipelines for StreamCo".
	Capability string
	// Summary is what the user was trying to do.
	Summary   string
	CreatedAt time.Time
}

// Store persists and lists capability-gap reports. Implementations must be
// safe for concurrent use.
type Store interface {
	// List returns a provider project's reports, newest first. An unknown
	// project yields nil, nil.
	List(ctx context.Context, providerProject string) ([]Report, error)
	// Insert records a new report, assigning ID and CreatedAt. It returns
	// ErrCapabilityTooLong, ErrSummaryTooLong, or ErrProjectFull if a bound
	// is violated; the store is left unchanged.
	Insert(ctx context.Context, providerProject, serviceName, consumerProject, contextID, capability, summary string) (Report, error)
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
func (s *MemoryStore) Insert(_ context.Context, providerProject, serviceName, consumerProject, contextID, capability, summary string) (Report, error) {
	if len(capability) > MaxCapabilityLen {
		return Report{}, ErrCapabilityTooLong
	}
	if len(summary) > MaxSummaryLen {
		return Report{}, ErrSummaryTooLong
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports[providerProject]) >= MaxReportsPerProject {
		return Report{}, ErrProjectFull
	}
	r := Report{
		ID:              newReportID(),
		ProviderProject: providerProject,
		ServiceName:     serviceName,
		ConsumerProject: consumerProject,
		ContextID:       contextID,
		Capability:      capability,
		Summary:         summary,
		CreatedAt:       time.Now(),
	}
	s.reports[providerProject] = append(s.reports[providerProject], r)
	return r, nil
}
