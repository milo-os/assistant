// Package history stores conversation turns so a follow-up message in the
// same A2A context is answered with the prior exchange in the prompt. The
// store keeps only what conversation memory needs — the user's text and the
// assistant's final answer per turn — never tool transcripts, which can be
// arbitrarily large and are re-derivable by calling the tool again.
//
// Keying is (projectName, contextID), not contextID alone: the project is the
// authorization boundary, so a caller who guesses another project's contextID
// must not inherit its history.
package history

import (
	"context"
	"sync"

	"github.com/milo-os/assistant/agentcore"
)

// Turn is one completed exchange: what the user said and what the assistant
// answered.
type Turn struct {
	UserText      string
	AssistantText string
}

// Store persists and recalls a conversation's turns. Implementations must be
// safe for concurrent use.
type Store interface {
	// Turns returns the conversation's turns, oldest first. A conversation
	// that has never been seen yields nil, nil.
	Turns(ctx context.Context, projectName, contextID string) ([]Turn, error)
	// Append records one completed turn at the end of the conversation.
	Append(ctx context.Context, projectName, contextID string, turn Turn) error
}

// MemoryStore is an in-process [Store]. History lives for the lifetime of the
// service process — enough for the playground and for proving the replay
// path; durability is the conversation-store slice.
type MemoryStore struct {
	mu    sync.Mutex
	turns map[storeKey][]Turn
}

type storeKey struct {
	project string
	context string
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{turns: make(map[storeKey][]Turn)}
}

// Turns implements [Store].
func (s *MemoryStore) Turns(_ context.Context, projectName, contextID string) ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.turns[storeKey{project: projectName, context: contextID}]
	if len(stored) == 0 {
		return nil, nil
	}
	out := make([]Turn, len(stored))
	copy(out, stored)
	return out, nil
}

// Append implements [Store].
func (s *MemoryStore) Append(_ context.Context, projectName, contextID string, turn Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	s.turns[key] = append(s.turns[key], turn)
	return nil
}

// Truncate drops the oldest turns until the estimated token cost of what
// remains fits budget. Turns are dropped whole (a user message without its
// answer, or vice versa, would mislead the model more than a shorter memory
// does). A budget <= 0 keeps nothing.
func Truncate(turns []Turn, budgetTokens int) []Turn {
	if budgetTokens <= 0 {
		return nil
	}
	total := 0
	// Walk backward accumulating cost; the first turn that overflows the
	// budget marks the cut.
	for i := len(turns) - 1; i >= 0; i-- {
		total += estimateTokens(turns[i])
		if total > budgetTokens {
			return turns[i+1:]
		}
	}
	return turns
}

// Messages renders turns as the alternating user/assistant prompt prefix.
func Messages(turns []Turn) []agentcore.Message {
	if len(turns) == 0 {
		return nil
	}
	msgs := make([]agentcore.Message, 0, 2*len(turns))
	for _, t := range turns {
		msgs = append(msgs, agentcore.UserMessage(t.UserText))
		msgs = append(msgs, agentcore.Message{
			Role:    agentcore.RoleAssistant,
			Content: []agentcore.ContentPart{agentcore.TextPart(t.AssistantText)},
		})
	}
	return msgs
}

// estimateTokens is the standard chars/4 heuristic — replayed history is a
// budget guard, not an exact bill (the provider reports the real count, which
// metering records).
func estimateTokens(t Turn) int {
	return (len(t.UserText) + len(t.AssistantText)) / 4
}
