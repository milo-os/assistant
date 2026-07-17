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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
	"github.com/milo-os/assistant/internal/server"
	"github.com/milo-os/assistant/internal/taskstore"
)

// shutdownGrace bounds the graceful-drain window: after a signal the server
// stops accepting new connections and lets in-flight requests — including
// long-lived SSE streams from SendStreamingMessage — finish within this budget
// before the process exits.
const shutdownGrace = 20 * time.Second

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
	authorizer, err := selectAuthorizer(cfg, log)
	if err != nil {
		return fmt.Errorf("failed to initialize authorizer: %w", err)
	}

	// Durable task store when a conversation store URL is configured (the two
	// share the same database); in-memory otherwise (dev). Fails boot on an
	// unreachable database — a service configured for durable tasks must not
	// silently forget them across restarts.
	var durableTasks *taskstore.PostgresStore
	taskStoreCleanup := func() {}
	if cfg.ConversationStoreURL != "" {
		durableTasks, err = taskstore.NewPostgresStore(ctx, cfg.ConversationStoreURL, log)
		if err != nil {
			return fmt.Errorf("failed to initialize task store: %w", err)
		}
		taskStoreCleanup = durableTasks.Close
	}

	runner, runnerCleanup, err := newAgentRunner(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("failed to initialize agent runner: %w", err)
	}
	defer runnerCleanup()
	defer taskStoreCleanup()

	deps := server.Deps{
		Config:        cfg,
		Logger:        log,
		Authenticator: authenticator,
		Authorizer:    authorizer,
		Runner:        runner,
		ReadyCheck:    readyCheck(cfg, durableTasks, log),
	}
	if durableTasks != nil {
		deps.TaskStore = durableTasks
	}
	app := server.New(deps)

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	srv := &http.Server{
		Addr:    addr,
		Handler: app,
		// ReadHeaderTimeout guards slowloris on headers; ReadTimeout bounds the
		// whole request read so a slow-drip body (paired with the middleware's
		// MaxBytesReader size cap) can't tie up a connection indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}

	log.Info("server.listening",
		"addr", addr,
		"publicBaseUrl", cfg.PublicBaseURL,
		"authMode", string(cfg.Auth.Mode),
		"authzMode", string(cfg.Auth.AuthzMode),
		"modelMode", string(cfg.Model.Mode),
		"durableTasks", durableTasks != nil,
		"capabilityDocsFixture", nullable(cfg.CapabilityDocsFixture),
		"capabilityProviderUrl", nullable(cfg.CapabilityProviderURL),
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
		log.Info("server.shutdown.begin", "grace", shutdownGrace.String())
		// srv.Shutdown stops accepting new connections and BLOCKS until every
		// in-flight request handler returns (or the grace deadline passes).
		// Because a request is what drives an agent turn, and the usage emitter
		// posts its CloudEvents synchronously WITHIN that turn (no async buffer),
		// draining in-flight requests inherently flushes usage — there is no
		// separate emitter queue to drain. Long-lived SSE streams count as
		// in-flight, so they finish (or are cut at the deadline) here.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown failed: %w", err)
		}
		log.Info("server.shutdown.done")
		return nil
	}
}

// selectAuthorizer chooses the authorizer by AUTHZ_MODE: "claims" (default)
// trusts the grants the credential carries; "sar" issues a SubjectAccessReview
// to the Milo control plane. The SAR call authenticates with the assistant's own
// service-account token/CA (read from the mounted paths), distinct from the
// principal under review.
func selectAuthorizer(cfg *config.Config, log *slog.Logger) (auth.Authorizer, error) {
	if cfg.Auth.AuthzMode != config.AuthzModeSAR {
		return auth.NewAuthorizer(cfg, log), nil
	}
	// Best-effort read of the mounted service-account credentials: absent files
	// leave the reviewer on system roots / no bearer token, which the SAR call
	// surfaces as a fail-closed deny rather than a boot failure.
	token, _ := os.ReadFile(cfg.Auth.SARTokenPath)
	caCert, _ := os.ReadFile(cfg.Auth.SARCACertPath)
	log.Info("authz.mode", "type", "sar", "apiUrl", cfg.Auth.SARAPIURL)
	return auth.NewSubjectAccessReviewAuthorizer(auth.SARConfig{
		APIURL:      cfg.Auth.SARAPIURL,
		BearerToken: string(token),
		CACert:      caCert,
		Group:       cfg.Auth.SARGroup,
		Resource:    cfg.Auth.SARResource,
		Verb:        cfg.Auth.SARVerb,
	})
}

// readyCheck composes the readiness probe: the durable task store must be
// reachable, and in gateway mode the model gateway must accept a TCP connection.
// A nil result (no durable store, non-gateway mode) means "no external
// dependencies" ⇒ always ready.
func readyCheck(cfg *config.Config, tasks *taskstore.PostgresStore, log *slog.Logger) func(context.Context) error {
	needGateway := cfg.Model.Mode == config.ModelModeGateway && cfg.Model.GatewayURL != ""
	if tasks == nil && !needGateway {
		return nil
	}
	return func(ctx context.Context) error {
		if tasks != nil {
			if err := tasks.Ping(ctx); err != nil {
				return err
			}
		}
		if needGateway {
			if err := dialGateway(ctx, cfg.Model.GatewayURL); err != nil {
				return fmt.Errorf("model gateway unreachable: %w", err)
			}
		}
		return nil
	}
}

// dialGateway is a cheap reachability signal: a bounded TCP connect to the
// gateway host:port. It does not issue a model call (no credentials, no cost) —
// a successful connection is enough to report the dependency reachable.
func dialGateway(ctx context.Context, gatewayURL string) error {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return fmt.Errorf("invalid GATEWAY_URL: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			host = net.JoinHostPort(u.Hostname(), "443")
		default:
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
