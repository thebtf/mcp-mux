// Package mux implements the core multiplexer logic for mcp-mux.
//
// Owner, OwnerConfig, InflightRequest and all routing logic live in
// internal/muxcore/owner. This file re-exports them as type/var aliases
// so existing call sites in cmd/ and internal/daemon/ continue to compile
// without changes.
package mux

import "github.com/thebtf/mcp-mux/internal/muxcore/owner"

// Version is the mcp-mux build version.
// Override at build time via: -ldflags "-X github.com/thebtf/mcp-mux/internal/muxcore/owner.Version=..."
var Version = owner.Version

// Owner is the multiplexer core. It manages a single upstream process and
// routes requests from multiple downstream sessions through it.
type Owner = owner.Owner

// OwnerConfig holds parameters for creating an Owner.
type OwnerConfig = owner.OwnerConfig

// InflightRequest holds metadata about a request currently being processed by upstream.
type InflightRequest = owner.InflightRequest

// NewOwner creates and starts a new Owner.
var NewOwner = owner.NewOwner

// NewOwnerFromSnapshot creates an Owner with pre-populated caches from a snapshot.
var NewOwnerFromSnapshot = owner.NewOwnerFromSnapshot

// RunClient connects to an existing owner via IPC and bridges the caller's stdio.
var RunClient = owner.RunClient

// RunResilientClient proxies CC stdio <-> IPC with automatic reconnect on IPC failure.
var RunResilientClient = owner.RunResilientClient

// ResilientClientConfig configures the resilient shim proxy.
type ResilientClientConfig = owner.ResilientClientConfig

// ReconnectFunc reconnects to the daemon and returns the new IPC path and token.
type ReconnectFunc = owner.ReconnectFunc
