package capability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// Source supplies the capability documents that apply to a project. The local
// slice uses [FixtureSource] (a JSON file exported from the control plane); a
// production HTTP-backed source is a follow-up behind this same seam.
type Source interface {
	// Documents returns the capability documents entitling projectName.
	Documents(ctx context.Context, projectName string) ([]CapabilityDocument, error)
}

// FixtureSource is a [Source] backed by a JSON file of capability documents —
// the output of a project-scoped export. The file path is injected (not read
// from the environment) so the type stays env-free and testable.
type FixtureSource struct {
	path   string
	logger *slog.Logger
}

// NewFixtureSource returns a fixture source reading path. A nil logger
// discards skip warnings.
func NewFixtureSource(path string, logger *slog.Logger) *FixtureSource {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &FixtureSource{path: path, logger: logger}
}

// Documents reads and parses the fixture file. The export is already
// project-scoped, so projectName does not filter here (that is the HTTP
// source's job). Individual documents that fail validation are skipped with a
// warning; a missing file or malformed root is an error.
func (s *FixtureSource) Documents(_ context.Context, _ string) ([]CapabilityDocument, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read capability documents fixture %s: %w", s.path, err)
	}
	docs, err := ParseDocuments(raw, func(index int, skipErr error) {
		s.logger.Warn("capability.fixture.entry_skipped",
			"path", s.path, "index", index, "error", skipErr.Error())
	})
	if err != nil {
		return nil, fmt.Errorf("parse capability documents fixture %s: %w", s.path, err)
	}
	return docs, nil
}
