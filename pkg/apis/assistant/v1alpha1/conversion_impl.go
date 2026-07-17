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
	}
	return nil
}

func convert_assistant_Conversation_To_v1alpha1(in *assistant.Conversation, out *Conversation) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status = ConversationStatus{
		LastActiveAt: in.Status.LastActiveAt,
		MessageCount: in.Status.MessageCount,
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
