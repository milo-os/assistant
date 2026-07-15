package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestAssistantBootsAndServesHealthAndCard is a smoke test: it builds and boots
// the REAL assistant binary with the mock-mode env contract, then asserts the
// public surface a client hits before it holds a token — /healthz and the agent
// card at both well-known paths. It exercises config parsing, the full wiring
// graph, and graceful shutdown end to end. (A2A message runs need the agent
// orchestration layer, which is stubbed until internal/agent lands, so this
// deliberately covers only the always-available public surface.)
//
// Guarded by -short since it compiles a binary and opens a socket.
func TestAssistantBootsAndServesHealthAndCard(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test builds and boots the binary; skipped in -short mode")
	}

	bin := filepath.Join(t.TempDir(), "assistant")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build assistant binary: %v", err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"PORT="+port,
		"HOST=127.0.0.1",
		"AUTH_MODE=dev",
		"AUTH_DEV_TOKENS=dev-token:local-user:demo-project",
		"MODEL_MODE=mock",
		"PUBLIC_BASE_URL="+base,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start assistant: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	waitReady(t, base+"/healthz", 10*time.Second, &stderr)

	// Agent card at the canonical path and the legacy alias.
	for _, path := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		res, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		func() {
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("%s status = %d, want 200", path, res.StatusCode)
			}
			var card struct {
				Name         string `json:"name"`
				Capabilities struct {
					Streaming bool `json:"streaming"`
				} `json:"capabilities"`
				SupportedInterfaces []struct {
					URL             string `json:"url"`
					ProtocolBinding string `json:"protocolBinding"`
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"supportedInterfaces"`
			}
			if err := json.NewDecoder(res.Body).Decode(&card); err != nil {
				t.Fatalf("decode card at %s: %v", path, err)
			}
			if card.Name != "Patch" {
				t.Errorf("%s card.name = %q, want Patch", path, card.Name)
			}
			if !card.Capabilities.Streaming {
				t.Errorf("%s card.capabilities.streaming = false, want true", path)
			}
			if len(card.SupportedInterfaces) == 0 || card.SupportedInterfaces[0].URL != base+"/a2a" {
				t.Errorf("%s card supportedInterfaces = %+v, want a JSONRPC interface at %s/a2a", path, card.SupportedInterfaces, base)
			}
		}()
	}
}

// freePort reserves and immediately releases a TCP port, returning it as a
// string. There is a small race before the child rebinds it; acceptable for a
// local smoke test.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// waitReady polls url until it returns any HTTP response or the deadline passes.
func waitReady(t *testing.T, url string, timeout time.Duration, stderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("assistant did not become ready at %s within %s\n--- stderr ---\n%s", url, timeout, stderr.String())
}
