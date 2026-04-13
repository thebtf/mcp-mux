# Implementation Plan: muxcore — Embeddable MCP Multiplexer Engine

**Spec:** .agent/specs/muxcore-library/spec.md
**Architecture:** .agent/specs/muxcore-library/architecture.md
**Created:** 2026-04-13
**Status:** Draft

> **Provenance:** Planned by claude-opus-4-6 on 2026-04-13.
> Evidence from: spec.md (10 FR, 11 clarifications), architecture.md (8 ADRs),
> codebase analysis (51 Go files in 10 internal packages), consumer explorations
> (aimux pkg/executor, engram cmd/).
> Key decisions by: user (4 key facts) + AI (phase ordering).
> Confidence: VERIFIED — all file counts, package structures, and APIs confirmed via tools.

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.25+ | Project language, all consumers are Go |
| Supervisor | thejerf/suture/v4 | Already in mcp-mux, OTP-style crash recovery |
| UUID | google/uuid | Already in mcp-mux, for isolated mode IDs |
| Win32 Process | golang.org/x/sys/windows v0.31.0 | Job Objects API (CreateJobObject, AssignProcess, Terminate) |
| All other | stdlib | No new external dependencies |

## Architecture

See `architecture.md` in same directory for full diagrams. Summary:

```
internal/muxcore/
├── engine/        ← NEW: embeddable entry point (Config → Run)
├── owner/         ← MOVE: from internal/mux/owner.go + related
├── session/       ← MOVE: from internal/mux/session*.go
├── daemon/        ← MOVE: from internal/daemon/
├── procgroup/     ← NEW: replaces internal/upstream/ with tree kill
├── ipc/           ← MOVE: from internal/ipc/ (as-is)
├── control/       ← MOVE: from internal/control/ (as-is)
├── jsonrpc/       ← MOVE: from internal/jsonrpc/ (as-is)
├── remap/         ← MOVE: from internal/remap/ (as-is)
├── classify/      ← MOVE: from internal/classify/ (as-is)
├── serverid/      ← MOVE: from internal/serverid/ (+ configurable BaseDir)
├── shim/          ← NEW: extracted from resilient_client + cmd/daemon.go
├── snapshot/      ← MOVE: from internal/daemon/snapshot.go
├── busy/          ← MOVE: from internal/mux/busy_protocol.go
├── progress/      ← NEW: synthetic progress reporter
└─�� listchanged/   ← MOVE: from internal/mux/listchanged_inject.go
```

**REVERSIBLE** — pure refactor. If extraction fails at any phase, revert to `internal/`.

### DX Review (Library consumed by developers)

**TTHW (Time to Hello World):**
```go
// 3 lines to embed muxcore into any MCP server
mux := engine.New(engine.Config{Name: "myserver", Handler: myHandler})
mux.Run(context.Background())
```
Target: **<2 min** (Champion tier). `go get` + 3 lines in main.go.

## API Contracts

### engine.Config (public)

```go
type Config struct {
    Name             string                    // server identity for IPC paths
    Command          string                    // binary path (for re-exec in daemon mode)
    Args             []string                  // binary args
    Handler          func(ctx context.Context, // MCP server logic
                          stdin io.Reader,
                          stdout io.Writer) error
    IdleTimeout      time.Duration             // 0 = 10 min default
    ProgressInterval time.Duration             // 0 = 5s default
    Persistent       bool                      // survive CC disconnect
    BaseDir          string                    // socket dir, "" = os.TempDir()
    Logger           func(string)              // optional, nil = silent
    DaemonFlag       string                    // arg to detect daemon mode, default "--muxcore-daemon"
}
```

**PARTIALLY REVERSIBLE** — changing Config fields after consumers adopt requires migration.

### engine.MuxEngine (public)

```go
func New(cfg Config) *MuxEngine
func (e *MuxEngine) Run(ctx context.Context) error  // blocks until shutdown
```

**REVERSIBLE** — two-function API, additive changes only.

### procgroup.Process (public)

```go
func Spawn(name string, args []string, opts Options) (*Process, error)
func (p *Process) GracefulKill(timeout time.Duration) error  // tree kill
func (p *Process) Kill() error                                // immediate tree kill
func (p *Process) Wait() error
func (p *Process) Alive() bool
func (p *Process) PID() int
func (p *Process) Done() <-chan struct{}
```

**REVERSIBLE** — standalone functions, no global state.

### shim (public)

```go
func Connect(cfg ConnectConfig) (ipcPath string, err error)
func Bridge(stdin io.Reader, stdout io.Writer, ipcPath string) error
```

**REVERSIBLE** — additive.

## File Structure

