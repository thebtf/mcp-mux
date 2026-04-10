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

// ClassifyCapabilities parses an initialize JSON-RPC response and extracts
// the x-mux capability to determine the server's declared sharing mode.
//
// Returns the declared mode and true if x-mux was found, or ("", false) if absent.
// This takes priority over tool-name classification when present.
func ClassifyCapabilities(initJSON []byte) (SharingMode, bool) {
	// Try direct x-mux capability first
	var resp struct {
		Result struct {
			Capabilities struct {
				XMux *struct {
					Sharing string `json:"sharing"`
				} `json:"x-mux"`
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initJSON, &resp); err != nil {
		return "", false
	}

	// Check direct x-mux capability
	xmux := resp.Result.Capabilities.XMux

	// Fallback: check experimental.x-mux (TypeScript SDK puts custom capabilities here)
	if xmux == nil && resp.Result.Capabilities.Experimental != nil {
		if raw, ok := resp.Result.Capabilities.Experimental["x-mux"]; ok {
			xmux = &struct {
				Sharing string `json:"sharing"`
			}{}
			if err := json.Unmarshal(raw, xmux); err != nil {
				xmux = nil
			}
		}
	}

	if xmux == nil {
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
	// Try direct x-mux capability
	var resp struct {
		Result struct {
			Capabilities struct {
				XMux *struct {
					Persistent bool `json:"persistent"`
				} `json:"x-mux"`
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initJSON, &resp); err != nil {
		return false
	}

	xmux := resp.Result.Capabilities.XMux

	// Fallback: check experimental.x-mux
	if xmux == nil && resp.Result.Capabilities.Experimental != nil {
		if raw, ok := resp.Result.Capabilities.Experimental["x-mux"]; ok {
			xmux = &struct {
				Persistent bool `json:"persistent"`
			}{}
			if err := json.Unmarshal(raw, xmux); err != nil {
				return false
			}
		}
	}

	if xmux == nil {
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
	var resp struct {
		Result struct {
			Capabilities struct {
				XMux *struct {
					ToolTimeout int `json:"toolTimeout"`
				} `json:"x-mux"`
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initJSON, &resp); err != nil {
		return 0
	}

	xmux := resp.Result.Capabilities.XMux

	if xmux == nil && resp.Result.Capabilities.Experimental != nil {
		if raw, ok := resp.Result.Capabilities.Experimental["x-mux"]; ok {
			xmux = &struct {
				ToolTimeout int `json:"toolTimeout"`
			}{}
			if err := json.Unmarshal(raw, xmux); err != nil {
				return 0
			}
		}
	}

	if xmux == nil || xmux.ToolTimeout <= 0 {
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
	var resp struct {
		Result struct {
			Capabilities struct {
				XMux *struct {
					DrainTimeout int `json:"drainTimeout"`
				} `json:"x-mux"`
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initJSON, &resp); err != nil {
		return 0
	}

	xmux := resp.Result.Capabilities.XMux

	// Fallback: check experimental.x-mux
	if xmux == nil && resp.Result.Capabilities.Experimental != nil {
		if raw, ok := resp.Result.Capabilities.Experimental["x-mux"]; ok {
			xmux = &struct {
				DrainTimeout int `json:"drainTimeout"`
			}{}
			if err := json.Unmarshal(raw, xmux); err != nil {
				return 0
			}
		}
	}

	if xmux == nil || xmux.DrainTimeout <= 0 {
		return 0
	}
	// Cap at 5 minutes to prevent runaway drain
	if xmux.DrainTimeout > 300 {
		return 300
	}
	return xmux.DrainTimeout
}
