// Package control provides a separate control plane for managing mcp-mux owners.
//
// The control protocol uses NDJSON (one JSON object per line) over a Unix domain socket,
// intentionally NOT JSON-RPC to avoid confusion with MCP data traffic.
// Each connection handles exactly one request-response pair, then closes.
package control

import "encoding/json"

// Request is a control plane command sent by the CLI to an owner or daemon.
// Supported daemon commands include "spawn", "remove", "graceful-restart",
// and "refresh-token".
type Request struct {
	Cmd            string `json:"cmd"`
	DrainTimeoutMs int    `json:"drain_timeout_ms,omitempty"`
	ServerID       string `json:"server_id,omitempty"`
	// SuccessorExe optionally tells a graceful-restart capable daemon which
	// executable should be launched as the successor. When empty, the daemon
	// falls back to its environment-driven successor resolution.
	SuccessorExe string `json:"successor_exe,omitempty"`

	// Spawn fields (daemon control protocol)
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Mode    string            `json:"mode,omitempty"` // "cwd", "global", "isolated"
	Env     map[string]string `json:"env,omitempty"`
	// ReconnectReason marks reconnect-related control requests such as a
	// fallback spawn or final give-up report.
	ReconnectReason string `json:"reconnect_reason,omitempty"`

	// PrevToken is the previously consumed session token used by the daemon's
	// "refresh-token" command to mint a fresh reconnect token for the same owner.
	PrevToken string `json:"prev_token,omitempty"`
}

// Response is the reply to a control command.
type Response struct {
	OK       bool            `json:"ok"`
	Message  string          `json:"message,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	IPCPath  string          `json:"ipc_path,omitempty"`
	ServerID string          `json:"server_id,omitempty"`
	Token    string          `json:"token,omitempty"` // handshake token for session binding
}

// CommandHandler is implemented by the Owner to handle control commands.
type CommandHandler interface {
	HandleShutdown(drainTimeoutMs int) string
	HandleStatus() map[string]interface{}
}

// OwnerInfo contains summary data for a single managed owner returned by list_owners.
type OwnerInfo struct {
	ServerID             string   `json:"server_id"`
	EngineName           string   `json:"engine_name,omitempty"`
	Command              string   `json:"command"`
	Args                 []string `json:"args"`
	Cwd                  string   `json:"cwd"`
	CwdSet               []string `json:"cwd_set"`
	Sessions             int      `json:"sessions"`
	Pending              int      `json:"pending"`
	UpstreamPID          int      `json:"upstream_pid,omitempty"`
	Classification       string   `json:"classification"`
	ClassificationSource string   `json:"classification_source,omitempty"`
	ClassificationReason []string `json:"classification_reason,omitempty"`
	MuxVersion           string   `json:"mux_version"`
	Persistent           bool     `json:"persistent"`
	CachedInit           bool     `json:"cached_init,omitempty"`
	CachedTools          bool     `json:"cached_tools,omitempty"`
	CachedPrompts        bool     `json:"cached_prompts,omitempty"`
	CachedResources      bool     `json:"cached_resources,omitempty"`
}

// ListOwnersResponse is the response payload for the "list_owners" daemon RPC.
type ListOwnersResponse struct {
	Owners    []OwnerInfo `json:"owners"`
	Truncated bool        `json:"truncated"`
}

// DaemonHandler extends CommandHandler with daemon-specific commands.
type DaemonHandler interface {
	CommandHandler
	HandleSpawn(req Request) (ipcPath, serverID, token string, err error)
	HandleRemove(serverID string) error
	HandleGracefulRestart(drainTimeoutMs int) (snapshotPath string, afterResponse func(), err error)
	HandleRefreshSessionToken(prevToken string) (newToken string, err error)
	HandleReconnectGiveUp(reason string) error
	HandleListOwners(req Request) (ListOwnersResponse, error)
}

// SpawnResponseFailureHandler is an optional daemon-side lifecycle hook. The
// control server invokes it only when HandleSpawn succeeded but the response
// could not be delivered to the requesting shim. Implementations should revoke
// the exact unconsumed reservation so an abandoned startup cannot pin an owner
// until the generic pending-token TTL expires.
type SpawnResponseFailureHandler interface {
	HandleSpawnResponseFailure(serverID, token string)
}

// SuspendCheckResponse is the safety verdict for intentionally parking one
// reconnectable shim session.
type SuspendCheckResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// SuspendCheckHandler is an optional daemon control-plane extension.
type SuspendCheckHandler interface {
	HandleCanSuspend(prevToken string) (SuspendCheckResponse, error)
}

// SuspendCheckForOwnerHandler optionally binds a suspend check to the exact
// owner returned by spawn. It prevents a daemon-wide token-history scan while
// retaining the legacy SuspendCheckHandler path for older consumers.
type SuspendCheckForOwnerHandler interface {
	HandleCanSuspendForOwner(prevToken, serverID string) (SuspendCheckResponse, error)
}

// OwnerStopHandler is an optional daemon-side extension for stopping a specific
// owner through the daemon registry instead of the owner's own control socket.
type OwnerStopHandler interface {
	HandleStopOwner(req Request) (message string, err error)
}

// GracefulRestartOptions carries additive graceful-restart parameters. Daemon
// handlers that implement GracefulRestartOptionsHandler receive this richer
// shape; older handlers continue through HandleGracefulRestart.
type GracefulRestartOptions struct {
	DrainTimeoutMs int
	SuccessorExe   string
}

// GracefulRestartOptionsHandler is an optional daemon handler upgrade for
// graceful-restart requests that need to specify the successor executable.
type GracefulRestartOptionsHandler interface {
	HandleGracefulRestartWithOptions(opts GracefulRestartOptions) (snapshotPath string, afterResponse func(), err error)
}