```
internal/muxcore/
  engine/
    engine.go              ← MuxEngine + Config + Run() + mode detection
    engine_test.go
  owner/
    owner.go               ← from internal/mux/owner.go (core routing, caching)
    owner_serve_test.go    ← from internal/mux/owner_serve_test.go
  session/
    session.go             ← from internal/mux/session.go
    session_manager.go     ← from internal/mux/session_manager.go
    session_manager_test.go
  daemon/
    daemon.go              ← from internal/daemon/daemon.go
    daemon_test.go
    reaper.go              ← from internal/daemon/reaper.go
    reaper_gate_test.go
    lifecycle_test.go
    supervisor_test.go
  procgroup/
    process.go             ← NEW: Spawn + GracefulKill + tree kill dispatch
    process_unix.go        ← NEW: Setpgid + Kill(-pgid) 
    process_windows.go     ← NEW: Job Objects (CreateJobObject + KILL_ON_JOB_CLOSE)
    process_test.go        ← NEW: spawn + kill tree + graceful phases
  ipc/
    transport.go           ← from internal/ipc/transport.go (as-is)
    ipc_test.go
  control/
    protocol.go            ← from internal/control/protocol.go
    server.go              ← from internal/control/server.go
    client.go              ← from internal/control/client.go
    control_test.go
  jsonrpc/
    message.go             ← as-is
    scanner.go             ← as-is
    meta.go                ← as-is
    (tests)
  remap/
    remap.go               ← as-is
    remap_test.go
  classify/
    classify.go            ← as-is (+ ParseProgressInterval from Sprint 1)
    patterns.go            ← as-is
    (tests)
  serverid/
    serverid.go            ← as-is (+ configurable BaseDir)
    serverid_test.go
  shim/
    connect.go             ← NEW: extracted from cmd/mcp-mux/daemon.go
    bridge.go              ← NEW: extracted from internal/mux/resilient_client.go
    proxy.go               ← NEW: proxy mode (behind parent mux)
    shim_test.go
  snapshot/
    snapshot.go            ← from internal/daemon/snapshot.go
    snapshot_test.go
  busy/
    busy_protocol.go       ← from internal/mux/busy_protocol.go
    busy_protocol_test.go
  progress/
    reporter.go            ← from Sprint 1 synthetic-progress-reporter tasks
    reporter_test.go
  listchanged/
    inject.go              ← from internal/mux/listchanged_inject.go
    inject_test.go
```

**Retained in mcp-mux (NOT moved):**
```
internal/mcpserver/         ← mux_list/mux_stop/mux_restart (mcp-mux-specific)
cmd/mcp-mux/main.go         ← CLI entry point (thin wrapper)
cmd/mcp-mux/daemon.go       ← gutted, delegates to muxcore/engine
```

## Phases

### Phase 1: Create structure + move leaf packages
**Goal:** `internal/muxcore/` exists with leaf packages. mcp-mux compiles and tests pass.
**Touches:** ipc, jsonrpc, remap, serverid, classify (5 packages, 20 files)
**Risk:** LOW — no logic changes, only import path updates
**Reversibility:** REVERSIBLE — `git revert`

- Create `internal/muxcore/` directory tree
- Move leaf packages (zero internal deps): ipc, jsonrpc, remap, serverid, classify
- Update all import paths in mcp-mux
- Run `go build ./...` + `go test ./...` — must pass identically
- Commit: `refactor: move leaf packages to internal/muxcore/`

### Phase 2: Move control + snapshot
**Goal:** Protocol and persistence layers in muxcore.
**Touches:** control (depends on ipc), snapshot (standalone)
**Risk:** LOW — control depends only on ipc (already moved)
**Reversibility:** REVERSIBLE

- Move `internal/control/` → `internal/muxcore/control/`
- Move `internal/daemon/snapshot.go` → `internal/muxcore/snapshot/`
- Update imports in daemon.go, owner.go, cmd/
- Build + test
- Commit: `refactor: move control + snapshot to internal/muxcore/`

### Phase 3: Build procgroup (new code — replaces upstream)
**Goal:** Process tree kill works on both platforms. FR-3 fulfilled.
**Touches:** NEW `internal/muxcore/procgroup/`, replaces `internal/upstream/`
**Risk:** HIGH — platform-specific Win32 API, changes kill semantics
**Reversibility:** REVERSIBLE — old upstream/ kept until procgroup proven

- Create `procgroup/process.go`: Spawn, GracefulKill, Kill, Wait, Alive, PID, Done
- Create `procgroup/process_unix.go`: Setpgid in SysProcAttr, Kill(-pgid, SIGTERM/SIGKILL)
- Create `procgroup/process_windows.go`: CreateJobObject, AssignProcessToJobObject, TerminateJobObject
- Write tests: spawn child tree → kill → verify all dead (use `sleep` children)
- Wire into owner.go: replace `upstream.Start()` with `procgroup.Spawn()`
- Keep old `internal/upstream/` until Phase 6 (delete after full verification)
- Build + test + verify zero orphans after kill
- Commit: `feat: procgroup with platform tree kill`

### Phase 4: Extract owner + session + daemon + protocol packages
**Goal:** Core multiplexer in muxcore. FR-4, FR-5, FR-6 fulfilled.
**Touches:** owner (16 files, largest package), session, daemon, busy, listchanged, progress
**Risk:** MEDIUM — owner.go is ~2000 lines, many cross-references
**Reversibility:** REVERSIBLE — import path changes only

