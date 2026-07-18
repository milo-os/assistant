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
	"errors"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/milo-os/assistant/agentcore"
)

// ErrConversationNotFound is returned by [Reader.GetConversation] when no
// conversation exists for the (project, context) key. Callers (the apiserver
// read view) map it to a 404.
var ErrConversationNotFound = errors.New("conversation not found")

// Message is a single stored message row, exposed to the read view
// (apiserver messages subresource). seq is the absolute 1-based message index
// within the conversation.
type Message struct {
	Seq       int64
	Role      string
	Content   string
	CreatedAt time.Time
}

// Reader is the read-only view over stored conversations, consumed by the
// conversations apiserver. It is separate from [Store] (the chat hot path) and
// [Lister] so the apiserver depends only on what it needs. Both [MemoryStore]
// and [PostgresStore] implement it.
type Reader interface {
	Lister
	// GetConversation returns one conversation's metadata, or
	// [ErrConversationNotFound] if the (project, context) key is unknown.
	GetConversation(ctx context.Context, projectName, contextID string) (Conversation, error)
	// Messages returns a conversation's messages, oldest first (ascending seq).
	// An unknown conversation yields nil, nil.
	Messages(ctx context.Context, projectName, contextID string) ([]Message, error)
}

// Turn is one completed exchange: what the user said and what the assistant
// answered.
type Turn struct {
	UserText      string
	AssistantText string
}

// MaxStoredContentLen caps how many bytes of a single user/assistant message
// any store persists. Conversation memory truncates by token budget on replay
// (see Truncate), so this is not a functional limit — it is a storage guard so
// one pathological message (a pasted file, a runaway generation) cannot balloon
// a row or an in-memory slice without bound. Both stores enforce it identically
// so a conversation reads back the same whether durable or in-process.
const MaxStoredContentLen = 32 * 1024

// MaxTurnsPerConversation caps how many turns any one conversation retains.
// Replay only ever reads the newest DefaultMaxRecentTurns (200), so a cap well
// above that keeps far more than memory needs while bounding growth: the
// Postgres store deletes older message pairs at append time, and the memory
// store drops older turns. Fleet-level age-based retention is a follow-up.
const MaxTurnsPerConversation = 1000

// truncateContent clamps s to at most MaxStoredContentLen bytes, backing off to
// a UTF-8 rune boundary so a text column never receives a split rune.
func truncateContent(s string) string {
	if len(s) <= MaxStoredContentLen {
		return s
	}
	cut := MaxStoredContentLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// clampTurn returns turn with its texts truncated to MaxStoredContentLen.
func clampTurn(turn Turn) Turn {
	turn.UserText = truncateContent(turn.UserText)
	turn.AssistantText = truncateContent(turn.AssistantText)
	return turn
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

// MemoryStore is an in-process [Store] and [Lister]. History lives for the
// lifetime of the service process; [PostgresStore] is the durable equivalent
// behind the same interfaces.
type MemoryStore struct {
	mu    sync.Mutex
	turns map[storeKey][]Turn
	meta  map[storeKey]*memoryMeta
}

type memoryMeta struct {
	createdAt    time.Time
	lastActiveAt time.Time
}

type storeKey struct {
	project string
	context string
}

var (
	_ Store  = (*MemoryStore)(nil)
	_ Lister = (*MemoryStore)(nil)
	_ Reader = (*MemoryStore)(nil)
)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		turns: make(map[storeKey][]Turn),
		meta:  make(map[storeKey]*memoryMeta),
	}
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

// Append implements [Store]. Over-long turn text is truncated to
// MaxStoredContentLen, and a conversation retains at most
// MaxTurnsPerConversation turns (oldest dropped) so a long-lived process cannot
// grow without bound.
func (s *MemoryStore) Append(_ context.Context, projectName, contextID string, turn Turn) error {
	turn = clampTurn(turn)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	now := time.Now()
	if m, ok := s.meta[key]; ok {
		m.lastActiveAt = now
	} else {
		s.meta[key] = &memoryMeta{createdAt: now, lastActiveAt: now}
	}
	turns := append(s.turns[key], turn)
	if len(turns) > MaxTurnsPerConversation {
		// Copy the kept tail into a fresh slice so the dropped turns' text is
		// released — re-slicing alone would pin the whole backing array.
		trimmed := make([]Turn, MaxTurnsPerConversation)
		copy(trimmed, turns[len(turns)-MaxTurnsPerConversation:])
		turns = trimmed
	}
	s.turns[key] = turns
	return nil
}

// ListConversations implements [Lister]: the project's conversations, newest
// activity first. limit <= 0 uses 100.
func (s *MemoryStore) ListConversations(_ context.Context, projectName string, limit int) ([]Conversation, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Conversation
	for key, turns := range s.turns {
		if key.project != projectName {
			continue
		}
		m := s.meta[key]
		out = append(out, Conversation{
			ProjectName:  key.project,
			ContextID:    key.context,
			CreatedAt:    m.createdAt,
			LastActiveAt: m.lastActiveAt,
			TurnCount:    int64(len(turns)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActiveAt.After(out[j].LastActiveAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetConversation implements [Reader].
func (s *MemoryStore) GetConversation(_ context.Context, projectName, contextID string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	m, ok := s.meta[key]
	if !ok {
		return Conversation{}, ErrConversationNotFound
	}
	return Conversation{
		ProjectName:  projectName,
		ContextID:    contextID,
		CreatedAt:    m.createdAt,
		LastActiveAt: m.lastActiveAt,
		TurnCount:    int64(len(s.turns[key])),
	}, nil
}

// Messages implements [Reader]. Each stored turn expands to a user message
// (seq 2k-1) and an assistant message (seq 2k); createdAt is the
// conversation's last-active time since the memory store keeps no per-message
// timestamp (the durable store does).
func (s *MemoryStore) Messages(_ context.Context, projectName, contextID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	turns := s.turns[key]
	if len(turns) == 0 {
		return nil, nil
	}
	ts := time.Time{}
	if m, ok := s.meta[key]; ok {
		ts = m.lastActiveAt
	}
	out := make([]Message, 0, 2*len(turns))
	for i, t := range turns {
		seq := int64(2 * i)
		out = append(out,
			Message{Seq: seq + 1, Role: "user", Content: t.UserText, CreatedAt: ts},
			Message{Seq: seq + 2, Role: "assistant", Content: t.AssistantText, CreatedAt: ts},
		)
	}
	return out, nil
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
