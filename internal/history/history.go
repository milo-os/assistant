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
	"fmt"
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

// ErrCompactRange is returned by [Store.Compact] when through names a turn
// position the conversation has not reached (or is negative). It means the
// caller's read and its compaction disagree about the conversation, and
// nothing was changed.
var ErrCompactRange = errors.New("compaction position outside the conversation")

// ErrCompactStale is returned by [Store.Compact] when the conversation has
// been compacted since the caller read the [Window] it computed through from.
// Compaction renumbers positions, so a through from the older numbering would
// name turns that were never summarized; the store refuses it and changes
// nothing. It means another compaction already ran.
var ErrCompactStale = errors.New("conversation compacted since the window was read")

// Turn is one completed exchange: what the user said and what the assistant
// answered.
type Turn struct {
	UserText      string
	AssistantText string
}

// Window is one read of a conversation's replay window: Turns oldest first
// (the same turns [Store.Turns] returns), Positions[i] the stored position of
// Turns[i], and Head, the position of the newest of them. Positions are
// 1-based turn numbers that only grow as turns are appended, so a turn
// appended after the read sits past Head. They are not always contiguous: a
// store may leave a turn out of the window (the Postgres store skips one with
// no text), so a caller naming turns by position uses Positions, not
// arithmetic from Head. Head is 0 when the window is empty.
//
// Compactions is how many times the conversation had been compacted when the
// window was read. Compaction renumbers positions, so [Store.Compact] takes it
// back and refuses ([ErrCompactStale]) a position from an older numbering.
type Window struct {
	Turns       []Turn
	Positions   []int64
	Head        int64
	Compactions int64
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
	// Window is Turns plus the stored position of the newest turn returned,
	// which is what a later Compact needs to name the turns it replaces.
	Window(ctx context.Context, projectName, contextID string) (Window, error)
	// Append records one completed turn at the end of the conversation.
	Append(ctx context.Context, projectName, contextID string, turn Turn) error
	// Compact atomically replaces every stored turn at a position up to and
	// including through with summary. Turns after through stay, in order,
	// after the summary — including turns appended since the caller read the
	// [Window] it computed through from, so a turn recorded while the summary
	// was being generated is never lost. through is 0 to replace nothing (the
	// summary is prepended), and past the conversation's newest position it
	// is [ErrCompactRange] with nothing changed. compactions is the
	// [Window.Compactions] of that same read: if the conversation has been
	// compacted since, through is in a numbering that no longer exists and
	// the call is [ErrCompactStale] with nothing changed. Positions are
	// renumbered densely from 1 afterwards and the compaction count goes up
	// by one. Used only by the summarization compaction step
	// (internal/agent); never called from the normal chat Append path.
	Compact(ctx context.Context, projectName, contextID string, summary Turn, through, compactions int64) error
	Renamer
}

// Archiver hides a conversation from default listings without destroying it,
// and brings it back. It is its own interface for the same reason [Renamer]
// is: the conversations apiserver's update path needs this one method, not
// the chat-path [Store].
//
// Archive is deliberately a flag rather than a move to somewhere else: an
// archived conversation keeps its key, its turns and its name, stays gettable
// and resumable by id, and continuing it ([Store.Append]) unarchives it — the
// user sending another message is the clearest possible signal they want it
// back in the list.
type Archiver interface {
	// SetArchived archives (archived=true) or unarchives the conversation. An
	// already-archived conversation keeps its original archive time, so
	// re-archiving is idempotent rather than a way to reorder the archive.
	// Like Rename it leaves last-active untouched. An unknown (project,
	// context) key yields [ErrConversationNotFound].
	SetArchived(ctx context.Context, projectName, contextID string, archived bool) error
}

// Deleter permanently removes a conversation and every message in it. There
// is no soft-delete and no undo — that is what [Archiver] is for — so callers
// that face a human are expected to confirm first.
type Deleter interface {
	// Delete removes the conversation. An unknown (project, context) key
	// yields [ErrConversationNotFound], so a delete that raced another delete
	// reports the truth instead of a silent success.
	Delete(ctx context.Context, projectName, contextID string) error
}

// Editor is everything the conversations apiserver does with the store: the
// read view plus the two lifecycle writes it serves (update of spec.archived,
// and delete). Spelled out so the apiserver's dependency stays narrower than
// the chat path's [Store] — it never appends, compacts or renames.
type Editor interface {
	Reader
	Archiver
	Deleter
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
	// archivedAt is when the conversation was archived; zero when it is not.
	archivedAt time.Time
	// head is the position of the newest stored turn, the counterpart of the
	// Postgres store's turn_count: it grows by one per Append, is untouched
	// by the retention trim, and Compact renumbers it. The stored slice holds
	// positions head-len+1 .. head.
	head int64
	// compactions counts successful Compact calls, the counterpart of the
	// Postgres store's compactions column.
	compactions int64
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
	_ Editor  = (*MemoryStore)(nil)
)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		turns: make(map[storeKey][]Turn),
		meta:  make(map[storeKey]*memoryMeta),
	}
}

