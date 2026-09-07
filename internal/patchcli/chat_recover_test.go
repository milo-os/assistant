package patchcli

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/a2aproject/a2a-go/v2/a2a"
)

// collectMsgs runs consumeStream over a scripted event sequence and returns
// every model message it produced.
func collectMsgs(ctx context.Context, events iter.Seq2[a2a.Event, error]) []tea.Msg {
	var got []tea.Msg
	consumeStream(ctx, events, 0, "", func(msg tea.Msg) { got = append(got, msg) })
	return got
}

func lastMsg(msgs []tea.Msg) tea.Msg {
	if len(msgs) == 0 {
		return nil
	}
	return msgs[len(msgs)-1]
}

func taskEvent() *a2a.Task {
	return &a2a.Task{ID: "t-1", ContextID: "c-1", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
}

func chunkEvent(text string) *a2a.TaskArtifactUpdateEvent {
	return &a2a.TaskArtifactUpdateEvent{
		TaskID: "t-1", ContextID: "c-1",
		Artifact: &a2a.Artifact{ID: responseArtifactID, Parts: a2a.ContentParts{a2a.NewTextPart(text)}},
	}
}

func stateEvent(s a2a.TaskState) *a2a.TaskStatusUpdateEvent {
	return &a2a.TaskStatusUpdateEvent{TaskID: "t-1", ContextID: "c-1", Status: a2a.TaskStatus{State: s}}
}

// ── detecting the break ───────────────────────────────────────

// The regression this whole feature exists for: a graceful close ends the SSE
// iterator with no error at all, so a truncated turn used to look exactly like
// a finished one and its fragment was committed as the answer.
func TestCleanEOFWithoutTerminalStateIsABreakNotACompletion(t *testing.T) {
	msgs := collectMsgs(context.Background(), seqOf(
		taskEvent(),
		stateEvent(a2a.TaskStateWorking),
		chunkEvent("half an ans"),
	))
	broke, ok := lastMsg(msgs).(streamBrokeMsg)
	if !ok {
		t.Fatalf("a stream that stops mid-turn must break, got %T", lastMsg(msgs))
	}
	if broke.taskID != "t-1" {
		t.Errorf("taskID = %q, want t-1 — recovery has nothing to poll without it", broke.taskID)
	}
	if broke.err != nil {
		t.Errorf("a clean close carries no error, got %v", broke.err)
	}
}

func TestTerminalStateEndsTheTurnNormally(t *testing.T) {
	msgs := collectMsgs(context.Background(), seqOf(
		taskEvent(),
		stateEvent(a2a.TaskStateWorking),
		chunkEvent("the answer"),
		stateEvent(a2a.TaskStateCompleted),
	))
	if _, ok := lastMsg(msgs).(streamDoneMsg); !ok {
		t.Fatalf("a completed turn must finish, got %T", lastMsg(msgs))
	}
}

// A hard reset surfaces an error, but it is the same recoverable break — the
// server is still working on the turn.
func TestStreamErrorWithATaskIDIsRecoverable(t *testing.T) {
	boom := errors.New("connection reset by peer")
	events := func(yield func(a2a.Event, error) bool) {
		yield(taskEvent(), nil)
		yield(nil, boom)
	}
	broke, ok := lastMsg(collectMsgs(context.Background(), events)).(streamBrokeMsg)
	if !ok {
		t.Fatalf("a reset mid-turn should be recoverable, got %T", lastMsg(collectMsgs(context.Background(), events)))
	}
	if !errors.Is(broke.err, boom) {
		t.Errorf("the reader's error should ride along, got %v", broke.err)
	}
}

// Without a task id there is nothing to poll, so the break stays a plain
// error the user can see.
func TestStreamErrorBeforeAnyTaskIDStaysAnError(t *testing.T) {
	boom := errors.New("dial tcp: connection refused")
	events := func(yield func(a2a.Event, error) bool) { yield(nil, boom) }
	if _, ok := lastMsg(collectMsgs(context.Background(), events)).(streamErrMsg); !ok {
		t.Fatalf("no task id means no recovery, want streamErrMsg")
	}
}

// An interrupt cancels the turn context; the abandoned stream must not try to
// recover a turn the user deliberately stopped.
func TestCanceledTurnDoesNotRecover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msgs := collectMsgs(ctx, seqOf(taskEvent(), chunkEvent("partial")))
	if _, ok := lastMsg(msgs).(streamBrokeMsg); ok {
		t.Fatal("a canceled turn must not start recovery")
	}
}

// ── recovering the turn ───────────────────────────────────────

