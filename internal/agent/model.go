package agent

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"

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
//     where the gateway requires that. Custom CA / insecure TLS from
//     config is honored via the HTTP client.
//   - anthropic: the Anthropic Messages adapter over the configured API key.
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
		httpClient, err := gatewayHTTPClient(cfg, logger)
		if err != nil {
			return nil, err
		}
		logger.Info("model.resolved", "mode", "gateway", "modelId", cfg.GatewayModel, "baseURL", cfg.GatewayURL)
		return openaicompat.New(openaicompat.Options{
			ModelID:    cfg.GatewayModel,
			BaseURL:    cfg.GatewayURL,
			TokenFile:  cfg.GatewayTokenFile,
			HTTPClient: httpClient,
		}), nil

	default: // anthropic
		logger.Info("model.resolved", "mode", "anthropic", "modelId", cfg.AnthropicModel)
		return anthropic.New(anthropic.Options{
			ModelID: cfg.AnthropicModel,
			APIKey:  cfg.AnthropicAPIKey,
		}), nil
	}
}

// gatewayHTTPClient builds an HTTP client for the gateway only when a custom CA
// or insecure TLS is configured (local dev). Over plain HTTP or default TLS it
// returns nil so the adapter uses its default client.
func gatewayHTTPClient(cfg config.ModelConfig, logger *slog.Logger) (*http.Client, error) {
	if cfg.GatewayCACert == "" && !cfg.GatewayTLSInsecure {
		return nil, nil
	}

	tlsConfig := &tls.Config{}
	if cfg.GatewayCACert != "" {
		pem, err := os.ReadFile(cfg.GatewayCACert)
		if err != nil {
			return nil, fmt.Errorf("read GATEWAY_CA_CERT %s: %w", cfg.GatewayCACert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("GATEWAY_CA_CERT %s: no valid certificates found", cfg.GatewayCACert)
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.GatewayTLSInsecure {
		logger.Warn("model.gateway.tls_insecure",
			"reason", "GATEWAY_TLS_INSECURE set — gateway TLS verification disabled (local only)")
		tlsConfig.InsecureSkipVerify = true
	}

	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}
