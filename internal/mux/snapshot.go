// Package mux implements the core multiplexer logic for mcp-mux.
//
// This file defines snapshot types for graceful daemon restart.
// OwnerSnapshot captures the state of an Owner that can be serialized
// and restored across daemon restarts.
package mux

import "github.com/thebtf/mcp-mux/internal/muxcore/classify"

// OwnerSnapshot captures the serializable state of an Owner for graceful restart.
// Cached responses are base64-encoded raw JSON-RPC messages.
type OwnerSnapshot struct {
	ServerID              string            `json:"server_id"`
	Command               string            `json:"command"`
	Args                  []string          `json:"args"`
	Env                   map[string]string `json:"env,omitempty"`
	Cwd                   string            `json:"cwd"`
	CwdSet                []string          `json:"cwd_set"`
	Mode                  string            `json:"mode"`
	Classification        classify.SharingMode `json:"classification,omitempty"`
	ClassificationSource  string            `json:"classification_source,omitempty"`
	ClassificationReason  []string          `json:"classification_reason,omitempty"`
	Persistent            bool              `json:"persistent,omitempty"`
	CachedInit            string            `json:"cached_init,omitempty"`
	CachedTools           string            `json:"cached_tools,omitempty"`
	CachedPrompts         string            `json:"cached_prompts,omitempty"`
	CachedResources       string            `json:"cached_resources,omitempty"`
	CachedResourceTemplates string          `json:"cached_resource_templates,omitempty"`
}

// SessionSnapshot captures the serializable state of a Session for graceful restart.
type SessionSnapshot struct {
	MuxSessionID  string            `json:"mux_session_id"`
	Cwd           string            `json:"cwd"`
	Env           map[string]string `json:"env,omitempty"`
	OwnerServerID string            `json:"owner_server_id"`
}
