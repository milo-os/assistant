package capability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/milo-os/assistant/agentcore"
)

// nilClosePanics mimics *mcptool.Session: Close on a nil receiver panics. A
// connector returning (*nilClosePanics)(nil) with an error yields a typed-nil
// mcpSession — the exact shape that crashed the assistant.
type nilClosePanics struct{}

func (s *nilClosePanics) Tools(context.Context) (map[string]agentcore.Tool, error) { return nil, nil }
func (s *nilClosePanics) Close() error {
	if s == nil {
		panic("Close called on nil session")
	}
	return nil
}

// TestConnectWithTimeout_SlowFailureDoesNotPanic pins the fix for the nil-
// interface crash: when a connect exceeds the timeout and then returns an
// error with a typed-nil session, the late-close watcher must NOT call Close
// (which would panic on the nil receiver and crash the process).
func TestConnectWithTimeout_SlowFailureDoesNotPanic(t *testing.T) {
	connect := func(ctx context.Context, endpoint string) (mcpSession, error) {
		time.Sleep(40 * time.Millisecond) // exceed the timeout below
		return (*nilClosePanics)(nil), errors.New("connect failed")
	}

	_, err := connectWithTimeout(context.Background(), connect, "http://x/mcp", 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	// Give the late-close watcher goroutine time to run and (if the bug were
	// present) panic. A panic in that goroutine crashes the test binary.
	time.Sleep(80 * time.Millisecond)
}
