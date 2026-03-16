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
	var resp struct {
		Result struct {
			Capabilities struct {
				XMux *struct {
					Sharing string `json:"sharing"`
				} `json:"x-mux"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initJSON, &resp); err != nil {
		return "", false
	}

	if resp.Result.Capabilities.XMux == nil {
		return "", false
	}

	mode := SharingMode(resp.Result.Capabilities.XMux.Sharing)
	switch mode {
	case ModeShared, ModeIsolated, ModeSessionAware:
		return mode, true
	default:
		return "", false
	}
}
