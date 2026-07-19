package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
)

// compactRequestBody is the POST /v1/compact body. contextId identifies the
// conversation (same meaning as the A2A contextId); projectName is required
// for the same reason SendMessage requires it — it is both the authorization
// key and (via History) part of the storage key.
type compactRequestBody struct {
	ContextID   string `json:"contextId"`
	ProjectName string `json:"projectName"`
}

// compactHandler serves POST /v1/compact, the manual, user-triggered analog
// of "/compact": unlike a normal turn it doesn't go through POST /a2a because
// there is no message to answer, just a store mutation to perform. It reuses
// the exact bearer-token authn/project authz POST /a2a applies (via
// authenticateBearer and the same Authorizer) so this endpoint carries no
// separate auth scheme, and drives the same [assistanta2a.Compactor] wired
// from the same agent.Conversation the A2A executor runs turns against (see
// cmd/assistant's runner.go and main.go) rather than a second instance of it.
//
// Unlike a normal turn's fail-open compaction, a failure here is a real error
// response — the whole point of a manual command is that the user sees
// whether it worked.
func compactHandler(compactor assistanta2a.Compactor, authenticator auth.Authenticator, authorizer auth.Authorizer, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		principal, err := authenticateBearer(ctx, authenticator, r)
		if err != nil {
			writeAuthErrWith(w, logger, err, "Authentication failed")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var body compactRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeAuthError(w, http.StatusRequestEntityTooLarge, "Request body too large")
				return
			}
			writeAuthError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}
		if body.ContextID == "" {
			writeAuthError(w, http.StatusBadRequest, "contextId is required")
			return
		}
		if body.ProjectName == "" {
			writeAuthError(w, http.StatusBadRequest, "projectName is required")
			return
		}

		if err := authorizer.AuthorizeProject(ctx, principal, body.ProjectName); err != nil {
			writeAuthErrWith(w, logger, err, "Authorization failed")
			return
		}

		if compactor == nil {
			writeAuthError(w, http.StatusServiceUnavailable, "Compaction is not available")
			return
		}

		err = compactor.Compact(ctx, assistanta2a.CompactRequest{
			ProjectName: body.ProjectName,
			ContextID:   body.ContextID,
		})
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, map[string]any{"compacted": true})
		case errors.Is(err, assistanta2a.ErrNothingToCompact):
			// Not an error from the caller's point of view: there was simply
			// nothing to do. 200 with compacted:false lets callers (the TUI)
			// distinguish "nothing to do" from a real failure without parsing
			// error text.
			writeJSON(w, http.StatusOK, map[string]any{"compacted": false, "reason": "nothing to compact"})
		default:
			logger.Warn("http.compact.failed",
				"projectName", body.ProjectName, "contextId", body.ContextID, "error", err.Error())
			writeAuthError(w, http.StatusInternalServerError, "Compaction failed: "+err.Error())
		}
	}
}