// Turns implements [Store].
func (s *MemoryStore) Turns(ctx context.Context, projectName, contextID string) ([]Turn, error) {
	w, err := s.Window(ctx, projectName, contextID)
	return w.Turns, err
}

// Window implements [Store].
func (s *MemoryStore) Window(_ context.Context, projectName, contextID string) (Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	stored := s.turns[key]
	if len(stored) == 0 {
		return Window{}, nil
	}
	m := s.meta[key]
	out := make([]Turn, len(stored))
	copy(out, stored)
	positions := make([]int64, len(stored))
	for i := range positions {
		positions[i] = m.head - int64(len(stored)) + 1 + int64(i)
	}
	return Window{Turns: out, Positions: positions, Head: m.head, Compactions: m.compactions}, nil
}

// Append implements [Store]. Over-long turn text is truncated to
// MaxStoredContentLen, and a conversation retains at most
// MaxTurnsPerConversation turns (oldest dropped) so a long-lived process cannot
// grow without bound. A new turn unarchives the conversation (see [Archiver]).
func (s *MemoryStore) Append(_ context.Context, projectName, contextID string, turn Turn) error {
	turn = clampTurn(turn)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	now := time.Now()
	m, ok := s.meta[key]
	if !ok {
		m = &memoryMeta{createdAt: now}
		s.meta[key] = m
	}
	m.lastActiveAt = now
	m.archivedAt = time.Time{}
	m.head++
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

// Compact implements [Store.Compact] under the same mutex Append uses, so a
// turn appended before it is kept and one appended after it continues the
// renumbered sequence — the same outcome the Postgres store gets from its
// row lock. It touches lastActiveAt the way Append does (compaction is
// conversation activity, not a background sweep) but leaves the archive
// state alone: compaction can run on the service's own initiative, and a
// sweep the user never asked for must not undo an archive they did.
func (s *MemoryStore) Compact(_ context.Context, projectName, contextID string, summary Turn, through, compactions int64) error {
	summary = clampTurn(summary)

	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	m := s.meta[key]
	var head, done int64
	if m != nil {
		head, done = m.head, m.compactions
	}
	if compactions != done {
		return fmt.Errorf("history: compact from compaction %d of %d: %w", compactions, done, ErrCompactStale)
	}
	if through < 0 || through > head {
		return fmt.Errorf("history: compact through %d of %d: %w", through, head, ErrCompactRange)
	}
	now := time.Now()
	if m == nil {
		m = &memoryMeta{createdAt: now}
		s.meta[key] = m
	}

	// stored[i] sits at position first+i; drop the ones at or before through.
	stored := s.turns[key]
	first := head - int64(len(stored)) + 1
	drop := min(max(through-first+1, 0), int64(len(stored)))
	turns := make([]Turn, 0, 1+len(stored)-int(drop))
	turns = append(turns, summary)
	turns = append(turns, stored[drop:]...)

	m.head = head - through + 1
	m.compactions++
	m.lastActiveAt = now
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

// SetArchived implements [Archiver]. Like Rename it leaves lastActiveAt
// alone: filing a conversation away is not activity in it.
func (s *MemoryStore) SetArchived(_ context.Context, projectName, contextID string, archived bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.meta[storeKey{project: projectName, context: contextID}]
	if !ok {
		return ErrConversationNotFound
	}
	switch {
	case !archived:
		m.archivedAt = time.Time{}
	case m.archivedAt.IsZero():
		m.archivedAt = time.Now()
	}
	return nil
}

// Delete implements [Deleter]: the turns and the metadata go together, so
// nothing of the conversation — not even its name — outlives it.
func (s *MemoryStore) Delete(_ context.Context, projectName, contextID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{project: projectName, context: contextID}
	if _, ok := s.meta[key]; !ok {
		return ErrConversationNotFound
	}
	delete(s.meta, key)
	delete(s.turns, key)
	return nil
}

// ListConversations implements [Lister]: the project's conversations on the
// archived side opts selects, newest activity first. A limit <= 0 uses 100.
func (s *MemoryStore) ListConversations(_ context.Context, projectName string, opts ListOptions) ([]Conversation, error) {
	limit := opts.Limit
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
		if archived := !m.archivedAt.IsZero(); archived != opts.Archived {
			continue
		}
		out = append(out, Conversation{
			ProjectName:  key.project,
			ContextID:    key.context,
			CreatedAt:    m.createdAt,
			LastActiveAt: m.lastActiveAt,
			TurnCount:    int64(len(turns)),
			Title:        TitleOf(openingUserText(turns)),
			Name:         m.name,
			ArchivedAt:   m.archivedAt,
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
		ArchivedAt:   m.archivedAt,
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
