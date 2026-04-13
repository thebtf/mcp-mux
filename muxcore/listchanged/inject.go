// Package listchanged rewrites MCP initialize responses so that tools,
// prompts and resources capabilities each advertise listChanged=true.
//
// Why this is necessary: Claude Code's MCP client only subscribes to
// notifications/tools/list_changed (and the prompts/resources siblings)
// when the server declares listChanged in its initialize capabilities.
// See claude-code leaked source, src/services/mcp/useManageMCPConnections.ts
// lines 618-699: the notification handler registration is gated on
// client.capabilities?.tools?.listChanged (and prompts/resources variants).
//
// mcp-mux is responsible for cache-busting the CC side whenever upstream
// tools/prompts/resources change (hot reload, explicit mux_restart, etc.).
// To do that we must convince CC to subscribe, which means overriding
// whatever the upstream advertised in its initializeResult. We inject
// listChanged=true unconditionally.
package listchanged

import (
	"bytes"
	"encoding/json"
)

// InjectInitializeCapability rewrites a cached initialize response so that
// tools, prompts and resources capabilities each advertise listChanged=true.
//
// The injected result is a new []byte; the input is not mutated.
//
// Returns ok=false when the input is not a parseable JSON-RPC response
// with a result.capabilities object, in which case the caller must keep
// the original bytes untouched. Invalid JSON should never reach this
// function (only cached server responses do) but the fallback keeps
// the layer idempotent and panic-free.
func InjectInitializeCapability(raw []byte) (out []byte, ok bool) {
	// Parse into a generic map so we preserve unknown fields verbatim on
	// round-trip (this is important for protocolVersion, serverInfo,
	// instructions, experimental capabilities, _meta — none of which
	// we want to drop).
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	resultRaw, hasResult := root["result"]
	if !hasResult {
		return nil, false
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return nil, false
	}

	capsRaw, hasCaps := result["capabilities"]
	var caps map[string]json.RawMessage
	if hasCaps {
		if err := json.Unmarshal(capsRaw, &caps); err != nil {
			return nil, false
		}
	}
	if caps == nil {
		caps = make(map[string]json.RawMessage)
	}

	changed := false
	for _, key := range []string{"tools", "prompts", "resources"} {
		if forced, mutated := InjectCapability(caps[key]); mutated {
			caps[key] = forced
			changed = true
		}
	}
	if !changed {
		// All three capabilities already had listChanged=true (or the
		// upstream is so minimal it has no tools/prompts/resources at
		// all). No edit needed.
		return nil, false
	}

	encodedCaps, err := json.Marshal(caps)
	if err != nil {
		return nil, false
	}
	result["capabilities"] = encodedCaps

	encodedResult, err := json.Marshal(result)
	if err != nil {
		return nil, false
	}
	root["result"] = encodedResult

	encodedRoot, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	// json.Marshal strips trailing newlines; the MCP wire protocol is line
	// delimited, so callers rely on a trailing \n matching the upstream
	// original. Preserve trailing newline if the input had one.
	if bytes.HasSuffix(raw, []byte("\n")) {
		encodedRoot = append(encodedRoot, '\n')
	}
	return encodedRoot, true
}

// InjectCapability returns a JSON object with listChanged=true set.
// Accepts: nil/missing input (returns mutated=false — we must not synthesize
// a capability the upstream didn't declare), an existing object (adds/overrides
// listChanged=true), or any other shape (treats as absent and returns false).
//
// Returns mutated=false only when:
//   - the input is nil/empty (upstream didn't declare the capability)
//   - the input was already an object with listChanged=true (no rewrite necessary)
//   - the input is not a JSON object (cannot safely mutate)
func InjectCapability(input json.RawMessage) (out json.RawMessage, mutated bool) {
	if len(input) == 0 {
		// Upstream didn't declare this capability at all. We must not
		// synthesize it: declaring tools support when upstream has none
		// would make CC attempt tools/list against a server that will
		// error out. Skip — only mutate existing capability blocks.
		return nil, false
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil {
		// Not an object (maybe null, boolean, or array); cannot safely
		// mutate. Leave alone.
		return nil, false
	}
	if obj == nil {
		obj = make(map[string]json.RawMessage)
	}

	// Already has listChanged=true? No-op.
	if existing, ok := obj["listChanged"]; ok {
		var b bool
		if err := json.Unmarshal(existing, &b); err == nil && b {
			return nil, false
		}
	}
	obj["listChanged"] = json.RawMessage("true")

	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return encoded, true
}
