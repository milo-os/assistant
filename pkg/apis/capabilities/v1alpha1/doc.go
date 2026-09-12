// +k8s:deepcopy-gen=package
// +k8s:openapi-gen=true
// +k8s:openapi-model-package=com.miloapis.assistant.pkg.apis.capabilities.v1alpha1
// +groupName=capabilities.assistant.miloapis.com

// Package v1alpha1 contains the CapabilityBinding CRD — the assistant-owned,
// project-scoped record of which provider service a project is entitled to and
// what that service contributes (knowledge, tools, skills, authority).
//
// It lives in its OWN group rather than in assistant.miloapis.com because that
// group/version is claimed by an APIService (config/components/api-registration):
// the kube-aggregator routes every assistant.miloapis.com/v1alpha1 request to
// the aggregated apiserver, which would shadow a CRD registered there — the
// object would apply, and then never be readable. A separate group keeps the
// aggregated read surface (conversations, gap reports, endpoints) and this
// etcd-backed write surface from ever contending for the same route.
package v1alpha1
