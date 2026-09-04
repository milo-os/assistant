// Package endpoint is the read-only REST storage backing the aggregated
// apiserver's assistantendpoints resource: it tells a client where to send A2A
// traffic.
//
// Nothing is stored. The service reports the address it was configured to
// advertise, so the answer cannot drift from what the agent card says — both
// read the same value.
//
// Cluster-scoped, unlike conversations and capabilitygapreports: one assistant
// serves every project on a control plane, so its address is not a per-project
// fact and does not go through tenant.ProjectFromContext. A caller who can
// reach this API at all may read it; knowing where the service lives grants
// nothing on its own, and every A2A request is still authenticated and
// authorized per project by the service itself.
package endpoint

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/milo-os/assistant/pkg/apis/assistant"
)

var endpointsResource = assistant.Resource("assistantendpoints")

// AssistantEndpointREST serves the singleton endpoint object.
type AssistantEndpointREST struct {
	publicBaseURL string
	rest.TableConvertor
}

var (
	_ rest.Storage              = (*AssistantEndpointREST)(nil)
	_ rest.Scoper               = (*AssistantEndpointREST)(nil)
	_ rest.Getter               = (*AssistantEndpointREST)(nil)
	_ rest.Lister               = (*AssistantEndpointREST)(nil)
	_ rest.SingularNameProvider = (*AssistantEndpointREST)(nil)
)

// NewAssistantEndpointREST builds the endpoint storage. publicBaseURL is the
// address the service advertises (PUBLIC_BASE_URL); empty is allowed and
// reported as empty rather than guessed.
func NewAssistantEndpointREST(publicBaseURL string) *AssistantEndpointREST {
	return &AssistantEndpointREST{
		publicBaseURL:  publicBaseURL,
		TableConvertor: rest.NewDefaultTableConvertor(endpointsResource),
	}
}

func (r *AssistantEndpointREST) New() runtime.Object     { return &assistant.AssistantEndpoint{} }
func (r *AssistantEndpointREST) NewList() runtime.Object { return &assistant.AssistantEndpointList{} }
func (r *AssistantEndpointREST) Destroy()                {}
func (r *AssistantEndpointREST) NamespaceScoped() bool   { return false }
func (r *AssistantEndpointREST) GetSingularName() string { return "assistantendpoint" }

// object builds the singleton.
func (r *AssistantEndpointREST) object() *assistant.AssistantEndpoint {
	return &assistant.AssistantEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: assistant.AssistantEndpointName},
		Spec: assistant.AssistantEndpointSpec{
			URL:           r.publicBaseURL,
			AgentCardPath: assistant.DefaultAgentCardPath,
		},
	}
}

// Get returns the singleton. Any other name is a genuine 404 rather than the
// singleton under an alias — a client that asked for something else should be
// told it does not exist, not handed this.
func (r *AssistantEndpointREST) Get(_ context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if name != assistant.AssistantEndpointName {
		return nil, apierrors.NewNotFound(endpointsResource, name)
	}
	return r.object(), nil
}

// List returns the one endpoint. Selectors are not supported: there is a single
// object with no labels to select on.
func (r *AssistantEndpointREST) List(_ context.Context, _ *metainternalversion.ListOptions) (runtime.Object, error) {
	return &assistant.AssistantEndpointList{
		Items: []assistant.AssistantEndpoint{*r.object()},
	}, nil
}

// String makes the storage identifiable in apiserver logs.
func (r *AssistantEndpointREST) String() string {
	return fmt.Sprintf("assistantendpoints(url=%q)", r.publicBaseURL)
}
