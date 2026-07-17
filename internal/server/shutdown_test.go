package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
)

// blockingRunner holds its Run until released, so a request can be pinned
// "in-flight" while graceful shutdown runs — the exact condition D must handle
// (a long-lived stream must be allowed to finish during drain).
type blockingRunner struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (r *blockingRunner) Run(ctx context.Context, _ assistanta2a.RunRequest, sink assistanta2a.RunSink) assistanta2a.RunResult {
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	sink.OnTextDelta("drained-ok")
	close(r.finished)
	return assistanta2a.RunResult{State: assistanta2a.RunCompleted, Text: "drained-ok"}
}

// TestGracefulShutdownDrainsInFlight proves the mechanism main.go relies on: an
// in-flight request is allowed to COMPLETE during srv.Shutdown rather than being
// cut off, and Shutdown returns cleanly (drained within grace, not deadline).
func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	cfg, err := config.Load(config.MapGetenv(map[string]string{
		"AUTH_MODE":       "dev",
		"AUTH_DEV_TOKENS": goodToken + ":alice:" + project,
		"MODEL_MODE":      "mock",
		"PUBLIC_BASE_URL": "http://assistant.test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	log := logger.Silent()
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	app := New(Deps{
		Config:        cfg,
		Logger:        log,
		Authenticator: mustAuthenticator(t, cfg, log),
		Authorizer:    auth.NewAuthorizer(cfg, log),
		Runner:        runner,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: app}
	go func() { _ = srv.Serve(ln) }()

	// Fire a request that will block in the runner.
	var (
		wg        sync.WaitGroup
		reqStatus int
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		res := rpcTo(t, "http://"+ln.Addr().String(), goodToken, "SendMessage", sendMessageParams(project, "hi"))
		if res != nil {
			reqStatus = res.StatusCode
			res.Body.Close()
		}
	}()

	// Wait until the handler is genuinely in-flight.
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("request never reached the runner")
	}

	// Begin graceful shutdown while the request is in-flight; release the runner
	// shortly after so it finishes inside the grace window.
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	close(runner.release) // let the in-flight turn complete

	select {
	case <-runner.finished:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request was cut off instead of draining")
	}

	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown returned %v, want clean drain", err)
	}
	wg.Wait()
	if reqStatus != http.StatusOK {
		t.Fatalf("in-flight request status = %d, want 200 (completed during drain)", reqStatus)
	}
}

// rpcTo posts a JSON-RPC request to an absolute base URL (the drain test drives
// a real *http.Server, not an httptest.Server, so it needs its own poster).
func rpcTo(t *testing.T, base, token, method string, params any) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "drain", "method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, base+"/a2a", bytes.NewReader(body))
	if err != nil {
		t.Errorf("build rpc %s: %v", method, err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("rpc %s: %v", method, err)
		return nil
	}
	return res
}
