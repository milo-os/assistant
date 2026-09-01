package main

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/milo-os/assistant/internal/config"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
)

func loadTestConfig(t *testing.T, extra map[string]string) *config.Config {
	t.Helper()
	env := map[string]string{
		"AUTHN_TOKENREVIEW_API_URL": "https://control-plane.test",
		"AUTHZ_SAR_API_URL":         "https://control-plane.test",
	}
	maps.Copy(env, extra)
	cfg, err := config.Load(config.MapGetenv(env))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestNewAgentRunner_PersonaPromptFileMissing(t *testing.T) {
	cfg := loadTestConfig(t, map[string]string{"PERSONA_PROMPT_FILE": filepath.Join(t.TempDir(), "missing.md")})
	_, _, err := newAgentRunner(context.Background(), cfg, slog.New(slog.DiscardHandler), appmetrics.New())
	if err == nil {
		t.Fatal("want error for missing persona prompt file, got nil")
	}
}

func TestNewAgentRunner_PersonaPromptFileRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persona.md")
	if err := os.WriteFile(path, []byte("You are Acme's helper.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadTestConfig(t, map[string]string{"PERSONA_PROMPT_FILE": path})
	_, cleanup, err := newAgentRunner(context.Background(), cfg, slog.New(slog.DiscardHandler), appmetrics.New())
	if err != nil {
		t.Fatalf("newAgentRunner: %v", err)
	}
	cleanup()
}
