package sse

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe response body. Writer already serializes its
// own writes; this only makes the test's reads race-free against them.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
	err     error
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	return b.buf.Write(p)
}

func (b *syncBuffer) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushes++
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestData_WritesFrameAndFlushes(t *testing.T) {
	out := &syncBuffer{}
	w := NewWriter(out, out)
	if err := w.Data([]byte(`{"type":"done"}`)); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "data: {\"type\":\"done\"}\n\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if out.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", out.flushes)
	}
}

func TestKeepAlive_WritesCommentsUntilStopped(t *testing.T) {
	out := &syncBuffer{}
	w := NewWriter(out, out)
	stop := w.KeepAlive(5 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(out.String(), ": ping\n\n") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeats after 2s; body = %q", out.String())
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	stop() // idempotent

	// After stop returns, the goroutine is gone: the body must not grow.
	before := out.String()
	time.Sleep(20 * time.Millisecond)
	if after := out.String(); after != before {
		t.Fatalf("heartbeat written after stop: %q -> %q", before, after)
	}
}

func TestKeepAlive_InterleavesWithDataOnFrameBoundaries(t *testing.T) {
	out := &syncBuffer{}
	w := NewWriter(out, out)
	stop := w.KeepAlive(time.Millisecond)
	for range 200 {
		if err := w.Data([]byte(`{"type":"text_delta","text":"x"}`)); err != nil {
			t.Fatal(err)
		}
	}
	stop()
	for _, frame := range strings.Split(strings.TrimSuffix(out.String(), "\n\n"), "\n\n") {
		if frame != ": ping" && frame != `data: {"type":"text_delta","text":"x"}` {
			t.Fatalf("torn frame %q", frame)
		}
	}
}

func TestWriteError_IsStickyAndStopsHeartbeat(t *testing.T) {
	gone := errors.New("client went away")
	out := &syncBuffer{err: gone}
	w := NewWriter(out, out)
	stop := w.KeepAlive(time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for w.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat never hit the write error")
		}
		time.Sleep(time.Millisecond)
	}
	if err := w.Data([]byte(`{}`)); !errors.Is(err, gone) {
		t.Fatalf("Data err = %v, want %v", err, gone)
	}
	if out.flushes != 0 {
		t.Fatalf("flushed %d times after failed writes", out.flushes)
	}
}
