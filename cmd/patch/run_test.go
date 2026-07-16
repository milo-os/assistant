package main

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// diagnoseExecutor is a scripted AgentExecutor: it streams a fixed "pipeline
// diagnosis" answer (submitted → working → artifact chunks → completed),
// exercising the CLI's streaming render without a real model.
type diagnoseExecutor struct{}

func (*diagnoseExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		first := a2a.NewArtifactEvent(ec, a2a.NewTextPart("Ran the pipeline diagnosis. The provider tool reported: "))
		id := first.Artifact.ID
		if !yield(first, nil) {
			return
		}
		if !yield(a2a.NewArtifactUpdateEvent(ec, id, a2a.NewTextPart(`{"id":"p-1","findings":["CONSUMER_LAG"]}.`)), nil) {
			return
		}
		final := a2a.NewMessage(a2a.MessageRoleAgent,
			a2a.NewTextPart(`Ran the pipeline diagnosis. The provider tool reported: {"id":"p-1","findings":["CONSUMER_LAG"]}.`))
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, final), nil)
	}
}

func (*diagnoseExecutor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// devAuth mirrors the service's dev-token semantics: a token maps to a project,
// and a request may only target that token's project. It rejects missing/
// unknown tokens with ErrUnauthenticated and cross-project access with
// ErrUnauthorized — the two failures the CLI must distinguish.
type devAuth struct {
	tokens map[string]string // token -> project
}

func (a *devAuth) Before(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	token := bearerToken(cc)
	if token == "" {
		return ctx, nil, a2a.NewError(a2a.ErrUnauthenticated, "no bearer token")
	}
	project, known := a.tokens[token]
	if !known {
		return ctx, nil, a2a.NewError(a2a.ErrUnauthenticated, "unknown token")
	}
	// Project scoping only applies to message send/stream (task get/cancel
	// carry no project of their own here).
	if send, ok := req.Payload.(*a2a.SendMessageRequest); ok && send.Message != nil {
		if requested, _ := send.Message.Meta()["projectName"].(string); requested != "" && requested != project {
			return ctx, nil, a2a.NewError(a2a.ErrUnauthorized, "token does not grant project "+requested)
		}
	}
	return ctx, nil, nil
}

func (a *devAuth) After(ctx context.Context, cc *a2asrv.CallContext, resp *a2asrv.Response) error {
	return nil
}

func bearerToken(cc *a2asrv.CallContext) string {
	vals, _ := cc.ServiceParams().Get("Authorization")
	for _, v := range vals {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return after
		}
	}
	return ""
}

// newTestService starts an in-process A2A JSON-RPC service (real a2a-go server
// stack + scripted executor + dev-token auth) and returns its base URL.
func newTestService(t *testing.T) string {
	t.Helper()
	return newTestServiceWith(t, &diagnoseExecutor{})
}

// newTestServiceWith is newTestService with a caller-supplied executor.
func newTestServiceWith(t *testing.T, executor a2asrv.AgentExecutor) string {
	t.Helper()
	handler := a2asrv.NewHandler(
		executor,
		a2asrv.WithCallInterceptors(&devAuth{tokens: map[string]string{
			"good":  "demo-project",
			"wrong": "other-project",
		}}),
		a2asrv.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	mux := http.NewServeMux()
	mux.Handle("/a2a", a2asrv.NewJSONRPCHandler(handler))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	card := &a2a.AgentCard{
		Name:        "Patch",
		Description: "Patch is the Datum Cloud assistant.",
		Version:     "0.1.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(srv.URL+"/a2a", a2a.TransportProtocolJSONRPC),
		},
		Provider:           &a2a.AgentProvider{Org: "Datum", URL: "https://www.datum.net"},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		SecuritySchemes:    a2a.NamedSecuritySchemes{"bearer": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
		Skills:             []a2a.AgentSkill{{ID: "project-assistant", Name: "Project assistant", Description: "d"}},
	}
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	return srv.URL
}

// envFn builds a getenv closure over a fixed map.
func envFn(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestRun_ChatStreamsFindings(t *testing.T) {
	base := newTestService(t)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(),
		[]string{"chat", "Diagnose pipeline p-1 for StreamCo", "--project", "demo-project"},
		env, &io)
	if code != 0 {
		t.Fatalf("code = %d, want 0\nstderr: %s", code, io.err.String())
	}
	if !strings.Contains(io.out.String(), "CONSUMER_LAG") {
		t.Errorf("stdout missing findings: %q", io.out.String())
	}
	if !strings.Contains(io.err.String(), "completed") {
		t.Errorf("stderr missing completed transition: %q", io.err.String())
	}
}

func TestRun_Card(t *testing.T) {
	base := newTestService(t)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(), []string{"card"}, env, &io)
	if code != 0 {
		t.Fatalf("code = %d, want 0\nstderr: %s", code, io.err.String())
	}
	if !strings.Contains(io.out.String(), "Patch") {
		t.Errorf("card output missing name: %q", io.out.String())
	}
	if !strings.Contains(io.out.String(), base+"/a2a") {
		t.Errorf("card output missing endpoint: %q", io.out.String())
	}
}

