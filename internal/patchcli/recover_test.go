package patchcli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// fakeTasks answers GetTask from a scripted list, repeating the last entry
// once the script runs out, and counts the calls.
type fakeTasks struct {
	script []*a2a.Task
	err    error
	// errFor is how many leading calls fail with err before the script starts.
	errFor int
	calls  int
}

func (f *fakeTasks) GetTask(_ context.Context, _ *a2a.GetTaskRequest) (*a2a.Task, error) {
	f.calls++
	if f.calls <= f.errFor {
		return nil, f.err
	}
	i := f.calls - f.errFor - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	if i < 0 {
		return nil, f.err
	}
	return f.script[i], nil
}

// fastPolls runs the poll loop without any real waiting.
func fastPolls(budget time.Duration) recoverOptions {
	return recoverOptions{
		first:  time.Millisecond,
		max:    time.Millisecond,
		budget: budget,
		sleep:  func(context.Context, time.Duration) bool { return true },
	}
}

func workingTask() *a2a.Task {
	return &a2a.Task{ID: "t-1", ContextID: "c-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
}

func completedTask(text string) *a2a.Task {
	return &a2a.Task{
		ID:        "t-1",
		ContextID: "c-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{
			ID:    responseArtifactID,
			Parts: a2a.ContentParts{a2a.NewTextPart(text)},
		}},
	}
}

func TestTerminalTaskState(t *testing.T) {
	terminal := []a2a.TaskState{
		a2a.TaskStateCompleted, a2a.TaskStateFailed,
		a2a.TaskStateCanceled, a2a.TaskStateRejected,
	}
	for _, s := range terminal {
		if !terminalTaskState(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	inFlight := []a2a.TaskState{
		a2a.TaskStateUnspecified, a2a.TaskStateSubmitted,
		a2a.TaskStateWorking, a2a.TaskStateInputRequired,
	}
	for _, s := range inFlight {
		if terminalTaskState(s) {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

// The service appends every text delta as a further part of one "response"
// artifact, so the stored task holds the whole answer and recovery is a
// wholesale read — never a splice at an offset.
func TestAnswerFromTaskJoinsResponseArtifactParts(t *testing.T) {
	task := &a2a.Task{
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{
			{ID: "other", Parts: a2a.ContentParts{a2a.NewTextPart("ignore me")}},
			{ID: responseArtifactID, Parts: a2a.ContentParts{
				a2a.NewTextPart("Hello, "),
				a2a.NewTextPart("world"),
				a2a.NewTextPart("!"),
			}},
		},
	}
	if got := answerFromTask(task); got != "Hello, world!" {
		t.Fatalf("answerFromTask = %q, want %q", got, "Hello, world!")
	}
}

func TestAnswerFromTaskFallsBackToStatusMessage(t *testing.T) {
	task := &a2a.Task{Status: a2a.TaskStatus{
		State:   a2a.TaskStateCompleted,
		Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("final answer")),
	}}
	if got := answerFromTask(task); got != "final answer" {
		t.Fatalf("answerFromTask = %q, want %q", got, "final answer")
	}
	if got := answerFromTask(nil); got != "" {
		t.Fatalf("answerFromTask(nil) = %q, want empty", got)
	}
}

func TestFailureFromTask(t *testing.T) {
	failed := &a2a.Task{Status: a2a.TaskStatus{
		State:   a2a.TaskStateFailed,
		Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Agent run failed: boom")),
	}}
	if got := failureFromTask(failed); got != "Agent run failed: boom" {
		t.Errorf("failureFromTask(failed) = %q", got)
	}
	if got := failureFromTask(completedTask("done")); got != "" {
		t.Errorf("a completed task has no failure note, got %q", got)
	}
	canceled := &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}
	if got := failureFromTask(canceled); got != "the task was canceled" {
		t.Errorf("failureFromTask(canceled) = %q", got)
	}
}

func TestPollTaskWaitsForTerminalState(t *testing.T) {
	tasks := &fakeTasks{script: []*a2a.Task{
		workingTask(), workingTask(), completedTask("the whole answer"),
	}}
	task, err := pollTask(context.Background(), tasks, "t-1", fastPolls(time.Minute))
	if err != nil {
		t.Fatalf("pollTask err = %v", err)
	}
	if tasks.calls != 3 {
		t.Errorf("calls = %d, want 3 (it must keep polling while working)", tasks.calls)
	}
	if got := answerFromTask(task); got != "the whole answer" {
		t.Errorf("recovered answer = %q", got)
	}
}

// The same drop that broke the stream can fail the first poll too, so a
// GetTask error inside the budget is retried rather than surfaced.
func TestPollTaskRetriesTransientErrors(t *testing.T) {
	tasks := &fakeTasks{
		errFor: 2,
		err:    errors.New("connection refused"),
		script: []*a2a.Task{completedTask("recovered")},
	}
	task, err := pollTask(context.Background(), tasks, "t-1", fastPolls(time.Minute))
	if err != nil {
		t.Fatalf("pollTask err = %v, want nil after retries", err)
	}
	if got := answerFromTask(task); got != "recovered" {
		t.Errorf("recovered answer = %q", got)
	}
}

// A turn still running when the budget runs out gets an ending the user can
// read, and the last snapshot comes back with it so the partial is not lost.
func TestPollTaskBudgetExhausted(t *testing.T) {
	tasks := &fakeTasks{script: []*a2a.Task{workingTask()}}
	task, err := pollTask(context.Background(), tasks, "t-1", fastPolls(5*time.Millisecond))
	if !errors.Is(err, errRecoveryTimedOut) {
		t.Fatalf("err = %v, want errRecoveryTimedOut", err)
	}
	if task == nil || task.Status.State != a2a.TaskStateWorking {
		t.Errorf("the last snapshot should still come back, got %v", task)
	}
}

// Persistent failure surfaces the real error, not a generic timeout.
func TestPollTaskSurfacesLastErrorWhenNothingEverAnswered(t *testing.T) {
	boom := errors.New("403 Forbidden")
	tasks := &fakeTasks{errFor: 100, err: boom}
	if _, err := pollTask(context.Background(), tasks, "t-1", fastPolls(5*time.Millisecond)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestPollTaskStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tasks := &fakeTasks{script: []*a2a.Task{workingTask()}}
	if _, err := pollTask(ctx, tasks, "t-1", fastPolls(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
