package patchcli

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// echoExecutor answers every turn with a fixed text and records the
// (contextID, userText) of each turn it served, so tests can assert how the
// CLI threads the conversation id across turns.
type echoExecutor struct {
	mu    sync.Mutex
	turns []struct{ contextID, text string }
}

func (e *echoExecutor) record(ec *a2asrv.ExecutorContext) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = append(e.turns, struct{ contextID, text string }{ec.ContextID, textOf(ec.Message.Parts)})
}

func (e *echoExecutor) seen() []struct{ contextID, text string } {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]struct{ contextID, text string }(nil), e.turns...)
}

func (e *echoExecutor) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	e.record(ec)
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("echo: "+textOf(ec.Message.Parts))), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *echoExecutor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// replIo is a capture whose ReadLine plays back scripted input lines.
type replIo struct {
	capture
	lines []string
	next  int
}

func (r *replIo) ReadLine() (string, bool) {
	if r.next >= len(r.lines) {
		return "", false
	}
	line := r.lines[r.next]
	r.next++
	return line, true
}

func TestRun_OneShotChatPrintsContextHint(t *testing.T) {
	exec := &echoExecutor{}
	base := newTestServiceWith(t, exec)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})

	var io capture
	code := Run(context.Background(),
		[]string{"chat", "hello", "--project", "demo-project"}, env, &io)
	if code != 0 {
		t.Fatalf("code = %d\nstderr: %s", code, io.err.String())
	}
	if !strings.Contains(io.err.String(), "context: ") {
		t.Errorf("stderr should carry the context hint, got: %q", io.err.String())
	}
}

func TestRun_ChatContextIDFlagContinuesConversation(t *testing.T) {
	exec := &echoExecutor{}
	base := newTestServiceWith(t, exec)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})

	var io capture
	code := Run(context.Background(),
		[]string{"chat", "again", "--project", "demo-project", "--context-id", "ctx-cli-1"}, env, &io)
	if code != 0 {
		t.Fatalf("code = %d\nstderr: %s", code, io.err.String())
	}
	turns := exec.seen()
	if len(turns) != 1 || turns[0].contextID != "ctx-cli-1" {
		t.Fatalf("service saw turns %+v, want one turn with contextID ctx-cli-1", turns)
	}
}

// TestRun_InteractiveReplThreadsContext is the CLI-side multi-turn proof: a
// REPL session's later turns carry the contextId the service assigned to the
// first one, so the whole session is one conversation.
func TestRun_InteractiveReplThreadsContext(t *testing.T) {
	exec := &echoExecutor{}
	base := newTestServiceWith(t, exec)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})

	io := &replIo{lines: []string{"turn two", "/quit"}}
	code := Run(context.Background(),
		[]string{"chat", "-i", "turn one", "--project", "demo-project"}, env, io)
	if code != 0 {
		t.Fatalf("code = %d\nstderr: %s", code, io.err.String())
	}

	turns := exec.seen()
	if len(turns) != 2 {
		t.Fatalf("service saw %d turns, want 2: %+v", len(turns), turns)
	}
	if turns[0].text != "turn one" || turns[1].text != "turn two" {
		t.Fatalf("turn texts = %+v", turns)
	}
	if turns[0].contextID == "" || turns[1].contextID != turns[0].contextID {
		t.Fatalf("contextIds not threaded: turn1=%q turn2=%q", turns[0].contextID, turns[1].contextID)
	}
	if !strings.Contains(io.out.String(), "echo: turn two") {
		t.Errorf("stdout missing second answer: %q", io.out.String())
	}
}

func TestRun_InteractiveWithoutLineReaderFails(t *testing.T) {
	exec := &echoExecutor{}
	base := newTestServiceWith(t, exec)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})

	var io capture // no ReadLine
	code := Run(context.Background(),
		[]string{"chat", "-i", "--project", "demo-project"}, env, &io)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
