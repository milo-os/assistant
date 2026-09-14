package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the group name for the assistant API.
const GroupName = "assistant.miloapis.com"

// SchemeGroupVersion is the group version used to register these objects.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes, RegisterConversions)
	AddToScheme   = SchemeBuilder.AddToScheme
)

// Resource takes an unqualified resource and returns a Group-qualified GroupResource.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&Conversation{}, &ConversationList{},
		&ConversationMessages{},
		&CapabilityGapReport{}, &CapabilityGapReportList{},
		&CapabilityGap{}, &CapabilityGapList{},
		&AssistantEndpoint{}, &AssistantEndpointList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Conversation"), conversationFieldLabel)
}

// ConversationArchivedField is the one field selector conversations support:
// spec.archived=true lists the archive, spec.archived=false (the default when
// no selector is sent) everything else.
const ConversationArchivedField = "spec.archived"

// conversationFieldLabel admits spec.archived as a Conversation field
// selector. Without a registered conversion the generic list handler rejects
// every label except metadata.name/metadata.namespace before the storage ever
// sees the request. Those two stay admitted here as well, so the handler's
// own error text does not change for them — the conversation storage is what
// decides which selectors it actually serves.
func conversationFieldLabel(label, value string) (string, string, error) {
	if label == ConversationArchivedField {
		return label, value, nil
	}
	return runtime.DefaultMetaV1FieldSelectorConversion(label, value)
}
