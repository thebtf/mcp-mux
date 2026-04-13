# Implementation Plan: Graceful Daemon Restart

**Spec:** .agent/specs/graceful-restart/spec.md
**Created:** 2026-04-07
**Status:** Draft

> **Provenance:** Planned by Claude Opus 4.6 on 2026-04-07.
> Evidence from: spec.md, investigation report, codebase analysis (daemon.go, owner.go,
> control/protocol.go, resilient_client.go), constitution.md.
> Key decisions by: AI + user (state snapshot approach chosen over FD-passing).
> Confidence: VERIFIED (codebase patterns), INFERRED (snapshot format).

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Serialization | encoding/json (stdlib) | Human-readable, debuggable, already used in control protocol |
| Atomic write | os.CreateTemp + os.Rename | Cross-platform, Go stdlib |
| Snapshot path | os.TempDir()/mcp-muxd-snapshot.json | Same dir as daemon socket, cleaned on boot |
| Control command | "graceful-restart" on existing control socket | No new protocol, reuses existing infrastructure |

## Architecture

```
OLD DAEMON                          NEW DAEMON
┌──────────┐                        ┌──────────┐
│ Owner A  │  ─serialize─►  JSON    │ Owner A' │ ◄─ pre-populated cache
│ Owner B  │  ─serialize─►  file    │ Owner B' │ ◄─ spawn upstream in bg
│ Owner C  │  ─serialize─►         │ Owner C' │ ◄─ serve from cache first
└──────────┘                        └──────────┘
     │                                   ▲
     ▼                                   │
  shutdown                        load snapshot
     │                                   │
     ▼                                   │
  IPC EOF ──► shims reconnect ──────────►│
```

**Key insight:** Owners serve cached responses immediately. Upstream processes spawn in
background. When upstream responds, cache is refreshed (in case server was updated).

## Data Model

### OwnerSnapshot
| Field | Type | Constraints | Notes |
|-------|------|-------------|-------|
| server_id | string | required | hex ID from serverid package |
| command | string | required | upstream command |
| args | []string | required | upstream args |
| env | map[string]string | optional | env diff |
| cwd | string | required | primary cwd |
| cwd_set | []string | required | all registered cwds |
| mode | string | required | "cwd", "global", "isolated" |
| classification | string | optional | "shared", "isolated", "session-aware" |
| classification_source | string | optional | "capability", "tools" |
| classification_reason | []string | optional | tool names that triggered |
| persistent | bool | | daemon-persistent flag |
| cached_init | string | optional | base64-encoded raw JSON-RPC response |
| cached_tools | string | optional | base64-encoded |
| cached_prompts | string | optional | base64-encoded |
| cached_resources | string | optional | base64-encoded |
| cached_resource_templates | string | optional | base64-encoded |

### SessionSnapshot
| Field | Type | Constraints | Notes |
|-------|------|-------------|-------|
| mux_session_id | string | required | e.g. "sess_a1b2c3d4" |
| cwd | string | required | session working directory |
| env | map[string]string | optional | session env diff |
| owner_server_id | string | required | which owner this session belongs to |

### DaemonSnapshot
| Field | Type | Constraints | Notes |
|-------|------|-------------|-------|
| version | int | required | snapshot format version (1) |
| mux_version | string | required | mcp-mux binary version hash |
| timestamp | string | required | RFC3339 |
| owners | []OwnerSnapshot | required | |
| sessions | []SessionSnapshot | required | |

## API Contracts

### Control Protocol: graceful-restart
- **Command:** `{"cmd": "graceful-restart", "drain_timeout_ms": 30000}`
- **Response:** `{"ok": true, "message": "snapshot written, shutting down", "snapshot_path": "/tmp/mcp-muxd-snapshot.json"}`
- **Errors:** `{"ok": false, "message": "snapshot write failed: ..."}`

### Owner.ExportSnapshot()
- **Signature:** `func (o *Owner) ExportSnapshot() OwnerSnapshot`
- **Returns:** Snapshot struct with all cached responses (base64-encoded)
- **Thread-safe:** Acquires o.mu.RLock

