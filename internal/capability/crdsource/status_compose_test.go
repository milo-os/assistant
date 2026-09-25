package crdsource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/milo-os/assistant/internal/capability"
	capv1alpha1 "github.com/milo-os/assistant/pkg/apis/capabilities/v1alpha1"
)

// These tests join the two halves that ship separately — capability.Compose's
// per-document outcome hook and this package's coalescing writer — because the
// property that matters lives only at the seam: Compose runs on EVERY turn,
// whereas Accepted is observed once per cache TTL. If the writer did not
// coalesce, wiring Composed up would turn one chatty conversation into one
// control-plane PATCH per binding per message, which is precisely the hot loop
// the design forbids. Asserting it here, end to end, is the difference between
// knowing that and assuming it.

// knowledgePlane serves one capability knowledge source, either 200 or 404, so
// a document's compose outcome is stable and controllable across turns.
func knowledgePlane(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("StreamCo streams video at the edge."))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func bindingDoc(name, url string) capability.CapabilityDocument {
	return capability.CapabilityDocument{
		// No namespace: cluster-scoped, exactly what the CRD source yields.
		Metadata: &capability.Metadata{Name: name},
		Spec: capability.CapabilitySpec{
			ServiceRef:           capability.Ref{Name: name},
			ServiceName:          name + ".example",
			ServiceAgentRef:      capability.Ref{Name: name + "-agent"},
			ConfigurationVersion: "v1",
			Knowledge: &capability.Knowledge{
				Sources: []capability.KnowledgeSource{{Type: capability.KnowledgeLLMDocs, Title: "overview", URL: url}},
			},
		},
	}
}

func composeTurns(t *testing.T, turns int, docs []capability.CapabilityDocument, w *StatusWriter) {
	t.Helper()
	for i := 0; i < turns; i++ {
		composed, err := capability.Compose(context.Background(), docs, capability.ComposeOptions{
			// httptest listens on loopback, which the SSRF guard blocks by default.
			AllowPrivateNetworks: true,
			ExpectedProject:      "demo-project",
			// The project comes from the turn, never from the document — a
			// cluster-scoped binding has no namespace to read one off. This
			// mirrors agent.Conversation.composeObserver.
			OnDocumentComposed: func(doc capability.CapabilityDocument, composeErr error) {
				w.ObserveComposed("demo-project", doc.Metadata.Name, composeErr)
			},
		})
		if err != nil {
			t.Fatalf("Compose: %v", err)
		}
		_ = composed.Close()
	}
}

// The anti-amplification proof: a stable outcome — composed or degraded — costs
// exactly one write no matter how many turns observe it.
func TestComposedConditionIsWrittenOncePerStableOutcome(t *testing.T) {
	plane := newStatusPlane(t)
	writer := newTestWriter(t, plane.URL, nil)

	healthy := knowledgePlane(t, http.StatusOK)
	dead := knowledgePlane(t, http.StatusNotFound)
	docs := []capability.CapabilityDocument{
		bindingDoc("streamco", healthy.URL+"/llms.txt"),
		bindingDoc("acme", dead.URL+"/llms.txt"),
	}

	composeTurns(t, 10, docs, writer)

	// Two bindings, two outcomes, twenty observations: two writes.
	patches := plane.waitForPatches(t, 2)
	quiesce(t, plane, 2)

	byPath := map[string]recordedPatch{}
	for _, p := range patches {
		byPath[p.path] = p
	}
	for name, want := range map[string]string{"streamco": "True", "acme": "False"} {
		p, ok := byPath[statusPathFor(name)]
		if !ok {
			t.Fatalf("no status patch for %s; got %v", name, byPath)
		}
		cond := condOf(t, p, capv1alpha1.CapabilityBindingConditionComposed)
		if string(cond.Status) != want {
			t.Fatalf("%s Composed = %s, want %s (%s)", name, cond.Status, want, cond.Message)
		}
	}
	if msg := condOf(t, byPath[statusPathFor("acme")], capv1alpha1.CapabilityBindingConditionComposed).Message; msg == "" {
		t.Fatal("a degraded binding must carry a message naming what failed")
	}
}

// statusPathFor is the status subresource path of one binding in demo-project.
func statusPathFor(name string) string {
	gv := capv1alpha1.SchemeGroupVersion
	return "/apis/resourcemanager.miloapis.com/v1alpha1/projects/demo-project/control-plane/apis/" +
		gv.Group + "/" + gv.Version + "/capabilitybindings/" + name + "/status"
}

// Recovery is the other half of the contract: a binding fixed between turns has
// to transition back to True, or the condition is a one-way trap.
func TestComposedConditionRecordsRecovery(t *testing.T) {
	plane := newStatusPlane(t)
	writer := newTestWriter(t, plane.URL, nil)

	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	docs := []capability.CapabilityDocument{bindingDoc("streamco", srv.URL+"/llms.txt")}

	composeTurns(t, 3, docs, writer)
	plane.waitForPatches(t, 1)

	status = http.StatusOK
	composeTurns(t, 3, docs, writer)

	patches := plane.waitForPatches(t, 2)
	quiesce(t, plane, 2)
	if got := string(condOf(t, patches[0], capv1alpha1.CapabilityBindingConditionComposed).Status); got != "False" {
		t.Fatalf("first write Composed = %s, want False", got)
	}
	if got := string(condOf(t, patches[1], capv1alpha1.CapabilityBindingConditionComposed).Status); got != "True" {
		t.Fatalf("recovery write Composed = %s, want True", got)
	}
}
