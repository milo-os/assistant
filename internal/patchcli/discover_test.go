package patchcli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeKubectl puts a kubectl on PATH that emits body (stdout when code is 0,
// stderr otherwise) and exits with code. Heredocs both ways so the body may
// contain quotes — real kubectl errors do.
func fakeKubectl(t *testing.T, body string, code int) {
	t.Helper()
	stream := "cat <<'JSON'\n" + body + "\nJSON\n"
	if code != 0 {
		stream = "cat >&2 <<'ERR'\n" + body + "\nERR\n"
	}
	script := "#!/bin/sh\n" + stream + "exit " + strconv.Itoa(code) + "\n"

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDiscoverReturnsPublishedURL is the behaviour that removes PATCH_URL from
// the normal path.
func TestDiscoverReturnsPublishedURL(t *testing.T) {
	fakeKubectl(t, `{"spec":{"url":"https://patch.staging.env.datum.net","agentCardPath":"/.well-known/agent-card.json"}}`, 0)

	url, err := DiscoverBaseURL(context.Background(), "")
	if err != nil {
		t.Fatalf("DiscoverBaseURL: %v", err)
	}
	if url != "https://patch.staging.env.datum.net" {
		t.Fatalf("url = %q", url)
	}
}

// An endpoint that exists but publishes nothing is an error, not "". A caller
// handed "" would build a request against a relative URL and fail obscurely.
func TestDiscoverEmptyURLIsAnError(t *testing.T) {
	fakeKubectl(t, `{"spec":{"url":""}}`, 0)

	_, err := DiscoverBaseURL(context.Background(), "")
	if err == nil {
		t.Fatal("want an error for an unpublished address")
	}
	if !strings.Contains(err.Error(), "PUBLIC_BASE_URL") {
		t.Errorf("error should name the unset setting, got: %v", err)
	}
}

// kubectl's own stderr is the useful part when the resource is not installed;
// it must reach the user rather than being swallowed.
func TestDiscoverSurfacesKubectlError(t *testing.T) {
	fakeKubectl(t, `error: the server doesn't have a resource type "assistantendpoints"`, 1)

	_, err := DiscoverBaseURL(context.Background(), "")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "assistantendpoints") {
		t.Errorf("kubectl stderr was lost, got: %v", err)
	}
}
