package jsonrpc

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestParseRequest(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`)
	msg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if msg.Type != TypeRequest {
		t.Errorf("Type = %v, want TypeRequest", msg.Type)
	}
	if msg.Method != "tools/call" {
		t.Errorf("Method = %q, want 'tools/call'", msg.Method)
	}
	if string(msg.ID) != "1" {
		t.Errorf("ID = %s, want 1", string(msg.ID))
	}
}

func TestParseResponse(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
	msg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if msg.Type != TypeResponse {
		t.Errorf("Type = %v, want TypeResponse", msg.Type)
	}
	if string(msg.ID) != "1" {
		t.Errorf("ID = %s, want 1", string(msg.ID))
	}
}

func TestParseNotification(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`)
	msg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if msg.Type != TypeNotification {
		t.Errorf("Type = %v, want TypeNotification", msg.Type)
	}
	if msg.Method != "notifications/message" {
		t.Errorf("Method = %q, want 'notifications/message'", msg.Method)
	}
	if msg.ID != nil {
		t.Errorf("ID = %s, want nil", string(msg.ID))
	}
}

func TestParseErrorResponse(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":"abc","error":{"code":-32600,"message":"Invalid Request"}}`)
	msg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if msg.Type != TypeResponse {
		t.Errorf("Type = %v, want TypeResponse", msg.Type)
	}
	if string(msg.ID) != `"abc"` {
		t.Errorf("ID = %s, want \"abc\"", string(msg.ID))
	}
}

func TestParseIDTypes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"number", `{"jsonrpc":"2.0","id":42,"method":"ping"}`, "42"},
		{"zero", `{"jsonrpc":"2.0","id":0,"method":"ping"}`, "0"},
		{"string", `{"jsonrpc":"2.0","id":"req-1","method":"ping"}`, `"req-1"`},
		{"null", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := Parse([]byte(tt.data))
			if err != nil {
				t.Fatalf("Parse() error: %v", err)
			}
			if string(msg.ID) != tt.want {
				t.Errorf("ID = %s, want %s", string(msg.ID), tt.want)
			}
		})
	}
}

func TestParseMalformed(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"not json", "hello"},
		{"no method or result", `{"jsonrpc":"2.0","id":1}`},
		{"response without id", `{"jsonrpc":"2.0","result":{}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.data))
			if err == nil {
				t.Error("Parse() expected error, got nil")
			}
		})
	}
}

func TestReplaceID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`)
	newID := json.RawMessage(`"s3:n:1"`)

	result, err := ReplaceID(raw, newID)
	if err != nil {
		t.Fatalf("ReplaceID() error: %v", err)
	}

	// Parse result to verify
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if string(obj["id"]) != `"s3:n:1"` {
		t.Errorf("id = %s, want \"s3:n:1\"", string(obj["id"]))
	}

	// Verify other fields preserved
	if string(obj["method"]) != `"ping"` {
		t.Errorf("method = %s, want \"ping\"", string(obj["method"]))
	}
}

func TestScannerMultipleMessages(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n")

	scanner := NewScanner(strings.NewReader(input))

	// Message 1: request
	msg, err := scanner.Scan()
	if err != nil {
		t.Fatalf("Scan 1 error: %v", err)
	}
	if msg.Type != TypeRequest || msg.Method != "initialize" {
		t.Errorf("msg 1: type=%v method=%s", msg.Type, msg.Method)
	}

	// Message 2: notification
	msg, err = scanner.Scan()
	if err != nil {
		t.Fatalf("Scan 2 error: %v", err)
	}
	if msg.Type != TypeNotification || msg.Method != "notifications/initialized" {
		t.Errorf("msg 2: type=%v method=%s", msg.Type, msg.Method)
	}

	// Message 3: request
	msg, err = scanner.Scan()
	if err != nil {
		t.Fatalf("Scan 3 error: %v", err)
	}
	if msg.Type != TypeRequest || msg.Method != "tools/list" {
		t.Errorf("msg 3: type=%v method=%s", msg.Type, msg.Method)
	}

	// EOF
	_, err = scanner.Scan()
	if err != io.EOF {
		t.Errorf("Scan 4 = %v, want io.EOF", err)
	}
}

func TestScannerSkipsEmptyLines(t *testing.T) {
	input := "\n\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n"

	scanner := NewScanner(strings.NewReader(input))

	msg, err := scanner.Scan()
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if msg.Method != "ping" {
		t.Errorf("Method = %q, want 'ping'", msg.Method)
	}

	_, err = scanner.Scan()
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestMessageTypeString(t *testing.T) {
	if TypeRequest.String() != "request" {
		t.Errorf("TypeRequest.String() = %q", TypeRequest.String())
	}
	if TypeNotification.String() != "notification" {
		t.Errorf("TypeNotification.String() = %q", TypeNotification.String())
	}
	if TypeResponse.String() != "response" {
		t.Errorf("TypeResponse.String() = %q", TypeResponse.String())
	}
}

func TestMessageHelpers(t *testing.T) {
	req, _ := Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if !req.IsRequest() || req.IsNotification() || req.IsResponse() {
		t.Error("request helpers wrong")
	}

	notif, _ := Parse([]byte(`{"jsonrpc":"2.0","method":"notify"}`))
	if notif.IsRequest() || !notif.IsNotification() || notif.IsResponse() {
		t.Error("notification helpers wrong")
	}

	resp, _ := Parse([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	if resp.IsRequest() || resp.IsNotification() || !resp.IsResponse() {
		t.Error("response helpers wrong")
	}
}

func TestRawPreserved(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","query":"hello"}}`)
	msg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Raw should be an exact copy
	if string(msg.Raw) != string(data) {
		t.Errorf("Raw not preserved:\ngot:  %s\nwant: %s", string(msg.Raw), string(data))
	}
}
