package main

import (
	"context"
	"log/slog"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/agentwiring"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/history"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
)

// newAgentRunner builds this binary's [assistanta2a.AgentRunner] via the
// shared construction path in internal/agentwiring — the same one
// cmd/assistant-apiserver uses for the conversations/sendmessage subresource,
// so the two binaries never drift in how they build agent.Conversation. See
// [agentwiring.NewRunner] for the wiring itself.
func newAgentRunner(ctx context.Context, cfg *config.Config, log *slog.Logger, metrics *appmetrics.Metrics) (assistanta2a.AgentRunner, history.Store, func(), error) {
	return agentwiring.NewRunner(ctx, cfg, log, metrics)
}
