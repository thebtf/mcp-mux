// Package control provides a separate control plane for managing mcp-mux owners.
//
// The control protocol uses NDJSON (one JSON object per line) over a Unix domain socket,
// intentionally NOT JSON-RPC to avoid confusion with MCP data traffic.
// Each connection handles exactly one request-response pair, then closes.
package control

import "encoding/json"

// Request is a control plane command sent by the CLI to an owner or daemon.
type Request struct {
	Cmd            string            `json:"cmd"`
	DrainTimeoutMs int               `json:"drain_timeout_ms,omitempty"`

	// Spawn fields (daemon control protocol)
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Mode    string            `json:"mode,omitempty"` // "cwd", "global", "isolated"
	Env     map[string]string `json:"env,omitempty"`
}

// Response is the reply to a control command.
type Response struct {
	OK       bool            `json:"ok"`
	Message  string          `json:"message,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	IPCPath  string          `json:"ipc_path,omitempty"`
	ServerID string          `json:"server_id,omitempty"`
}

// CommandHandler is implemented by the Owner to handle control commands.
type CommandHandler interface {
	HandleShutdown(drainTimeoutMs int) string
	HandleStatus() map[string]interface{}
}

// DaemonHandler extends CommandHandler with daemon-specific commands.
type DaemonHandler interface {
	CommandHandler
	HandleSpawn(req Request) (ipcPath, serverID string, err error)
	HandleRemove(serverID string) error
}
