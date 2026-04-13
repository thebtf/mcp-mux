package owner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

// ---------------------------------------------------------------------------
// Wiring tests: cacheResponse and broadcastListChanged
// ---------------------------------------------------------------------------

// TestCacheResponse_InjectsListChanged verifies that cacheResponse rewrites an
// initialize response that lacks listChanged so that o.initResp contains
// listChanged:true for each capability present in the upstream response.
func TestCacheResponse_InjectsListChanged(t *testing.T) {
	o := newMinimalOwner()

	// Upstream response deliberately omits listChanged on tools/prompts/resources.
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"prompts":{},"resources":{}},"serverInfo":{"name":"test","version":"1.0"}}}`)

	o.cacheResponse("initialize", raw)

	o.mu.RLock()
	cached := o.initResp
	o.mu.RUnlock()

	if len(cached) == 0 {
		t.Fatal("initResp is empty after cacheResponse")
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
	if err := json.Unmarshal(cached, &root); err != nil {
		t.Fatalf("initResp is not valid JSON: %v", err)
	}
	if !root.Result.Capabilities.Tools["listChanged"] {
		t.Error("tools.listChanged not injected into initResp")
	}
	if !root.Result.Capabilities.Prompts["listChanged"] {
		t.Error("prompts.listChanged not injected into initResp")
	}
	if !root.Result.Capabilities.Resources["listChanged"] {
		t.Error("resources.listChanged not injected into initResp")
	}
}

// TestCacheResponse_InjectIdempotent verifies that cacheResponse does not
// double-inject listChanged when the upstream response already declares it.
// The stored initResp bytes must contain exactly one occurrence of
// "listChanged":true per capability, not two.
func TestCacheResponse_InjectIdempotent(t *testing.T) {
	o := newMinimalOwner()

	// Upstream already advertises listChanged:true on all three capabilities.
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{"listChanged":true},"prompts":{"listChanged":true},"resources":{"listChanged":true}}}}`)

	o.cacheResponse("initialize", raw)

	o.mu.RLock()
	cached := o.initResp
	o.mu.RUnlock()

	if len(cached) == 0 {
		t.Fatal("initResp is empty after cacheResponse")
	}

	// Count occurrences of "listChanged" in the stored JSON.
	// Each capability contributes exactly one key; three capabilities = three occurrences.
	count := bytes.Count(cached, []byte("listChanged"))
	if count != 3 {
		t.Errorf("expected exactly 3 listChanged occurrences in initResp, got %d: %s", count, string(cached))
	}
}

// TestBroadcastListChanged_SendsToAllSessions verifies that broadcastListChanged
// delivers all three list_changed notifications to every registered session.
func TestBroadcastListChanged_SendsToAllSessions(t *testing.T) {
	o := newMinimalOwner()

	// Two sessions, each backed by a safeBuf so we can inspect output.
	var buf1, buf2 safeBuf
	s1 := NewSession(strings.NewReader(""), &buf1)
	s2 := NewSession(strings.NewReader(""), &buf2)

	o.mu.Lock()
	o.sessions[s1.ID] = s1
	o.sessions[s2.ID] = s2
	o.mu.Unlock()

	o.broadcastListChanged()

	// Give drainNotifications goroutines time to flush.
	time.Sleep(50 * time.Millisecond)

	for i, buf := range []*safeBuf{&buf1, &buf2} {
		got := buf.String()
		if !strings.Contains(got, "notifications/tools/list_changed") {
			t.Errorf("session %d: missing tools/list_changed; got: %s", i+1, got)
		}
		if !strings.Contains(got, "notifications/prompts/list_changed") {
			t.Errorf("session %d: missing prompts/list_changed; got: %s", i+1, got)
		}
		if !strings.Contains(got, "notifications/resources/list_changed") {
			t.Errorf("session %d: missing resources/list_changed; got: %s", i+1, got)
		}
	}
}

// TestBroadcastListChanged_NoSessions verifies that broadcastListChanged does
// not panic when there are no registered sessions.
func TestBroadcastListChanged_NoSessions(t *testing.T) {
	o := newMinimalOwner()
	// Intentionally no sessions registered.
	o.broadcastListChanged() // must not panic
}
