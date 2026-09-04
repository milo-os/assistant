package capability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpFetchTimeout bounds a single capability-document fetch. The source is
// consulted once per conversation on a request-serving path, so the budget is
// small: a slow or hung provider must degrade to "no capabilities", never
// stall a chat.
const httpFetchTimeout = 5 * time.Second

// HTTPSource is a [Source] backed by the capability-provider HTTP API. For each
// project it fetches
//
//	GET {baseURL}/projects/{projectName}/capability-documents
//
// and parses the response with the same validation path as [FixtureSource]
// (unknown fields ignored, individual invalid documents skipped with a
// warning). There is no caching in v0 — every call performs a fresh fetch.
//
// The source degrades rather than fails: any transport error, non-2xx status,
// body-read error, or malformed root JSON is logged and yields an empty
// document set (nil error). That matches the assistant's fixture-missing
// behavior — the agent composes built-ins only — so a provider outage can
// never fail a chat. It never panics.
type HTTPSource struct {
	baseURL string
	client  *http.Client
	logger  *slog.Logger
}

// NewHTTPSource returns an HTTP source targeting baseURL (the capability
// provider's base URL, e.g. http://capability-adapter). A trailing slash is
// tolerated. A nil client uses a default client with a 5s timeout; a nil
// logger discards degradation warnings.
func NewHTTPSource(baseURL string, client *http.Client, logger *slog.Logger) *HTTPSource {
	if client == nil {
		client = &http.Client{Timeout: httpFetchTimeout}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &HTTPSource{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
		logger:  logger,
	}
}

// Documents fetches and parses projectName's capability documents. On any
// failure it logs and returns an empty set (nil error) — see [HTTPSource].
func (s *HTTPSource) Documents(ctx context.Context, projectName string) ([]CapabilityDocument, error) {
	endpoint := fmt.Sprintf("%s/projects/%s/capability-documents",
		s.baseURL, url.PathEscape(projectName))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		// A malformed baseURL is the only way here; degrade like any other
		// fetch failure rather than surfacing an error onto the chat path.
		s.degrade(projectName, endpoint, "build_request", err)
		return nil, nil
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		s.degrade(projectName, endpoint, "transport", err)
		return nil, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.degrade(projectName, endpoint, "status",
			fmt.Errorf("unexpected status %d", resp.StatusCode))
		return nil, nil
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.degrade(projectName, endpoint, "read_body", err)
		return nil, nil
	}

	docs, err := ParseDocuments(raw, func(index int, skipErr error) {
		s.logger.Warn("capability.http.entry_skipped",
			"url", endpoint, "projectName", projectName,
			"index", index, "error", skipErr.Error())
	})
	if err != nil {
		s.degrade(projectName, endpoint, "parse", err)
		return nil, nil
	}
	return docs, nil
}

// degrade logs a fetch failure at warn level. The assistant continues with no
// provider capabilities.
func (s *HTTPSource) degrade(projectName, endpoint, stage string, err error) {
	s.logger.Warn("capability.http.fetch_failed",
		"url", endpoint, "projectName", projectName,
		"stage", stage, "error", err.Error())
}
