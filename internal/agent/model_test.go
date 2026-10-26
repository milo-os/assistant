package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/internal/config"
)

func TestResolveModel_Mock(t *testing.T) {
	m, err := ResolveModel(config.ModelConfig{Mode: config.ModelModeMock}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelID() != mockmodel.ModelID {
		t.Fatalf("mock model id = %q", m.ModelID())
	}
}

func TestResolveModel_Anthropic(t *testing.T) {
	m, err := ResolveModel(config.ModelConfig{Mode: config.ModelModeAnthropic, AnthropicModel: "claude-x", AnthropicAPIKey: "sk"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelID() != "claude-x" {
		t.Fatalf("anthropic model id = %q", m.ModelID())
	}
}

func TestResolveModel_Gateway(t *testing.T) {
	m, err := ResolveModel(config.ModelConfig{Mode: config.ModelModeGateway, GatewayModel: "patch-stub-v1", GatewayURL: "http://gw"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelID() != "patch-stub-v1" {
		t.Fatalf("gateway model id = %q", m.ModelID())
	}
}

func TestResolveModel_GatewayInsecureTLSBuildsClient(t *testing.T) {
	// Insecure TLS should resolve without error and yield a usable model.
	m, err := ResolveModel(config.ModelConfig{
		Mode: config.ModelModeGateway, GatewayModel: "patch-stub-v1",
		GatewayURL: "https://gw", GatewayTLSInsecure: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("nil model")
	}
}

func TestResolveModel_GatewayBadCACertErrors(t *testing.T) {
	_, err := ResolveModel(config.ModelConfig{
		Mode: config.ModelModeGateway, GatewayURL: "https://gw", GatewayCACert: "/nonexistent/ca.pem",
	}, nil)
	if err == nil {
		t.Fatal("expected an error for a missing CA cert")
	}
}

// GATEWAY_URL ends in /v1 for the OpenAI client, which appends
// /chat/completions. The Anthropic client appends /v1/messages itself, so the
// same value must not produce /v1/v1/messages.
func TestResolveModel_GatewayAnthropicSchema(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("workload-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotPath, gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAPIKey = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	m, err := ResolveModel(config.ModelConfig{
		Mode:             config.ModelModeGateway,
		GatewaySchema:    config.GatewaySchemaAnthropic,
		GatewayModel:     "claude-sonnet-5",
		GatewayURL:       srv.URL + "/v1/",
		GatewayTokenFile: tokenPath,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelID() != "claude-sonnet-5" {
		t.Fatalf("gateway model id = %q", m.ModelID())
	}

	s, err := m.Stream(context.Background(), agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := s.Recv(); err != nil {
			break
		}
	}
	_ = s.Close()

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotAuth != "Bearer workload-token" {
		t.Errorf("Authorization = %q, want the workload token", gotAuth)
	}
	if gotAPIKey != "" {
		t.Errorf("no model credential may be sent, got x-api-key=%q", gotAPIKey)
	}
}
