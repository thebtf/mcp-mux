# Plan: Init-Ready Gate

## Architecture

```
CC → mcp-mux (shim) → spawnViaDaemon() → Daemon.Spawn():
                                           │
                                           ├─ Create Owner (IPC listener + upstream process)
                                           ├─ Wait on owner.InitReady() ←── NEW
                                           │   ├─ init cached → return IPC path
                                           │   ├─ upstream died → return error
                                           │   └─ 60s timeout → return error
                                           │
                                           └─ Return IPC path to shim
                                               │
                                               └─ Shim connects → init from cache → instant
```

## Phase 1: Owner Init-Ready Channel (owner.go)

### 1.1 Add initReady channel
- Add `initReady chan struct{}` field to Owner struct
- Add `initReadyOnce sync.Once` for safe close
- Initialize in `NewOwner()`: `initReady: make(chan struct{})`

### 1.2 Close initReady on init cache
- In `cacheResponse("initialize")`: close initReady via initReadyOnce
- In upstream exit handler: close initReady via initReadyOnce (signals "upstream dead, no init coming")

### 1.3 Add InitReady() accessor
- `func (o *Owner) InitReady() <-chan struct{}` — returns the channel

### 1.4 Add InitSuccess() check
- `func (o *Owner) InitSuccess() bool` — returns true if initDone=true (vs upstream crash)
- Used by Daemon.Spawn to distinguish success vs failure

## Phase 2: Daemon.Spawn Waits for Init (daemon.go)

### 2.1 After owner creation, wait for init
- After `placeholder.Owner = owner` and promoting placeholder
- `select { case <-owner.InitReady(): case <-time.After(60s): }`
- If timeout or !owner.InitSuccess() → remove owner, return error
- If success → return IPC path as before

### 2.2 Adjust error handling
- If init wait fails, clean up: remove owner from d.owners, shutdown owner
- Return error to shim (shim falls back to legacy owner)

## Phase 3: Shim Timeout (daemon.go, main.go)

### 3.1 Increase spawnViaDaemon timeout
- `cmd/mcp-mux/daemon.go`: change `30*time.Second` to `90*time.Second`

## Phase 4: Tests

### 4.1 Test: slow upstream init
- Mock server with 5s delay before init response
- Verify Spawn blocks until init cached, then returns IPC path
- Verify shim connects and gets instant cached init response

### 4.2 Test: upstream crash before init
- Mock server that exits immediately (before sending init response)
- Verify Spawn returns error within 2s

### 4.3 Test: dedup path unchanged
- Existing owner with cached init
- Verify findSharedOwner returns immediately (no extra wait)

### 4.4 Verify existing tests pass
- Full test suite regression check

## Files Changed

| File | Change |
|------|--------|
| `internal/mux/owner.go` | initReady channel, InitReady(), InitSuccess() |
| `internal/daemon/daemon.go` | Spawn waits on InitReady() |
| `cmd/mcp-mux/daemon.go` | spawnViaDaemon timeout 30s→90s |
| `internal/daemon/daemon_test.go` | New tests for init-ready gate |

## Risk Assessment

- **Low**: Owner struct changes — additive only, no breaking changes
- **Medium**: Spawn blocking — increases spawn time for slow servers (by design)
- **Mitigated**: 60s timeout prevents infinite block on crash
- **Mitigated**: Dedup path unaffected (already has cached init)
