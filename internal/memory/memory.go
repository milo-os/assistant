// Package memory stores durable project-scoped facts the assistant learns
// while working with a project — goals, conventions, decisions, standing
// constraints — so they persist across conversations and are visible to
// every user working on the same project. This is distinct from
// internal/history, which replays recent turns within one conversation:
// memory here is small, explicit (written only via the memory_remember /
// memory_forget capability tools), and keyed by projectName alone.
package memory

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrValueTooLong is returned by Upsert when value exceeds MaxFactValueLen.
var ErrValueTooLong = errors.New("memory: value exceeds MaxFactValueLen")

// ErrProjectFull is returned by Upsert when writing a brand-new key would
// exceed MaxFactsPerProject for that project. Losing a remembered fact
// silently is higher-stakes than losing an old chat turn, so unlike
// history's turn-count pruning this rejects instead of evicting — the
// calling tool surfaces the error to the model, which can ask the user to
// forget something first.
var ErrProjectFull = errors.New("memory: project fact count exceeds MaxFactsPerProject")

// MaxFactValueLen caps a single fact's value in bytes.
const MaxFactValueLen = 2000

// MaxFactsPerProject caps how many distinct keys a project's memory holds.
const MaxFactsPerProject = 200

// Fact is one remembered project-scoped key/value pair.
type Fact struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

// Store persists and recalls a project's facts. Implementations must be safe
// for concurrent use.
type Store interface {
	// List returns a project's facts, unordered. An unknown project yields
	// nil, nil.
	List(ctx context.Context, projectName string) ([]Fact, error)
	// Get returns one fact by key, or ok=false if it is not set.
	Get(ctx context.Context, projectName, key string) (fact Fact, ok bool, err error)
	// Upsert writes or replaces a fact. It returns ErrValueTooLong or
	// ErrProjectFull if a bound is violated; the store is left unchanged.
	Upsert(ctx context.Context, projectName, key, value string) error
	// Delete removes a fact. Deleting an unset key is a no-op, not an error.
	Delete(ctx context.Context, projectName, key string) error
}

// MemoryStore is an in-process [Store]. Facts live for the lifetime of the
// service process; [PostgresStore] is the durable equivalent behind the same
// interface.
type MemoryStore struct {
	mu    sync.Mutex
	facts map[string]map[string]Fact // projectName -> key -> Fact
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{facts: make(map[string]map[string]Fact)}
}

// List implements [Store].
func (s *MemoryStore) List(_ context.Context, projectName string) ([]Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project := s.facts[projectName]
	if len(project) == 0 {
		return nil, nil
	}
	out := make([]Fact, 0, len(project))
	for _, f := range project {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Get implements [Store].
func (s *MemoryStore) Get(_ context.Context, projectName, key string) (Fact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.facts[projectName][key]
	return f, ok, nil
}

// Upsert implements [Store].
func (s *MemoryStore) Upsert(_ context.Context, projectName, key, value string) error {
	if len(value) > MaxFactValueLen {
		return ErrValueTooLong
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	project := s.facts[projectName]
	if project == nil {
		project = make(map[string]Fact)
		s.facts[projectName] = project
	}
	if _, exists := project[key]; !exists && len(project) >= MaxFactsPerProject {
		return ErrProjectFull
	}
	project[key] = Fact{Key: key, Value: value, UpdatedAt: time.Now()}
	return nil
}

// Delete implements [Store].
func (s *MemoryStore) Delete(_ context.Context, projectName, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.facts[projectName], key)
	return nil
}
