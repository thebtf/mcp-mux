# Tasks: Init-Ready Gate

## Phase 1: Owner Init-Ready Channel
- [x] T1: Add initReady channel + initReadyOnce to Owner struct and NewOwner (owner.go)
- [x] T2: Close initReady in cacheResponse("initialize") via initReadyOnce (owner.go)
- [x] T3: Close initReady on upstream exit — handleUpstreamExit or similar (owner.go)
- [x] T4: Add InitReady() and InitSuccess() accessors (owner.go)
- [x] T4b: Add sendProactiveInit — owner sends init to upstream immediately (chicken-egg fix)

## Phase 2: Daemon.Spawn Waits for Init
- [x] T5: Daemon.Spawn waits on owner.InitReady() with 60s timeout for new owners (daemon.go)
- [x] T6: Error handling: if init fails/timeout, cleanup owner and return error (daemon.go)

## Phase 3: Shim Timeout
- [x] T7: Increase spawnViaDaemon timeout from 30s to 90s (cmd/mcp-mux/daemon.go)

## Phase 4: Tests + Verification
- [x] T8: Test: slow upstream (5s delay) — Spawn blocks then succeeds (daemon_test.go) — implemented in lifecycle_test.go
- [x] T9: Test: crashing upstream — Spawn returns error within 2s (daemon_test.go) — implemented in lifecycle_test.go
- [x] T10: Test: dedup path — findSharedOwner returns immediately (daemon_test.go) — TestDedup_SharedServersReusedAcrossCwd in lifecycle_test.go
- [x] T11: Full test suite regression check (all 10 packages pass)
- [x] T12: Build and deploy new binary, verify serena/cclsp/time connect on fresh session — shipped in v0.8.0

## Phase 5: Cleanup
- [x] T13: Update CONTINUITY.md and TECHNICAL_DEBT.md — done (725946b)
- [x] T14: Commit, push, tag release — shipped (515b337, ef43ef2, 725946b)
