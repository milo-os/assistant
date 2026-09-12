package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/capability"
)

type composedVerdict struct {
	project, name string
	err           error
}

func observerFor(t *testing.T, log *slog.Logger, got *[]composedVerdict) func(capability.CapabilityDocument, error) {
	t.Helper()
	return observerForProject(t, log, got, "demo-project")
}

func observerForProject(t *testing.T, log *slog.Logger, got *[]composedVerdict, project string) func(capability.CapabilityDocument, error) {
	t.Helper()
	conv := New(Deps{
		Logger: log,
		ObserveComposedBinding: func(projectName, bindingName string, composeErr error) {
			*got = append(*got, composedVerdict{projectName, bindingName, composeErr})
		},
	})
	return conv.composeObserver(Params{ProjectName: project, TaskID: "task-1"})
}

// The writer keys on (project, name). A document with no metadata has neither,
// and passing it through would PATCH .../capabilitybindings//status — a 404
// against an object that does not exist, once per turn, forever.
func TestComposeObserver_SkipsDocumentWithoutBindingIdentity(t *testing.T) {
	var buf bytes.Buffer
	var got []composedVerdict
	observe := observerFor(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &got)

	observe(capability.CapabilityDocument{}, nil)                                                  // nil Metadata pointer
	observe(capability.CapabilityDocument{Metadata: &capability.Metadata{Name: ""}}, nil)          // empty name
	observe(capability.CapabilityDocument{Metadata: &capability.Metadata{Namespace: "demo"}}, nil) // namespace is not a name

	if len(got) != 0 {
		t.Fatalf("nameless documents must produce no status write, got %+v", got)
	}
	if strings.Count(buf.String(), "compose_status_skipped") != 3 {
		t.Fatalf("every skip must be visible; logs:\n%s", buf.String())
	}
}

// The empty-project hazard, pinned. A CapabilityBinding is cluster-scoped, so
// doc.Metadata.Namespace is ALWAYS empty; if the project were ever derived from
// it again, every status PATCH would be addressed to a control-plane path for no
// project and conditions would silently stop appearing. The project must be the
// turn's, and a turn without one must write nothing at all.
func TestComposeObserver_ProjectComesFromTheTurnNotTheDocument(t *testing.T) {
	var got []composedVerdict
	observe := observerFor(t, slog.New(slog.DiscardHandler), &got)

	// Production shape: a name, and no namespace whatsoever.
	observe(capability.CapabilityDocument{Metadata: &capability.Metadata{Name: "streamco-binding"}}, nil)
	if len(got) != 1 || got[0].project != "demo-project" || got[0].name != "streamco-binding" {
		t.Fatalf("verdict must be keyed by the turn's project: %+v", got)
	}

	// A document that somehow carries a namespace must not redirect the write.
	observe(capability.CapabilityDocument{Metadata: &capability.Metadata{Name: "b", Namespace: "other-tenant"}}, nil)
	if len(got) != 2 || got[1].project != "demo-project" {
		t.Fatalf("a document must never choose the project written to: %+v", got)
	}

	// No project on the turn is no key at all: refuse rather than PATCH nowhere.
	var buf bytes.Buffer
	var unkeyed []composedVerdict
	observeUnkeyed := observerForProject(t,
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &unkeyed, "")
	observeUnkeyed(capability.CapabilityDocument{Metadata: &capability.Metadata{Name: "streamco-binding"}}, nil)
	if len(unkeyed) != 0 {
		t.Fatalf("a projectless turn must write no status, got %+v", unkeyed)
	}
	if !strings.Contains(buf.String(), "compose_status_skipped") {
		t.Fatalf("the skip must be visible; logs:\n%s", buf.String())
	}
}

func TestComposeObserver_ForwardsVerdictsForBothOutcomes(t *testing.T) {
	var got []composedVerdict
	observe := observerFor(t, slog.New(slog.DiscardHandler), &got)

	boom := errors.New(`mcp server "streamco": connect to http://gateway/mcp failed`)
	observe(capability.CapabilityDocument{Metadata: &capability.Metadata{Name: "streamco"}}, boom)
	observe(capability.CapabilityDocument{Metadata: &capability.Metadata{Name: "acme"}}, nil)

	if len(got) != 2 || got[0] != (composedVerdict{"demo-project", "streamco", boom}) {
		t.Fatalf("verdicts = %+v", got)
	}
	if got[1].err != nil || got[1].name != "acme" {
		t.Fatalf("successes must be forwarded too: %+v", got[1])
	}
}

// Nil (fixture/HTTP mode) must leave the hook off entirely, so composition is
// byte-for-byte what it was before the status feedback loop existed.
func TestComposeObserver_NilDependencyDisablesTheHook(t *testing.T) {
	conv := New(Deps{})
	if conv.composeObserver(Params{ProjectName: "demo-project"}) != nil {
		t.Fatal("want a nil hook when no observer is wired")
	}
}
