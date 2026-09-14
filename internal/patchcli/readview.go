// How the read views (`conversations`, `gaps`) reach the aggregated apiserver —
// and, despite the name, the conversation lifecycle writes too (archive,
// unarchive, delete). Those are writes to the same aggregated API the listing
// comes from, authorized against the same project, so they travel the same
// way as the reads: routing them through the assistant service instead would
// mean a second path, and potentially a second identity, for what the caller
// sees as one resource.
//
// There are two transports, and which one is used decides WHOSE IDENTITY the
// request carries:
//
//   - Milo directly, over HTTPS, with a token from datumctl's credentials
//     helper. This is the same identity `chat` already uses, and the same one
//     the caller selected with `datumctl context use`.
//   - kubectl, with the caller's ambient kubeconfig — the fallback for the
//     standalone `patch` binary, which has no datumctl to ask, and for anyone
//     who passes --kubeconfig deliberately.
//
// The first is preferred wherever it is available. The read views used to use
// only the second, on the reasoning that reading a Kubernetes API is a
// different act from calling the assistant service and should therefore use
// the caller's Kubernetes identity. That reasoning does not survive contact
// with the platform: Milo accepts the very token datumctl already mints, so
// there is no second identity to honor — only a second way to be pointed
// somewhere unintended. `kubectl` resolves its context from KUBECONFIG and
// ~/.kube/config, neither of which has anything to do with the datumctl
// context, so `datumctl assistant conversations list` would happily ask whichever
// unrelated cluster happened to be current and report that the API does not
// exist. That reads as "this feature is not deployed" when the truth is "you
// asked the wrong server".
//
// Both transports address a raw path rather than a named resource, so neither
// depends on client-side discovery. That matters beyond tidiness: kubectl
// caches discovery per API host, so a freshly registered APIService keeps
// reporting `the server doesn't have a resource type "conversations"` from a
// stale cache long after it is live.
package patchcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// readViewTimeout bounds one read-view request. These are interactive
// list/get/archive/delete calls against a control plane, not model calls, so
// they should fail fast rather than hang a terminal.
const readViewTimeout = 30 * time.Second

// projectControlPlanePath is the prefix Milo routes a project's aggregated
// APIs under. kubectl contexts written by `datumctl auth update-kubeconfig`
// already embed it in the server URL, which is why only the direct transport
// adds it.
const projectControlPlanePath = "/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane"

// ReadView reaches raw aggregated-API paths on the caller's behalf: it fetches
// them, and — for the conversation lifecycle — patches, replaces and deletes
// them.
type ReadView struct {
	// apiHost is Milo's host (DATUM_API_HOST), with or without a scheme.
	// Empty selects the kubectl transport.
	apiHost string
	// token mints a bearer token for apiHost. Nil selects the kubectl
	// transport even when apiHost is set — a host with no way to
	// authenticate to it is not usable.
	token TokenSource
	// kubeconfig overrides kubectl's normal resolution. Non-empty forces the
	// kubectl transport: passing --kubeconfig is an explicit request for that
	// identity, and silently ignoring it would be worse than not offering it.
	kubeconfig string
	// client is the HTTP client for the direct transport. Nil uses a default
	// bounded by readViewTimeout.
	client *http.Client
}

// readViewFor builds a [ReadView] from an invocation.
func ReadViewFor(inv Invocation) ReadView {
	return ReadView{apiHost: inv.APIHost, token: inv.Token, kubeconfig: inv.Kubeconfig}
}

// direct reports whether this ReadView talks to Milo itself.
func (r ReadView) direct() bool {
	return r.kubeconfig == "" && r.apiHost != "" && r.token != nil
}

// get fetches one aggregated-API path and returns the raw response body.
//
// path is the group-relative path (e.g.
// /apis/assistant.miloapis.com/v1alpha1/namespaces/<project>/conversations),
// identical for both transports; only the prefix in front of it differs.
func (r ReadView) get(ctx context.Context, project, path string) ([]byte, error) {
	return r.getAccept(ctx, project, path, "")
}

// getAccept is [ReadView.get] asking for a particular representation. Only the
// direct transport can send a header — `kubectl get --raw` has no equivalent —
// so an accept the transport cannot honor is simply not sent, and callers must
// recognize the default representation coming back rather than assume the one
// they asked for (see discoverResourceKinds).
func (r ReadView) getAccept(ctx context.Context, project, path, accept string) ([]byte, error) {
	if !r.direct() {
		return kubectlJSON(ctx, r.kubeconfig, "get", "--raw", path)
	}
	return r.send(ctx, project, http.MethodGet, path, accept, "", nil, http.StatusOK)
}

