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
	"strings"
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

// MaxSummaryTurnLen caps how many bytes of digest a summary turn's
// AssistantText holds. A summarization pass exists precisely to make stored
// history smaller, so its output must be far more compact than
// MaxStoredContentLen (the general per-message guard) — mirrors
// [gapreport.MaxSummaryLen]'s posture of a small fixed cap, sized up from
// gapreport's because a conversation digest needs more room than a one-line
// capability-gap summary.
const MaxSummaryTurnLen = 4000

// summaryUserMarker is the fixed UserText of a summary turn — a synthesized
// compaction digest, never something a user actually typed. See
// [IsSummaryTurn] and [NewSummaryTurn].
const summaryUserMarker = "[conversation summary]"

// MaxTitleLen caps [Conversation.Title] in runes: long enough to read what a
// conversation was about in a list, short enough that a listing of a hundred
// conversations stays a small payload.
const MaxTitleLen = 120

// TitleOf folds an opening user message into a [Conversation.Title]: internal
// whitespace (including newlines) collapses to single spaces and the result
// is cut to MaxTitleLen runes with an ellipsis. Empty in, empty out.
func TitleOf(opening string) string {
	s := strings.Join(strings.Fields(opening), " ")
	r := []rune(s)
	if len(r) <= MaxTitleLen {
		return s
	}
	return string(r[:MaxTitleLen-1]) + "…"
}

// MaxNameLen caps [Conversation.Name] — the name a user gives a conversation
// with /rename — in runes. Shorter than MaxTitleLen because a name is meant to
// be scanned in a list, not read: it competes with the derived title for the
// same column.
const MaxNameLen = 80

// NormalizeName folds a user-supplied conversation name into what the stores
// keep: whitespace (including newlines) collapsed to single spaces and the
// result cut to MaxNameLen runes. Empty — or all whitespace — means "no name",
// which clears one that was set.
func NormalizeName(name string) string {
	s := strings.Join(strings.Fields(name), " ")
	r := []rune(s)
	if len(r) > MaxNameLen {
		return string(r[:MaxNameLen])
	}
	return s
}

// openingUserText returns the first ordinary (non-summary) turn's UserText,
// or "" if the conversation holds only a compaction digest.
func openingUserText(turns []Turn) string {
	for _, t := range turns {
		if !IsSummaryTurn(t) {
			return t.UserText
		}
	}
	return ""
}

// IsSummaryTurn reports whether t is a compaction digest (produced by
// conversation summarization) rather than an ordinary user/assistant
// exchange. AssistantText holds the digest itself.
func IsSummaryTurn(t Turn) bool {
	return t.UserText == summaryUserMarker
}

// NewSummaryTurn returns a summary turn holding digest, truncated to
// MaxSummaryTurnLen (backing off to a UTF-8 rune boundary) so a pathological
// digest cannot itself become the next budget problem. Callers should use
// this rather than constructing a Turn directly so every summary turn is
// recognized identically by [IsSummaryTurn].
func NewSummaryTurn(digest string) Turn {
	return Turn{UserText: summaryUserMarker, AssistantText: truncateBytes(digest, MaxSummaryTurnLen)}
}

// truncateBytes clamps s to at most max bytes, backing off to a UTF-8 rune
// boundary so a text column never receives a split rune.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// truncateContent clamps s to at most MaxStoredContentLen bytes.
func truncateContent(s string) string {
	return truncateBytes(s, MaxStoredContentLen)
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
	// Compact atomically replaces every stored turn with summary followed by
	// keep, preserving keep's order. Used only by the summarization
	// compaction step (internal/agent); never called from the normal chat
	// Append path.
	Compact(ctx context.Context, projectName, contextID string, summary Turn, keep []Turn) error
	Renamer
}

// Renamer sets a conversation's user-given name. It is spelled as its own
// interface, the way [Lister] and [Reader] are, so the HTTP layer's rename
// endpoint can depend on this one method instead of the whole chat-path
// [Store].
type Renamer interface {
	// Rename sets the conversation's name (see [Conversation.Name]), or
	// clears it when name normalizes to empty. An unknown (project, context)
	// key yields [ErrConversationNotFound] — a rename creates nothing.
	Rename(ctx context.Context, projectName, contextID, name string) error
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
	name         string
}

type storeKey struct {
	project string
	context string
}

var (
	_ Store   = (*MemoryStore)(nil)
	_ Lister  = (*MemoryStore)(nil)
	_ Reader  = (*MemoryStore)(nil)
	_ Renamer = (*MemoryStore)(nil)
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

// Compact implements [Store.Compact]: replaces the conversation's turn slice
// under the same mutex Append uses, and touches lastActiveAt the same way
// Append does — compaction is conversation activity, not a background sweep.
func (s *MemoryStore) Compact(_ context.Context, projectName, contextID string, summary Turn, keep []Turn) error {
	summary = clampTurn(summary)
	turns := make([]Turn, 0, 1+len(keep))
	turns = append(turns, summary)
	turns = append(turns, keep...)

	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	now := time.Now()
	if m, ok := s.meta[key]; ok {
		m.lastActiveAt = now
	} else {
		s.meta[key] = &memoryMeta{createdAt: now, lastActiveAt: now}
	}
	s.turns[key] = turns
	return nil
}

// Rename implements [Renamer]. It deliberately leaves lastActiveAt alone:
// naming a conversation says something about it, it is not a turn in it, and
// reordering the picker under the user's cursor would be a surprise.
func (s *MemoryStore) Rename(_ context.Context, projectName, contextID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.meta[storeKey{project: projectName, context: contextID}]
	if !ok {
		return ErrConversationNotFound
	}
	m.name = NormalizeName(name)
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
			Title:        TitleOf(openingUserText(turns)),
			Name:         m.name,
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
		Title:        TitleOf(openingUserText(s.turns[key])),
		Name:         m.name,
	}, nil
}

// Messages implements [Reader]. Each ordinary stored turn expands to a user
// message and an assistant message; a summary turn (see [IsSummaryTurn])
// renders as a single message with Role "summary" instead — its UserText is
// the internal compaction marker, not something a human said, so pairing it
// with a synthetic user row would misrepresent it as part of the transcript.
// seq is assigned per emitted message (1 for a summary turn, 2 for an
// ordinary turn), so it stays a dense, monotonically increasing 1-based index
// regardless of how many summary turns a conversation has accumulated;
// createdAt is the conversation's last-active time since the memory store
// keeps no per-message timestamp (the durable store does).
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
	seq := int64(0)
	for _, t := range turns {
		if IsSummaryTurn(t) {
			seq++
			out = append(out, Message{Seq: seq, Role: "summary", Content: t.AssistantText, CreatedAt: ts})
			continue
		}
		seq++
		out = append(out, Message{Seq: seq, Role: "user", Content: t.UserText, CreatedAt: ts})
		seq++
		out = append(out, Message{Seq: seq, Role: "assistant", Content: t.AssistantText, CreatedAt: ts})
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

// EstimateTokens sums [estimateTokens]'s heuristic across turns. Exposed so
// callers that need to reason about replay cost before it's time to actually
// call [Truncate] (the summarization compaction trigger, in particular) share
// the exact same heuristic Truncate uses rather than reimplementing it.
func EstimateTokens(turns []Turn) int {
	total := 0
	for _, t := range turns {
		total += estimateTokens(t)
	}
	return total
}
