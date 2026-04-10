package mux

import (
	"encoding/json"
	"testing"
)

func TestInjectListChanged_AddsMissingFlag(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"prompts":{},"resources":{}},"serverInfo":{"name":"x","version":"1.0"}}}`)

	out, ok := injectListChangedCapabilities(input)
	if !ok {
		t.Fatal("expected mutation for caps without listChanged")
	}

	var root struct {
		Result struct {
			Capabilities struct {
				Tools     map[string]bool `json:"tools"`
				Prompts   map[string]bool `json:"prompts"`
				Resources map[string]bool `json:"resources"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if !root.Result.Capabilities.Tools["listChanged"] {
		t.Error("tools.listChanged not set")
	}
	if !root.Result.Capabilities.Prompts["listChanged"] {
		t.Error("prompts.listChanged not set")
	}
	if !root.Result.Capabilities.Resources["listChanged"] {
		t.Error("resources.listChanged not set")
	}
}

func TestInjectListChanged_PreservesOtherFields(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"otherFlag":true},"experimental":{"x":1}},"serverInfo":{"name":"srv","version":"2.3"},"instructions":"hi"}}`)

	out, ok := injectListChangedCapabilities(input)
	if !ok {
		t.Fatal("expected mutation")
	}

	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	result := root["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Error("protocolVersion lost")
	}
	if result["instructions"] != "hi" {
		t.Error("instructions lost")
	}
	si := result["serverInfo"].(map[string]any)
	if si["name"] != "srv" || si["version"] != "2.3" {
		t.Error("serverInfo lost")
	}
	caps := result["capabilities"].(map[string]any)
	if exp := caps["experimental"].(map[string]any); exp["x"] == nil {
		t.Error("experimental.x lost")
	}
	tools := caps["tools"].(map[string]any)
	if tools["otherFlag"] != true {
		t.Error("tools.otherFlag lost")
	}
	if tools["listChanged"] != true {
		t.Error("tools.listChanged not set")
	}
}

func TestInjectListChanged_SkipsWhenAbsent(t *testing.T) {
	// Upstream declares no tools/prompts/resources at all. We must NOT
	// synthesize a tools capability where there is none — CC would then
	// call tools/list against an upstream that does not support it.
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"experimental":{"x-mux":{"persistent":true}}}}}`)

	out, ok := injectListChangedCapabilities(input)
	if ok {
		t.Fatalf("expected no mutation, got %s", string(out))
	}
}

func TestInjectListChanged_NoopWhenAlreadyTrue(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{"listChanged":true},"prompts":{"listChanged":true},"resources":{"listChanged":true}}}}`)

	_, ok := injectListChangedCapabilities(input)
	if ok {
		t.Fatal("expected no mutation when all three already declare listChanged=true")
	}
}

func TestInjectListChanged_InvalidJSON(t *testing.T) {
	_, ok := injectListChangedCapabilities([]byte("not json"))
	if ok {
		t.Fatal("expected no mutation on invalid JSON")
	}
}

func TestInjectListChanged_PreservesTrailingNewline(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}}}}` + "\n")
	out, ok := injectListChangedCapabilities(input)
	if !ok {
		t.Fatal("expected mutation")
	}
	if out[len(out)-1] != '\n' {
		t.Error("trailing newline not preserved")
	}
}
