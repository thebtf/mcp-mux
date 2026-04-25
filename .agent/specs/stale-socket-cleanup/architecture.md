# Architecture: Stale control socket cleanup (#100)

## Problem

On Windows, AF_UNIX socket files persist after process death. Current cleanup
uses dial-based liveness check (`isDaemonRunning`) which can hang 5.5s on
half-open sockets. After force-kill, the socket file blocks new daemon startup.

## Current flow

```
engine.runDaemon:
  1. os.Stat(ctlPath) → file exists?
  2. isDaemonRunning(ctlPath) → ipc.IsAvailable (dial 500ms) + control.Send("ping", 5s)
  3. if dead → os.Remove
  4. daemon.New → control.NewServer → ipc.Listen
     ipc.Listen:
       a. IsAvailable (dial 500ms) → if alive, error "listener already active"
       b. os.Remove (stale file)
       c. sockperm.Listen
```

Two dial-based checks, each up to 5.5s. Total worst case: 11s on stale socket.

## Key insight

If code reached `runDaemon`, this process IS the daemon (`isDaemonMode() == true`).
No other daemon should be running. If one is, we entered daemon mode by mistake
(predecessor still alive). The dial check is defensive but causes more harm than
it prevents on Windows.

## Design: unconditional remove in engine.runDaemon

### ADR-001: Replace dial-based cleanup with unconditional remove

**Status:** Proposed

**Context:** `isDaemonRunning` dial + ping can hang 5.5s on Windows stale sockets.
The check protects against accidentally stomping a live daemon's socket, but this
scenario shouldn't happen — engine mode detection prevents it.

**Decision:** In `engine.runDaemon`, unconditionally remove the socket file before
`daemon.New`. Keep `ipc.Listen`'s `IsAvailable` guard as defense-in-depth — it
catches the edge case where two daemons race into `runDaemon` simultaneously.

**Consequences:**
- Startup is instant (no 5.5s hang)
- If a live daemon somehow exists, `ipc.Listen` catches it (defense-in-depth)
- If `ipc.Listen` also fails, the error message is clear
- Removes dead code path (`isDaemonRunning` in runDaemon context)

**Reversibility:** REVERSIBLE

### Changes

**File: `muxcore/engine/engine.go`, `runDaemon` method (~line 271-279)**

Replace:
```go
if _, err := os.Stat(ctlPath); err == nil {
    if !isDaemonRunning(ctlPath) {
        os.Remove(ctlPath)
        e.logger.Printf("removed stale daemon socket: %s", ctlPath)
    }
}
```

With:
```go
if err := os.Remove(ctlPath); err == nil {
    e.logger.Printf("removed stale daemon socket: %s", ctlPath)
}
```

No Stat needed — `os.Remove` on non-existent file returns an error we ignore.
No dial needed — this process IS the daemon.

**File: `muxcore/ipc/transport.go`, `Listen` function**

No change. Keep `IsAvailable` guard as defense-in-depth.

### Test

Existing `TestNew_Defaults` covers daemon startup. Add a test that creates a
stale socket file, calls `runDaemon` (via engine), and verifies it starts without
hanging.

## Domain Modeling

DDD evaluated — not needed (rationale: infrastructure-only change, no domain entities).

## Reusability Awareness

No candidates — infrastructure plumbing, not extractable.
