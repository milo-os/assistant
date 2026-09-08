package v1alpha1

import (
	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// ----------------------------------------------------------------------------
// Sub-type helpers
// ----------------------------------------------------------------------------

func toAssistantMessages(in []ConversationMessage) []assistant.ConversationMessage {
	if in == nil {
		return nil
	}
	out := make([]assistant.ConversationMessage, len(in))
	for i := range in {
		out[i] = assistant.ConversationMessage{
			Seq:       in[i].Seq,
			Role:      in[i].Role,
			Content:   in[i].Content,
			CreatedAt: in[i].CreatedAt,
		}
	}
	return out
}

func toV1Messages(in []assistant.ConversationMessage) []ConversationMessage {
	if in == nil {
		return nil
	}
	out := make([]ConversationMessage, len(in))
	for i := range in {
		out[i] = ConversationMessage{
			Seq:       in[i].Seq,
			Role:      in[i].Role,
			Content:   in[i].Content,
			CreatedAt: in[i].CreatedAt,
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Conversation
// ----------------------------------------------------------------------------

func convert_v1alpha1_Conversation_To_assistant(in *Conversation, out *assistant.Conversation) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status = assistant.ConversationStatus{
		LastActiveAt: in.Status.LastActiveAt,
		MessageCount: in.Status.MessageCount,
		Title:        in.Status.Title,
		Name:         in.Status.Name,
	}
	return nil
}

func convert_assistant_Conversation_To_v1alpha1(in *assistant.Conversation, out *Conversation) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status = ConversationStatus{
		LastActiveAt: in.Status.LastActiveAt,
		MessageCount: in.Status.MessageCount,
		Title:        in.Status.Title,
		Name:         in.Status.Name,
	}
	return nil
}

func convert_v1alpha1_ConversationList_To_assistant(in *ConversationList, out *assistant.ConversationList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]assistant.Conversation, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_Conversation_To_assistant(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func convert_assistant_ConversationList_To_v1alpha1(in *assistant.ConversationList, out *ConversationList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]Conversation, len(in.Items))
		for i := range in.Items {
			if err := convert_assistant_Conversation_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// ConversationMessages
// ----------------------------------------------------------------------------

func convert_v1alpha1_ConversationMessages_To_assistant(in *ConversationMessages, out *assistant.ConversationMessages) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Items = toAssistantMessages(in.Items)
	return nil
}

func convert_assistant_ConversationMessages_To_v1alpha1(in *assistant.ConversationMessages, out *ConversationMessages) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Items = toV1Messages(in.Items)
	return nil
}

// ----------------------------------------------------------------------------
// CapabilityGapReport
// ----------------------------------------------------------------------------

func convert_v1alpha1_CapabilityGapReport_To_assistant(in *CapabilityGapReport, out *assistant.CapabilityGapReport) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status = assistant.CapabilityGapReportStatus{
		ServiceName:     in.Status.ServiceName,
		ConsumerProject: in.Status.ConsumerProject,
		ContextID:       in.Status.ContextID,
		Capability:      in.Status.Capability,
		Summary:         in.Status.Summary,
	}
	return nil
}

func convert_assistant_CapabilityGapReport_To_v1alpha1(in *assistant.CapabilityGapReport, out *CapabilityGapReport) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status = CapabilityGapReportStatus{
		ServiceName:     in.Status.ServiceName,
		ConsumerProject: in.Status.ConsumerProject,
		ContextID:       in.Status.ContextID,
		Capability:      in.Status.Capability,
		Summary:         in.Status.Summary,
	}
	return nil
}

func convert_v1alpha1_CapabilityGapReportList_To_assistant(in *CapabilityGapReportList, out *assistant.CapabilityGapReportList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]assistant.CapabilityGapReport, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_CapabilityGapReport_To_assistant(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func convert_assistant_CapabilityGapReportList_To_v1alpha1(in *assistant.CapabilityGapReportList, out *CapabilityGapReportList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]CapabilityGapReport, len(in.Items))
		for i := range in.Items {
			if err := convert_assistant_CapabilityGapReport_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- AssistantEndpoint ---

func convert_v1alpha1_AssistantEndpoint_To_assistant(in *AssistantEndpoint, out *assistant.AssistantEndpoint) error {
	out.ObjectMeta = in.ObjectMeta
	out.Spec = assistant.AssistantEndpointSpec{
		URL:           in.Spec.URL,
		AgentCardPath: in.Spec.AgentCardPath,
	}
	return nil
}

func convert_assistant_AssistantEndpoint_To_v1alpha1(in *assistant.AssistantEndpoint, out *AssistantEndpoint) error {
	out.ObjectMeta = in.ObjectMeta
	out.Spec = AssistantEndpointSpec{
		URL:           in.Spec.URL,
		AgentCardPath: in.Spec.AgentCardPath,
	}
	return nil
}

func convert_v1alpha1_AssistantEndpointList_To_assistant(in *AssistantEndpointList, out *assistant.AssistantEndpointList) error {
	out.ListMeta = in.ListMeta
	out.Items = make([]assistant.AssistantEndpoint, len(in.Items))
	for i := range in.Items {
		if err := convert_v1alpha1_AssistantEndpoint_To_assistant(&in.Items[i], &out.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

func convert_assistant_AssistantEndpointList_To_v1alpha1(in *assistant.AssistantEndpointList, out *AssistantEndpointList) error {
	out.ListMeta = in.ListMeta
	out.Items = make([]AssistantEndpoint, len(in.Items))
	for i := range in.Items {
		if err := convert_assistant_AssistantEndpoint_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
			return err
		}
	}
	return nil
}
