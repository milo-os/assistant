package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/history"
)

// renameRequestBody is the POST /v1/conversations/rename body. contextId and
// projectName mean exactly what they do on POST /v1/compact; name is what the
// user wants this conversation called.
type renameRequestBody struct {
	ContextID   string `json:"contextId"`
	ProjectName string `json:"projectName"`
	Name        string `json:"name"`
}

// renameHandler serves POST /v1/conversations/rename — the "/rename" command's
// endpoint, a sibling of POST /v1/compact and shaped like it: a small REST
// route outside the A2A protocol (there is no message to answer, just a store
// mutation) reusing the same bearer-token authn and project authz POST /a2a
// applies, so no endpoint here carries a second auth scheme.
//
// Unlike compaction this needs no agent at all — it writes the conversation
// row the chat path already owns — so it takes the [history.Renamer] directly
// rather than going through the a2a runner seam.
func renameHandler(renamer history.Renamer, authenticator auth.Authenticator, authorizer auth.Authorizer, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		principal, err := authenticateBearer(ctx, authenticator, r)
		if err != nil {
			writeAuthErrWith(w, logger, err, "Authentication failed")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var body renameRequestBody
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
		// Validate before authorizing so a malformed name is a 400 whether or
		// not the caller holds the project — the two answers are independent.
		// Over-long is rejected rather than silently truncated: the user typed
		// this, so they should be told it didn't fit.
		name := strings.TrimSpace(body.Name)
		if name == "" {
			writeAuthError(w, http.StatusBadRequest, "name is required")
			return
		}
		if utf8.RuneCountInString(name) > history.MaxNameLen {
			writeAuthError(w, http.StatusBadRequest, "name is too long (max 80 characters)")
			return
		}

		if err := authorizer.AuthorizeProject(ctx, principal, body.ProjectName); err != nil {
			writeAuthErrWith(w, logger, err, "Authorization failed")
			return
		}

		if renamer == nil {
			writeAuthError(w, http.StatusServiceUnavailable, "Renaming is not available")
			return
		}

		err = renamer.Rename(ctx, body.ProjectName, body.ContextID, name)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, map[string]any{"renamed": true, "name": history.NormalizeName(name)})
		case errors.Is(err, history.ErrConversationNotFound):
			writeAuthError(w, http.StatusNotFound, "Conversation not found")
		default:
			logger.Warn("http.rename.failed",
				"projectName", body.ProjectName, "contextId", body.ContextID, "error", err.Error())
			writeAuthError(w, http.StatusInternalServerError, "Rename failed: "+err.Error())
		}
	}
}
