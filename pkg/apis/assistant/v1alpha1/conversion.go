package v1alpha1

import (
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// RegisterConversions wires conversion functions for round-tripping between
// v1alpha1 and internal assistant types. The internal and external structs are
// declared with identical field shapes; sub-types differ only by tag, so
// conversion is a series of mechanical field copies.
func RegisterConversions(s *runtime.Scheme) error {
	pairs := []struct {
		internal, external any
		toInternal         conversion.ConversionFunc
		toExternal         conversion.ConversionFunc
	}{
		{
			(*assistant.Conversation)(nil), (*Conversation)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_Conversation_To_assistant(a.(*Conversation), b.(*assistant.Conversation))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_assistant_Conversation_To_v1alpha1(a.(*assistant.Conversation), b.(*Conversation))
			},
		},
		{
			(*assistant.ConversationList)(nil), (*ConversationList)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_ConversationList_To_assistant(a.(*ConversationList), b.(*assistant.ConversationList))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_assistant_ConversationList_To_v1alpha1(a.(*assistant.ConversationList), b.(*ConversationList))
			},
		},
		{
			(*assistant.ConversationMessages)(nil), (*ConversationMessages)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_ConversationMessages_To_assistant(a.(*ConversationMessages), b.(*assistant.ConversationMessages))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_assistant_ConversationMessages_To_v1alpha1(a.(*assistant.ConversationMessages), b.(*ConversationMessages))
			},
		},
	}
	for _, p := range pairs {
		if err := s.AddGeneratedConversionFunc(p.external, p.internal, p.toInternal); err != nil {
			return err
		}
		if err := s.AddGeneratedConversionFunc(p.internal, p.external, p.toExternal); err != nil {
			return err
		}
	}
	return nil
}
