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

// maxRequestBodyBytes caps the JSON-RPC body the middleware buffers before
// peeking the method/params. A2A requests are small (a message plus a little
// metadata), so a few MiB is generous headroom; the cap stops an authenticated
// caller from streaming a multi-GB body into the unbounded io.ReadAll below and
// OOM-killing the pod.
const maxRequestBodyBytes = 4 << 20 // 4 MiB

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
	// then restore it for the downstream JSON-RPC handler. Cap the read: an
	// over-limit body trips http.MaxBytesReader before it is fully buffered, so
	// we answer 413 instead of OOMing on an unbounded io.ReadAll.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAuthError(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
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
// against the principal. It is DENY-BY-DEFAULT: only the four project-scoped
// methods (SendMessage/SendStreamingMessage/GetTask/CancelTask) can be allowed,
// and only when their project check passes. Every other method a2a-go v2
// dispatches is rejected outright — see the default case for why.
//
// For an allowed method it still returns nil (defer to the handler) when it
// cannot resolve the project (missing project, unknown task); those surface
// downstream as InvalidParams / TaskNotFound, matching the TS flow.
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
		// DENY-BY-DEFAULT. a2a-go v2 also dispatches ListTasks, SubscribeToTask
		// and the push-config methods (CreateTaskPushNotificationConfig,
		// GetTaskPushNotificationConfig, ListTaskPushNotificationConfigs,
		// DeleteTaskPushNotificationConfig) plus GetExtendedAgentCard.
		//
		// ListTasks stays DENIED even though the durable store now carries the
		// owning project on every row (internal/taskstore): a2a-go's ListTasks
		// RPC — and the a2astore.Store.List signature it dispatches to — carry NO
		// caller identity, so the handler cannot pass the caller's granted
		// projects into the store. Scoping ListTasks safely therefore needs a
		// dedicated endpoint that threads the principal's grants into
		// PostgresStore.ListForProjects; until that endpoint exists, an exposed
		// ListTasks would hand every project's tasks — with their message
		// History — to any valid token, even a zero-grant one, so we reject it
		// here. (The store's interface-level List is itself tenant-safe by
		// default: with no scope on the context it returns an empty page.)
		//
		// The same reasoning denies SubscribeToTask and the push-config methods.
		// This also covers truly-unknown methods, which a2a-go would otherwise
		// pass straight to the handler.
		return auth.Unauthorized("Method not permitted")
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
