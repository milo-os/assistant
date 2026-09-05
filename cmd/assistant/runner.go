package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/agent"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/gapreport"
	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/memory"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	"github.com/milo-os/assistant/internal/usage"
)

// newAgentRunner builds the [assistanta2a.AgentRunner] the A2A executor drives:
// it resolves the model from config, wires the capability source, conversation
// store, and usage emitter, constructs the agent orchestrator, and adapts it
// to the A2A seam. metrics is shared with server.Deps.Metrics by the caller so
// conversation/tool/model/compaction/gap-report telemetry lands on the same
// /metrics endpoint as the HTTP metrics. The returned cleanup releases the
// conversation store's resources (call it on shutdown; it is never nil).
//
// The conversation store is returned alongside the runner because the HTTP
// layer needs it directly for POST /v1/conversations/rename — naming a
// conversation is a row update with no agent in it, so routing it through the
// runner seam (the way compaction is) would be a category error.
func newAgentRunner(ctx context.Context, cfg *config.Config, log *slog.Logger, metrics *appmetrics.Metrics) (assistanta2a.AgentRunner, history.Store, func(), error) {
	model, err := agent.ResolveModel(cfg.Model, log)
	if err != nil {
		return nil, nil, nil, err
	}

	var persona string
	if cfg.PersonaPromptFile != "" {
		raw, err := os.ReadFile(cfg.PersonaPromptFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read persona prompt file %s: %w", cfg.PersonaPromptFile, err)
		}
		persona = string(raw)
		log.Info("agent.persona.source", "type", "file", "path", cfg.PersonaPromptFile)
	} else {
		log.Info("agent.persona.source", "type", "default")
	}

	// Source selection (fixture and provider URL are mutually exclusive — the
	// config loader rejects setting both).
	var source capability.Source
	switch {
	case cfg.CapabilityProviderURL != "":
		source = capability.NewHTTPSource(cfg.CapabilityProviderURL, nil, log)
		log.Info("agent.capability.source", "type", "http", "url", cfg.CapabilityProviderURL)
	case cfg.CapabilityDocsFixture != "":
		source = capability.NewFixtureSource(cfg.CapabilityDocsFixture, log)
		log.Info("agent.capability.source", "type", "fixture", "path", cfg.CapabilityDocsFixture)
	default:
		log.Warn("agent.capability.source",
			"type", "none",
			"reason", "neither CAPABILITY_PROVIDER_URL nor CAPABILITY_DOCS_FIXTURE set — no provider capabilities will be composed")
	}

	emitter := usage.NewEmitter(usage.EmitterConfig{
		GatewayURL: cfg.Usage.GatewayURL,
		APIKey:     cfg.Usage.GatewayAPIKey,
		Source:     cfg.PublicBaseURL + "/a2a",
		Logger:     log,
	})

	// StepLimit and MaxOutputTokens are left at zero: the agent layer applies
	// the TS-parity defaults (step limit 8, MaxOutputTokens 4096) — that policy
	// lives in internal/agent, where the TS agent/loop.ts had it.
	// Conversation memory: durable (Postgres) when CONVERSATION_STORE_URL is
	// set, in-process otherwise. A follow-up message with the same A2A
	// contextId gets the prior turns replayed into its prompt either way; the
	// Postgres store additionally survives restarts and is scoped by
	// (project, contextId) at the query layer.
	var (
		store   history.Store
		cleanup = func() {}
	)
	if cfg.ConversationStoreURL != "" {
		pg, err := history.NewPostgresStore(ctx, cfg.ConversationStoreURL, log)
		if err != nil {
			return nil, nil, nil, err
		}
		store, cleanup = pg, pg.Close
	} else {
		store = history.NewMemoryStore()
		log.Info("history.store", "type", "memory",
			"note", "CONVERSATION_STORE_URL not set — conversation history will not survive restarts")
	}

	// Project memory (memory_remember/memory_forget): same database as
	// conversation history, a separate table. Durable when
	// CONVERSATION_STORE_URL is set, in-process otherwise — same fallback
	// shape as history, just for project-scoped facts instead of per-turn
	// replay.
	var mem memory.Store
	if cfg.ConversationStoreURL != "" {
		pg, err := memory.NewPostgresStore(ctx, cfg.ConversationStoreURL, log)
		if err != nil {
			return nil, nil, nil, err
		}
		mem = pg
		prevCleanup := cleanup
		cleanup = func() { prevCleanup(); pg.Close() }
	} else {
		mem = memory.NewMemoryStore()
		log.Info("memory.store", "type", "memory",
			"note", "CONVERSATION_STORE_URL not set — project memory will not survive restarts")
	}

	// Capability-gap reports (report_capability_gap__<service>): same database
	// as history/memory, a separate table, same durable-vs-in-process
	// fallback. Unlike memory this is keyed by the PROVIDER's own project
	// (spec.reportingProject on the capability document), never by the
	// conversation's project — see internal/gapreport.
	var gaps gapreport.Store
	if cfg.ConversationStoreURL != "" {
		pg, err := gapreport.NewPostgresStore(ctx, cfg.ConversationStoreURL, log)
		if err != nil {
			return nil, nil, nil, err
		}
		gaps = pg
		prevCleanup := cleanup
		cleanup = func() { prevCleanup(); pg.Close() }
	} else {
		gaps = gapreport.NewMemoryStore()
		log.Info("gapreport.store", "type", "memory",
			"note", "CONVERSATION_STORE_URL not set — capability-gap reports will not survive restarts")
	}

	conv := agent.New(agent.Deps{
		Model:                          model,
		ModelMode:                      string(cfg.Model.Mode),
		Source:                         source,
		Persona:                        persona,
		Emitter:                        emitter,
		History:                        store,
		Memory:                         mem,
		GapReports:                     gaps,
		AllowPrivateCapabilityNetworks: cfg.AllowPrivateCapabilityNetworks,
		Logger:                         log,
		Metrics:                        metrics,
	})
	return conversationRunner{conv: conv}, store, cleanup, nil
}

