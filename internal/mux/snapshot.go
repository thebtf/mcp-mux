// Package mux implements the core multiplexer logic for mcp-mux.
//
// Snapshot types live in internal/muxcore/snapshot and are re-exported here
// as type aliases so existing call sites within this package continue to
// compile without changes.
package mux

import "github.com/thebtf/mcp-mux/internal/muxcore/snapshot"

// OwnerSnapshot is an alias for snapshot.OwnerSnapshot.
// Captures the serializable state of an Owner for graceful restart.
type OwnerSnapshot = snapshot.OwnerSnapshot

// SessionSnapshot is an alias for snapshot.SessionSnapshot.
// Captures the serializable state of a Session for graceful restart.
type SessionSnapshot = snapshot.SessionSnapshot