func TestRun_ChatNoToken_Unauthorized(t *testing.T) {
	base := newTestService(t)
	env := envFn(map[string]string{"PATCH_URL": base}) // no token
	var io capture
	code := Run(context.Background(),
		[]string{"chat", "Diagnose pipeline p-1", "--project", "demo-project"}, env, &io)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(io.err.String()), "unauthorized") {
		t.Errorf("stderr should mention unauthorized: %q", io.err.String())
	}
}

func TestRun_ChatWrongProject_Forbidden(t *testing.T) {
	base := newTestService(t)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "wrong"})
	var io capture
	code := Run(context.Background(),
		[]string{"chat", "Diagnose pipeline p-1", "--project", "demo-project"}, env, &io)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(io.err.String()), "forbidden") {
		t.Errorf("stderr should mention forbidden: %q", io.err.String())
	}
}

func TestRun_TaskGetThenCancel(t *testing.T) {
	base := newTestService(t)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})

	// Create a completed task by running a chat first.
	var chatIo capture
	if code := Run(context.Background(),
		[]string{"chat", "Diagnose pipeline p-1", "--project", "demo-project"}, env, &chatIo); code != 0 {
		t.Fatalf("chat setup failed: code=%d err=%s", code, chatIo.err.String())
	}
	taskID := firstTaskID(t, base)

	// task get → exit 0, shows completed.
	var getIo capture
	if code := Run(context.Background(), []string{"task", "get", taskID}, env, &getIo); code != 0 {
		t.Fatalf("task get code = %d, want 0\nstderr: %s", code, getIo.err.String())
	}
	if !strings.Contains(getIo.out.String(), taskID) || !strings.Contains(getIo.out.String(), "completed") {
		t.Errorf("task get output unexpected: %q", getIo.out.String())
	}

	// task cancel on a completed task → exit 1 with a clear message.
	var cancelIo capture
	if code := Run(context.Background(), []string{"task", "cancel", taskID}, env, &cancelIo); code != 1 {
		t.Fatalf("task cancel code = %d, want 1\nstderr: %s", code, cancelIo.err.String())
	}
	if !strings.Contains(cancelIo.err.String(), "cannot be canceled") {
		t.Errorf("cancel error should explain non-cancelable state: %q", cancelIo.err.String())
	}
}

func TestRun_MissingURL(t *testing.T) {
	var io capture
	code := Run(context.Background(), []string{"card"}, envFn(nil), &io)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(io.err.String(), "PATCH_URL") {
		t.Errorf("stderr should mention PATCH_URL: %q", io.err.String())
	}
}

// firstTaskID sends one message via a fresh client and returns the created
// task's ID, so the get/cancel test has a real server-generated id to target.
func firstTaskID(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()
	client, err := newClient(ctx, base, "good")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Destroy()
	msg := buildMessage("Diagnose pipeline p-1", "demo-project", "")
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("expected *a2a.Task, got %T", res)
	}
	return string(task.ID)
}
