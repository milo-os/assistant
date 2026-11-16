package capability

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	connect := func(ctx context.Context, endpoint string, _ map[string]string) (mcpSession, error) {
		time.Sleep(40 * time.Millisecond) // exceed the timeout below
		return (*nilClosePanics)(nil), errors.New("connect failed")
	}

	_, err := connectWithTimeout(context.Background(), connect, "http://x/mcp", nil, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	// Give the late-close watcher goroutine time to run and (if the bug were
	// present) panic. A panic in that goroutine crashes the test binary.
	time.Sleep(80 * time.Millisecond)
}

// hangingCloseSession is a session whose Close blocks until release is
// closed, like an MCP server that never answers the session DELETE.
type hangingCloseSession struct {
	release chan struct{}
}

func (s *hangingCloseSession) Tools(context.Context) (map[string]agentcore.Tool, error) {
	return map[string]agentcore.Tool{}, nil
}

func (s *hangingCloseSession) Close() error {
	<-s.release
	return nil
}

// TestCloseSessions_HungSessionIsBoundedAndLogged: one hung session must not
// stop the others from closing or hold the caller past the deadline, and the
// hung one is named in the log.
func TestCloseSessions_HungSessionIsBoundedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	hung := &hangingCloseSession{release: make(chan struct{})}
	defer close(hung.release)
	ok := newFakeSession()

	start := time.Now()
	closeSessions([]openSession{
		{service: "svc-a", server: "hung", session: hung},
		{service: "svc-b", server: "fine", session: ok},
	}, 50*time.Millisecond, testLogger(&buf))
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("closeSessions took %s, want about the 50ms deadline", elapsed)
	}
	ok.mu.Lock()
	calls := ok.closeCalls
	ok.mu.Unlock()
	if calls != 1 {
		t.Fatalf("healthy session closed %d times, want 1", calls)
	}
	logged := buf.String()
	if !strings.Contains(logged, "capability.mcp.close_timed_out") || !strings.Contains(logged, "server=hung") {
		t.Fatalf("log does not name the hung session:\n%s", logged)
	}
	if strings.Contains(logged, "server=fine") {
		t.Fatalf("log names a session that closed:\n%s", logged)
	}
}

// TestComposedClose_DoesNotHangOnAHungSession: the per-turn Close that
// finalization calls returns within MCPCloseTimeout even when a server never
// answers its session DELETE.
func TestComposedClose_DoesNotHangOnAHungSession(t *testing.T) {
	var buf bytes.Buffer
	hung := &hangingCloseSession{release: make(chan struct{})}
	defer close(hung.release)
	doc := streamcoDoc(func(d *CapabilityDocument) { d.Spec.Knowledge = nil })

	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		connect: func(context.Context, string, map[string]string) (mcpSession, error) {
			return hung, nil
		},
		MCPCloseTimeout: 50 * time.Millisecond,
		Logger:          testLogger(&buf),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_ = composed.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Composed.Close hung on a session whose Close never returns")
	}
	if !strings.Contains(buf.String(), "capability.mcp.close_timed_out") {
		t.Fatalf("hung session close was not logged:\n%s", buf.String())
	}
}