// send is one request over the direct transport: method to path under the
// project's control plane, with an optional body of contentType, succeeding
// only on one of the expected statuses. Everything else — the apiserver's own
// error message included — comes back as an error, via [apiStatusError].
func (r ReadView) send(ctx context.Context, project, method, path, accept, contentType string, body []byte, expect ...int) ([]byte, error) {
	token, err := r.token()
	if err != nil {
		return nil, err
	}

	base := r.apiHost
	if !strings.Contains(base, "://") {
		// DATUM_API_HOST is a bare hostname ("api.datum.net"). Default to
		// HTTPS rather than letting url.Parse read the host as a path.
		base = "https://" + base
	}
	endpoint := strings.TrimRight(base, "/") +
		fmt.Sprintf(projectControlPlanePath, url.PathEscape(project)) + path

	reqCtx, cancel := context.WithTimeout(ctx, readViewTimeout)
	defer cancel()

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, reqBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	client := r.client
	if client == nil {
		client = &http.Client{Timeout: readViewTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if !slices.Contains(expect, resp.StatusCode) {
		return nil, apiStatusError(resp.StatusCode, respBody)
	}
	return respBody, nil
}

// mergePatchContentType is the one patch flavor the archive write sends: a
// JSON merge patch touches exactly the fields it names, so {"spec":{"archived":
// true}} cannot disturb anything else on the object.
const mergePatchContentType = "application/merge-patch+json"

// setConversationArchived archives (archived=true) or unarchives one
// conversation and returns it as the apiserver reports it afterwards.
//
// The two transports cannot send the same request. The direct transport sends
// the merge patch. kubectl has `get --raw`, `replace --raw` and `delete --raw`
// but no `patch --raw`, so that transport reads the object, flips
// spec.archived, and PUTs the whole thing back — which is safe only because
// the apiserver's update ignores everything in the body but spec.archived; a
// stale status or metadata in the round-tripped object cannot be written.
func (r ReadView) setConversationArchived(ctx context.Context, project, contextID string, archived bool) (assistantv1alpha1.Conversation, error) {
	path := conversationPath(project, contextID)
	var (
		out []byte
		err error
	)
	if r.direct() {
		patch := fmt.Appendf(nil, `{"spec":{"archived":%t}}`, archived)
		out, err = r.send(ctx, project, http.MethodPatch, path, "", mergePatchContentType, patch, http.StatusOK)
	} else {
		out, err = r.replaceArchivedViaKubectl(ctx, path, archived)
	}
	if err != nil {
		return assistantv1alpha1.Conversation{}, err
	}
	var conv assistantv1alpha1.Conversation
	if err := json.Unmarshal(out, &conv); err != nil {
		return assistantv1alpha1.Conversation{}, fmt.Errorf("could not parse conversation response: %w", err)
	}
	return conv, nil
}

// replaceArchivedViaKubectl is the kubectl transport's archive: GET, set
// spec.archived, PUT. The object is carried as a generic map rather than the
// typed struct so a field this client does not know about survives the round
// trip instead of being silently dropped from the body.
func (r ReadView) replaceArchivedViaKubectl(ctx context.Context, path string, archived bool) ([]byte, error) {
	current, err := kubectlJSON(ctx, r.kubeconfig, "get", "--raw", path)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(current, &obj); err != nil {
		return nil, fmt.Errorf("could not parse conversation response: %w", err)
	}
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	spec["archived"] = archived
	obj["spec"] = spec
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return kubectlRun(ctx, r.kubeconfig, body, "replace", "--raw", path, "-f", "-")
}

// deleteConversation permanently deletes one conversation. 202 is accepted
// alongside 200 because that is how an apiserver says "deletion is under way";
// this one always deletes immediately, but a client that treated the
// Kubernetes-standard answer as a failure would be wrong the day it did not.
func (r ReadView) deleteConversation(ctx context.Context, project, contextID string) error {
	path := conversationPath(project, contextID)
	if !r.direct() {
		_, err := kubectlJSON(ctx, r.kubeconfig, "delete", "--raw", path)
		return err
	}
	_, err := r.send(ctx, project, http.MethodDelete, path, "", "", nil, http.StatusOK, http.StatusAccepted)
	return err
}

// getConversation fetches one conversation object — how `conversations
// delete` names what it is about to destroy before asking.
func (r ReadView) getConversation(ctx context.Context, project, contextID string) (assistantv1alpha1.Conversation, error) {
	out, err := r.get(ctx, project, conversationPath(project, contextID))
	if err != nil {
		return assistantv1alpha1.Conversation{}, err
	}
	var conv assistantv1alpha1.Conversation
	if err := json.Unmarshal(out, &conv); err != nil {
		return assistantv1alpha1.Conversation{}, fmt.Errorf("could not parse conversation response: %w", err)
	}
	return conv, nil
}

// apiStatusError renders a non-200 as the apiserver's own message where it sent
// one. A Kubernetes API error body carries a human-readable `message` that says
// far more than the status line — "conversations.assistant.miloapis.com is
// forbidden: User ... cannot list resource ..." versus a bare 403.
func apiStatusError(code int, body []byte) error {
	var status struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(body, &status); err == nil && status.Message != "" {
		return fmt.Errorf("%s", status.Message)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return fmt.Errorf("%s", http.StatusText(code))
	}
	return fmt.Errorf("%s: %s", http.StatusText(code), trimmed)
}

// readViewErrorText renders a failed read-view fetch as one line, WITHOUT a
// "patch: " prefix — callers add that, and some wrap this in a larger message
// that carries its own. The kubectl transport needs separate handling because
// kubectl's real message arrives on the ExitError's stderr, not in the error.
func readViewErrorText(r ReadView, err error) string {
	if r.direct() {
		return err.Error()
	}
	return kubectlErrorText(err)
}

// readViewError is [readViewErrorText] as an error. The direct transport's
// error is returned as-is so callers can still errors.As through it — a
// failure to mint a token is not a failure of whatever was being fetched.
func readViewError(r ReadView, err error) error {
	if r.direct() {
		return err
	}
	return errors.New(kubectlErrorText(err))
}
