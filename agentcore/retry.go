package agentcore

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Retry defaults. They are deliberately conservative: a real provider's
// transient failures (rate limits, brief overloads) clear in seconds, and the
// per-turn deadline (see internal/agent) bounds the total wait regardless.
const (
	// DefaultMaxRetries is the number of ADDITIONAL attempts made after the
	// first on a retryable failure (so up to DefaultMaxRetries+1 attempts).
	DefaultMaxRetries = 2
	// DefaultRetryBaseDelay is the base of the exponential backoff.
	DefaultRetryBaseDelay = 500 * time.Millisecond
	// DefaultRetryMaxDelay caps a single backoff wait (before honoring a
	// server Retry-After, which is obeyed verbatim).
	DefaultRetryMaxDelay = 8 * time.Second
)

// ErrorClass categorizes a model failure so the loop can decide whether to
// retry it and how to surface it. Adapters classify their SDK/transport errors
// into these buckets (see [ModelError]); the loop never inspects a provider
// SDK type directly.
type ErrorClass int

const (
	// ErrClassUpstream is a generic, non-retryable upstream failure (e.g. a
	// 500 the SDK did not attribute to overload). It is terminal.
	ErrClassUpstream ErrorClass = iota
	// ErrClassRateLimited is a 429 rate-limit response. Retryable.
	ErrClassRateLimited
	// ErrClassOverloaded is a provider-overloaded response (Anthropic 529,
	// or a 503 service-unavailable). Retryable.
	ErrClassOverloaded
	// ErrClassTransient is a transient transport failure (connection reset,
	// timeout, unexpected EOF before any output). Retryable.
	ErrClassTransient
	// ErrClassAuth is an authentication/authorization failure (401/403).
	// Terminal — retrying with the same credential cannot succeed.
	ErrClassAuth
	// ErrClassInvalidRequest is a malformed request (400). Terminal.
	ErrClassInvalidRequest
	// ErrClassContextLength means the prompt (usually a long history) exceeded
	// the model's context window (413, or a 400 the provider attributes to
	// length). Terminal — a real model WILL hit this and the caller must see a
	// clear message rather than a retry storm.
	ErrClassContextLength
)

// Retryable reports whether a failure of this class is worth retrying with
// backoff. Only transient, provider-side conditions are retryable; client
// errors (auth, invalid request, context length) never are.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ErrClassRateLimited, ErrClassOverloaded, ErrClassTransient:
		return true
	default:
		return false
	}
}

// ModelError wraps a provider failure with its [ErrorClass] and an optional
// server-supplied Retry-After hint. Adapters return one of these from a failed
// Stream so the loop can classify the failure without importing any provider
// SDK. It unwraps to the underlying cause so errors.Is/As on the original
// error still work.
type ModelError struct {
	// Class is the failure bucket used for the retry decision.
	Class ErrorClass
	// RetryAfter, when > 0, is the server's requested wait before retrying
	// (from a Retry-After header). It is honored verbatim over the computed
	// backoff.
	RetryAfter time.Duration
	// Err is the underlying provider/transport error.
	Err error
}

func (e *ModelError) Error() string { return e.Err.Error() }
func (e *ModelError) Unwrap() error { return e.Err }

// NewModelError builds a [ModelError]. It is the constructor adapters use once
// they have classified an SDK error.
func NewModelError(class ErrorClass, retryAfter time.Duration, err error) *ModelError {
	return &ModelError{Class: class, RetryAfter: retryAfter, Err: err}
}

// ClassifyStatus maps an HTTP status code to an [ErrorClass]. It is the shared
// classifier both HTTP adapters use so 429/503/529/4xx are bucketed
// identically regardless of provider. contextLengthHint lets an adapter force
// [ErrClassContextLength] when it recognizes a length error the status code
// alone does not reveal (e.g. a 400 whose body says the prompt is too long).
func ClassifyStatus(status int, contextLengthHint bool) ErrorClass {
	switch status {
	case http.StatusTooManyRequests: // 429
		return ErrClassRateLimited
	case http.StatusServiceUnavailable, 529: // 503 / Anthropic overloaded
		return ErrClassOverloaded
	case http.StatusUnauthorized, http.StatusForbidden: // 401 / 403
		return ErrClassAuth
	case http.StatusRequestEntityTooLarge: // 413
		return ErrClassContextLength
	case http.StatusBadRequest: // 400
		if contextLengthHint {
			return ErrClassContextLength
		}
		return ErrClassInvalidRequest
	default:
		if status >= 500 && status != 501 {
			// Unattributed 5xx: treat as overloaded-ish and let backoff try
			// once or twice; a persistent one still gives up terminally.
			return ErrClassOverloaded
		}
		return ErrClassUpstream
	}
}

// RetryAfterFromHeader extracts a Retry-After wait from a response header. It
// accepts both forms the RFC allows — a delay in seconds ("Retry-After: 30")
// and an HTTP-date — returning 0 when the header is absent or unparseable.
func RetryAfterFromHeader(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retryDelay returns how long to wait before the given retry attempt (0-based).
// A server Retry-After on the failure wins; otherwise it is exponential
// backoff (base * 2^attempt, capped) with equal jitter so a fleet of clients
// does not retry in lockstep.
func retryDelay(attempt int, err error, base, max time.Duration) time.Duration {
	var me *ModelError
	if errors.As(err, &me) && me.RetryAfter > 0 {
		return me.RetryAfter
	}
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	if max <= 0 {
		max = DefaultRetryMaxDelay
	}
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= max || d <= 0 { // >= max or overflow
			d = max
			break
		}
	}
	// Equal jitter: wait half the window deterministically, half at random, so
	// there is always a minimum backoff but no thundering herd.
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// isCancellation reports whether an error is a context cancellation or a
// deadline (per-turn timeout) expiry. Both end a run as [FinishCanceled] and
// are never retried.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isRetryable reports whether a failed attempt should be retried: it must be a
// classified [ModelError] whose class is retryable, and never a cancellation.
func isRetryable(err error) bool {
	if isCancellation(err) {
		return false
	}
	var me *ModelError
	return errors.As(err, &me) && me.Class.Retryable()
}
