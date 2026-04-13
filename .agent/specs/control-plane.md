# Specification: mcp-mux Control Plane

## Overview

A separate control protocol for managing mcp-mux instances (stop, status, upgrade). Currently, the `mux/shutdown` command is sent through the same IPC socket as MCP traffic — this is a hack that mixes control and data planes. The control plane must be separate, authenticated, and support graceful operations.

## Functional Requirements

- FR1: Separate control socket per owner (e.g., `mcp-mux-{id}.ctl.sock`) — control traffic never mixes with MCP data
- FR2: `mcp-mux stop` sends shutdown command via control socket, waits for confirmation
- FR3: Graceful drain on shutdown — owner stops accepting new IPC connections, waits for pending requests to complete (with timeout), then exits
- FR4: `mcp-mux status` queries live state via control socket (sessions, upstream PID, pending requests, uptime)
- FR5: `mcp-mux upgrade` — graceful stop all → binary unlocked → print rebuild instructions
- FR6: Control protocol is simple line-based JSON (not JSON-RPC) to avoid confusion with MCP traffic
- FR7: Control socket only accepts connections from same user (file permissions)

## Non-Functional Requirements

- NFR1: Shutdown drain timeout: 5 seconds (configurable via flag)
- NFR2: Control socket overhead < 1 file descriptor per owner when idle
- NFR3: `mcp-mux stop` completes within drain timeout + 1 second

## Acceptance Criteria

- [ ] AC1: `mux/shutdown` hack removed from owner.go handleDownstreamMessage
- [ ] AC2: `mcp-mux stop` gracefully shuts down all owners, CC can reconnect automatically
- [ ] AC3: `mcp-mux status` returns live JSON with session count, upstream PID, uptime
- [ ] AC4: Pending requests complete before shutdown (up to timeout)
- [ ] AC5: Control socket cleaned up on owner exit
- [ ] AC6: No control traffic can reach upstream MCP server

## Out of Scope

- Authentication beyond file permissions (local-only, trusted)
- Remote control (control socket is local only)
- Hot binary swap (user manually rebuilds)

## Dependencies

- Existing IPC package (`internal/ipc`)
- Existing owner lifecycle management (`internal/mux`)
