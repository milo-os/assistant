// A2A client construction for the `patch` CLI, built on the official a2a-go
// client (a2aclient + agentcard resolver) — no hand-rolled protocol code.
//
// The agent card is public, so it is fetched with the default resolver. The
// JSON-RPC transport is given an http.Client whose RoundTripper attaches the
// bearer token to every request; the well-known card fetch stays unauthed,
// matching the service (which serves the card publicly).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// resolveCard fetches the A2A agent card from the service's well-known path.
func resolveCard(ctx context.Context, baseURL string) (*a2a.AgentCard, error) {
	return agentcard.DefaultResolver.Resolve(ctx, baseURL)
}

// newClient resolves the card and builds an a2a-go client bound to the
// service's JSON-RPC interface, attaching the bearer token (if any) to
// protocol requests.
func newClient(ctx context.Context, baseURL, token string) (*a2aclient.Client, error) {
	card, err := resolveCard(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}}
	return a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(httpClient))
}

// buildMessage builds the user message for a chat request: a single text part
// plus the projectName metadata extension the service uses to select the Milo
// project. A non-empty contextID continues that conversation — the service
// replays its history into the turn's prompt.
func buildMessage(text, project, contextID string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.SetMeta("projectName", project)
	msg.ContextID = contextID
	return msg
}

// ErrNothingToCompact is returned by [requestCompact] when the server ran the
// request successfully but found nothing to compact (its compacted:false
// response — see internal/server/compact.go). Distinguished from a transport
// or server-side failure so callers (patch compact, the chat TUI's /compact)
// can show a friendlier message instead of treating it as an error.
var ErrNothingToCompact = errors.New("nothing to compact")

// requestCompact calls POST /v1/compact — the manual, user-triggered history
// compaction endpoint outside the A2A JSON-RPC surface (there is no message
// to answer, just a store mutation) — for one project/conversation. It reuses
// this CLI's usual bearer-token auth ([bearerTransport]) rather than a
// separate scheme, matching the server's own reuse of POST /a2a's auth for
// this route.
func requestCompact(ctx context.Context, baseURL, token, project, contextID string) error {
	body, err := json.Marshal(map[string]string{"contextId": contextID, "projectName": project})
	if err != nil {
		return err
	}
	url := strings.TrimRight(baseURL, "/") + "/v1/compact"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&errBody)
		msg := errBody.Error
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("compact: %s", msg)
	}

	var out struct {
		Compacted bool   `json:"compacted"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return fmt.Errorf("compact: decode response: %w", err)
	}
	if !out.Compacted {
		return ErrNothingToCompact
	}
	return nil
}

// bearerTransport is an http.RoundTripper that injects "Authorization: Bearer
// <token>" on every request. An empty token leaves requests unauthenticated
// (the service answers with an auth error, which Run maps to exit 1).
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.token != "" {
		// Clone before mutating headers; RoundTrippers must not modify the
		// caller's request.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return base.RoundTrip(req)
}
