// Package jsonrpc provides JSON-RPC 2.0 message parsing for MCP multiplexing.
//
// It parses just enough to route messages (method, id) while preserving the
// raw JSON for efficient forwarding with only the id field replaced.
package jsonrpc

import (
	"encoding/json"
	"fmt"
)

// MessageType indicates the kind of JSON-RPC 2.0 message.
type MessageType int

const (
	TypeRequest      MessageType = iota // Has method + id
	TypeNotification                    // Has method, no id
	TypeResponse                        // Has result or error + id
)

// String returns a human-readable name for the message type.
func (t MessageType) String() string {
	switch t {
	case TypeRequest:
		return "request"
	case TypeNotification:
		return "notification"
	case TypeResponse:
		return "response"
	default:
		return "unknown"
	}
}

// Message is a parsed JSON-RPC 2.0 message.
// Only the fields needed for routing are extracted; the full raw JSON is preserved.
type Message struct {
	Type   MessageType
	ID     json.RawMessage // nil for notifications
	Method string          // For requests/notifications; empty for responses
	Raw    []byte          // The complete original JSON line
}

// rawEnvelope is the internal struct for partial JSON-RPC parsing.
// We do NOT use it for ID detection — a map-based lookup handles absent vs null.
type rawEnvelope struct {
	Method *string          `json:"method,omitempty"`
	Result *json.RawMessage `json:"result,omitempty"`
	Error  *json.RawMessage `json:"error,omitempty"`
}

// Parse parses a raw JSON line into a Message.
func Parse(data []byte) (*Message, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("jsonrpc: empty message")
	}

	// Use a raw map to distinguish absent id from null id.
	// JSON-RPC spec: absent id = notification; null id = request with null id.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("jsonrpc: parse: %w", err)
	}

	var env rawEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("jsonrpc: parse: %w", err)
	}

	msg := &Message{
		Raw: make([]byte, len(data)),
	}
	copy(msg.Raw, data)

	// Detect id presence and preserve raw value (including null).
	idRaw, idPresent := raw["id"]
	if idPresent {
		msg.ID = make(json.RawMessage, len(idRaw))
		copy(msg.ID, idRaw)
	}

	// Determine message type
	if env.Method != nil {
		msg.Method = *env.Method
		if idPresent {
			msg.Type = TypeRequest
		} else {
			msg.Type = TypeNotification
		}
	} else if env.Result != nil || env.Error != nil {
		msg.Type = TypeResponse
		if !idPresent {
			return nil, fmt.Errorf("jsonrpc: response without id")
		}
	} else {
		return nil, fmt.Errorf("jsonrpc: message has neither method nor result/error")
	}

	return msg, nil
}

// IsNotification returns true if the message is a notification (no id).
func (m *Message) IsNotification() bool {
	return m.Type == TypeNotification
}

// IsRequest returns true if the message is a request (has method and id).
func (m *Message) IsRequest() bool {
	return m.Type == TypeRequest
}

// IsResponse returns true if the message is a response (has result/error and id).
func (m *Message) IsResponse() bool {
	return m.Type == TypeResponse
}

// ReplaceID returns a new JSON message with the id field replaced.
// The rest of the message is preserved as-is.
func ReplaceID(raw []byte, newID json.RawMessage) ([]byte, error) {
	// Parse into a generic map to preserve all fields
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("jsonrpc: replace id: %w", err)
	}

	obj["id"] = newID

	result, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: marshal after replace: %w", err)
	}

	return result, nil
}