// conversationRunner adapts an [agent.Conversation] to [assistanta2a.AgentRunner]:
// it drives the event stream, forwarding text deltas and tool activity to the
// sink, then returns the terminal result.
type conversationRunner struct {
	conv *agent.Conversation
}

func (r conversationRunner) Run(ctx context.Context, req assistanta2a.RunRequest, sink assistanta2a.RunSink) assistanta2a.RunResult {
	stream := r.conv.Run(ctx, agent.Params{
		UserText:    req.UserText,
		ProjectName: req.ProjectName,
		ContextID:   req.ContextID,
		TaskID:      req.TaskID,
	})
	defer stream.Close()

	activity := newToolActivityTracker()
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A stream error still finalizes into a terminal Result below.
			break
		}
		switch ev.Kind {
		case agent.EventText:
			if ev.Text != "" {
				sink.OnTextDelta(ev.Text)
			}
		case agent.EventToolCall, agent.EventToolResult:
			activity.forward(ev, sink)
		}
	}

	res := stream.Result()
	return assistanta2a.RunResult{
		State: assistanta2a.RunState(res.State),
		Text:  res.Text,
		Error: res.Error,
	}
}

// toolActivityTracker turns the run loop's tool-call/tool-result event pair
// into the sink's started/finished callbacks, timing the gap between them.
//
// Elapsed is measured from the moment the model asked for the tool rather than
// from the loop's own execution start (which it does not report): that is also
// what the user is waiting on, and the difference is the tail of one model
// stream.
type toolActivityTracker struct {
	started map[string]time.Time
	// names remembers the tool name per call id, since providers may omit the
	// name on the result.
	names map[string]string
}

func newToolActivityTracker() *toolActivityTracker {
	return &toolActivityTracker{started: map[string]time.Time{}, names: map[string]string{}}
}

func (t *toolActivityTracker) forward(ev agent.Event, sink assistanta2a.RunSink) {
	key := ev.ToolCallID
	if key == "" {
		key = ev.ToolName // providers that assign no id: one call per name in flight
	}
	if ev.Kind == agent.EventToolCall {
		t.started[key] = time.Now()
		t.names[key] = ev.ToolName
		sink.OnToolStart(assistanta2a.ToolActivity{
			ID:      ev.ToolCallID,
			Name:    ev.ToolName,
			Summary: assistanta2a.SummarizeToolInput(ev.ToolInput),
		})
		return
	}

	name := ev.ToolName
	if name == "" {
		name = t.names[key]
	}
	var elapsed time.Duration
	if start, ok := t.started[key]; ok {
		elapsed = time.Since(start)
	}
	delete(t.started, key)
	delete(t.names, key)
	sink.OnToolFinish(assistanta2a.ToolActivity{
		ID:      ev.ToolCallID,
		Name:    name,
		OK:      !ev.ToolFailed,
		Elapsed: elapsed,
	})
}

// Compact implements [assistanta2a.Compactor] over the same [agent.Conversation]
// this runner drives Run turns against, so manual "/compact" and the
// automatic threshold-triggered path in internal/agent operate on identical
// wiring. agent.ErrNothingToCompact is translated to the a2a-layer sentinel so
// internal/server can recognize the case without importing internal/agent.
func (r conversationRunner) Compact(ctx context.Context, req assistanta2a.CompactRequest) error {
	err := r.conv.Compact(ctx, agent.Params{
		ProjectName: req.ProjectName,
		ContextID:   req.ContextID,
	})
	if errors.Is(err, agent.ErrNothingToCompact) {
		return assistanta2a.ErrNothingToCompact
	}
	return err
}
