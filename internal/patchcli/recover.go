// Turn recovery: what the client does when the SSE stream for a turn breaks
// before the service reported a terminal task state.
//
// Streaming stays the default and the good case is untouched. This is the
// recovery path only: the work continues server-side and lands in the durable
// task store, so the client polls GetTask on the id it recorded and delivers
// the answer when the task finishes.
//
// Two breaks reach here and they look different on the wire. An abrupt reset
// surfaces an error from the SSE reader. A graceful close does not: a2a-go
// parses the stream with a bufio.Scanner, and a clean EOF leaves scanner.Err()
// nil, so the event iterator simply ends. Neither carries a terminal state,
// which is what makes "no terminal state" — not "an error" — the signal that a
// turn was cut short.
package patchcli

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// responseArtifactID is the stable id the service appends every text delta
// into (internal/a2a/executor.go). One artifact per turn is what makes
// recovery a wholesale read rather than a resume from an offset.
const responseArtifactID a2a.ArtifactID = "response"

// Poll schedule for a recovered turn. The first look is quick — a turn that
// was nearly done when the connection dropped should come back immediately —
// then the interval backs off so a long turn costs a handful of requests
// rather than hundreds.
const (
	recoverFirstDelay = 1 * time.Second
	recoverMaxDelay   = 8 * time.Second
	recoverBackoff    = 1.6
	// recoverBudget bounds the wait. A turn still running after this gets an
	// ending the user can read instead of an indefinite spinner.
	recoverBudget = 5 * time.Minute
)

// errRecoveryTimedOut ends a poll loop that used its whole budget without the
// task reaching a terminal state. The task itself is not lost — it is still in
// the store, reachable with `patch task get <id>`.
var errRecoveryTimedOut = errors.New("the turn is still running")

// taskGetter is the one client method recovery needs. Declared here rather
// than taking *serviceClient so the poll loop is testable without a transport.
type taskGetter interface {
	GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error)
}

// terminalTaskState reports whether a task has stopped moving. Everything else
// (submitted, working, and the input/auth-required states this service never
// emits) means the turn is still in flight.
func terminalTaskState(s a2a.TaskState) bool {
	switch s {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	default:
		return false
	}
}

// answerFromTask reconstructs the whole answer produced so far from a stored
// task. Text deltas append as separate parts of the one "response" artifact,
// so joining them is the complete answer — there is nothing to splice onto
// what was already rendered. A task with no artifact falls back to the text on
// its terminal status message, which is what the executor puts there.
func answerFromTask(task *a2a.Task) string {
	if task == nil {
		return ""
	}
	var b strings.Builder
	for _, art := range task.Artifacts {
		if art == nil || art.ID != responseArtifactID {
			continue
		}
		b.WriteString(textOf(art.Parts))
	}
	if b.Len() > 0 {
		return b.String()
	}
	if task.Status.Message != nil {
		return textOf(task.Status.Message.Parts)
	}
	return ""
}

// failureFromTask is the note to show for a task that ended badly, or "" for
// one that completed.
func failureFromTask(task *a2a.Task) string {
	if task == nil {
		return ""
	}
	switch task.Status.State {
	case a2a.TaskStateFailed, a2a.TaskStateRejected:
		if task.Status.Message != nil {
			if note := textOf(task.Status.Message.Parts); note != "" {
				return note
			}
		}
		return "the task failed"
	case a2a.TaskStateCanceled:
		return "the task was canceled"
	default:
		return ""
	}
}

// recoverOptions are the poll schedule, injectable so tests run instantly.
// A zero value means the constants above.
type recoverOptions struct {
	first  time.Duration
	max    time.Duration
	budget time.Duration
	// sleep waits, or returns false if ctx ended first. Nil uses a real timer.
	sleep func(ctx context.Context, d time.Duration) bool
}

func (o recoverOptions) withDefaults() recoverOptions {
	if o.first <= 0 {
		o.first = recoverFirstDelay
	}
	if o.max <= 0 {
		o.max = recoverMaxDelay
	}
	if o.budget <= 0 {
		o.budget = recoverBudget
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
	return o
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pollTask waits for a task to reach a terminal state, returning it when it
// does. A GetTask failure is not fatal — the same drop that broke the stream
// can fail the first poll too — so it retries within the budget and only
// surfaces the last error if the budget runs out. Running out of budget with
// the task still working returns [errRecoveryTimedOut] along with the most
// recent snapshot, so a caller can still show what the answer had reached.
func pollTask(ctx context.Context, getter taskGetter, id a2a.TaskID, opts recoverOptions) (*a2a.Task, error) {
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.budget)

	var last *a2a.Task
	var lastErr error
	delay := opts.first

	for {
		task, err := getter.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
		switch {
		case err != nil:
			lastErr = err
		case task != nil:
			last, lastErr = task, nil
			if terminalTaskState(task.Status.State) {
				return task, nil
			}
		}

		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		if time.Now().Add(delay).After(deadline) {
			if lastErr != nil {
				return last, lastErr
			}
			return last, errRecoveryTimedOut
		}
		if !opts.sleep(ctx, delay) {
			return last, ctx.Err()
		}
		if delay = time.Duration(float64(delay) * recoverBackoff); delay > opts.max {
			delay = opts.max
		}
	}
}
