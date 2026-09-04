package endpoint

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// TestGetReportsConfiguredURL is the point of the resource: a client that can
// reach this API learns where to send A2A traffic, without being told a
// hostname out of band.
func TestGetReportsConfiguredURL(t *testing.T) {
	r := NewAssistantEndpointREST("https://patch.staging.env.datum.net")

	obj, err := r.Get(context.Background(), assistant.AssistantEndpointName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ep := obj.(*assistant.AssistantEndpoint)
	if ep.Spec.URL != "https://patch.staging.env.datum.net" {
		t.Errorf("URL = %q", ep.Spec.URL)
	}
	if ep.Spec.AgentCardPath != assistant.DefaultAgentCardPath {
		t.Errorf("AgentCardPath = %q", ep.Spec.AgentCardPath)
	}
	if ep.Name != assistant.AssistantEndpointName {
		t.Errorf("Name = %q", ep.Name)
	}
}

// An unconfigured service reports empty rather than inventing an address: a
// wrong URL here would silently point every client at another service.
func TestUnsetURLIsReportedEmptyNotGuessed(t *testing.T) {
	r := NewAssistantEndpointREST("")
	obj, err := r.Get(context.Background(), assistant.AssistantEndpointName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if url := obj.(*assistant.AssistantEndpoint).Spec.URL; url != "" {
		t.Errorf("URL = %q, want empty", url)
	}
}

// Any other name is a real 404, not the singleton under an alias.
func TestOtherNameIsNotFound(t *testing.T) {
	r := NewAssistantEndpointREST("https://x")
	_, err := r.Get(context.Background(), "something-else", &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestListReturnsTheSingleton(t *testing.T) {
	r := NewAssistantEndpointREST("https://x")
	obj, err := r.List(context.Background(), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list := obj.(*assistant.AssistantEndpointList)
	if len(list.Items) != 1 || list.Items[0].Spec.URL != "https://x" {
		t.Fatalf("Items = %+v", list.Items)
	}
}

// Cluster-scoped: the assistant's address is not a per-project fact, so this
// must not go through the project-scoping the other resources use.
func TestClusterScoped(t *testing.T) {
	if NewAssistantEndpointREST("").NamespaceScoped() {
		t.Fatal("NamespaceScoped() = true, want false")
	}
}
