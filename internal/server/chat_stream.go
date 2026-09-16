package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
)

// chatStreamRequestBody is the POST /chat/conversations/{contextId}/sendmessage
// body: the same shape cloud-portal's assistant plugin already sends today
// against POST /a2a's SendStreamingMessage (see cloud-portal's
// app/server/routes/assistant-chat.ts), minus the A2A envelope around it.
type chatStreamRequestBody struct {
	Text     string                 `json:"text"`
	Mentions []assistanta2a.Mention `json:"mentions,omitempty"`
}

// chatStreamHandler serves POST /chat/conversations/{contextId}/sendmessage: a
// second, REST-shaped front door onto the exact same [assistanta2a.AgentRunner]
// POST /a2a drives, for callers that want the assistant's flat SSE event
// vocabulary (text_delta/tool_start/tool_finish/done) instead of A2A's
// JSON-RPC-over-SSE framing.
//
// It exists so that vocabulary — and the JSON-RPC/task-event translation that
// produces it — lives here, next to the runner that emits the events, rather
// than in a caller that has to parse A2A's wire protocol just to throw the
// framing away. cloud-portal's assistant-chat proxy route used to do exactly
// that: decode task/statusUpdate/artifactUpdate frames and re-encode them as
// text_delta/tool_start/tool_finish/done. That translation is now this
// handler's [chatStreamSink] instead, so the portal route is a plain
// bearer-token-forwarding byte proxy with no protocol awareness.
//
// Auth is the same bearer-token authn + project authz every other endpoint
// here applies (see compact.go, rename.go) — this is a sibling of those, not a
// separate scheme. Unlike POST /a2a it carries no task/message-id lifecycle:
// each call is one turn, run and returned; there is nothing to GetTask or
// CancelTask afterward.
func chatStreamHandler(runner assistanta2a.AgentRunner, authenticator auth.Authenticator, authorizer auth.Authorizer, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		principal, err := authenticateBearer(ctx, authenticator, r)
		if err != nil {
			writeAuthErrWith(w, logger, err, "Authentication failed")
			return
		}

		contextID := r.PathValue("contextId")
		if contextID == "" {
			writeAuthError(w, http.StatusBadRequest, "contextId is required")
			return
		}
		projectName := r.URL.Query().Get("projectId")
		if projectName == "" {
			writeAuthError(w, http.StatusBadRequest, "projectId query parameter is required")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var body chatStreamRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeAuthError(w, http.StatusRequestEntityTooLarge, "Request body too large")
				return
			}
			writeAuthError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}
		if body.Text == "" {
			writeAuthError(w, http.StatusBadRequest, "text is required")
			return
		}

		if err := authorizer.AuthorizeProject(ctx, principal, projectName); err != nil {
			writeAuthErrWith(w, logger, err, "Authorization failed")
			return
		}

		if runner == nil {
			writeAuthError(w, http.StatusServiceUnavailable, "Chat is not available")
			return
		}

		// Same step authMiddleware applies in front of POST /a2a: only past
		// this point does the caller's own credential go downstream, so
		// internal/capability can forward it to a capability provider (e.g.
		// compute's MCP tool). Without it, the executor sees no identity to
		// forward at all, indistinguishable from Milo's impersonation-header
		// path this endpoint exists to avoid.
		ctx = auth.ContextWithBearerToken(ctx, auth.ExtractBearerToken(r.Header.Get("Authorization")))

		flusher, ok := w.(http.Flusher)
		if !ok {
			// Can't stream on this ResponseWriter (only in tests with an
			// exotic recorder — every real transport supports it).
			writeAuthError(w, http.StatusInternalServerError, "Streaming not supported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		sink := &chatStreamSink{w: w, flusher: flusher}
		result := runner.Run(ctx, assistanta2a.RunRequest{
			UserText:    body.Text,
			ProjectName: projectName,
			ContextID:   contextID,
			TaskID:      newRequestID(),
			Mentions:    body.Mentions,
		}, sink)
		if sink.aborted {
			return
		}

		switch result.State {
		case assistanta2a.RunCanceled:
			sink.writeDone("canceled", "", "")
		case assistanta2a.RunFailed:
			msg := result.Error
			if msg == "" {
				msg = "Agent run failed"
			}
			logger.Error("http.chatstream.failed", "contextId", contextID, "projectName", projectName, "error", msg)
			sink.writeDone("failed", "", msg)
		default: // RunCompleted
			sink.writeDone("completed", result.Text, "")
		}
	}
}

// chatStreamSink implements [assistanta2a.RunSink], writing each callback
// straight out as an SSE frame in the flat vocabulary
// ui/consumer/src/lib/sse.ts parses in the assistant repo (cloud-portal side),
// instead of accumulating an A2A artifact/status-update sequence for something
// downstream to translate later.
type chatStreamSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	// aborted is set once a write fails (the client went away). Once set,
	// every later callback and the terminal frame in chatStreamHandler are
	// skipped — there is no one left to write to.
	aborted bool
}

func (s *chatStreamSink) OnTextDelta(text string) {
	if s.aborted || text == "" {
		return
	}
	s.write(map[string]any{"type": "text_delta", "text": text})
}

func (s *chatStreamSink) OnToolStart(act assistanta2a.ToolActivity) {
	if s.aborted || act.Name == "" {
		return
	}
	s.write(map[string]any{"type": "tool_start", "id": act.ID, "name": act.Name, "summary": act.Summary})
}

func (s *chatStreamSink) OnToolFinish(act assistanta2a.ToolActivity) {
	if s.aborted || act.Name == "" {
		return
	}
	s.write(map[string]any{
		"type": "tool_finish", "id": act.ID, "name": act.Name,
		"ok": act.OK, "elapsedMs": act.Elapsed.Milliseconds(),
	})
}

// writeDone emits the terminal frame. text is the final answer (completed
// only); errMsg is the failure reason (failed only) — mirroring
// assistant-chat.ts's `done` frame shape exactly, so the portal's proxy needs
// no translation on this frame either.
func (s *chatStreamSink) writeDone(state, text, errMsg string) {
	if s.aborted {
		return
	}
	frame := map[string]any{"type": "done", "state": state}
	if text != "" {
		frame["text"] = text
	}
	if errMsg != "" {
		frame["error"] = errMsg
	}
	s.write(frame)
}

func (s *chatStreamSink) write(frame map[string]any) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		s.aborted = true
		return
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", encoded); err != nil {
		s.aborted = true
		return
	}
	s.flusher.Flush()
}
