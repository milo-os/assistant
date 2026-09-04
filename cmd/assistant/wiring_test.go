package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
)

// loadCfg loads a config with the control-plane endpoint every deployment
// needs. Tests that care about other settings pass them in env.
func loadCfg(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	full := map[string]string{
		"AUTHN_TOKENREVIEW_API_URL": "https://control-plane.test",
		"AUTHZ_SAR_API_URL":         "https://control-plane.test",
	}
	for k, v := range env {
		full[k] = v
	}
	env = full
	cfg, err := config.Load(config.MapGetenv(env))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestControlPlaneAuth pins that both seams are built from config and are the
// only implementations available: identity via TokenReview, access via
// SubjectAccessReview. Neither dials on construction, so this needs no cluster.
func TestControlPlaneAuth(t *testing.T) {
	cfg := loadCfg(t, nil)

	authn, err := auth.NewAuthenticator(context.Background(), cfg, logger.Silent())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if authn == nil {
		t.Fatal("nil authenticator")
	}

	authz, err := auth.NewAuthorizer(cfg, logger.Silent())
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	if authz == nil {
		t.Fatal("nil authorizer")
	}
}

// TestReadyCheck_NilWhenNoDeps pins that with no durable store and non-gateway
// model mode, readiness has nothing to wait on ⇒ nil check (always ready).
func TestReadyCheck_NilWhenNoDeps(t *testing.T) {
	cfg := loadCfg(t, map[string]string{"MODEL_MODE": "mock"})
	if rc := readyCheck(cfg, nil, logger.Silent()); rc != nil {
		t.Fatal("expected nil ready check when there are no dependencies")
	}
}

// TestReadyCheck_GatewayReachability pins the gateway readiness signal: a
// listening gateway is ready, a closed port is not.
func TestReadyCheck_GatewayReachability(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	upURL := "http://" + ln.Addr().String() + "/v1"

	cfg := loadCfg(t, map[string]string{
		"MODEL_MODE": "gateway", "GATEWAY_URL": upURL,
	})
	rc := readyCheck(cfg, nil, logger.Silent())
	if rc == nil {
		t.Fatal("expected a ready check in gateway mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rc(ctx); err != nil {
		t.Fatalf("gateway up should be ready: %v", err)
	}

	// Now point at a closed port: not ready.
	closed := freePort(t)
	cfgDown := loadCfg(t, map[string]string{
		"MODEL_MODE":  "gateway",
		"GATEWAY_URL": "http://127.0.0.1:" + closed + "/v1",
	})
	if err := readyCheck(cfgDown, nil, logger.Silent())(ctx); err == nil {
		t.Fatal("closed gateway port should report not ready")
	}
}
