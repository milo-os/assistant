package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/agent"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/usage"
)

// newAgentRunner builds the [assistanta2a.AgentRunner] the A2A executor drives:
// it resolves the model from config, wires the capability source, conversation
// store, and usage emitter, constructs the agent orchestrator, and adapts it
// to the A2A seam. The returned cleanup releases the conversation store's
// resources (call it on shutdown; it is never nil).
func newAgentRunner(ctx context.Context, cfg *config.Config, log *slog.Logger) (assistanta2a.AgentRunner, func(), error) {
	model, err := agent.ResolveModel(cfg.Model, log)
	if err != nil {
		return nil, nil, err
	}

	var persona string
	if cfg.PersonaPromptFile != "" {
		raw, err := os.ReadFile(cfg.PersonaPromptFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read persona prompt file %s: %w", cfg.PersonaPromptFile, err)
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
			return nil, nil, err
		}
		store, cleanup = pg, pg.Close
	} else {
		store = history.NewMemoryStore()
		log.Info("history.store", "type", "memory",
			"note", "CONVERSATION_STORE_URL not set — conversation history will not survive restarts")
	}

	conv := agent.New(agent.Deps{
		Model:                          model,
		ModelMode:                      string(cfg.Model.Mode),
		Source:                         source,
		Persona:                        persona,
		Emitter:                        emitter,
		History:                        store,
		AllowPrivateCapabilityNetworks: cfg.AllowPrivateCapabilityNetworks,
		Logger:                         log,
	})
	return conversationRunner{conv: conv}, cleanup, nil
}

// conversationRunner adapts an [agent.Conversation] to [assistanta2a.AgentRunner]:
// it drives the event stream, forwarding text deltas to the sink, then returns
// the terminal result. Tool-call events are not surfaced to the A2A layer (v0),
// matching the TS behavior.
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

	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A stream error still finalizes into a terminal Result below.
			break
		}
		if ev.Kind == agent.EventText && ev.Text != "" {
			sink.OnTextDelta(ev.Text)
		}
	}

	res := stream.Result()
	return assistanta2a.RunResult{
		State: assistanta2a.RunState(res.State),
		Text:  res.Text,
		Error: res.Error,
	}
}
