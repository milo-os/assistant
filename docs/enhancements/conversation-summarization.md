---
status: implemented
---

# Conversation history summarization

> Design record. It describes the decision as it was taken; the shipped
> behavior is documented under [docs/architecture](../architecture/README.md).

## Context

`internal/history` replays a conversation's prior turns on every message in
the same `(project, contextId)`, so a follow-up is answered with context. The
replay is bounded by [`Truncate`](../../internal/history/history.go) — it walks
the stored turns newest-first, accumulating an estimated token cost, and cuts
at the first turn that would overflow `HistoryTokenBudget` (default 6000
tokens, see `DefaultHistoryTokenBudget` in `internal/agent/conversation.go`).
Turns are dropped **whole**, oldest first, on **every single turn** — nothing
is cached or persisted about what was dropped.

That's a correct, cheap policy for a conversation that never gets long. It's
a bad one for a conversation that does: once a `--tui`/`-i` session or a
long-running chat crosses the budget, its oldest turns don't degrade
gracefully — they vanish, silently, on every subsequent message, and the
model has no way to know a decision made 40 turns ago even happened. This is
a different problem from what `internal/memory` (project-scoped core memory,
shipped) solves: memory persists small, explicit, model-decided facts across
*every* conversation and user in a project; it does nothing for the raw
back-and-forth of *this* conversation once it outgrows the budget. The two
were evaluated together at memory's design time — see
`internal/memory`'s design history — and summarization was deliberately
scoped out there because it's inherently session-bound, which is exactly the
shape of the problem this doc addresses instead.

## Design

### 1. Compaction is triggered where truncation already happens

`Conversation.loadHistory` (`internal/agent/conversation.go`) already asks
"would replaying all stored turns overflow the budget?" via `Truncate`. Reuse
exactly that check as the compaction trigger, rather than inventing a
separate schedule:

- Fetch the conversation's full stored turns (`History.Turns`), same as
  today.
- **Trigger early, not at overflow.** Compact when the estimated token cost
  of stored turns crosses ~80% of `HistoryTokenBudget`, not when `Truncate`
  would actually have to drop something. This matches how provider-native
  and CLI-agent compaction converge in practice — Claude Code's own
  auto-compact fires at ~95% of its (much larger) context window rather than
  waiting for the hard limit — specifically so there's always headroom left
  and a turn is never caught needing to both compact *and* still overflow.
  Below that threshold, replay as today — no change, no extra cost, for the
  overwhelming majority of conversations that never approach the budget.
