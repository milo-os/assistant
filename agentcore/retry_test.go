package agentcore

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// errPart builds a terminal error part carrying a classified [ModelError], the
// way an adapter reports a failed model stream.
func errPart(class ErrorClass, retryAfter time.Duration, msg string) StreamPart {
	return StreamPart{
		Kind:         StreamPartError,
		FinishReason: FinishError,
		Err:          NewModelError(class, retryAfter, errors.New(msg)),
	}
}

// fastRetry keeps backoff negligible so the retry tests run instantly.
var fastRetry = LoopOptions{RetryBaseDelay: time.Microsecond, RetryMaxDelay: time.Millisecond}

// ── A: retry succeeds after a transient failure ───────────────

// TestRetry_RateLimitedThenSuccess pins that a 429 on the first attempt, before
// any output has streamed, is retried and the second attempt's answer is the
// one delivered — the turn survives a transient rate limit instead of failing.
func TestRetry_RateLimitedThenSuccess(t *testing.T) {
	m := &scriptedModel{turns: [][]StreamPart{
		{errPart(ErrClassRateLimited, 0, "429 rate limited")},
		{textDelta("recovered answer"), stepFinish(Usage{Input: 10, Output: 5}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("hi")},
		RetryBaseDelay: fastRetry.RetryBaseDelay, RetryMaxDelay: fastRetry.RetryMaxDelay,
	}))

	fin := terminal(t, parts)
	if fin.Kind != StreamPartFinish || fin.FinishReason != FinishStop {
		t.Fatalf("want finish/stop after retry, got %v/%v (err=%v)", fin.Kind, fin.FinishReason, fin.Err)
	}
	if m.call != 2 {
		t.Fatalf("want exactly 2 attempts (retry once), got %d", m.call)
	}
	// Only the successful attempt's usage is billed — the failed 429 attempt
	// produced no step-finish and must not be counted.
	if fin.TotalUsage != (Usage{Input: 10, Output: 5}) {
		t.Fatalf("failed attempt must not double-count usage, got %+v", fin.TotalUsage)
	}
	var gotText string
	for _, p := range parts {
		if p.Kind == StreamPartTextDelta {
			gotText += p.Text
		}
	}
	if gotText != "recovered answer" {
		t.Fatalf("text = %q, want the retried attempt's answer", gotText)
	}
}

// ── B: give up on a persistent retryable failure ──────────────

// TestRetry_PersistentOverloadGivesUp pins that a provider that stays overloaded
// (529) is retried up to the bound and then surfaces the error rather than
// looping forever.
func TestRetry_PersistentOverloadGivesUp(t *testing.T) {
	turns := make([][]StreamPart, 0, 10)
	for i := 0; i < 10; i++ {
		turns = append(turns, []StreamPart{errPart(ErrClassOverloaded, 0, "529 overloaded")})
	}
	m := &scriptedModel{turns: turns}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("hi")}, MaxRetries: 3,
		RetryBaseDelay: fastRetry.RetryBaseDelay, RetryMaxDelay: fastRetry.RetryMaxDelay,
	}))

	fin := terminal(t, parts)
	if fin.Kind != StreamPartError {
		t.Fatalf("persistent overload must surface an error, got %v", fin.Kind)
	}
	if fin.FinishReason != FinishError {
		t.Fatalf("finish reason = %v, want error", fin.FinishReason)
	}
	if m.call != 4 { // 1 initial + 3 retries
		t.Fatalf("want MaxRetries+1 = 4 attempts, got %d", m.call)
	}
}

// ── C: never retry a terminal client error ────────────────────

// TestRetry_InvalidRequestNotRetried pins that a 400 is terminal: it surfaces
// immediately with no retry, because retrying an identical malformed request
// cannot succeed.
func TestRetry_InvalidRequestNotRetried(t *testing.T) {
	m := &scriptedModel{turns: [][]StreamPart{
		{errPart(ErrClassInvalidRequest, 0, "400 bad request")},
		{textDelta("should not reach here"), stepFinish(Usage{}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("hi")},
		RetryBaseDelay: fastRetry.RetryBaseDelay, RetryMaxDelay: fastRetry.RetryMaxDelay,
	}))

	if fin := terminal(t, parts); fin.Kind != StreamPartError {
		t.Fatalf("400 must surface an error, got %v", fin.Kind)
	}
	if m.call != 1 {
		t.Fatalf("a 400 must not be retried, got %d attempts", m.call)
	}
}

// TestRetry_AuthNotRetried pins that a 401/403 is terminal.
func TestRetry_AuthNotRetried(t *testing.T) {
	m := &scriptedModel{turns: [][]StreamPart{
		{errPart(ErrClassAuth, 0, "401 unauthorized")},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("hi")},
		RetryBaseDelay: fastRetry.RetryBaseDelay, RetryMaxDelay: fastRetry.RetryMaxDelay,
	}))
	if fin := terminal(t, parts); fin.Kind != StreamPartError {
		t.Fatalf("auth error must surface, got %v", fin.Kind)
	}
	if m.call != 1 {
		t.Fatalf("auth error must not be retried, got %d attempts", m.call)
	}
}

// ── D: a failure AFTER partial output is never silently retried ─

