package assistant

// This file provides minimal protobuf Marshal/Unmarshal methods for the
// assistant internal types. See v1alpha1/protobuf.go for rationale.

import "encoding/json"

// --- Conversation ---

func (in *Conversation) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *Conversation) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *ConversationList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *ConversationList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- ConversationMessages ---

func (in *ConversationMessages) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *ConversationMessages) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- AssistantEndpoint ---

func (in *AssistantEndpoint) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *AssistantEndpoint) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *AssistantEndpointList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *AssistantEndpointList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }
