package classify

import "encoding/json"

// SharingMode indicates whether an MCP server should be shared or isolated.
type SharingMode string

const (
	ModeShared       SharingMode = "shared"
	ModeIsolated     SharingMode = "isolated"
	ModeSessionAware SharingMode = "session-aware"
)

// ClassifyTools parses a tools/list JSON-RPC response and classifies the server
// as shared or isolated based on tool name patterns.
//
// ANY isolation pattern match → isolated. Otherwise → shared.
// Returns the mode and a list of tool names that triggered isolation.
func ClassifyTools(toolsListJSON []byte) (SharingMode, []string) {
	// Parse as JSON-RPC response: {"jsonrpc":"2.0","id":...,"result":{"tools":[...]}}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(toolsListJSON, &resp); err != nil {
		// Can't parse — assume shared (safe default)
		return ModeShared, nil
	}

	var matched []string
	for _, tool := range resp.Result.Tools {
		if isIsolationPattern(tool.Name) {
			matched = append(matched, tool.Name)
		}
	}

	if len(matched) > 0 {
		return ModeIsolated, matched
	}
	return ModeShared, nil
}

// unmarshalXMux extracts and unmarshals the x-mux capability block from an
// initialize JSON-RPC response into out. Checks capabilities.x-mux directly
// first, then falls back to capabilities.experimental["x-mux"] (TypeScript SDK
// places custom capabilities there). Returns false if absent or if unmarshalling
// fails.
func unmarshalXMux(initJSON []byte, out any) bool {
	var resp struct {
		Result struct {
			Capabilities struct {
				XMux         json.RawMessage            `json:"x-mux"`
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initJSON, &resp); err != nil {
		return false
	}
	if len(resp.Result.Capabilities.XMux) > 0 {
		return json.Unmarshal(resp.Result.Capabilities.XMux, out) == nil
	}
	raw, ok := resp.Result.Capabilities.Experimental["x-mux"]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

// ClassifyCapabilities parses an initialize JSON-RPC response and extracts
// the x-mux capability to determine the server's declared sharing mode.
//
// Returns the declared mode and true if x-mux was found, or ("", false) if absent.
// This takes priority over tool-name classification when present.
func ClassifyCapabilities(initJSON []byte) (SharingMode, bool) {
	var xmux struct {
		Sharing string `json:"sharing"`
	}
	if !unmarshalXMux(initJSON, &xmux) {
		return "", false
	}
	mode := SharingMode(xmux.Sharing)
	switch mode {
	case ModeShared, ModeIsolated, ModeSessionAware:
		return mode, true
	default:
		return "", false
	}
}

// ParsePersistent extracts x-mux.persistent from a cached initialize response.
// Returns true if the server declares itself as persistent.
func ParsePersistent(initJSON []byte) bool {
	var xmux struct {
		Persistent bool `json:"persistent"`
	}
	if !unmarshalXMux(initJSON, &xmux) {
		return false
	}
	return xmux.Persistent
}

// ParseToolTimeout extracts x-mux.toolTimeout from a cached initialize response.
// Returns the declared tool call timeout in seconds, or 0 if not declared.
// When set, mux wraps tools/call requests in a watchdog that synthesizes a
// JSON-RPC error response if upstream doesn't respond within the timeout.
// Prevents eternal hangs when upstream deadlocks or crashes silently.
func ParseToolTimeout(initJSON []byte) int {
	var xmux struct {
		ToolTimeout int `json:"toolTimeout"`
	}
	if !unmarshalXMux(initJSON, &xmux) {
		return 0
	}
	if xmux.ToolTimeout <= 0 {
		return 0
	}
	// Cap at 1 hour to prevent unreasonable values
	if xmux.ToolTimeout > 3600 {
		return 3600
	}
	return xmux.ToolTimeout
}

// ParseDrainTimeout extracts x-mux.drainTimeout from a cached initialize response.
// Returns the declared drain timeout in seconds, or 0 if not declared.
// Servers use this to tell mux how long they need to gracefully shut down
// (e.g., drain running async jobs, flush state).
func ParseDrainTimeout(initJSON []byte) int {
	var xmux struct {
		DrainTimeout int `json:"drainTimeout"`
	}
	if !unmarshalXMux(initJSON, &xmux) {
		return 0
	}
	if xmux.DrainTimeout <= 0 {
		return 0
	}
	// Cap at 5 minutes to prevent runaway drain
	if xmux.DrainTimeout > 300 {
		return 300
	}
	return xmux.DrainTimeout
}

// ParseIdleTimeout extracts x-mux.idleTimeout from a cached initialize response.
// Returns the declared owner idle timeout in seconds, or 0 if not declared.
// When set, the daemon reaper uses this value instead of the daemon default
// (MCP_MUX_OWNER_IDLE or 10m) to decide when to evict this owner.
//
// Upstreams that know they hold stateful background work (async job queues,
// persistent LLM sessions, in-memory caches) should declare a long idle
// timeout to survive multi-hour quiet periods. Upstreams that are cheap to
// re-spawn can declare a short timeout to free RAM faster.
func ParseIdleTimeout(initJSON []byte) int {
	var xmux struct {
		IdleTimeout int `json:"idleTimeout"`
	}
	if !unmarshalXMux(initJSON, &xmux) {
		return 0
	}
	if xmux.IdleTimeout <= 0 {
		return 0
	}
	// Cap at 24 hours. Larger values are almost certainly a unit mistake
	// (ms vs seconds) and would effectively disable the reaper.
	if xmux.IdleTimeout > 86400 {
		return 86400
	}
	return xmux.IdleTimeout
}

// ParseProgressInterval reads the x-mux.progressInterval field from an
// initialize JSON response and returns the value in seconds.
// Returns 0 if absent or invalid. Clamps to 60 if over.
func ParseProgressInterval(initJSON []byte) int {
	var xmux struct {
		ProgressInterval int `json:"progressInterval"`
	}
	if !unmarshalXMux(initJSON, &xmux) {
		return 0
	}
	if xmux.ProgressInterval <= 0 {
		return 0
	}
	// Cap at 60 seconds. Larger values defeat the purpose of progress reporting.
	if xmux.ProgressInterval > 60 {
		return 60
	}
	return xmux.ProgressInterval
}