- When triggered, synchronously summarize the oldest batch of turns (see
  #3's fixed batch size, not "everything `Truncate` would otherwise drop")
  into one compact digest, persist it, and replay
  `[summary turn] + kept turns` for this request.
- On later turns, the store already reflects the compaction — fewer,
  smaller stored turns — so the trigger typically doesn't fire again until
  the conversation grows past the threshold once more. Compaction is
  self-limiting: it only fires when the budget is actually being approached,
  not on a fixed turn-count schedule.

### 2. Representing a summary as a turn

No new storage concept — a summary is stored as an ordinary `history.Turn`
with a fixed marker so replay and future compactions can recognize it:

```go
const summaryUserMarker = "[conversation summary]"

// A turn is a summary turn iff UserText == summaryUserMarker. AssistantText
// holds the digest itself.
```

This keeps `history.Store`'s shape untouched (`Turns`/`Append` unchanged) but
adds one new method both stores must implement:

```go
// Compact atomically replaces every stored turn with summary followed by
// keep, preserving keep's order. Used only by the summarization compaction
// step; never called from the normal chat Append path.
Compact(ctx context.Context, projectName, contextID string, summary Turn, keep []Turn) error
```

- `MemoryStore.Compact`: replace the slice under the existing mutex.
- `PostgresStore.Compact`: a transaction — delete the context's message rows,
  insert the summary as one message pair, then re-insert `keep` preserving
  relative order, all with fresh sequential `seq` values (the existing rows'
  `seq` cannot simply be reused once older rows are deleted from the middle
  of the sequence).

`Messages` (the apiserver read view) renders a summary turn like any other,
prefixed distinctly (e.g. `role: "summary"` instead of `"assistant"`) so
`patch conversations show` and `--tui`'s `/export` can display it as what it
is — a synthetic digest — not mistake it for something the assistant said to
the user mid-conversation.

### 3. Producing the summary

A plain, non-tool model completion — not a full `Conversation.Run` turn (no
capability composition, no tool loop, nothing user-facing). Reuses the same
injected `agentcore.Model`:

- Prompt: the turns being compacted, rendered the same way `history.Messages`
  already renders replay context, plus a fixed instruction to produce a
  compact third-person digest of what was discussed, decided, and any
  open threads — not a transcript, a summary.
- If an existing summary turn is already at the head of the stored turns
  (a conversation being compacted for the second+ time), it's included as
  the first thing to summarize *alongside* the newly-aging turns, so digests
  compound rather than being lost. This is "anchored iterative
  summarization" — the pattern 2026 production agent frameworks have
  converged on (fold the existing summary plus a bounded new batch each
  pass, never re-summarize the whole history at once) — not a one-shot
  summary that never updates again.
- **Fixed batch size, not "everything that would otherwise be dropped."**
  Summarize the oldest `SummaryBatchTurns` turns (a small constant, e.g.
  10-20) per compaction pass, keeping the rest verbatim. This mirrors the
  "hot layer" shape most 2026 agent context-management systems settle on —
  a bounded window of recent turns always kept at full fidelity, with only
  the aging tail ever compressed — and avoids a pathological case where a
  conversation's *first* compaction has to summarize a huge, unbounded span
  in one pass.
- Bounded output: cap the summary's stored length (`MaxSummaryTurnLen`,
  mirroring `internal/gapreport.MaxSummaryLen`'s posture) so a pathological
  digest can't itself become the next budget problem.

**Considered and declined: provider-native compaction.** Anthropic now
offers server-side context compaction (summarizing older turns
transparently within a session) for models accessed directly through their
API. `patch` runs against real Anthropic models in `dev-anthropic`/prod, so
this was weighed as an alternative to building our own pipeline. Declined
for now: it only covers `ModelMode == "anthropic"` (the mock and gateway
paths still need something), and a provider-side compaction isn't a
`history.Turn` we control — it can't be rendered by `patch conversations
show`/`--tui`'s `/export`, which read our own store, not the provider's
internal session state. Revisit if maintaining our own summarization step
becomes a real cost center; the design here keeps `summarize` as an
isolated, swappable step for exactly that reason.

### 4. Failure posture: fail open to today's behavior

If the summarization model call errors, times out, or produces something
that fails validation, **do not block the turn** — fall back to plain
`Truncate` for this request exactly as today, log it, and try compaction
again on a later turn. A user must never see a failed turn because history
maintenance failed; conversation memory degrading to today's baseline is
always an acceptable fallback, silently worsening it is not.

### 5. Wiring

- `internal/history`: `Store` gains `Compact`; both `MemoryStore` and
  `PostgresStore` implement it.
- `internal/agent/conversation.go`: `loadHistory` becomes compaction-aware
  per the trigger in #1; a new unexported `summarize` helper issues the
  plain model completion described in #3.
- `Deps` gains nothing new — compaction reuses `Deps.Model` and
  `Deps.History`, which already exist. A `Deps.SummarizationDisabled bool`
  (or similar) may be worth adding for a clean escape hatch (falls back to
  pure `Truncate`, today's behavior) without removing the History store
  entirely, e.g. for load-testing or if a customer wants zero synthetic
  model calls injected into their history.

### 6. Manual compaction (`/compact`)

Everything above is the *automatic* path: `maybeCompact` fires only once
`CompactionThresholdRatio` is crossed, and always fails open (#4) — a bad fit
for a user who explicitly wants to compact right now, e.g. right before a
long session to reclaim budget, or to confirm compaction works at all. That
needed a second, user-triggered entry point with the opposite failure
posture:

- `internal/agent/conversation.go` splits the actual summarize-then-persist
  work out of `maybeCompact` into an unexported `compactNow(ctx, params,
  turns)`, shared by both callers. `maybeCompact` keeps its threshold check
  and fail-open behavior around it; a new exported `Conversation.Compact(ctx,
  params) error` calls `compactNow` unconditionally and **never fails open**
  — the whole point of a manual command is that the user sees whether it
  actually worked. It returns the exported `ErrNothingToCompact` when there's
  no stored history, or the stored history is already a single summary turn
  (nothing left to fold), and a real error for any summarize/store failure or
  a summarization that didn't actually shrink the estimated token count.
- **Transport: a plain REST endpoint, not the A2A `/a2a` JSON-RPC surface.**
  `/a2a` is a message-turn protocol (`SendMessage`/`SendStreamingMessage`
  expect a task and an answer); compaction produces no answer, just a store
  mutation. `POST /v1/compact` (`internal/server/compact.go`) takes
  `{"contextId", "projectName"}` and reuses `/a2a`'s exact bearer-token authn
  and per-project authz (`authenticateBearer`, `Authorizer.AuthorizeProject`
  in `internal/server/middleware.go`) rather than inventing a second auth
  scheme, and drives the same `*agent.Conversation` the A2A executor runs
  turns against — via a new `assistanta2a.Compactor` interface
  (`internal/a2a/runner.go`) that `cmd/assistant`'s `conversationRunner`
  (already the `AgentRunner` wrapping that `*agent.Conversation`) also
  implements, so `cmd/assistant/main.go` just type-asserts the existing
  runner into `server.Deps.Compactor` instead of building a second
  orchestration instance.
- **Clients:** `cmd/patch`'s `patch compact --project <p> --context-id <c>`
  subcommand, and the chat `--tui`'s `/compact` slash command (same pattern
  as `/clear`/`/export`: reset the input, flip to the working/spinner state,
  run the request in a goroutine, report success/"nothing to
  compact"/failure as a transcript line). Both go through a shared
  `requestCompact` HTTP client helper (`cmd/patch/client.go`) using the same
  `bearerTransport`/`PATCH_URL`/`PATCH_TOKEN` plumbing every other `patch`
  command already uses — `/v1/compact` needed a plain `http.Client` call
  since it isn't reachable through the a2a-go client used for `/a2a`.

## Non-goals

- **Cross-conversation summarization.** Scope stays inside one `contextId`,
  same boundary `History` already enforces. Summarizing *across*
  conversations in a project is a different problem (much closer to the
  "archival memory" retrieval tier flagged as a follow-up when
  `internal/memory` was designed, not this one).
- **Summarizing tool-call transcripts.** `history.Turn` already excludes
  those (only user text / assistant final text is stored) — nothing changes
  here.
- **Auto-promoting summarized content into `internal/memory`.** A summary is
  a lossy digest of *what happened*; memory is a curated set of facts the
  model explicitly decided are durable. Blurring them would make memory's
  "explicit, model-decided, ask-before-overwrite" contract meaningless.
  Nothing here writes to memory; the model can still call `memory_remember`
  on its own if something in a long conversation is worth keeping forever.
- **A live progress indicator (e.g. "summarizing turn 3 of 15…") while a
  compaction is in flight**, beyond the plain working/spinner state
  `--tui`/`patch compact` already show for any in-flight request. The manual
  `/compact` command (see "Manual compaction" below) does have a
  user-visible trigger and result now — what's still out of scope is a
  step-by-step progress readout for a single (short) summarize call.

## Resolved design questions

These were open at first draft; resolved against how 2026 production agent
frameworks (Claude Code's own auto-compact, provider-native compaction APIs,
and the "anchored iterative summarization" / hierarchical-summarization
literature) have converged on this problem — see #1 and #3's designs above
for where each lands:

1. **Compaction latency on the hot path — trigger at ~80% of budget, not at
   overflow.** Resolved in #1: compacting early, before the budget is
   actually exceeded, is the pattern every reference point above shares
   (Claude Code compacts at ~95% of its window specifically to keep
   headroom). One extra model round trip on the rare turn that crosses the
   threshold is an acceptable cost; it should still be confirmed against
   `DefaultTurnTimeout` (120s) once built, but the *design* question — sync
   and early vs. some deferred/async scheme — is settled in favor of sync
   and early, matching industry practice rather than inventing a novel
   scheme.
2. **Compaction span size — fixed batch, not "everything that would be
   dropped."** Resolved in #3: a small fixed `SummaryBatchTurns` constant
   (10-20 turns) per pass, anchored against the prior summary each time.
   This is the "hot layer + hierarchical/anchored-iterative summarization"
   shape most 2026 systems use, and avoids the unbounded-first-compaction
   risk the original open question flagged.
3. **`PostgresStore.Compact`'s interaction with `MaxTurnsPerConversation`
   age-based deletion — compaction runs first, the row cap is a backstop
   only.** The store already drops the oldest raw rows past 1000 turns at
   append time (`internal/history/postgres.go`). This is a design
   *invariant*, not just an ordering preference: once compaction exists and
   fires per #1's early trigger, the row cap should essentially never bind
   in practice, because compaction keeps the live stored-turn count well
   below it. If the row cap is observed firing in production once this
   ships, that's a signal the compaction trigger threshold needs tuning, not
   that the two mechanisms need tighter coordination.

## Still open

- **Provider-native compaction was considered and declined for now** (see
  #3's callout) — revisit if maintaining `summarize` as custom code becomes
  a real cost.
- Whether `Deps.SummarizationDisabled` is worth adding at build time or only
  if a real need for the escape hatch shows up (load-testing, a customer
  wanting zero synthetic model calls in their history) — leaning toward
  building it from the start given how cheap the flag is, but not load-
  bearing to the rest of the design.

## Files touched/added

- `internal/history/history.go` — `Store.Compact`, `summaryUserMarker`,
  `MaxSummaryTurnLen`, `MemoryStore.Compact`.
- `internal/history/postgres.go` — `PostgresStore.Compact` (transactional
  delete + reinsert).
- `internal/history/*_test.go` — compaction conformance tests (mirroring the
  existing `MemoryStore`/`PostgresStore` conformance suite), summary-turn
  rendering, fail-open-on-summarization-error behavior.
- `internal/agent/conversation.go` — compaction-aware `loadHistory`,
  `summarize` helper, optional `Deps.SummarizationDisabled`.
- `internal/agent/*_test.go` — extended coverage: compaction fires only at
  the ~80% threshold (not before), respects the fixed batch size, fails open
  on a model error, compounds a second compaction correctly.
- `cmd/patch/gaps.go`-adjacent read-path files (`cmd/patch/chat.go` /
  `render.go`, `internal/apiserver/registry/conversation`) — render a summary
  turn distinctly wherever messages are displayed.
- `internal/agent/conversation.go` — `compactNow` extracted from
  `maybeCompact`, exported `Conversation.Compact`, `ErrNothingToCompact`
  (manual compaction, #6).
- `internal/agent/compact_test.go` — `Conversation.Compact` coverage: empty
  history, single-summary-turn, normal compaction, summarize failure.
- `internal/a2a/runner.go` — `CompactRequest`, `Compactor` interface,
  `ErrNothingToCompact` sentinel.
- `cmd/assistant/runner.go` — `conversationRunner.Compact` (adapts
  `agent.Conversation.Compact` to `assistanta2a.Compactor`).
- `cmd/assistant/main.go` — type-asserts the existing runner into
  `server.Deps.Compactor`.
- `internal/server/compact.go` — `POST /v1/compact` handler.
- `internal/server/middleware.go` — `authenticateBearer`/`writeAuthErrWith`
  extracted for reuse by `compact.go`.
- `internal/server/server.go` — `Deps.Compactor`, route registration.
- `internal/server/compact_test.go` — endpoint coverage: auth rejection,
  missing fields, success, `ErrNothingToCompact`, store/runner failure, nil
  compactor.
- `cmd/patch/args.go` — `patch compact` subcommand parsing + usage text.
- `cmd/patch/client.go` — `requestCompact`, `ErrNothingToCompact`.
- `cmd/patch/render.go` — `renderCompactResult`.
- `cmd/patch/run.go` — `cmdCompact` dispatch; `runChatTUI` gains
  `baseURL`/`token` params for `/compact`.
- `cmd/patch/chat_tui.go` — `/compact` slash command, `compactDoneMsg`,
  `chatModel.compact`, `commandNames`/`commandDescriptions`/`helpText`
  entries.
- `cmd/patch/args_test.go`, `cmd/patch/compact_test.go`,
  `cmd/patch/chat_tui_test.go` — CLI flag parsing, `requestCompact`/`Run`
  dispatch, and TUI state-transition coverage for `/compact`.
