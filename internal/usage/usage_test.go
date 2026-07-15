package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixedNow is the timestamp the TS assistant-events.test.ts pins.
var fixedNow = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC).UnixMilli() // 2026-07-11T12:00:00.000Z

func TestBuildUsageEvents_OneEventPerNonZeroAxisPlusMessages(t *testing.T) {
	events := BuildUsageEvents(BuildUsageInput{
		ProjectName:    "demo-project",
		ConversationID: "conv-123",
		Model:          "claude-sonnet-4-6",
		Tokens:         UsageTokens{InputTokens: 10, OutputTokens: 20, CachedInputTokens: 0},
		NowMillis:      fixedNow,
	})

	gotMeters := make([]string, len(events))
	for i, e := range events {
		gotMeters[i] = e.MeterName
		if got := e.Dimensions["model"]; got != "claude-sonnet-4-6" {
			t.Errorf("event %d dimension model = %q, want claude-sonnet-4-6", i, got)
		}
		if !IsULID(e.EventID) {
			t.Errorf("event %d eventID %q is not a ULID", i, e.EventID)
		}
	}
	want := []string{MeterInputTokens, MeterOutputTokens, MeterMessages}
	if strings.Join(gotMeters, ",") != strings.Join(want, ",") {
		t.Errorf("meters = %v, want %v (cache axes must be dropped when zero)", gotMeters, want)
	}
}

func TestBuildUsageEvents_TimestampMatchesJSToISOString(t *testing.T) {
	events := BuildUsageEvents(BuildUsageInput{
		ProjectName: "p", ConversationID: "c", Model: "m",
		Tokens: UsageTokens{InputTokens: 1}, NowMillis: fixedNow,
	})
	if events[0].Timestamp != "2026-07-11T12:00:00.000Z" {
		t.Errorf("timestamp = %q, want 2026-07-11T12:00:00.000Z", events[0].Timestamp)
	}
}

func TestBuildToolInvocationEvent(t *testing.T) {
	e := BuildToolInvocationEvent(BuildToolInvocationInput{
		ProjectName:     "demo-project",
		ConversationID:  "conv-123",
		ConversationUID: "uid-1",
		ServiceName:     "streaming.streamco.example",
		NowMillis:       fixedNow,
	})
	if e.MeterName != MeterToolInvocations {
		t.Errorf("meter = %q, want %q", e.MeterName, MeterToolInvocations)
	}
	if e.Value != "1" {
		t.Errorf("value = %q, want 1", e.Value)
	}
	if e.Dimensions["service"] != "streaming.streamco.example" {
		t.Errorf("service dimension = %q", e.Dimensions["service"])
	}
	if e.Timestamp != "2026-07-11T12:00:00.000Z" {
		t.Errorf("timestamp = %q", e.Timestamp)
	}
	if e.Resource.Ref.Name != "conv-123" || e.Resource.Ref.UID != "uid-1" {
		t.Errorf("resource ref = %+v", e.Resource.Ref)
	}
	if !IsULID(e.EventID) {
		t.Errorf("eventID %q is not a ULID", e.EventID)
	}
}

func TestBuildToolInvocationEvent_FreshEventIDPerInvocation(t *testing.T) {
	in := BuildToolInvocationInput{ProjectName: "p", ConversationID: "c", ServiceName: "s", NowMillis: fixedNow}
	a := BuildToolInvocationEvent(in)
	b := BuildToolInvocationEvent(in)
	if a.EventID == b.EventID {
		t.Error("expected distinct eventIDs per invocation")
	}
}

func TestToCloudEvent_EnvelopeRules(t *testing.T) {
	e := BuildToolInvocationEvent(BuildToolInvocationInput{
		ProjectName: "demo-project", ConversationID: "conv-123",
		ServiceName: "streaming.streamco.example", NowMillis: fixedNow,
	})
	ce := ToCloudEvent(e, "http://portal.test/api/assistant")
	if ce.Type != MeterToolInvocations {
		t.Errorf("type = %q", ce.Type)
	}
	if ce.Subject != "projects/demo-project" {
		t.Errorf("subject = %q, want projects/demo-project", ce.Subject)
	}
	if ce.DataContentType != "application/json" {
		t.Errorf("datacontenttype = %q", ce.DataContentType)
	}
	if ce.SpecVersion != "1.0" {
		t.Errorf("specversion = %q", ce.SpecVersion)
	}
	if ce.Data.Value != "1" {
		t.Errorf("data.value = %q", ce.Data.Value)
	}
}

