// A2A client construction for the patch CLI, built on the official a2a-go
// client (a2aclient + agentcard resolver) — no hand-rolled protocol code.
//
// The agent card is public, so it is fetched with the default resolver. The
// JSON-RPC transport is given an http.Client whose RoundTripper attaches the
// bearer token to every request; the well-known card fetch stays unauthed,
// matching the service (which serves the card publicly).
//
// The token is a [TokenSource], not a string, because the two entrypoints mint
// it differently: the standalone CLI has it in hand from PATCH_TOKEN, while the
// datumctl plugin shells out to datumctl's credentials helper for a short-lived
// one. Resolving per request rather than per process is what lets a long-lived
// `chat --tui` session outlive the token it started with.
package patchcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// TokenSource yields a bearer token for the assistant service. It is called
// once per outgoing request, so a source backed by a credentials helper can
// return a freshly minted token as the previous one ages out.
type TokenSource func() (string, error)

// StaticToken is the [TokenSource] for a token already in hand (the standalone
// CLI's --token / PATCH_TOKEN). An empty token leaves requests unauthenticated
// — the service answers with an auth error, which Run maps to exit 1.
func StaticToken(token string) TokenSource {
	return func() (string, error) { return token, nil }
}

// serviceClient is the a2a-go client plus the transport-level record of the
// last HTTP error body (see [httpErrRecorder]). It embeds the client, so
// callers use it exactly as they used *a2aclient.Client.
type serviceClient struct {
	*a2aclient.Client
	errs *httpErrRecorder
}

// resolveCard fetches the A2A agent card from the service's well-known path.
func resolveCard(ctx context.Context, baseURL string) (*a2a.AgentCard, error) {
	return agentcard.DefaultResolver.Resolve(ctx, baseURL)
}

// newClient resolves the card and builds an a2a-go client bound to the
// service's JSON-RPC interface, attaching a bearer token to protocol requests.
func newClient(ctx context.Context, baseURL string, token TokenSource) (*serviceClient, error) {
	card, err := resolveCard(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	errs := &httpErrRecorder{}
	httpClient := &http.Client{Transport: bearerTransport{token: token, errs: errs, base: http.DefaultTransport}}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(httpClient))
	if err != nil {
		return nil, err
	}
	return &serviceClient{Client: client, errs: errs}, nil
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
func requestCompact(ctx context.Context, baseURL string, token TokenSource, project, contextID string) error {
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

// maxConversationNameLen mirrors the service's own cap (history.MaxNameLen) so
// the TUI can say "too long" without a round trip. It is duplicated rather than
// imported because this package is linked into a CLI, and internal/history
// would drag the Postgres driver in with it; the service validates regardless,
// so a drift here costs a worse message, never a wrong outcome.
const maxConversationNameLen = 80

// requestRename calls POST /v1/conversations/rename — the sibling of
// /v1/compact for naming a conversation, outside the A2A surface for the same
// reason (a store mutation, not a message to answer). Same bearer-token auth
// ([bearerTransport]) as every other call this CLI makes.
func requestRename(ctx context.Context, baseURL string, token TokenSource, project, contextID, name string) error {
	body, err := json.Marshal(map[string]string{"contextId": contextID, "projectName": project, "name": name})
	if err != nil {
		return err
	}
	url := strings.TrimRight(baseURL, "/") + "/v1/conversations/rename"
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
		return fmt.Errorf("rename: %s", msg)
	}
	return nil
}

// httpErrRecorder remembers the message from the last non-2xx response the
// transport saw.
//
// The service reports auth failures as HTTP 401/403 with a JSON body that says
// what actually went wrong ("Unknown or invalid bearer token", or which project
// a token does not grant). a2a-go's JSON-RPC transport discards that body and
// surfaces only `unexpected HTTP status: 403 Forbidden`, so the transport is
// the last place the real message is still in hand. [friendlyError] pairs the
// two back up.
type httpErrRecorder struct {
	mu     sync.Mutex
	status int
	msg    string
}

func (r *httpErrRecorder) record(status int, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.msg = status, msg
}

// last returns the recorded status and message, if any.
func (r *httpErrRecorder) last() (int, string) {
	if r == nil {
		return 0, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.msg
}

// maxErrBody caps how much of an error response is read before it is handed
// back to the client. Error bodies are a single short JSON object; the cap is
// there so a misrouted request to something that streams cannot balloon.
const maxErrBody = 8 << 10

// bearerTransport is an http.RoundTripper that injects "Authorization: Bearer
// <token>" on every request, and records the body of any error response for
// [httpErrRecorder].
type bearerTransport struct {
	token TokenSource
	errs  *httpErrRecorder
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.token != nil {
		token, err := t.token()
		if err != nil {
			return nil, err
		}
		if token != "" {
			// Clone before mutating headers; RoundTrippers must not modify the
			// caller's request.
			req = req.Clone(req.Context())
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	res, err := base.RoundTrip(req)
	if err != nil || t.errs == nil || res.StatusCode < 400 {
		return res, err
	}
	t.errs.record(res.StatusCode, errorMessageFrom(res))
	return res, nil
}

// errorMessageFrom reads an error response's body far enough to pull out the
// service's {"error":"..."} message, then puts the bytes back so the caller
// still sees an intact response.
func errorMessageFrom(res *http.Response) string {
	if res.Body == nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxErrBody))
	res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errBody) == nil && errBody.Error != "" {
		return errBody.Error
	}
	return strings.TrimSpace(string(body))
}