// A break keeps the turn open. Nothing is committed to the transcript yet —
// that is what stops a fragment being recorded as the answer.
func TestBreakKeepsTheTurnOpenAndSaysSo(t *testing.T) {
	m := newTestModel()
	m.client = &serviceClient{}
	m.recoverOpts = fastPolls(time.Millisecond)
	workingSince(m, 5*time.Second)
	m.Update(streamChunkMsg{text: "half an ans"})

	before := len(m.turns)
	m.Update(streamBrokeMsg{taskID: "t-1"})

	if !m.recovering || !m.working {
		t.Fatalf("a break must stay working and recovering, got working=%v recovering=%v", m.working, m.recovering)
	}
	if len(m.turns) != before {
		t.Fatal("a broken stream must not commit the partial answer as a finished turn")
	}
	if got := plain(m.footer()); !strings.Contains(got, "reconnected") {
		t.Errorf("the footer must say the turn is still running: %q", got)
	}
}

// The store holds the whole answer, so recovery replaces the fragment
// wholesale rather than splicing onto it.
func TestRecoveryReplacesThePartialAnswer(t *testing.T) {
	m := newTestModel()
	m.client = &serviceClient{}
	workingSince(m, 5*time.Second)
	m.Update(streamChunkMsg{text: "half an ans"})
	m.Update(streamBrokeMsg{taskID: "t-1"})
	m.Update(recoverDoneMsg{task: completedTask("half an answer, then the rest")})

	got := transcript(m)
	if !strings.Contains(got, "half an answer, then the rest") {
		t.Fatalf("the recovered answer should be in the transcript: %q", got)
	}
	if m.recovering || m.working {
		t.Errorf("a recovered turn is finished, got working=%v recovering=%v", m.working, m.recovering)
	}
	if m.contextID != "c-1" {
		t.Errorf("the recovered task should thread its contextId forward, got %q", m.contextID)
	}
}

// A task that failed while the client was away still reports its failure.
func TestRecoveryReportsAFailedTask(t *testing.T) {
	m := newTestModel()
	m.client = &serviceClient{}
	workingSince(m, 5*time.Second)
	m.Update(streamBrokeMsg{taskID: "t-1"})
	m.Update(recoverDoneMsg{task: &a2a.Task{
		ID: "t-1", ContextID: "c-1",
		Status: a2a.TaskStatus{
			State:   a2a.TaskStateFailed,
			Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Agent run failed: boom")),
		},
	}})
	if got := transcript(m); !strings.Contains(got, "boom") {
		t.Fatalf("a failure found on recovery should be shown: %q", got)
	}
}

// Giving up has to be legible, and has to leave the user a way back to the
// answer rather than pretending the turn is over.
func TestRecoveryThatGivesUpSaysHowToGetTheAnswer(t *testing.T) {
	m := newTestModel()
	m.client = &serviceClient{}
	workingSince(m, 5*time.Second)
	m.Update(streamChunkMsg{text: "partial"})
	m.Update(streamBrokeMsg{taskID: "t-1"})
	m.Update(recoverDoneMsg{task: workingTask(), err: errRecoveryTimedOut})

	got := transcript(m)
	if !containsAll(got, "could not be recovered", "patch task get t-1") {
		t.Fatalf("a give-up must name the task: %q", got)
	}
	if m.working || m.recovering {
		t.Errorf("giving up ends the turn, got working=%v recovering=%v", m.working, m.recovering)
	}
}

// Recovery messages from an interrupted turn are stale and must be dropped,
// exactly like the stream's own late messages.
func TestStaleRecoveryMessagesAreIgnored(t *testing.T) {
	m := newTestModel()
	m.client = &serviceClient{}
	workingSince(m, 5*time.Second)
	m.turnGen = 3
	before := len(m.turns)
	m.Update(recoverDoneMsg{gen: 2, task: completedTask("from an abandoned turn")})
	if len(m.turns) != before {
		t.Fatal("a recovery from an earlier generation must not land in the transcript")
	}
}

// ── the line-based modes ──────────────────────────────────────

// The plain CLI already exits 1 on a truncated stream, but said nothing about
// why. It has the task id in hand, so it can point at the answer it lost.
func TestRenderChatReportsATruncatedStream(t *testing.T) {
	var io capture
	code, err := renderChat(seqOf(
		taskEvent(),
		stateEvent(a2a.TaskStateWorking),
		chunkEvent("half an ans"),
	), false, &io)
	if err != nil {
		t.Fatalf("a clean close is not a stream error, got %v", err)
	}
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !containsAll(io.err.String(), "connection dropped", "patch task get t-1") {
		t.Fatalf("stderr should explain the break and name the task: %q", io.err.String())
	}
}

// A completed turn says nothing extra.
func TestRenderChatStaysQuietOnACompletedTurn(t *testing.T) {
	var io capture
	code, _ := renderChat(seqOf(
		taskEvent(),
		chunkEvent("the answer"),
		stateEvent(a2a.TaskStateCompleted),
	), false, &io)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Contains(io.err.String(), "connection dropped") {
		t.Fatalf("a finished turn must not warn: %q", io.err.String())
	}
}
