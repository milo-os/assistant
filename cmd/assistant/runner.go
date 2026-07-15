package main

import (
	"context"
	"log/slog"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/config"
)

// newAgentRunner builds the [assistanta2a.AgentRunner] the A2A executor drives.
//
// SWAP POINT: the agent-orchestration layer (internal/agent, owned by the core
// engineer) provides the real runner — it composes capability documents, runs
// the model/tool loop via agentcore, and emits usage events through
// internal/usage. Until that lands, this returns a stub so the binary boots with
// the full env contract (health check and agent card work; A2A calls fail
// cleanly). Replace the body with the real wiring — e.g.:
//
//	emitter := usage.NewEmitter(usage.EmitterConfig{
//	    GatewayURL: cfg.Usage.GatewayURL, APIKey: cfg.Usage.GatewayAPIKey,
//	    Source: cfg.PublicBaseURL + "/a2a", Logger: log,
//	})
//	return agent.New(cfg, log, emitter) // implements assistanta2a.AgentRunner
func newAgentRunner(_ *config.Config, log *slog.Logger) assistanta2a.AgentRunner {
	log.Warn("agent.runner.stub",
		"reason", "internal/agent not yet wired; A2A runs will fail until the orchestration layer is connected")
	return stubRunner{}
}

// stubRunner is a placeholder [assistanta2a.AgentRunner] used until the real
// orchestration layer is wired in [newAgentRunner].
type stubRunner struct{}

func (stubRunner) Run(_ context.Context, _ assistanta2a.RunRequest, _ assistanta2a.RunSink) assistanta2a.RunResult {
	return assistanta2a.RunResult{
		State: assistanta2a.RunFailed,
		Error: "agent orchestration layer not wired (stub runner)",
	}
}
