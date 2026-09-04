package v1alpha1

// This file provides minimal protobuf Marshal/Unmarshal methods for the
// assistant types so they can be served to clients that request the protobuf
// content type (e.g., the kube-apiserver's namespace garbage collector).
//
// The k8s.io/apimachinery protobuf serializer wraps objects that implement
// Marshal() ([]byte, error) in a runtime.Unknown envelope. We delegate to
// JSON encoding since our types don't have generated protobuf definitions.
// This is the standard approach for aggregated apiservers that don't want
// to generate protobuf bindings.

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
