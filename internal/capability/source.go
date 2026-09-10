package capability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
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
	// platform parses the file as PLATFORM capability documents — see
	// [ParsePlatformDocuments] and [PlatformSource].
	platform bool
}

// NewFixtureSource returns a fixture source reading path. A nil logger
// discards skip warnings.
func NewFixtureSource(path string, logger *slog.Logger) *FixtureSource {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &FixtureSource{path: path, logger: logger}
}

// NewPlatformFixtureSource returns a fixture source whose documents are
// PLATFORM capabilities: composed into every project, parsed with the
// catalog-only fields optional. Pass it to [NewPlatformSource].
func NewPlatformFixtureSource(path string, logger *slog.Logger) *FixtureSource {
	s := NewFixtureSource(path, logger)
	s.platform = true
	return s
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
	onSkip := func(index int, skipErr error) {
		s.logger.Warn("capability.fixture.entry_skipped",
			"path", s.path, "index", index, "error", skipErr.Error())
	}
	parse := ParseDocuments
	if s.platform {
		parse = ParsePlatformDocuments
	}
	docs, err := parse(raw, onSkip)
	if err != nil {
		return nil, fmt.Errorf("parse capability documents fixture %s: %w", s.path, err)
	}
	return docs, nil
}

// PlatformSource composes the platform's own capabilities into every project.
//
// Not every service that an assistant needs is a catalog service. Where a
// service is offered and what a project's allowance has left are records the
// platform itself keeps, published by services that are platform
// infrastructure: nothing entitles a project to them, no catalog entry exists
// to express them, and every project needs them. So the platform OPERATOR
// declares them to this service directly — a fixture file or a platform
// provider URL — and this source composes them alongside whatever the project
// is separately entitled to.
//
// Three properties make that work:
//
//   - Platform documents come FIRST. Tool registration is first-wins on a name
//     collision, so a project-scoped document can never shadow a platform tool
//     with one of its own.
//   - They carry no namespace. [markPlatform] clears it, so [ScopeDocuments]
//     keeps them for whichever project is asking instead of dropping them as
//     another tenant's.
//   - They are unmetered by default. See [ComposeOptions.UnmeteredServices]:
//     a project did not choose a platform capability, so it is not billed a
//     tool invocation for one.
//
// A failure in either source degrades to that source contributing nothing,
// logged: the platform provider being unreachable must not cost a project the
// capabilities it is entitled to, and the reverse holds just as much.
type PlatformSource struct {
	platform Source
	project  Source
	logger   *slog.Logger
	logOnce  sync.Once
}

// NewPlatformSource returns a source serving platform documents ahead of
// project's. Either may be nil: a deployment can run on platform capabilities
// alone, or (the default) on project ones alone.
func NewPlatformSource(platform, project Source, logger *slog.Logger) *PlatformSource {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &PlatformSource{platform: platform, project: project, logger: logger}
}

// Documents returns the platform documents followed by projectName's own.
func (s *PlatformSource) Documents(ctx context.Context, projectName string) ([]CapabilityDocument, error) {
	var docs []CapabilityDocument

	if s.platform != nil {
		platformDocs, err := s.platform.Documents(ctx, projectName)
		if err != nil {
			s.logger.Warn("capability.platform.load_failed",
				"projectName", projectName, "error", err.Error())
		}
		for i := range platformDocs {
			// Defensive: the platform constructors already did this at parse
			// time. Doing it again here means a source wired up without them
			// still cannot contribute a namespaced or a metered document.
			markPlatform(&platformDocs[i])
		}
		s.logPlatformServices(platformDocs)
		docs = append(docs, platformDocs...)
	}

	if s.project != nil {
		projectDocs, err := s.project.Documents(ctx, projectName)
		if err != nil {
			// Degrade rather than propagate: the platform capabilities above
			// are still good, and returning an error here would discard them
			// along with the failure.
			s.logger.Warn("capability.project.load_failed",
				"projectName", projectName, "error", err.Error())
			return docs, nil
		}
		docs = append(docs, projectDocs...)
	}
	return docs, nil
}

// logPlatformServices records, once, which services reach every project this
// way. It fires on the first fetch rather than at construction because a
// platform provider URL is only readable over the network, on a request — for
// a fixture that is the first conversation, for an HTTP source the first one
// after it became reachable.
func (s *PlatformSource) logPlatformServices(docs []CapabilityDocument) {
	if len(docs) == 0 {
		return
	}
	s.logOnce.Do(func() {
		names := make([]string, 0, len(docs))
		for _, doc := range docs {
			names = append(names, doc.Spec.ServiceName)
		}
		sort.Strings(names)
		s.logger.Info("capability.platform.services",
			"services", names,
			"note", "composed into every project regardless of entitlement, and not metered as tool invocations")
	})
}
