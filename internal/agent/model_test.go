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
	m, err := ResolveModel(config.ModelConfig{Mode: config.ModelModeGateway, GatewayModel: "patch-stub-v1", GatewayURL: "https://gw"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelID() != "patch-stub-v1" {
		t.Fatalf("gateway model id = %q", m.ModelID())
	}
}
