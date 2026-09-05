// How the read views (`conversations`, `gaps`) reach the aggregated apiserver.
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
// Both transports fetch a raw path rather than a named resource, so neither
// depends on client-side discovery. That matters beyond tidiness: kubectl
// caches discovery per API host, so a freshly registered APIService keeps
// reporting `the server doesn't have a resource type "conversations"` from a
// stale cache long after it is live.
package patchcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// readViewTimeout bounds one read-view request. These are interactive list/get
// calls against a control plane, not model calls, so they should fail fast
// rather than hang a terminal.
const readViewTimeout = 30 * time.Second

// projectControlPlanePath is the prefix Milo routes a project's aggregated
// APIs under. kubectl contexts written by `datumctl auth update-kubeconfig`
// already embed it in the server URL, which is why only the direct transport
// adds it.
const projectControlPlanePath = "/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane"

// ReadView fetches raw aggregated-API paths on the caller's behalf.
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
	if !r.direct() {
		return kubectlJSON(ctx, r.kubeconfig, "get", "--raw", path)
	}

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

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	client := r.client
	if client == nil {
		client = &http.Client{Timeout: readViewTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiStatusError(resp.StatusCode, body)
	}
	return body, nil
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