- Split `internal/mux/owner.go` into `muxcore/owner/` (routing + caching) 
- Move session.go + session_manager.go → `muxcore/session/`
- Move daemon.go + reaper.go → `muxcore/daemon/`
- Move busy_protocol.go → `muxcore/busy/`
- Move listchanged_inject.go → `muxcore/listchanged/`
- Create `muxcore/progress/` (from Sprint 1 synthetic-progress-reporter)
- Update all imports across mcp-mux
- Build + test (all 10+ packages must pass)
- Commit: `refactor: move core packages to internal/muxcore/`

### Phase 5: Build engine + shim (new code)
**Goal:** Embeddable entry point. FR-1, FR-2, FR-8, FR-9 fulfilled.
**Touches:** NEW engine/, NEW shim/
**Risk:** MEDIUM — mode detection logic is new, proxy mode is new
**Reversibility:** REVERSIBLE — new packages, no existing code changed

- Create `engine/engine.go`: Config, MuxEngine, New(), Run()
- Implement mode detection: env var → proxy, socket exists → client, otherwise → daemon
- Extract `shim/connect.go` from `cmd/mcp-mux/daemon.go` (ensureDaemon, spawnViaDaemon)
- Extract `shim/bridge.go` from `internal/mux/resilient_client.go`
- Create `shim/proxy.go`: proxy mode (simple stdio passthrough when behind parent mux)
- Wire cmd/mcp-mux/main.go to use `engine.New(cfg).Run(ctx)`
- Build + test + verify all three modes work
- Commit: `feat: muxcore engine with daemon/client/proxy modes`

### Phase 6: Cleanup + consumer playbooks
**Goal:** mcp-mux is thin wrapper. Playbooks for aimux + engram. FR-10 fulfilled.
**Touches:** cmd/mcp-mux/, delete old internal/ packages
**Risk:** LOW — all code already moved, this is deletion + docs
**Reversibility:** PARTIALLY REVERSIBLE — deleted code recoverable from git

- Delete `internal/mux/`, `internal/daemon/`, `internal/upstream/`, `internal/ipc/`, etc.
- Verify cmd/mcp-mux/ imports only from `internal/muxcore/` + `internal/mcpserver/`
- Verify mcp-mux binary size change <10%
- Write consumer playbooks:
  - `.agent/specs/muxcore-library/playbook-aimux.md`
  - `.agent/specs/muxcore-library/playbook-engram.md`
- Full regression: `go test ./... -count 1`
- Commit: `refactor: mcp-mux uses muxcore engine, cleanup old packages`

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| Supervisor | thejerf/suture/v4 | v4.0.5 | Already used, OTP-style crash recovery for owners |
| UUID | google/uuid | v1.6.0 | Already used, for isolated mode unique IDs |
| Win32 Job Objects | golang.org/x/sys/windows | latest | Official Go extended syscall package for CreateJobObject |
| Everything else | stdlib | — | No new external deps |

## Unknowns and Risks

| Unknown | Impact | Resolution Strategy |
|---------|--------|-------------------|
| owner.go is ~2000 LOC with many internal cross-refs | HIGH | Phase 4: split carefully, keep routing core together, extract session/busy/progress as satellite packages |
| Windows Job Objects untested in CI (no Windows CI) | MED | Manual test on Windows dev machine. Add Windows CI later. |
| Proxy mode (behind mcp-mux) is new code | MED | Phase 5: test with real mcp-mux + engram setup |
| Re-exec daemon mode needs binary to know its own path | LOW | Use `os.Executable()` — standard Go approach |
| golang.org/x/sys/windows adds new dependency | LOW | Only imported on Windows builds (build tags). Zero impact on Unix. |

## FR → Phase Mapping

| FR | Phase | How |
|----|-------|-----|
| FR-1 Embeddable Engine | Phase 5 | engine.New(Config).Run(ctx) |
| FR-2 Three Modes | Phase 5 | mode detection in engine.Run() |
| FR-3 Process Tree Kill | Phase 3 | procgroup with Job Objects + pgid |
| FR-4 Multi-Session Mux | Phase 4 | owner + session + daemon packages |
| FR-5 Graceful Restart | Phase 2+4 | snapshot + daemon.HandleGracefulRestart |
| FR-6 Activity-Aware Reaping | Phase 4 | daemon/reaper.go |
| FR-7 Synthetic Progress | Phase 4 | progress/reporter.go |
| FR-8 Hierarchical Cooperation | Phase 5 | shim/proxy.go |
| FR-9 Self-Spawn Daemon | Phase 5 | engine daemon mode re-exec |
| FR-10 Backward Compat | ALL | tests pass after every phase |

## NFR Approach

| NFR | Approach |
|-----|---------|
| NFR-1 Performance | No changes to hot paths — same code, different package |
| NFR-2 Minimal Deps | Only add golang.org/x/sys/windows (build-tagged) |
| NFR-3 Platform Support | procgroup build tags: process_unix.go, process_windows.go |
| NFR-4 API Stability | v0 in internal/, keyed structs, small interfaces |
| NFR-5 Testability | Each muxcore/ package independently testable |
