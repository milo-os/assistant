package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// maxSendMessageBodyBytes bounds the request body: a chat message is a few
// paragraphs and a short mention list at most, never an upload.
const maxSendMessageBodyBytes = 64 << 10

// SendMessageREST implements the conversations/{name}/sendmessage
// subresource: a POST that runs one agent turn and streams its output back as
// Server-Sent Events. Unlike ConversationREST/MessagesREST — read views over
// the shared history store — this is the write/execution path: Connect drives
// the injected [a2a.AgentRunner] directly, the same seam internal/a2a's
// executor drives for A2A traffic, just adapted to SSE instead of JSON-RPC
// streaming (see the package doc on the SSE event shapes this emits).
type SendMessageREST struct {
	runner a2a.AgentRunner
}

var (
	_ rest.Storage   = (*SendMessageREST)(nil)
	_ rest.Scoper    = (*SendMessageREST)(nil)
	_ rest.Connecter = (*SendMessageREST)(nil)
)

// NewSendMessageREST builds the sendmessage subresource over the given runner.
func NewSendMessageREST(runner a2a.AgentRunner) *SendMessageREST {
	return &SendMessageREST{runner: runner}
}

// New returns the parent Conversation type: sendmessage has no KRM object of
// its own (its request/response bodies are plain JSON/SSE, not versioned
// types) — this only satisfies the installer's GetResourceKind lookup, which
// needs *some* type already registered in the scheme.
func (r *SendMessageREST) New() runtime.Object   { return &assistant.Conversation{} }
func (r *SendMessageREST) Destroy()              {}
func (r *SendMessageREST) NamespaceScoped() bool { return true }

// NewConnectOptions reports no query-string options: the request's payload is
// the POST body, decoded by Connect's own handler, not query parameters.
func (r *SendMessageREST) NewConnectOptions() (runtime.Object, bool, string) {
	return nil, false, ""
}

// ConnectMethods allows only POST — starting a turn is not idempotent and has
// no meaningful GET form.
func (r *SendMessageREST) ConnectMethods() []string { return []string{http.MethodPost} }

// sendMessageRequest is the POST body. Mentions reuses [a2a.Mention] — the
// same shape the A2A message-metadata path already parses — rather than
// inventing a second wire type for the identical client-facing concept.
type sendMessageRequest struct {
	Text     string        `json:"text"`
	Mentions []a2a.Mention `json:"mentions,omitempty"`
}

// SSE event envelope. Each frame is one JSON object on its own "data: " line.
// The vocabulary mirrors what the A2A path already streams (text deltas, tool
// start/finish, terminal state) — see internal/a2a/runner.go and activity.go —
// just carried as SSE instead of JSON-RPC/task-status-update framing.
type (
	// textDeltaEvent is one chunk of streamed assistant text.
	//   {"type":"text_delta","text":"..."}
	textDeltaEvent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	// toolStartEvent announces a tool call the model has requested.
	//   {"type":"tool_start","id":"...","name":"...","summary":"..."}
	toolStartEvent struct {
		Type    string `json:"type"`
		ID      string `json:"id,omitempty"`
		Name    string `json:"name"`
		Summary string `json:"summary,omitempty"`
	}
	// toolFinishEvent reports a tool call's outcome.
	//   {"type":"tool_finish","id":"...","name":"...","ok":true,"elapsedMs":123}
	toolFinishEvent struct {
		Type      string `json:"type"`
		ID        string `json:"id,omitempty"`
		Name      string `json:"name"`
		OK        bool   `json:"ok"`
		ElapsedMs int64  `json:"elapsedMs"`
	}
	// doneEvent is the final frame; the stream closes immediately after it.
	//   {"type":"done","state":"completed|failed|canceled","text":"...","error":"..."}
	doneEvent struct {
		Type  string `json:"type"`
		State string `json:"state"`
		Text  string `json:"text,omitempty"`
		Error string `json:"error,omitempty"`
	}
)

// Connect resolves and validates the target conversation exactly like
// [ConversationREST.Get] does, then returns a handler that decodes the body,
// switches the response to text/event-stream, and drives the run to
// completion. The project/name checks happen here (before the handler is
// returned) so a bad request never gets far enough to open the stream.
func (r *SendMessageREST) Connect(ctx context.Context, id string, _ runtime.Object, responder rest.Responder) (http.Handler, error) {
	if r.runner == nil {
		return nil, apierrors.NewServiceUnavailable("sendmessage is not configured: no agent runner")
	}
	project, err := tenant.ProjectFromContext(ctx, conversationsResource)
	if err != nil {
		return nil, err
	}
	if !validConversationName(id) {
		return nil, apierrors.NewBadRequest("invalid conversation name")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body sendMessageRequest
		dec := json.NewDecoder(io.LimitReader(req.Body, maxSendMessageBodyBytes+1))
		if err := dec.Decode(&body); err != nil {
			responder.Error(apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %v", err)))
			return
		}
		if strings.TrimSpace(body.Text) == "" {
			responder.Error(apierrors.NewBadRequest("text is required"))
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			responder.Error(apierrors.NewInternalError(fmt.Errorf("response writer does not support streaming")))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Disable any intermediary buffering (e.g. nginx) of the stream.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		sink := &sseSink{w: w, flusher: flusher}
		result := r.runner.Run(req.Context(), a2a.RunRequest{
			UserText:    body.Text,
			ProjectName: project,
			ContextID:   id,
			TaskID:      uuid.NewString(),
			Mentions:    body.Mentions,
		}, sink)

		sink.writeEvent(doneEvent{
			Type:  "done",
			State: string(result.State),
			Text:  result.Text,
			Error: result.Error,
		})
	}), nil
}

// sseSink adapts [a2a.RunSink] to Server-Sent-Event frames written directly to
// the response, flushing after every event so a client sees deltas as they
// happen instead of buffered until the turn completes.
type sseSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *sseSink) OnTextDelta(text string) {
	s.writeEvent(textDeltaEvent{Type: "text_delta", Text: text})
}

func (s *sseSink) OnToolStart(act a2a.ToolActivity) {
	s.writeEvent(toolStartEvent{Type: "tool_start", ID: act.ID, Name: act.Name, Summary: act.Summary})
}

func (s *sseSink) OnToolFinish(act a2a.ToolActivity) {
	s.writeEvent(toolFinishEvent{Type: "tool_finish", ID: act.ID, Name: act.Name, OK: act.OK, ElapsedMs: act.Elapsed.Milliseconds()})
}

// writeEvent marshals v (always one of the event structs above) and writes it
// as one SSE frame. A marshal failure here would be a bug in one of those
// fixed struct types, never client input, so it is swallowed rather than
// surfaced mid-stream (the headers are already committed).
func (s *sseSink) writeEvent(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.flusher.Flush()
}
