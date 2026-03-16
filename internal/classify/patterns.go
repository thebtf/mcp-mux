// Package classify provides convention-based auto-detection of MCP server
// sharing mode by analyzing tool names from the tools/list response.
package classify

import "strings"

// isolationPrefixes are tool name prefixes that indicate stateful server behavior.
// Any tool matching these patterns suggests the server maintains per-session state
// (browser tabs, editor buffers, navigation history) and should be isolated.
var isolationPrefixes = []string{
	"browser_",
	"session_",
	"editor_",
	"navigate",
	"page_",
	"tab_",
}

// isolationSubstrings are substrings that indicate stateful behavior when found
// anywhere in the tool name. Used for tools like "get_editor_state" or
// "interact_with_process" that don't match prefix patterns.
var isolationSubstrings = []string{
	"_process",   // desktop-commander: start_process, interact_with_process, kill_process
	"_document",  // pencil: open_document
	"_editor_",   // pencil: get_editor_state
	"snapshot",   // pencil: snapshot_layout (canvas state)
}

// isIsolationPattern returns true if the tool name matches any isolation pattern.
func isIsolationPattern(name string) bool {
	lower := strings.ToLower(name)

	for _, prefix := range isolationPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}

	for _, sub := range isolationSubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}

	return false
}
