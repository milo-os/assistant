package agent

import (
	"testing"

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
