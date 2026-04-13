package listchanged

import (
	"encoding/json"
	"testing"
)

func TestInjectInitializeCapability_AddsMissingFlag(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"prompts":{},"resources":{}},"serverInfo":{"name":"x","version":"1.0"}}}`)

	out, ok := InjectInitializeCapability(input)
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

func TestInjectInitializeCapability_PreservesOtherFields(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"otherFlag":true},"experimental":{"x":1}},"serverInfo":{"name":"srv","version":"2.3"},"instructions":"hi"}}`)

	out, ok := InjectInitializeCapability(input)
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

func TestInjectInitializeCapability_SkipsWhenAbsent(t *testing.T) {
	// Upstream declares no tools/prompts/resources at all. We must NOT
	// synthesize a tools capability where there is none — CC would then
	// call tools/list against an upstream that does not support it.
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"experimental":{"x-mux":{"persistent":true}}}}}`)

	out, ok := InjectInitializeCapability(input)
	if ok {
		t.Fatalf("expected no mutation, got %s", string(out))
	}
}

func TestInjectInitializeCapability_NoopWhenAlreadyTrue(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{"listChanged":true},"prompts":{"listChanged":true},"resources":{"listChanged":true}}}}`)

	_, ok := InjectInitializeCapability(input)
	if ok {
		t.Fatal("expected no mutation when all three already declare listChanged=true")
	}
}

func TestInjectInitializeCapability_InvalidJSON(t *testing.T) {
	_, ok := InjectInitializeCapability([]byte("not json"))
	if ok {
		t.Fatal("expected no mutation on invalid JSON")
	}
}

func TestInjectInitializeCapability_PreservesTrailingNewline(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}}}}` + "\n")
	out, ok := InjectInitializeCapability(input)
	if !ok {
		t.Fatal("expected mutation")
	}
	if out[len(out)-1] != '\n' {
		t.Error("trailing newline not preserved")
	}
}

func TestInjectCapability_AddsMissingListChanged(t *testing.T) {
	input := json.RawMessage(`{"someFlag":true}`)
	out, mutated := InjectCapability(input)
	if !mutated {
		t.Fatal("expected mutation")
	}
	var obj map[string]bool
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if !obj["listChanged"] {
		t.Error("listChanged not set")
	}
	if !obj["someFlag"] {
		t.Error("someFlag lost")
	}
}

func TestInjectCapability_NoopWhenEmpty(t *testing.T) {
	_, mutated := InjectCapability(nil)
	if mutated {
		t.Fatal("expected no mutation for nil input (capability not declared by upstream)")
	}
	_, mutated = InjectCapability(json.RawMessage{})
	if mutated {
		t.Fatal("expected no mutation for empty input")
	}
}

func TestInjectCapability_NoopWhenAlreadyTrue(t *testing.T) {
	input := json.RawMessage(`{"listChanged":true}`)
	_, mutated := InjectCapability(input)
	if mutated {
		t.Fatal("expected no mutation when listChanged already true")
	}
}

func TestInjectCapability_MutatesWhenFalse(t *testing.T) {
	input := json.RawMessage(`{"listChanged":false}`)
	out, mutated := InjectCapability(input)
	if !mutated {
		t.Fatal("expected mutation when listChanged=false")
	}
	var obj map[string]bool
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if !obj["listChanged"] {
		t.Error("listChanged not set to true")
	}
}
