package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
)

// A2A JSON-RPC method names as implemented by a2a-go v2 (real A2A v1.0). NOTE:
// these are PascalCase (SendMessage, …), NOT the v0.3-era message/send strings
// the TS service used — a deliberate wire change that this port adopts.
const (
	methodSendMessage          = "SendMessage"
	methodSendStreamingMessage = "SendStreamingMessage"
	methodGetTask              = "GetTask"
	methodCancelTask           = "CancelTask"
)

// authMiddleware enforces authentication (401) and project authorization (403)
// in front of the a2a-go JSON-RPC handler. Authentication is transport-level
// (bearer token → principal). Authorization needs the target project: for
// message methods it is read from the request body's projectName extension; for
// task methods it is read from the owning task's metadata via the shared store.
// Both stay HTTP-status responses, matching the TS service's auth semantics.
type authMiddleware struct {
	authenticator auth.Authenticator
	authorizer    auth.Authorizer
	taskStore     taskstore.Store
	logger        *slog.Logger
	next          http.Handler
}

func (m *authMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// ── AuthN ─────────────────────────────────────────────────
	token := auth.ExtractBearerToken(r.Header.Get("Authorization"))
	if token == "" {
		writeAuthError(w, http.StatusUnauthorized, "Missing bearer token")
		return
	}
	principal, err := m.authenticator.Authenticate(ctx, token)
	if err != nil {
		m.writeAuthErr(w, err, "Authentication failed")
		return
	}

	// Read the whole body so we can peek the method/params for authorization,
	// then restore it for the downstream JSON-RPC handler.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAuthError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	// ── AuthZ ─────────────────────────────────────────────────
	// A malformed body is left to the JSON-RPC handler to reject with the
	// proper parse/invalid-request error; we only authorize when we can.
	if err := m.authorize(ctx, principal, body); err != nil {
		m.writeAuthErr(w, err, "Authorization failed")
		return
	}

	m.next.ServeHTTP(w, r)
}

// authorize resolves the target project from the JSON-RPC request and checks it
// against the principal. It returns nil (allow) for requests it cannot or need
// not authorize (unknown method, missing project, unknown task) — those are
// handled downstream (InvalidParams / TaskNotFound), matching the TS flow.
func (m *authMiddleware) authorize(ctx context.Context, principal auth.Principal, body []byte) error {
	var peek struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return nil // let the JSON-RPC handler report the parse error
	}

	switch peek.Method {
	case methodSendMessage, methodSendStreamingMessage:
		project := projectFromSendParams(peek.Params)
		if project == "" {
			return nil // executor will reject with InvalidParams
		}
		return m.authorizer.AuthorizeProject(ctx, principal, project)

	case methodGetTask, methodCancelTask:
		id := taskIDFromParams(peek.Params)
		if id == "" {
			return nil
		}
		stored, err := m.taskStore.Get(ctx, a2a.TaskID(id))
		if err != nil {
			if errors.Is(err, a2a.ErrTaskNotFound) {
				return nil // library reports TaskNotFound
			}
			return nil
		}
		project := assistanta2a.ProjectName(nil, stored.Task.Metadata)
		if project == "" {
			return nil
		}
		return m.authorizer.AuthorizeProject(ctx, principal, project)

	default:
		return nil
	}
}

// projectFromSendParams reads the projectName extension from a SendMessage /
// SendStreamingMessage params object (message.metadata first, then params.metadata).
func projectFromSendParams(raw json.RawMessage) string {
	var p struct {
		Message  *a2a.Message   `json:"message"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return assistanta2a.ProjectName(p.Message, p.Metadata)
}

func taskIDFromParams(raw json.RawMessage) string {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.ID
}

// writeAuthErr maps an [*auth.Error] to its HTTP status, or falls back to 401.
func (m *authMiddleware) writeAuthErr(w http.ResponseWriter, err error, fallbackMsg string) {
	var authErr *auth.Error
	if errors.As(err, &authErr) {
		writeAuthError(w, authErr.Status, authErr.Message)
		return
	}
	m.logger.Error("a2a.auth.error", "error", err.Error())
	writeAuthError(w, http.StatusUnauthorized, fallbackMsg)
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
