package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	emitTimeout = 5 * time.Second
	// batchPath is Vector's http_server source path; the downstream gateway
	// enforces the 100-event batch cap.
	batchPath    = "/cloudevents"
	maxBatchSize = 100
)

// EmitResult reports the outcome of an [Emitter.Emit] call. It never carries an
// error return — emission is best-effort and must never fail a chat request.
type EmitResult struct {
	// OK is true when the collector accepted the batch (2xx) OR emission was disabled.
	OK bool
	// Noop is true when no collector is configured — events were intentionally dropped.
	Noop bool
	// Count is the number of events submitted (0 when noop or empty).
	Count int
	// Status is the HTTP status of the last request, if one was made.
	Status int
	// Error is a human-readable failure message when OK is false.
	Error string
}

// Emitter POSTs CloudEvent batches to the in-cluster usage collector (Vector).
// This is the blessed ingestion path (producer → Vector → gateway → NATS):
// Vector provides Tier-1 disk durability and injects the gateway api-key, so
// producers post plaintext with no auth.
type Emitter struct {
	gatewayURL string
	apiKey     string
	source     string
	client     *http.Client
	logger     *slog.Logger
}

// EmitterConfig configures [NewEmitter].
type EmitterConfig struct {
	// GatewayURL is the collector base URL (env USAGE_GATEWAY_URL). Empty ⇒ Emit is a no-op.
	GatewayURL string
	// APIKey is an optional collector api-key (env USAGE_GATEWAY_API_KEY).
	APIKey string
	// Source is the CloudEvents source URI identifying this producer.
	Source string
	// HTTPClient is injectable for tests; nil uses a client with the emit timeout.
	HTTPClient *http.Client
	// Logger is optional; nil discards emitter logs.
	Logger *slog.Logger
}

// NewEmitter constructs an [Emitter]. When GatewayURL is empty, Emit is a no-op
// so callers can wire emission unconditionally without breaking local dev.
func NewEmitter(cfg EmitterConfig) *Emitter {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: emitTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Emitter{
		gatewayURL: cfg.GatewayURL,
		apiKey:     cfg.APIKey,
		source:     cfg.Source,
		client:     client,
		logger:     logger,
	}
}

// Emit converts events to CloudEvents and POSTs them to <gateway>/cloudevents
// in batches of at most 100. It never returns an error: any failure is logged
// and reported via [EmitResult]. A missing gateway URL is a no-op.
func (e *Emitter) Emit(ctx context.Context, events []Event) EmitResult {
	if len(events) == 0 {
		return EmitResult{OK: true, Count: 0}
	}
	if e.gatewayURL == "" {
		e.logger.Debug("usage.emit.noop",
			"reason", "USAGE_GATEWAY_URL not configured",
			"eventCount", len(events))
		return EmitResult{OK: true, Noop: true, Count: len(events)}
	}

	cloudEvents := make([]CloudEvent, len(events))
	for i, ev := range events {
		cloudEvents[i] = ToCloudEvent(ev, e.source)
	}

	var lastStatus int
	for _, batch := range chunk(cloudEvents, maxBatchSize) {
		status, err := e.postBatch(ctx, batch)
		lastStatus = status
		if err != nil {
			return EmitResult{OK: false, Count: len(events), Status: status, Error: err.Error()}
		}
	}
	return EmitResult{OK: true, Count: len(events), Status: lastStatus}
}

func (e *Emitter) postBatch(ctx context.Context, batch []CloudEvent) (int, error) {
	target, err := batchURL(e.gatewayURL)
	if err != nil {
		return 0, err
	}

	// Match JavaScript's JSON.stringify: no HTML escaping of <, >, & (the wire
	// the TS emitter produces and the sink golden records), and no trailing
	// newline. json.Marshal would HTML-escape; the Encoder path disables it.
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(batch); err != nil {
		return 0, err
	}
	payload := bytes.TrimRight(body.Bytes(), "\n")

	reqCtx, cancel := context.WithTimeout(ctx, emitTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("content-type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("x-api-key", e.apiKey)
	}

	res, err := e.client.Do(req)
	if err != nil {
		e.logger.Warn("usage.emit.failed", "eventCount", len(batch), "error", err.Error())
		return 0, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		snippet := readSnippet(res.Body)
		e.logger.Warn("usage.emit.failed", "status", res.StatusCode, "eventCount", len(batch), "body", snippet)
		return res.StatusCode, fmt.Errorf("collector returned status %d: %s", res.StatusCode, snippet)
	}

	// 207 Multi-Status: some events were rejected structurally; the accepted
	// events are durable in NATS and will flow downstream. Log for operators.
	if res.StatusCode == http.StatusMultiStatus {
		e.logger.Warn("usage.emit.partial", "status", res.StatusCode, "eventCount", len(batch), "body", readSnippet(res.Body))
	} else {
		e.logger.Info("usage.emit.ok", "status", res.StatusCode, "eventCount", len(batch))
	}
	return res.StatusCode, nil
}

// batchURL resolves /cloudevents against the collector base URL, matching the
// TS `new URL('/cloudevents', gateway)` (an absolute path replaces the base path).
func batchURL(gateway string) (string, error) {
	u, err := url.Parse(gateway)
	if err != nil {
		return "", fmt.Errorf("invalid USAGE_GATEWAY_URL %q: %w", gateway, err)
	}
	u.Path = batchPath
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return string(b)
}

func chunk[T any](items []T, size int) [][]T {
	if len(items) <= size {
		return [][]T{items}
	}
	var out [][]T
	for i := 0; i < len(items); i += size {
		end := i + size
		if end > len(items) {
			end = len(items)
		}
		out = append(out, items[i:end])
	}
	return out
}
