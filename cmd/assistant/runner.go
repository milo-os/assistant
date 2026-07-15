package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/anthropic"
	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/agentcore/openaicompat"
	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/agent"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/usage"
)

// newAgentRunner builds the [assistanta2a.AgentRunner] the A2A executor drives:
// it resolves the model from config, wires the capability source and usage
// emitter, constructs the agent orchestrator, and adapts it to the A2A seam.
func newAgentRunner(cfg *config.Config, log *slog.Logger) (assistanta2a.AgentRunner, error) {
	model, err := resolveModel(cfg)
	if err != nil {
		return nil, err
	}

	var source capability.Source
	if cfg.CapabilityDocsFixture != "" {
		source = capability.NewFixtureSource(cfg.CapabilityDocsFixture, log)
		log.Info("agent.capability.source", "type", "fixture", "path", cfg.CapabilityDocsFixture)
	} else {
		log.Warn("agent.capability.source",
			"type", "none",
			"reason", "CAPABILITY_DOCS_FIXTURE unset — no provider capabilities will be composed")
	}

	emitter := usage.NewEmitter(usage.EmitterConfig{
		GatewayURL: cfg.Usage.GatewayURL,
		APIKey:     cfg.Usage.GatewayAPIKey,
		Source:     cfg.PublicBaseURL + "/a2a",
		Logger:     log,
	})

	conv := agent.New(agent.Deps{
		Model:     model,
		ModelMode: string(cfg.Model.Mode),
		Source:    source,
		Emitter:   emitter,
		Logger:    log,
	})
	return conversationRunner{conv: conv}, nil
}

// resolveModel selects and constructs the agentcore model from config. The
// gateway path sends no upstream credential (the gateway injects it) and may
// use a custom CA or skip TLS verification for local development.
func resolveModel(cfg *config.Config) (agentcore.Model, error) {
	switch cfg.Model.Mode {
	case config.ModelModeMock:
		return mockmodel.New(), nil

	case config.ModelModeAnthropic:
		return anthropic.New(anthropic.Options{
			ModelID: cfg.Model.AnthropicModel,
			APIKey:  cfg.Model.AnthropicAPIKey,
		}), nil

	case config.ModelModeGateway:
		httpClient, err := gatewayHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
		// APIKey deliberately empty: the Envoy AI Gateway's BackendSecurityPolicy
		// injects the upstream key, so the service sends no Authorization header.
		return openaicompat.New(openaicompat.Options{
			ModelID:    cfg.Model.GatewayModel,
			BaseURL:    cfg.Model.GatewayURL,
			HTTPClient: httpClient,
		}), nil

	default:
		return nil, fmt.Errorf("unsupported MODEL_MODE %q", cfg.Model.Mode)
	}
}

// gatewayHTTPClient builds the HTTP client for gateway mode, honoring the
// optional custom CA (GATEWAY_CA_CERT) and TLS-skip (GATEWAY_TLS_INSECURE)
// env settings. It returns nil (the SDK default client) when neither is set.
func gatewayHTTPClient(cfg *config.Config) (*http.Client, error) {
	if cfg.Model.GatewayCACert == "" && !cfg.Model.GatewayTLSInsecure {
		return nil, nil
	}
	// #nosec G402 -- InsecureSkipVerify is gated behind an explicit env flag
	// for local development against a self-signed gateway; never set in prod.
	tlsConfig := &tls.Config{InsecureSkipVerify: cfg.Model.GatewayTLSInsecure}
	if cfg.Model.GatewayCACert != "" {
		pem, err := os.ReadFile(cfg.Model.GatewayCACert)
		if err != nil {
			return nil, fmt.Errorf("read GATEWAY_CA_CERT %q: %w", cfg.Model.GatewayCACert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("GATEWAY_CA_CERT %q: no valid certificates found", cfg.Model.GatewayCACert)
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}

// conversationRunner adapts an [agent.Conversation] to [assistanta2a.AgentRunner]:
// it drives the event stream, forwarding text deltas to the sink, then returns
// the terminal result. Tool-call events are not surfaced to the A2A layer (v0),
// matching the TS behavior.
type conversationRunner struct {
	conv *agent.Conversation
}

func (r conversationRunner) Run(ctx context.Context, req assistanta2a.RunRequest, sink assistanta2a.RunSink) assistanta2a.RunResult {
	stream := r.conv.Run(ctx, agent.Params{
		UserText:    req.UserText,
		ProjectName: req.ProjectName,
		ContextID:   req.ContextID,
		TaskID:      req.TaskID,
	})
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A stream error still finalizes into a terminal Result below.
			break
		}
		if ev.Kind == agent.EventText && ev.Text != "" {
			sink.OnTextDelta(ev.Text)
		}
	}

	res := stream.Result()
	return assistanta2a.RunResult{
		State: assistanta2a.RunState(res.State),
		Text:  res.Text,
		Error: res.Error,
	}
}
