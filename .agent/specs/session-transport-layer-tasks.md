# Session Transport Layer — Tasks

## Phase 1: SessionManager core
- [x] T1: Create `internal/mux/session_manager.go`
- [x] T2: Add `Cwd string` field to Session struct
- [x] T3: Add TrackRequest/CompleteRequest for inflight correlation
- [x] T4: Unit tests (8 tests, all pass)

## Phase 2: Token handshake
- [x] T5: Add `Token string` to control.Response
- [x] T6: Daemon generates token + PreRegister
- [x] T7: Shim sends token on IPC connect
- [x] T8: Owner reads token, binds session cwd
- [x] T9: ResilientClient sends token on connect/reconnect

## Phase 3: Integration
- [x] T10: Owner uses sessionMgr.ResolveCallback()
- [x] T11: Owner calls TrackRequest/CompleteRequest
- [x] T12: Remove lastActiveSessionID
- [x] T13: All tests pass (13 files updated)

## Phase 4: Cleanup
- [x] T14: Remove TRACE debug logging
- [ ] T15: Update README/docs
