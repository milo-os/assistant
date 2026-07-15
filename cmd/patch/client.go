// A2A client construction for the `patch` CLI, built on the official a2a-go
// client (a2aclient + agentcard resolver) — no hand-rolled protocol code.
//
// The agent card is public, so it is fetched with the default resolver. The
// JSON-RPC transport is given an http.Client whose RoundTripper attaches the
// bearer token to every request; the well-known card fetch stays unauthed,
// matching the service (which serves the card publicly).
package main

import (
	"context"
	"net/http"

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
// project. The extension is set on both the message and the request params so
// either read path resolves it.
func buildMessage(text, project string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.SetMeta("projectName", project)
	return msg
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