func TestToCloudEvent_OmitsEmptyDimensionsAndUID(t *testing.T) {
	e := Event{
		EventID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", MeterName: "m",
		Timestamp: "2026-07-11T12:00:00.000Z", ProjectRef: ProjectRef{Name: "p"},
		Value:      "1",
		Dimensions: map[string]string{}, // empty ⇒ omitted
		Resource:   EventResource{Ref: ResourceRef{Group: "g", Kind: "k", Namespace: "n", Name: "c"}},
	}
	b, err := json.Marshal(ToCloudEvent(e, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"dimensions"`)) {
		t.Errorf("empty dimensions must be omitted: %s", b)
	}
	if bytes.Contains(b, []byte(`"uid"`)) {
		t.Errorf("empty uid must be omitted: %s", b)
	}
}

// TestSinkGolden is the byte-compatibility proof: this package's CloudEvents,
// run through the QA sink normalizer's canonicalization (volatile fields
// blanked, keys sorted, deduped, sorted), must equal the recorded TS wire in
// e2e/golden/sink-cloudevents.golden.jsonl. If the golden moves, this fails.
func TestSinkGolden(t *testing.T) {
	const source = "http://127.0.0.1:7820/a2a"
	// The four events the e2e "diagnose" turn produces (mock model: 84 in / 46
	// out summed across two steps; one messages event; one tool invocation).
	tokenEvents := BuildUsageEvents(BuildUsageInput{
		ProjectName:    "demo-project",
		ConversationID: "conv-e2e",
		Model:          "patch-mock-v0",
		Tokens:         UsageTokens{InputTokens: 84, OutputTokens: 46},
		NowMillis:      fixedNow,
	})
	toolEvent := BuildToolInvocationEvent(BuildToolInvocationInput{
		ProjectName:    "demo-project",
		ConversationID: "conv-e2e",
		ServiceName:    "streaming.streamco.example",
		NowMillis:      fixedNow,
	})
	all := append(tokenEvents, toolEvent)

	lines := make([]string, 0, len(all))
	seen := map[string]bool{}
	for _, e := range all {
		line := canonicalize(t, ToCloudEvent(e, source))
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"

	golden, err := os.ReadFile("../../e2e/golden/sink-cloudevents.golden.jsonl")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(golden) {
		t.Errorf("sink wire drifted from TS golden.\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

// canonicalize reproduces the QA normalize-sink.mjs canonicalization: blank the
// volatile fields (id, time, data.resource.name) and re-serialize with sorted
// keys (Go marshals map[string]any keys in sorted order).
func canonicalize(t *testing.T, ce CloudEvent) string {
	t.Helper()
	raw, err := json.Marshal(ce)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["id"] = "<ULID>"
	m["time"] = "<TIME>"
	if data, ok := m["data"].(map[string]any); ok {
		if res, ok := data["resource"].(map[string]any); ok {
			res["name"] = "<CONTEXTID>"
		}
	}
	// JSON.stringify does not HTML-escape, so the golden has literal < >.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// ── ULID ──────────────────────────────────────────────────────

func TestNewULID_FormatAndTimestampPrefix(t *testing.T) {
	id := NewULID(fixedNow)
	if len(id) != 26 {
		t.Fatalf("ULID length = %d, want 26", len(id))
	}
	if !IsULID(id) {
		t.Fatalf("%q not a valid ULID", id)
	}
	// The 10-char time prefix must decode back to fixedNow.
	if got := decodeTime(id[:10]); got != fixedNow {
		t.Errorf("decoded time = %d, want %d", got, fixedNow)
	}
}

func decodeTime(prefix string) int64 {
	var t int64
	for _, c := range prefix {
		t = t*32 + int64(strings.IndexRune(crockford, c))
	}
	return t
}

// ── Emitter ───────────────────────────────────────────────────

func sampleEvents() []Event {
	return BuildUsageEvents(BuildUsageInput{
		ProjectName: "demo-project", ConversationID: "conv-1",
		Model: "patch-mock-v0", Tokens: UsageTokens{InputTokens: 10, OutputTokens: 5},
	})
}

func TestEmitter_NoopWithoutGateway(t *testing.T) {
	e := NewEmitter(EmitterConfig{Source: "http://svc/a2a"})
	r := e.Emit(context.Background(), sampleEvents())
	if !r.Noop || !r.OK {
		t.Errorf("want noop+ok, got %+v", r)
	}
}

func TestEmitter_PostsCloudEventsWithAPIKey(t *testing.T) {
	var gotURL, gotAPIKey, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotContentType = r.Header.Get("content-type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	e := NewEmitter(EmitterConfig{GatewayURL: srv.URL, APIKey: "secret-key", Source: "http://svc/a2a"})
	r := e.Emit(context.Background(), sampleEvents())

	if !r.OK || r.Noop || r.Status != http.StatusAccepted {
		t.Errorf("result = %+v", r)
	}
	if gotURL != "/cloudevents" {
		t.Errorf("path = %q, want /cloudevents", gotURL)
	}
	if gotAPIKey != "secret-key" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
	// Body is a JSON array of CloudEvents with no trailing newline.
	if bytes.HasSuffix(gotBody, []byte("\n")) {
		t.Error("body must not have a trailing newline (JSON.stringify parity)")
	}
	var batch []map[string]any
	if err := json.Unmarshal(gotBody, &batch); err != nil {
		t.Fatalf("body not a JSON array: %v", err)
	}
	if batch[0]["specversion"] != "1.0" || batch[0]["subject"] != "projects/demo-project" {
		t.Errorf("first event = %v", batch[0])
	}
}

func TestEmitter_ReportsFailureWithoutThrowing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	e := NewEmitter(EmitterConfig{GatewayURL: srv.URL, Source: "s"})
	r := e.Emit(context.Background(), sampleEvents())
	if r.OK || r.Status != http.StatusInternalServerError {
		t.Errorf("want ok=false status=500, got %+v", r)
	}
}

func TestEmitter_NeverThrowsOnNetworkError(t *testing.T) {
	// A gateway URL that refuses connections.
	e := NewEmitter(EmitterConfig{GatewayURL: "http://127.0.0.1:1", Source: "s"})
	r := e.Emit(context.Background(), sampleEvents())
	if r.OK || r.Error == "" {
		t.Errorf("want ok=false with error, got %+v", r)
	}
}

func TestEmitter_EmptyList(t *testing.T) {
	e := NewEmitter(EmitterConfig{GatewayURL: "http://c", Source: "s"})
	r := e.Emit(context.Background(), nil)
	if !r.OK || r.Noop || r.Count != 0 {
		t.Errorf("want ok, not-noop, count 0, got %+v", r)
	}
}

func TestEmitter_BatchCap(t *testing.T) {
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &batch)
		batchSizes = append(batchSizes, len(batch))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 250 events ⇒ 100 + 100 + 50.
	var events []Event
	for i := 0; i < 250; i++ {
		events = append(events, BuildToolInvocationEvent(BuildToolInvocationInput{
			ProjectName: "p", ConversationID: "c", ServiceName: "s", NowMillis: fixedNow,
		}))
	}
	e := NewEmitter(EmitterConfig{GatewayURL: srv.URL, Source: "s"})
	r := e.Emit(context.Background(), events)
	if !r.OK {
		t.Fatalf("result = %+v", r)
	}
	want := []int{100, 100, 50}
	if len(batchSizes) != 3 || batchSizes[0] != want[0] || batchSizes[1] != want[1] || batchSizes[2] != want[2] {
		t.Errorf("batch sizes = %v, want %v", batchSizes, want)
	}
}
