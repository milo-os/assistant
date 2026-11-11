package agent

import (
	"log/slog"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/anthropic"
	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/agentcore/openaicompat"
	"github.com/milo-os/assistant/internal/config"
)

// ResolveModel builds the concrete [agentcore.Model] for a model configuration:
//
//   - mock:      the scripted in-process model (no secrets, no network).
//   - gateway:   the OpenAI-compatible adapter pointed at the Envoy AI Gateway,
//     with NO model credential (the gateway injects it). It may still
//     present GatewayTokenFile to prove which workload is calling,
//     where the gateway requires that.
//   - anthropic: the Anthropic Messages adapter over the configured API key.
//
// The gateway client deliberately uses Go's default HTTP transport and system
// root pool (honoring SSL_CERT_FILE / SSL_CERT_DIR): it sets no custom RootCAs
// and never disables verification. The token middleware refuses a plaintext
// request unless the dev escape hatch is on.
//
// The returned model's mode for attribution purposes is string(cfg.Mode);
// pass it as [Deps].ModelMode so gateway attribution headers are gated
// correctly.
func ResolveModel(cfg config.ModelConfig, logger *slog.Logger) (agentcore.Model, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	switch cfg.Mode {
	case config.ModelModeMock:
		logger.Info("model.resolved", "mode", "mock", "modelId", mockmodel.ModelID)
		return mockmodel.New(), nil

	case config.ModelModeGateway:
		logger.Info("model.resolved", "mode", "gateway", "modelId", cfg.GatewayModel, "baseURL", cfg.GatewayURL)
		return openaicompat.New(openaicompat.Options{
			ModelID:                           cfg.GatewayModel,
			BaseURL:                           cfg.GatewayURL,
			TokenFile:                         cfg.GatewayTokenFile,
			AllowInsecureCredentialForwarding: cfg.AllowInsecureCredentialForwarding,
		}), nil

	default: // anthropic
		logger.Info("model.resolved", "mode", "anthropic", "modelId", cfg.AnthropicModel)
		return anthropic.New(anthropic.Options{
			ModelID: cfg.AnthropicModel,
			APIKey:  cfg.AnthropicAPIKey,
		}), nil
	}
}