// TestRetry_MidStreamFailureNotRetried pins the "cannot retry once deltas are
// sent" rule: even a retryable class (overloaded) that strikes after text has
// streamed surfaces the error — retrying would emit a second, duplicate answer
// and bill it as a fresh turn.
func TestRetry_MidStreamFailureNotRetried(t *testing.T) {
	m := &scriptedModel{turns: [][]StreamPart{
		{textDelta("partial "), errPart(ErrClassOverloaded, 0, "529 mid-stream")},
		{textDelta("SECOND ATTEMPT"), stepFinish(Usage{}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("hi")},
		RetryBaseDelay: fastRetry.RetryBaseDelay, RetryMaxDelay: fastRetry.RetryMaxDelay,
	}))

	fin := terminal(t, parts)
	if fin.Kind != StreamPartError {
		t.Fatalf("a mid-stream failure must surface an error (no silent success), got %v", fin.Kind)
	}
	if m.call != 1 {
		t.Fatalf("a failure after streamed output must not be retried, got %d attempts", m.call)
	}
	for _, p := range parts {
		if p.Kind == StreamPartTextDelta && p.Text == "SECOND ATTEMPT" {
			t.Fatal("retried attempt's text leaked into an already-committed stream")
		}
	}
}

// ── retries disabled ──────────────────────────────────────────

// TestRetry_NegativeMaxRetriesDisables pins that MaxRetries < 0 turns retries
// off even for a retryable class.
func TestRetry_NegativeMaxRetriesDisables(t *testing.T) {
	m := &scriptedModel{turns: [][]StreamPart{
		{errPart(ErrClassRateLimited, 0, "429")},
		{textDelta("unreached"), stepFinish(Usage{}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("hi")}, MaxRetries: -1,
	}))
	if fin := terminal(t, parts); fin.Kind != StreamPartError {
		t.Fatalf("want error with retries disabled, got %v", fin.Kind)
	}
	if m.call != 1 {
		t.Fatalf("MaxRetries<0 must disable retry, got %d attempts", m.call)
	}
}

// ── Retry-After is honored ────────────────────────────────────

// TestRetry_HonorsRetryAfter pins that a server Retry-After hint is used as the
// backoff wait verbatim (here a small but observable delay), overriding the
// computed exponential backoff.
func TestRetry_HonorsRetryAfter(t *testing.T) {
	const wait = 40 * time.Millisecond
	m := &scriptedModel{turns: [][]StreamPart{
		{errPart(ErrClassRateLimited, wait, "429 with retry-after")},
		{textDelta("ok"), stepFinish(Usage{Input: 1}, FinishStop)},
	}}
	start := time.Now()
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("hi")},
		// A huge computed base would dwarf Retry-After if it were used instead.
		RetryBaseDelay: time.Microsecond, RetryMaxDelay: time.Millisecond,
	}))
	elapsed := time.Since(start)

	if fin := terminal(t, parts); fin.FinishReason != FinishStop {
		t.Fatalf("want success after honoring retry-after, got %v", fin.FinishReason)
	}
	if elapsed < wait {
		t.Fatalf("retry happened after %v, want >= Retry-After %v", elapsed, wait)
	}
}

// ── unit: classification, backoff, header parsing ─────────────

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		hint   bool
		want   ErrorClass
	}{
		{429, false, ErrClassRateLimited},
		{503, false, ErrClassOverloaded},
		{529, false, ErrClassOverloaded},
		{500, false, ErrClassOverloaded},
		{401, false, ErrClassAuth},
		{403, false, ErrClassAuth},
		{413, false, ErrClassContextLength},
		{400, false, ErrClassInvalidRequest},
		{400, true, ErrClassContextLength},
		{404, false, ErrClassUpstream},
	}
	for _, c := range cases {
		if got := ClassifyStatus(c.status, c.hint); got != c.want {
			t.Errorf("ClassifyStatus(%d, hint=%v) = %v, want %v", c.status, c.hint, got, c.want)
		}
	}
	// Only the transient/overload classes are retryable.
	retryable := map[ErrorClass]bool{ErrClassRateLimited: true, ErrClassOverloaded: true, ErrClassTransient: true}
	for _, c := range []ErrorClass{ErrClassUpstream, ErrClassRateLimited, ErrClassOverloaded, ErrClassTransient, ErrClassAuth, ErrClassInvalidRequest, ErrClassContextLength} {
		if c.Retryable() != retryable[c] {
			t.Errorf("class %v Retryable()=%v, want %v", c, c.Retryable(), retryable[c])
		}
	}
}

func TestRetryAfterFromHeader(t *testing.T) {
	h := http.Header{}
	if d := RetryAfterFromHeader(h); d != 0 {
		t.Fatalf("absent header = %v, want 0", d)
	}
	h.Set("Retry-After", "5")
	if d := RetryAfterFromHeader(h); d != 5*time.Second {
		t.Fatalf("seconds form = %v, want 5s", d)
	}
	h.Set("Retry-After", "not-a-number")
	if d := RetryAfterFromHeader(h); d != 0 {
		t.Fatalf("garbage = %v, want 0", d)
	}
	h.Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
	if d := RetryAfterFromHeader(h); d <= 0 || d > 4*time.Second {
		t.Fatalf("http-date form = %v, want ~3s", d)
	}
}

func TestRetryDelayGrowsAndCaps(t *testing.T) {
	base, max := 100*time.Millisecond, 800*time.Millisecond
	// Equal jitter: each delay is within [d/2, d] of the exponential window.
	prevCeil := time.Duration(0)
	for attempt, wantWindow := range []time.Duration{100, 200, 400, 800, 800} {
		w := wantWindow * time.Millisecond
		d := retryDelay(attempt, errors.New("x"), base, max)
		if d < w/2 || d > w {
			t.Fatalf("attempt %d: delay %v outside equal-jitter window [%v, %v]", attempt, d, w/2, w)
		}
		prevCeil = w
	}
	if prevCeil != max {
		t.Fatalf("backoff did not cap at max %v", max)
	}
	// A Retry-After on the error overrides the computed backoff.
	me := NewModelError(ErrClassRateLimited, 250*time.Millisecond, errors.New("x"))
	if d := retryDelay(0, me, base, max); d != 250*time.Millisecond {
		t.Fatalf("Retry-After override = %v, want 250ms", d)
	}
}
