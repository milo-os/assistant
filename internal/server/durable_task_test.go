package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
	"github.com/milo-os/assistant/internal/taskstore"
)

// TestDurableTaskStoreEndToEnd proves the Postgres task store integrates with the
// real a2a-go handler: a SendMessage persists a task the durable store can serve
// back via GetTask, and cross-tenant GetTask is still denied (403) when the owning
// project is read off the durably-stored task metadata. Gated on TEST_DATABASE_URL,
// matching the history and taskstore suites.
func TestDurableTaskStoreEndToEnd(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping durable task store integration test")
	}
	log := logger.Silent()
	authn, authz := testAuth()
	store, err := taskstore.NewPostgresStore(context.Background(), url, log)
	if err != nil {
		t.Fatalf("connect task store: %v", err)
	}
	t.Cleanup(store.Close)

	cfg, err := config.Load(config.MapGetenv(map[string]string{
		"MODEL_MODE":                "mock",
		"AUTHN_TOKENREVIEW_API_URL": "https://control-plane.test",
		"AUTHZ_SAR_API_URL":         "https://control-plane.test",
		"PUBLIC_BASE_URL":           "http://assistant.test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	app := New(Deps{
		Config:        cfg,
		Logger:        log,
		Authenticator: authn,
		Authorizer:    authz,
		Runner:        fakeRunner{},
		TaskStore:     store,
	})
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	// SendMessage persists a task in Postgres.
	sent := decodeRPC(t, rpc(t, srv, goodToken, "SendMessage", sendMessageParams(project, "hi"), "s"))
	if sent.Error != nil {
		t.Fatalf("SendMessage error: %+v", sent.Error)
	}
	taskID := sent.task(t).ID
	if taskID == "" {
		t.Fatal("no task id returned")
	}

	// The durable store serves it back on GetTask.
	got := decodeRPC(t, rpc(t, srv, goodToken, "GetTask", map[string]any{"id": string(taskID)}, "g"))
	if got.Error != nil {
		t.Fatalf("GetTask error: %+v", got.Error)
	}
	if got.task(t).ID != taskID {
		t.Fatalf("GetTask returned id %q, want %q", got.task(t).ID, taskID)
	}

	// Cross-tenant GetTask is denied using the project read off the durable task.
	res := rpc(t, srv, wrongToken, "GetTask", map[string]any{"id": string(taskID)}, "g2")
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant GetTask status = %d, want 403", res.StatusCode)
	}

	// And the stored task carries the owning project (metadata survived the JSONB
	// round-trip), so a restart-time authorization decision is still possible.
	stored, err := store.Get(context.Background(), a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if stored.Task.Metadata["projectName"] != project {
		t.Fatalf("durable task project = %v, want %q", stored.Task.Metadata["projectName"], project)
	}
}
