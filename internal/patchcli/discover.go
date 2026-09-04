package patchcli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DiscoverBaseURL asks the aggregated apiserver where the assistant lives.
//
// This is why `datumctl patch` needs no PATCH_URL. The control-plane address
// names Milo, not the assistant, and nothing else advertises the service's
// address — so the service publishes it as a resource in its own API group,
// which the CLI already reads with the caller's datumctl credentials for
// `conversations` and `gaps`. No new credential, no hostname convention.
//
// Cluster-scoped and singular: one assistant serves the control plane — hence
// a path with no namespace segment, unlike the other read views.
//
// An empty URL is returned as an error rather than an empty string: the
// resource exists but the operator has not told the service its own address,
// and a caller needs to be told that rather than handed "".
func DiscoverBaseURL(ctx context.Context, view ReadView, project string) (string, error) {
	out, err := view.get(ctx, project, endpointPath())
	if err != nil {
		return "", fmt.Errorf("%s", readViewErrorText(view, err))
	}
	var ep struct {
		Spec struct {
			URL string `json:"url"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &ep); err != nil {
		return "", fmt.Errorf("decode assistantendpoint: %w", err)
	}
	if strings.TrimSpace(ep.Spec.URL) == "" {
		return "", fmt.Errorf("the assistant has not published its address (PUBLIC_BASE_URL is unset on the service)")
	}
	return ep.Spec.URL, nil
}

// endpointPath builds the cluster-scoped path for the singleton endpoint.
func endpointPath() string {
	return "/apis/assistant.miloapis.com/v1alpha1/assistantendpoints/" + assistantEndpointName
}

// assistantEndpointName mirrors pkg/apis/assistant.AssistantEndpointName. It is
// duplicated rather than imported so the CLI does not pull the apiserver's type
// tree — the value is part of the API contract and changing it is a breaking
// change either way.
const assistantEndpointName = "assistant"