### NewOwnerFromSnapshot(cfg OwnerConfig, snap OwnerSnapshot)
- **Signature:** `func NewOwnerFromSnapshot(cfg OwnerConfig, snap OwnerSnapshot) (*Owner, error)`
- **Behavior:** Creates owner with pre-populated caches, starts IPC listener, spawns upstream + proactive init in background
- **Does NOT block** on upstream init (cache-first mode)

## File Structure

```
internal/
  daemon/
    daemon.go         # Add: HandleGracefulRestart, loadSnapshot, snapshotPath
    snapshot.go       # NEW: DaemonSnapshot type, Serialize/Deserialize, OwnerSnapshot
    snapshot_test.go  # NEW: serialization round-trip, staleness, corruption
  mux/
    owner.go          # Add: ExportSnapshot(), NewOwnerFromSnapshot()
    snapshot.go       # NEW: OwnerSnapshot type (exported for daemon)
  control/
    protocol.go       # Add: "graceful-restart" command routing
cmd/mcp-mux/
    main.go           # Modify: runUpgrade sends "graceful-restart" instead of "shutdown"
```

## Phases

### Phase 1: Snapshot Types + Serialization (foundation)
- Define OwnerSnapshot, SessionSnapshot, DaemonSnapshot types
- Implement Owner.ExportSnapshot() — extracts cached responses as base64
- Implement daemon.SerializeSnapshot() — walks all owners, writes atomic JSON
- Implement daemon.DeserializeSnapshot() — reads + validates JSON
- Tests: round-trip serialization, corrupt JSON, stale timestamp

**Deliverable:** Snapshot can be written and read back correctly.

### Phase 2: Snapshot Loading + Cache-First Owner (core)
- Implement NewOwnerFromSnapshot() — creates owner with pre-populated caches
- Modify daemon startup: check for snapshot file, load, create owners
- Cache-first mode: sessions get cached replay, upstream spawns in background
- Implement snapshot cleanup (delete after load, delete if stale)
- Tests: daemon loads snapshot, shim connects, gets cached init instantly

**Deliverable:** New daemon starts from snapshot with instant cached replay.

### Phase 3: Control Command + Upgrade Integration (wiring)
- Add "graceful-restart" to control protocol (protocol.go + server.go)
- Implement HandleGracefulRestart in daemon: serialize → shutdown
- Modify runUpgrade to send "graceful-restart" when --restart flag is set
- Tests: full cycle — upgrade → snapshot → restart → cached replay

**Deliverable:** `mcp-mux upgrade --restart` does graceful restart with state transfer.

### Phase 4: Edge Cases + Verification (polish)
- Handle: version mismatch, missing commands, concurrent restart
- Verify with 15 real servers: all reconnect < 2s
- Verify cold start unchanged (no snapshot = same as before)

**Deliverable:** Production-ready graceful restart.

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| JSON serialization | encoding/json | stdlib | Already used throughout, human-readable |
| Base64 encoding | encoding/base64 | stdlib | For binary cached responses in JSON |
| Atomic file write | os.CreateTemp + os.Rename | stdlib | Cross-platform, standard Go pattern |
| Temp file path | os.TempDir() | stdlib | Same location as existing daemon sockets |

No external dependencies needed. All stdlib.

## Unknowns and Risks

| Unknown | Impact | Resolution Strategy |
|---------|--------|-------------------|
| base64 encoding increases snapshot size ~33% | LOW | Cached responses are small (1-50KB each), total < 1MB even with 15 servers |
| Snapshot race if two daemons start simultaneously | LOW | Existing daemon lock file prevents this; snapshot is consumed (deleted) on load |
| Owner.ExportSnapshot during active request processing | MED | Use RLock (already done for Status()), snapshot is a consistent point-in-time read |

## Constitution Compliance

| Principle | Compliance |
|-----------|-----------|
| 1. Transparent Proxy | No protocol changes — shims reconnect normally |
| 2. Zero-Configuration | Snapshot is automatic during upgrade --restart |
| 5. Graceful Degradation | No snapshot → cold start fallback |
| 6. Cross-Platform | stdlib only, no platform-specific code |
| 9. Atomic Upgrades | Graceful restart preserves cached state |
| 10. No Stubs | Complete implementation with tests |
