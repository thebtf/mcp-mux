package jsonrpc

import (
	"encoding/json"
	"testing"
)

func TestInjectMetaNoParams(t *testing.T) {
	// Request with no params — InjectMeta should create params and _meta
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)
	result, err := InjectMeta(raw, "sessionId", "sess-1")
	if err != nil {
		t.Fatalf("InjectMeta() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(obj["params"], &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		t.Fatalf("unmarshal _meta: %v", err)
	}

	var val string
	if err := json.Unmarshal(meta["sessionId"], &val); err != nil {
		t.Fatalf("unmarshal sessionId: %v", err)
	}
	if val != "sess-1" {
		t.Errorf("sessionId = %q, want %q", val, "sess-1")
	}
}

func TestInjectMetaWithExistingParams(t *testing.T) {
	// Request already has params (but no _meta)
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`)
	result, err := InjectMeta(raw, "clientId", "c-42")
	if err != nil {
		t.Fatalf("InjectMeta() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(obj["params"], &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	// Existing field should still be present
	if _, ok := params["name"]; !ok {
		t.Error("existing params field 'name' was lost")
	}

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		t.Fatalf("unmarshal _meta: %v", err)
	}

	var val string
	if err := json.Unmarshal(meta["clientId"], &val); err != nil {
		t.Fatalf("unmarshal clientId: %v", err)
	}
	if val != "c-42" {
		t.Errorf("clientId = %q, want %q", val, "c-42")
	}
}

func TestInjectMetaWithExistingMeta(t *testing.T) {
	// Request already has params._meta with a different key
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"existing":"value"}}}`)
	result, err := InjectMeta(raw, "newKey", "newVal")
	if err != nil {
		t.Fatalf("InjectMeta() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(obj["params"], &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		t.Fatalf("unmarshal _meta: %v", err)
	}

	// Both existing and new key must be present
	if _, ok := meta["existing"]; !ok {
		t.Error("existing _meta field was lost")
	}
	if _, ok := meta["newKey"]; !ok {
		t.Error("new _meta field was not added")
	}
}

func TestInjectMetaOverwriteKey(t *testing.T) {
	// Overwrite an already-existing _meta key
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"sessionId":"old"}}}`)
	result, err := InjectMeta(raw, "sessionId", "new")
	if err != nil {
		t.Fatalf("InjectMeta() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(obj["params"], &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		t.Fatalf("unmarshal _meta: %v", err)
	}

	var val string
	if err := json.Unmarshal(meta["sessionId"], &val); err != nil {
		t.Fatalf("unmarshal sessionId: %v", err)
	}
	if val != "new" {
		t.Errorf("sessionId = %q, want %q", val, "new")
	}
}

func TestInjectMetaInvalidJSON(t *testing.T) {
	_, err := InjectMeta([]byte("not json"), "key", "val")
	if err == nil {
		t.Error("InjectMeta() expected error for invalid JSON, got nil")
	}
}

func TestInjectMetaParamsNotObject(t *testing.T) {
	// params exists but is not an object (it's an array) — should error
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":[1,2,3]}`)
	_, err := InjectMeta(raw, "key", "val")
	if err == nil {
		t.Error("InjectMeta() expected error when params is not an object, got nil")
	}
}

func TestInjectMetaMetaNonObjectFallback(t *testing.T) {
	// _meta exists but is not an object — should silently create a fresh _meta
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":"not-an-object"}}`)
	result, err := InjectMeta(raw, "k", "v")
	if err != nil {
		t.Fatalf("InjectMeta() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(obj["params"], &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		t.Fatalf("unmarshal _meta: %v", err)
	}

	if _, ok := meta["k"]; !ok {
		t.Error("expected key 'k' in freshly-created _meta, not found")
	}
}
