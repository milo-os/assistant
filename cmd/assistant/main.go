// Command assistant is the A2A runtime for the Datum AI Agent Framework — the
// Go port of the TypeScript assistant service. It loads configuration from the
// environment (same variables as the TS service, except CAPABILITY_DOCS_FIXTURE
// which replaces AGENT_BINDINGS_FIXTURE), wires the HTTP app, and serves it with
// graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
	"github.com/milo-os/assistant/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		// Config errors are user-facing: print the readable message, exit 1.
		return err
	}

	log := logger.New(logger.Level(cfg.LogLevel))

	// Boot-time context: used to prime the OIDC JWKS cache (if any) so a bad
	// issuer fails fast rather than on the first request.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authenticator, err := auth.NewAuthenticator(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("failed to initialize authenticator: %w", err)
	}
	authorizer := auth.NewAuthorizer(cfg, log)

	runner := newAgentRunner(cfg, log)

	app := server.New(server.Deps{
		Config:        cfg,
		Logger:        log,
		Authenticator: authenticator,
		Authorizer:    authorizer,
		Runner:        runner,
	})

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("server.listening",
		"addr", addr,
		"publicBaseUrl", cfg.PublicBaseURL,
		"authMode", string(cfg.Auth.Mode),
		"modelMode", string(cfg.Model.Mode),
		"capabilityDocsFixture", nullable(cfg.CapabilityDocsFixture),
		"usageGateway", nullable(cfg.Usage.GatewayURL),
	)

	// Serve until a signal arrives, then drain in-flight requests.
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		log.Info("server.shutdown.begin")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown failed: %w", err)
		}
		log.Info("server.shutdown.done")
		return nil
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
