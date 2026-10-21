package execapproval

import (
	"context"
	"crypto/sha256"
	"sync"
)

// Store atomically consumes one approved plan. A duplicate can never be sent
// to the provider, including when two requests race on different replicas.
type Store interface {
	Consume(ctx context.Context, project, planToken string) (bool, error)
}

// Key contains only a plan hash. A refreshed bearer token cannot make the
// same plan consumable again; the plan itself binds the original requester.
type Key struct {
	Project string
	Plan    [sha256.Size]byte
}

func MakeKey(project, planToken string) Key {
	return Key{Project: project, Plan: sha256.Sum256([]byte(planToken))}
}

// MemoryStore is for single-process tests and development only. Production
// must configure PostgresStore so all assistant replicas share the ledger.
type MemoryStore struct {
	mu   sync.Mutex
	used map[Key]struct{}
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{used: map[Key]struct{}{}} }

func (s *MemoryStore) Consume(_ context.Context, project, planToken string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := MakeKey(project, planToken)
	if _, ok := s.used[key]; ok {
		return false, nil
	}
	s.used[key] = struct{}{}
	return true, nil
}
