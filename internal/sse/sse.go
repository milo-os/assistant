// Package sse writes Server-Sent-Event streams that survive idle gaps.
//
// An agent turn can go quiet for a long time between frames: the model
// thinking before its first token, or a slow capability tool call. Proxies in
// front of the assistant close a streamed response that sends no bytes for a
// while — cloud-portal's Bun/Hono server drops one after ~12s idle (see
// SSE_IDLE_TIMEOUT_MS in cloud-portal's app/server/watch/watch-hub.ts) — and
// the browser surfaces that as a bare "network error" mid-turn. [Writer]
// keeps the connection busy with SSE comment lines, which every conforming
// parser (and the chat-kit's `data:`-only parser) ignores, so no caller
// downstream has to know heartbeats exist.
package sse

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// KeepAliveInterval is how often [Writer.KeepAlive] writes a heartbeat. It
// sits well under half the shortest idle close we know of (~12s, above):
// cloud-portal measured that a heartbeat just under the ceiling still drops,
// while 5s held.
const KeepAliveInterval = 5 * time.Second

// Writer serializes SSE writes to one response so event frames and heartbeats
// from [Writer.KeepAlive]'s goroutine never interleave. The first write error
// (the client went away) is sticky: every later write returns it without
// touching the response.
type Writer struct {
	mu      sync.Mutex
	w       io.Writer
	flusher http.Flusher
	err     error
}

// NewWriter wraps w, flushing through flusher after every write.
func NewWriter(w io.Writer, flusher http.Flusher) *Writer {
	return &Writer{w: w, flusher: flusher}
}

// Data writes payload as one `data:` frame and flushes it.
func (s *Writer) Data(payload []byte) error {
	return s.write("data: %s\n\n", payload)
}

// Err reports the sticky write error, if any.
func (s *Writer) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// KeepAlive writes a comment heartbeat every interval until the returned stop
// func is called or a write fails. stop blocks until the heartbeat goroutine
// has exited, so calling it before the handler returns guarantees nothing
// writes to the response afterward. stop is safe to call more than once.
func (s *Writer) KeepAlive(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if s.write(": ping\n\n") != nil {
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

func (s *Writer) write(format string, args ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, err := fmt.Fprintf(s.w, format, args...); err != nil {
		s.err = err
		return err
	}
	s.flusher.Flush()
	return nil
}
