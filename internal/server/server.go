// Package server wires the assistant's HTTP surface: GET /healthz, the public
// agent card at /.well-known/agent-card.json (+ the legacy /.well-known/agent.json
// alias), and the POST /a2a JSON-RPC endpoint. The A2A protocol itself (JSON-RPC
// framing, SSE, task store) is owned by a2a-go; this package adds only the mux,
// the auth middleware, and the dependency wiring.
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
)

// Deps are the fully-constructed dependencies of the HTTP app, so tests can
// inject fakes and boot wires the real graph.
type Deps struct {
	Config        *config.Config
	Logger        *slog.Logger
	Authenticator auth.Authenticator
	Authorizer    auth.Authorizer
	Runner        assistanta2a.AgentRunner
}

// New builds the assistant's HTTP handler from deps. The A2A JSON-RPC endpoint
// shares its task store with the auth middleware so authorization on GetTask and
// CancelTask can read the owning project off the stored task.
func New(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// A constant store user: per-user task filtering is unused (project
	// authorization is enforced by the auth middleware, not the store), so the
	// store just needs a non-empty name to satisfy Create.
	store := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: func(_ context.Context) (string, error) { return "assistant", nil },
	})

	executor := assistanta2a.NewExecutor(deps.Runner, logger)
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithTaskStore(store),
		a2asrv.WithLogger(logger),
		// Advertise streaming; leave push notifications unconfigured so the
		// push methods reject cleanly (a2a.ErrPushNotificationNotSupported),
		// matching the TS service's "push unimplemented" behavior.
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
	)
	jsonrpc := a2asrv.NewJSONRPCHandler(handler)

	card := a2asrv.NewStaticAgentCardHandler(assistanta2a.BuildAgentCard(deps.Config))

	mw := &authMiddleware{
		authenticator: deps.Authenticator,
		authorizer:    deps.Authorizer,
		taskStore:     store,
		logger:        logger,
		next:          jsonrpc,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// The card and health check are public: clients read the card to learn the
	// auth scheme before they hold a token.
	mux.Handle("GET /.well-known/agent-card.json", card)
	mux.Handle("GET /.well-known/agent.json", card) // legacy pre-1.0 well-known path
	mux.Handle("POST /a2a", mw)

	return mux
}
