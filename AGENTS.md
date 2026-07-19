# mcp-mux — MCP Stdio Multiplexer

## STACKS

```yaml
STACKS: [GO]
```

## PROJECT OVERVIEW

A local stdio multiplexer/proxy for MCP (Model Context Protocol) servers. Allows multiple Claude Code sessions to share a single instance of each MCP server, reducing process count and memory usage by ~3x.

### Problem

Each Claude Code session spawns its own copy of every configured MCP server (stdio transport). With 4 parallel sessions × 12 MCP servers = ~48 node processes consuming ~4.8 GB RAM. Most of these servers are stateless — they don't need per-session isolation.

### Solution

`mcp-mux` is a transparent command wrapper. User prefixes their MCP server command with `mcp-mux`:

```json
{ "command": "mcp-mux", "args": ["uvx", "--from", "...", "serena", ...] }
```

First mcp-mux instance for a given server becomes the "owner" (spawns upstream, listens on IPC). Subsequent instances connect as clients.

```
CC 1 ──stdio──> mcp-mux ──IPC──┐
CC 2 ──stdio──> mcp-mux ──IPC──┤──> mcp-mux (owner) ──stdio──> engram (1×)
CC 3 ──stdio──> mcp-mux ──IPC──┤
CC 4 ──stdio──> mcp-mux ──IPC──┘
```

### Key Concepts

- **Upstream**: A real MCP server process (e.g., engram, tavily)
- **Downstream**: A CC session connecting via mcp-mux wrapper
- **Owner**: First mcp-mux instance — spawns upstream, accepts IPC connections
- **Client**: Subsequent mcp-mux instances — connect to owner via IPC
- **Shared** (default): One upstream serves all clients
- **Isolated** (`--isolated`): Each client gets its own upstream

## CONVENTIONS

- Investigation reports: `.agent/reports/YYYY-MM-DD-topic.md`
- Diagnostic data: `.agent/data/topic.md`
- Action plans: `.agent/plans/topic.md`
- Specifications: `.agent/specs/topic.md`
- Architecture decisions: `.agent/arch/decisions/NNN-title.md`

## RULES

| Rule | Description |
|------|-------------|
| **No stubs** | Complete, working implementations only |
| **No guessing** | Verify with tools before using |
| **Reasoning first** | Document WHY before implementing |
| **Spec compliance** | MCP protocol spec is authoritative — verify all protocol behavior against it |

## RELEASE PROTOCOL

Public releases must follow [`docs/RELEASE-PROTOCOL.md`](docs/RELEASE-PROTOCOL.md).
When a release contains a critical or consumer-impacting `muxcore` update,
the release closeout must include `CONSUMER_HANDOFF_PASS`: fresh Engram
issues or comments for `aimux`, `engram`, and any other impacted muxcore
consumer. If Engram cannot be updated, report `CONSUMER_HANDOFF_BLOCKED` and
do not call the full critical muxcore scope shipped.

## muxcore Library API (v0.29.x)

### Upgrade

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.29.0
```

### v0.29.0 - public stable-stdio supervisor

**No required consumer code changes for ordinary `engine.New` users.** This
release adds `muxcore/supervisor` for products whose MCP host keeps one stdio
transport open while a replaceable child engine starts, parks, wakes, crashes,
or switches version. `supervisor.Run` owns strict MCP framing, bounded pending
FIFO, initialization-only replay, serialized host output, generation-safe
request/cancellation/progress/Tasks correlation, and explicit original-ID
errors for delivered requests lost during replacement.

Use `supervisor.StartCommand` when the supervisor must own full Unix process-
group or Windows Job Object retirement before a successor starts. Private
dormant control requires a verified generation-bound
`supervisor/attest.Parent` receipt: the parent endpoint is created before
spawn, bound to the exact returned child PID, and independently verified by the
child against its OS direct parent. Windows, Linux, and Darwin are supported;
other platforms fail closed for private control while ordinary supervision
continues.

Consumer impact: update to v0.29.0 only when adopting the new stable-transport
boundary or its attestation seam. Product code still owns executable and layout
authorization, active-version and fallback policy, command/environment
construction, update/bootstrap policy, shared-daemon placement, and operator
exit behavior. Do not copy the `mcp-mux` private protocol methods, exit code,
parser, replay loop, or launcher adapter; use `supervisor.ProtocolV2()`,
`supervisor.Run`, `supervisor.StartCommand`, and `supervisor/attest`.

Rollback: pin `muxcore/v0.28.0` or restore the prior product binary. Do not
force a mixed-version live supervisor handoff or forward an old attestation
advertisement. Old/new combinations remain ordinary MCP without private
dormancy and should restart through the product's bounded replacement path.

### v0.28.0 - demand-driven upstream materialization

**No required consumer code changes for ordinary `engine.New` users.** After a
compatible discovery template exists, muxcore can create a cache-only owner
that serves cached `initialize` and `tools/list` without an upstream process.
The first uncached request starts exactly one generation, completes MCP
initialization, and forwards the original request once on the same transport.

Template authorization uses the full SHA-256 identity of the effective
security-relevant environment; isolated templates also require the exact
canonical working directory. Missing, incompatible, or repeatedly racing
templates take one bounded cold/eager path and never replay partial or
cross-context discovery.

An installed generation remains authoritative through retirement until both
`Process.Done` and process-group or Windows Job authority retirement are
proven. `FINALIZE_BLOCKED` prevents an overlapping replacement while the exact
generation is retried. Restart negotiation failures before detach leave the
predecessor serving; post-detach failure proves successor exit and pre-starts
one clean snapshot successor before predecessor shutdown.

Consumer impact: update to v0.28.0. Persistent owners and explicitly eager
callers retain eager startup. Do not add consumer-local spawn locks, retry
loops, PID sweeps, stale-process kill loops, or parallel lifecycle mechanisms.

### v0.27.2 - template background-spawn ownership gate

**No required consumer code changes for ordinary `engine.New` users.** This
patch closes a process-authority race for snapshot/template owners:

- when snapshot restoration has already started the template-cache background
  upstream, the first uncached request waits for that existing start instead of
  opening a competing request-respawn generation;
- a successful pending generation releases the gate only after the upstream
  answers `initialize` and muxcore writes `notifications/initialized`, so
  ordinary requests cannot overtake the MCP lifecycle handshake; exact-
  generation terminal failure releases the gate into the existing explicit
  error/respawn path;
- proactive requests and ordinary responses are bound to one exact upstream
  generation; dead-generation registry entries are drained, and stale or
  unclaimed responses cannot mutate caches, pending counts, or sessions;
- the wait remains bounded by the existing upstream readiness timeout and owner
  shutdown, after which the request receives an explicit error rather than an
  unowned process tree;
- ordinary request-triggered respawn remains available when no background start
  is pending or the completed background start did not produce a writable
  upstream.

Consumer impact: update to v0.27.2. The fix prevents duplicate direct upstream
roots, locked source-checkout entrypoints, and respawn storms caused by two
startup paths writing the same owner process slot. Do not add product-local
spawn locks, file-replacement retries, PID sweeps, stale-process kill loops, or
parallel lifecycle mechanisms; muxcore retains single process-tree authority.

### v0.27.1 - idle-gate rolling-compatibility hotfix

**No required consumer code changes for ordinary `engine.New` users.** This
patch closes the control-plane retry herd discovered after the v0.27.0
lifecycle release:

- a v0.27.1 shim talking to a pre-v0.27 daemon treats the legacy
  `unknown command: can_suspend` response as a permanent unavailable safety
  gate for that connected shim. It keeps the data-plane IPC session open and
  stops polling instead of opening a fresh control connection every five
  seconds;
- unknown-token, owner-gone, persistent-owner, malformed, missing, and other
  non-transient protocol verdicts follow the same fail-closed one-shot path.
  Transport failure and daemon shutdown remain retryable with capped,
  per-token jittered exponential backoff; busy, pending-request, and
  active-progress denials remain recheckable;
- product shims send the spawn-returned `server_id` with `can_suspend`, so the
  daemon checks exactly one owner's reconnect history. Legacy callers that omit
  `server_id` retain the compatibility scan, while cross-owner and stale-token
  checks still fail closed.

Consumer impact: update to v0.27.1. Existing v0.27.0 shim engines must enter the
new binary generation before the permanent-retry fix applies; the stable
launcher/active-engine update path performs that replacement without requiring
consumer source changes. Do not add product-local gate polling, token indexes,
or stale-process kill loops.

### v0.27.0 - lifecycle convergence and process-tree authority

**No required consumer code changes for ordinary `engine.New` users.** This
release converges disposable owner, shim, launcher, and subprocess lifecycles:

- the `mcp-mux` product shim parks its daemon IPC session after 10 minutes of
  safe inactivity, waits 30 seconds for exact-owner demand, then lets the stable
  launcher become dormant until the next host frame. Product operators can
  override these Go durations with `MCPMUX_SHIM_IDLE_TIMEOUT` and
  `MCPMUX_SHIM_DORMANT_GRACE`; zero or negative disables the corresponding
  stage. Invalid values retain the defaults;
- persistent owners never enter this idle-suspend path. Keep `Persistent: true`
  or `x-mux.persistent: true` for background jobs, indexes, caches, or other
  state that must survive zero connected sessions;
- reconnect preserves the host stdio transport, not arbitrary requests. An
  already-sent in-flight request receives an explicit JSON-RPC error with its
  original id and is never replayed. Muxcore replays only the cached
  `initialize` handshake needed to warm the replacement connection, then sends
  new buffered host demand once;
- handoff `protocol_version` is now `2`. The first v1-to-v2 restart rejects live
  handoff before any owner detaches and takes one bounded snapshot-backed
  shutdown-and-respawn path. Later v2-to-v2 restarts transfer stdio and the
  single process-tree authority transactionally, preserving the existing
  upstream generation only after the successor's final adoption acknowledgment;
- subprocess cleanup now covers the full tree: Unix process groups and Windows
  Job Objects are finalized once, including descendants that inherit stdio or
  outlive their leader.

For direct `owner.RunResilientClient` consumers, the new
`ResilientClientConfig.IdleSuspendDelay`, `IdleSuspendGate`, and
`IdleDormantGrace` fields are opt-in; their zero values keep the previous
always-connected shim behavior. The private launcher dormant notifications and
exit code are an `mcp-mux` product protocol, not a consumer API to copy.

Consumer impact: update to v0.27.1. Do not add product-local request replay,
shim polling, stale-process sweeps, PID-only kills, launcher respawn loops, or
handoff retry protocols; those compete with muxcore ownership. If Serena's web
dashboard should not open automatically, use `--open-web-dashboard false` or
`web_dashboard_open_on_launch: false`. To disable the dashboard service itself,
set `web_dashboard: false` in `serena_config.yml`; dashboard policy is separate
from muxcore tree cleanup.

Rollback: point the product back to its previous muxcore/binary. Expect the
first restart across the v2/v1 boundary to take the same bounded snapshot
respawn path; do not force mixed-version live handoff.

Release evidence must include the root and muxcore test suites, `go vet`, the
repository critical suite, focused idle/dormant and full-tree tests on Windows
and Unix, one v1-to-v2 fallback proof, one same-v2 authority-retention proof,
and the consumer handoff gate from `docs/RELEASE-PROTOCOL.md`.

### v0.26.13 - transport degraded retry and automatic zero-session cleanup

**No required consumer code changes for ordinary `engine.New` users.** This
release hardens muxcore's lifecycle responsibilities:

- reconnect timeout no longer closes the parent stdio transport when a
  reconnect path is configured; the shim enters degraded retry, returns
  explicit JSON-RPC errors for new requests by original id, keeps retrying, and
  resumes proxying on the same transport after backend recovery;
- zero-session disposable owners are cleaned automatically after a 30-second
  safety-gated delay. Cleanup re-checks same owner entry, same zero-session
  timestamp, no sessions, non-persistent ownership, no pending requests, no
  active progress tokens, and no busy declarations.

| API | Semantics |
|-----|-----------|
| `engine.Config.ZeroSessionCleanupDelay` | Optional event-driven cleanup delay after the last session disconnects. Zero uses the muxcore default (`30s`); negative disables this path and leaves cleanup to the periodic reaper. |
| `daemon.Config.ZeroSessionCleanupDelay` | Lower-level daemon equivalent for direct daemon consumers. Ordinary consumers should prefer `engine.Config`. |

Consumer impact: update to v0.26.13. Do not add product-local transport restart
loops, stale-owner kill jobs, or manual cleanup hacks for recoverable
daemon/owner outages; muxcore owns the degraded reconnect and zero-session
cleanup protocols. Products with background state should keep `Persistent: true`
or declare `x-mux.persistent: true`.

### v0.26.9 - transparent upstream respawn for live sessions

**No breaking changes.** v0.26.9 makes pipe-backed owners treat an upstream
process exit as a recoverable upstream-generation failure instead of owner or
session teardown when live sessions remain attached. Muxcore drains current
in-flight requests with explicit JSON-RPC `upstream process exited` errors by
original id, respawns the replacement upstream inside the same owner, refreshes
MCP discovery caches, and lets the next request on the same downstream session
hit the replacement upstream automatically.

Consumer impact: update to v0.26.9. Do not add product-local direct-stdio
smokes, binary-renaming schemes, retry loops, or manual reconnect hacks for
ordinary subprocess upstream crash/update recovery; muxcore owns the
same-session respawn protocol. Pure `SessionHandler` native consumers still use
the explicit muxcore live-update helpers for product binary replacement.

### v0.26.8 - snapshot tools/list cache refresh fix

**No breaking changes.** v0.26.8 keeps snapshot-restored `tools/list` cache
available while the replacement upstream refreshes in the background. This
prevents startup-time `tools/list` timeouts where a restored owner had cached
tools, cleared them before refresh, and then a new MCP host session waited on a
slow live upstream until the host startup budget expired.

Consumer impact: update to v0.26.8. Do not add product-local cold-start,
binary-renaming, or cache invalidation workarounds for this issue; muxcore owns
the snapshot-cache refresh protocol.

### v0.26.7 - consumer docs target correction

**No runtime changes from v0.26.6.** Use v0.26.7 as the current consumer target
so tagged source docs and agent-facing instructions point at the released
auto-managed namespace implementation instead of v0.26.5.

### v0.26.6 - auto-managed engine namespace

**No breaking changes for ordinary `engine.New` consumers.** `engine.Config.Name`
is now a human-readable/status label, and muxcore derives a collision-resistant
transport namespace automatically from the label plus product identity. Daemon
control sockets, owner sockets, daemon locks, stale-socket cleanup, snapshot
restore, and reconnect behavior use the resolved namespace.

| API | Semantics |
|-----|-----------|
| `engine.Config.Name` | Optional display label surfaced in status and registry descriptors. Empty derives from Go build metadata or executable name. |
| `engine.Config.Namespace` | Optional legacy/advanced transport namespace override. Empty is the normal value; muxcore derives the namespace automatically. Set only to preserve a previously shipped namespace. |
| `daemon.Config.Namespace` | Lower-level direct-daemon override. Empty defaults to `daemon.Config.Name` for compatibility; ordinary consumers should use `engine.New`. |

Consumer impact: new consumers should not hand-pick pipe names. Existing
consumers that must keep a shipped namespace for rolling compatibility can set
`Namespace` explicitly during migration.

### v0.26.5 - process-explosion owner fanout reduction

**No breaking changes.** v0.26.5 reduces two confirmed owner/process fanout
vectors after startup storms:

- forced-isolated retry counters are cleaned when the final owner in that retry
  family is removed;
- global/shared env bucketing ignores ordinary launch noise (`PATH`, temp dirs,
  shell/session metadata) while preserving credential/config/proxy/URL/host/port
  and cert identity boundaries.

Consumer impact: update to v0.26.5; no muxcore API change is required. This
release does not kill or clean already-running stale workstation processes. It
prevents new fanout from these source-side vectors after consumers deploy the
new muxcore/runtime.

Operational invariant: a Windows named pipe that is already bound is healthy
only if the control endpoint answers ping/status within the bounded readiness
window. Bound but unresponsive is an abnormal recovery state, not replacement
readiness and not permission to spawn a competing daemon.

### v0.26.4 - Windows occupied control-pipe startup guard

**No breaking changes.** v0.26.4 includes the v0.26.3 restart reconnect /
launcher upgrade protocol plus a Windows startup guard for occupied but
unresponsive daemon control pipes. When a control endpoint is already bound but
does not answer ping, `mcp-mux` now waits and fails clearly instead of spawning a
competing daemon against the same named pipe. A genuinely missing pipe remains a
normal startup miss.

Consumer impact: update to v0.26.4 or newer; no muxcore API change is required. This
does not recover an already-unresponsive daemon process by killing it. Recovery
still needs explicit operator or product policy.

### v0.26.3 - restart reconnect and launcher upgrade protocol

**No breaking changes.** v0.26.3 fixes restart reconnect behavior and stable
launcher upgrade protocol issues. It is superseded by v0.26.4 for Windows
control-pipe startup reliability.

### v0.26.2 - consumer docs target correction

**No runtime changes from v0.26.1.** Use v0.26.2 as the current consumer target
so tagged source docs no longer point agents at the older v0.25.3 install
snippet.

### v0.26.1 - opt-in daemon registry ownership hardening

**No breaking changes.** v0.26.1 includes the v0.25.3 native SessionHandler
hot-update helpers and the v0.26.x opt-in daemon registry. Consumers that do
not set `engine.Config.Registry` remain invisible to cross-engine discovery and
keep existing behavior.

| API | Semantics |
|-----|-----------|
| `engine.Config.Registry *registry.Config` | Optional daemon advertisement. Nil is the opt-out zero value. |
| `registry.Config{ProductName, MuxcoreVersion, Capabilities}` | Descriptor metadata for products that want central read-only visibility. |
| `registry.Capabilities{ListOwners: true}` | CR-001 read-only capability used by `mux_engines` and scoped `mux_list(engine_name)`. |
| `mux_engines` | mcp-mux operator tool that lists opted-in muxcore daemons as healthy/stale/invalid/duplicate after status verification. `duplicate` means multiple healthy descriptors share an engine name; stale leftovers stay stale. |
| `mux_prune_engines` | mcp-mux operator tool that dry-runs by default and removes only stale / invalid native registry descriptor files when called with `dry_run:false`. It never stops native muxcore daemons, owners, processes, or control sockets. |
| `mux_list(engine_name: "...")` | Explicit read-only owner listing for one registered engine. Default `mux_list` remains scoped to `mcp-mux`; `sessions` counts downstream MCP clients/shims, which can exceed visible terminal sessions. |

Descriptor verification rejects PID mismatches, and daemon shutdown removes a
descriptor only if it still belongs to the same process. CR-001 does not add
cross-engine stop/restart/update. Stale descriptors left by crashed or test
daemons can be garbage-collected through `mux_prune_engines` after verification.

### v0.25.3 - native SessionHandler live update helpers (#239)

**No breaking changes.** The live-update helpers are additive; existing
`upgrade.Swap`, control commands, and engine configuration keep their current
behavior. v0.25.3 superseded the v0.25.1/v0.25.2 release candidates and
included the active-control-socket wait fix needed for reliable fallback start
after graceful restart.

| API | Semantics |
|-----|-----------|
| `engine.UpdateAndRestartOptions` | Caller supplies `CurrentExe`, `StagedExe`, drain/restart/shutdown/ready timeouts, stale cleanup preference, and optional lock-failure behavior. |
| `engine.RestartWithSuccessorOptions` | Caller supplies the explicit successor executable for stable-launcher/versioned-engine topologies without renaming the launcher. |
| `engine.UpdateAndRestartResult` | Reports partial success: `OldPath`, daemon-running state, lock acquisition, graceful restart, shutdown fallback, replacement start/readiness, stale cleanup count, and warnings. |
| `(*engine.MuxEngine).ApplyUpdateAndRestart(ctx, opts)` | Runs the reusable provider sequence: `upgrade.Swap`, daemon namespace lock, `graceful-restart`, shutdown fallback, wait-for-exit, replacement daemon start, and ready wait. |
| `(*engine.MuxEngine).RestartWithSuccessor(ctx, opts)` | Runs the restart sequence with an explicit `SuccessorExe`, but does not rename files. Use after the consumer has updated its active engine pointer. |
| `control.Request.SuccessorExe` + `control.GracefulRestartOptionsHandler` | Additive control-plane extension so a caller can tell the old daemon which executable should become the successor during graceful restart. Older handlers fall back to the legacy `HandleGracefulRestart(int)` path. |

**Migration for aimux after muxcore release:**

Use this helper only for the fixed replaceable engine path topology. If aimux
or another consumer uses a stable launcher plus versioned engine store, update
the active engine pointer and invoke graceful restart with the intended
successor executable instead of passing the launcher path as `CurrentExe`:

```go
result, err := eng.RestartWithSuccessor(ctx, engine.RestartWithSuccessorOptions{
    SuccessorExe:    nextEngineExe,
    DrainTimeout:    30 * time.Second,
    RestartTimeout:  60 * time.Second,
    ShutdownTimeout: 10 * time.Second,
    ReadyTimeout:    30 * time.Second,
})
```

Use `ApplyUpdateAndRestart` only when `CurrentExe` is the replaceable engine
binary that should exist after the swap:

```go
result, err := eng.ApplyUpdateAndRestart(ctx, engine.UpdateAndRestartOptions{
    CurrentExe:      currentExe,
    StagedExe:       currentExe + "~",
    DrainTimeout:    30 * time.Second,
    RestartTimeout:  60 * time.Second,
    ShutdownTimeout: 10 * time.Second,
    ReadyTimeout:    10 * time.Second,
    CleanStale:      true,
})
```

Contract: surviving shims reconnect automatically to the replacement daemon
when possible. This is transport continuity, not request replay; in-flight
requests may receive explicit JSON-RPC errors by original ID.

For `SessionHandler` consumers, this is not heap-state migration. Muxcore
preserves shim transport, owner/snapshot metadata, cached MCP discovery
responses, classification, cwd/env metadata, reconnect-token history, and
reconnect behavior. Product-private handler state must be persisted by the
consumer or reconstructed by the successor daemon.

Healthy SessionHandler hot-update evidence: same open session reports the new
product executable/version, `daemon_generation` changes,
`handoff.restored_owner_count > 0`, `shim_reconnect_refreshed > 0`,
`shim_reconnect_fallback_spawned == 0`, and `shim_reconnect_gave_up == 0`.

**Tests landed in this release:**
- `TestRestartWithSuccessor_ValidationErrors`
- `TestRestartWithSuccessor_GracefulSuccessUsesExplicitSuccessor`
- `TestRestartWithSuccessor_GracefulErrorFallbackStartsSuccessor`
- `TestRestartWithSuccessor_PreparesControlSocketBeforeCleanFallbackStart`
- `TestRestartWithSuccessor_TreatsAlreadyBoundReplacementAsReady`
- `TestPrepareControlSocketForReplacement_WaitsOnActiveSocketFalseNegative`
- `TestPrepareControlSocketForReplacement_ReturnsAfterEndpointUnavailable`
- `TestApplyUpdateAndRestart_GracefulSuccessUsesSuccessorExe`
- `TestApplyUpdateAndRestart_DaemonNotRunningOnlySwaps`
- `TestApplyUpdateAndRestart_SwapFailureStopsBeforeDaemonCalls`
- `TestApplyUpdateAndRestart_GracefulErrorFallsBackToShutdownAndStart`
- `TestApplyUpdateAndRestart_LockFailureIsPhaseError`
- `TestApplyUpdateAndRestart_FallbackExitTimeoutIsPhaseError`
- `TestApplyUpdateAndRestart_StartFailureIsPhaseError`
- `TestApplyUpdateAndRestart_ReadyTimeoutIsPhaseError`
- `TestApplyUpdateAndRestart_SessionHandlerConsumerUsesEngineNamespace`
- `TestGracefulRestartPassesSuccessorExeToOptionsHandler`
- `TestSuccessorExecutableRequestOverrideWins`

---

### v0.24.0 — multi-tenant extensions: ConnInfo + SessionMeta + AuthorizeSession + OnFrameReceived (#109, #110, #111, #112)

**No breaking changes.** All four extensions are additive — nil-default
callbacks and optional interface upgrades preserve pre-v0.24 behavior
byte-identically for non-adopting consumers.

| API | Semantics |
|-----|-----------|
| `muxcore.ConnInfo` | New value type carrying OS-level peer identity (`PeerPid`, `PeerUid`, `Platform`). Populated once at accept time via `peerCreds(conn)` on Linux (SO_PEERCRED), Windows (`GetNamedPipeClientProcessId` via the `Fd()` exposed by `*go-winio.win32File`), and macOS (LOCAL_PEERPID + GetsockoptXucred). Zero-value semantics: `PeerPid==0`/`PeerUid==0` ≡ "unavailable". Stable platform identifier constants: `PlatformLinuxUnix`, `PlatformWindowsNamedPipe`, `PlatformDarwinUnix`, `PlatformUnknown`. |
| `muxcore.SessionMeta` | New value type combining `Conn ConnInfo` (OS facts) with `TenantID string` + `AuthorizedAt time.Time` (consumer-policy fields produced by AuthorizeSession). `IsAuthorized()` discriminator distinguishes "callback not configured" (zero `AuthorizedAt`) from "AuthAllow with empty TenantID" (legitimate per FR-3). |
| `muxcore.AuthDecision` + `muxcore.SessionAuth` | New types backing AuthorizeSession verdicts (`AuthAllow`, `AuthDeny`). |
| `muxcore.FrameAction` | New type backing OnFrameReceived verdicts (`FramePass=0`, `FrameDrop=1`, `FrameError=2`). Numeric ordering preserves fail-open semantics — `FramePass` is the zero value, returned on timeout / panic / nil callback. |
| `muxcore.NotificationHandlerWithSessionMeta` | Optional handler upgrade — receives `SessionMeta` on every notification. Owner type-asserts at dispatch; handlers satisfying both legacy and `*WithSessionMeta` see ONLY the WithSessionMeta path (EC-7). |
| `muxcore.SessionHandlerWithSessionMeta` | Optional handler upgrade — receives `SessionMeta` on every request. Same dispatch precedence as above. |
| `engine.Config.AuthorizeSession` | Single-shot per-session admission gate. Invoked AFTER IPC handshake / peer-creds extraction and BEFORE AddSession. AuthDeny closes the connection with JSON-RPC -32000 + reason and never spawns upstream. AuthAllow stamps SessionMeta.TenantID + AuthorizedAt. Panics recovered → AuthDeny{Reason:"authorize panic"}. |
| `engine.Config.OnFrameReceived` | Per-frame inbound admission hook. Synchronous on reader goroutine, 1ms budget (overrun → FramePass), panic-safe (panic → FramePass). FrameDrop = silent discard; FrameError = JSON-RPC -32004 with msg.ID preserved; FramePass = normal dispatch. Scope is INBOUND ONLY (CHK014) — outbound responses and synthetic notifications are not intercepted. |

**Migration:** Existing consumers (aimux, engram, mcp-launcher) require zero source change. All callbacks default to nil; both new handler interfaces are optional upgrades.

**Migration for aimux (multi-tenant adoption):**

```go
import muxcore "github.com/thebtf/mcp-mux/muxcore"

// Optional handler upgrade — consume meta.Conn / meta.TenantID inside
// every dispatch.
type aimuxHandler struct{ /* ... */ }

func (h *aimuxHandler) HandleRequest(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error) {
    // legacy fallback — used only by sessions that bypassed the
    // *WithSessionMeta dispatch path
    return nil, fmt.Errorf("legacy dispatch not supported")
}

func (h *aimuxHandler) HandleRequestWithSessionMeta(
    ctx context.Context,
    p muxcore.ProjectContext,
    meta muxcore.SessionMeta,
    req []byte,
) ([]byte, error) {
    // meta.Conn.PeerPid / meta.Conn.PeerUid / meta.Conn.Platform
    // meta.TenantID — if AuthorizeSession was wired
    return doWork(ctx, p, meta, req)
}

eng, err := engine.New(engine.Config{
    Name:           "aimux",
    SessionHandler: &aimuxHandler{},

    // Pre-dispatch admission gate — runs once per session.
    AuthorizeSession: func(ctx context.Context, conn muxcore.ConnInfo, p muxcore.ProjectContext) muxcore.SessionAuth {
        tenant, ok := lookupTenant(conn.PeerPid)
        if !ok {
            return muxcore.SessionAuth{Decision: muxcore.AuthDeny, Reason: "tenant_not_enrolled"}
        }
        return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: tenant}
    },

    // Per-frame admission hook — runs on every inbound request/notification.
    // Sync on reader goroutine, 1ms budget. Heavy work belongs elsewhere.
    OnFrameReceived: func(sessionID string, frameSize int, method string) muxcore.FrameAction {
        if rateLimiter.Allow(sessionID) {
            return muxcore.FramePass
        }
        return muxcore.FrameError // -32004 to client; no dispatch
    },

    Logger: logger,
})
if err != nil {
    return err
}
```

**Cross-platform peer-credential extraction:**

| Platform | PeerPid source | PeerUid source |
|----------|----------------|----------------|
| Linux | `SO_PEERCRED` ucred (existing) | `SO_PEERCRED` ucred (shared `readPeerUcred` helper — one syscall returns Pid+Uid+Gid) |
| Windows (named pipe) | `GetNamedPipeClientProcessId(Fd())` via `interface{ Fd() uintptr }` assertion — winio's `*win32File` already exposes Fd publicly via embedding (no fork required) | always 0 (Windows has no UID concept comparable to Unix) |
| Darwin | `getsockopt(SOL_LOCAL, LOCAL_PEERPID)` | `GetsockoptXucred(SOL_LOCAL, LOCAL_PEERCRED)` — kernel xucred struct (Getpeereid is not exposed by `x/sys/unix`) |

**Tests landed in this release:**
- Phase 1 (`G001`): `TestPeerCreds_LoopbackPID_Linux`, `TestPeerCreds_LoopbackPID_Windows`, `TestPeerCreds_LoopbackPID_Darwin`, `TestDispatch_WithSessionMetaPreferredOverLegacy`, `TestDispatch_LegacyHandlerByteIdentical_v23`
- Phase 2 (`G002`): `TestAuthorize_DenyClosesConnection`, `TestAuthorize_AllowPopulatesSessionMeta`, `TestAuthorize_NilConfigUnchangedDispatch`, `TestAuthorize_PanicTreatedAsDeny`, `TestAuthorize_AllowEmptyTenantIdLegitimate`, `TestAuthorize_DenyNoUpstreamSpawn`
- Phase 3 (`G003`): `TestFrameHook_PassDispatchesNormally`, `TestFrameHook_DropSilentDiscard`, `TestFrameHook_ErrorRespondsWithJSONRPC`, `TestFrameHook_TimeoutGracefulDegradation`, `TestFrameHook_PanicTreatedAsPass`, `TestFrameHook_NilUnchangedDispatch`, `TestFrameHook_OutboundFramesNotIntercepted`, `TestFrameHook_OverheadP99`, `BenchmarkFrameHook_NoopOverhead` (804 ns/op — well under spec's 100µs budget)

**Cross-version coexistence:** v0.24.0 daemon coexists with pre-v0.24 consumers. nil-default for every new field preserves the v0.23 dispatch path byte-identically. v0.24 consumers can opt in incrementally — adopt `*WithSessionMeta` interfaces first, then `AuthorizeSession`, then `OnFrameReceived` independently as policy needs arise.

---

### v0.23.0 — engine.Config.OnInject for fire-and-forget IPC frame injection (#107)

**No breaking changes.** `OnInject == nil` is the zero value and preserves
pre-v0.23 behavior for non-adopting consumers.

| API | Semantics |
|-----|-----------|
| `engine.Config.OnInject` | Additive passthrough callback. Called once with `inject func([]byte) error` after the initial shim→owner handshake completes. |
| `owner.ResilientClientConfig.OnInject` | Real wiring point in the resilient shim. Injected frames enter the existing `msgFromCC` path and preserve FIFO ordering with normal CC traffic. |
| `owner.ErrInjectFull` | Sentinel returned when the `msgFromCC` buffer is saturated. Non-blocking backpressure signal; callers decide drop vs retry. |
| `owner.ErrInjectClosed` | Sentinel returned after the resilient proxy exits. Further inject attempts become lifecycle-aware no-ops. |

**Migration:** Existing consumers (aimux ≤v0.22.0, engram, mcp-launcher) require zero source change. `OnInject == nil` is the zero value.

**Migration for aimux:**

```go
eng, err := engine.New(engine.Config{
    Name: "aimux",
    Command: os.Args[0],
    Args: os.Args[1:],
    OnInject: func(inject func([]byte) error) {
        sink.SetSendFunc(func(frame []byte) error {
            // inject returns nil on success, owner.ErrInjectFull under
            // backpressure, or owner.ErrInjectClosed after the proxy exits.
            // Caller decides drop vs retry vs stop sending; we surface as-is.
            return inject(frame)
        })
    },
    Logger: logger,
})
if err != nil {
    return err
}
```

**Tests landed in this release:**
- `TestResilientClient_OnInject_DeliversFrames`
- `TestResilientClient_OnInject_BufferFull`

### v0.22.0 — multi-tenant FS isolation + Persistent propagation (#102, #103)

**BREAKING CHANGES.** Engine name now scopes the FS namespace for owner sockets.
Two muxcore consumers on one host (e.g. mcp-mux + aimux) no longer share the
literal `"mcp-mux-"` prefix and cannot interfere with each other.

Historical note: this section describes the v0.22 migration point. The current
consumer contract is the "v0.26.6 - auto-managed engine namespace" section above:
`engine.Config.Name` is a display label, and ordinary consumers should leave
`engine.Config.Namespace` empty.

| Change | Migration |
|--------|-----------|
| `serverid.IPCPath(id, baseDir)` → `serverid.IPCPath(baseDir, name, id)` | Pass engine name as the second argument. Format becomes `{baseDir}/{name}-{id}.sock`. |
| `serverid.ControlPath(id, baseDir)` → `serverid.ControlPath(baseDir, name, id)` | Same — argument-order change + new name parameter. |
| `serverid.LockPath(id, baseDir)` → `serverid.LockPath(baseDir, name, id)` | Same. |
| v0.22-only `engine.Config.Name` requirement | At that release point, `engine.New(Config{Name: "..."})` rejected empty Name. Current muxcore derives Name and Namespace automatically for `engine.New`. |
| `daemon.Config.Name string` (additive) | The engine layer now passes `cfg.Name` through to `daemon.Config.Name`. Internal callers are updated; library consumers that construct `daemon.Config` directly must set `Name`. |
| `daemon.Config.Persistent bool` (additive) | The engine layer now passes `cfg.Persistent` through. Closes #103: SessionHandler-topology owners now hydrate `OwnerEntry.Persistent` from this field. Subprocess topology continues to derive Persistent from the upstream's `x-mux.persistent` capability — both paths converge on `OwnerEntry.Persistent` before the reaper reads it. |
| `daemon.HandleListOwners` control RPC (new) | New `Cmd: "list_owners"` returns `ListOwnersResponse{Owners []OwnerInfo, Truncated bool}`. mcp-mux's `mux_list/mux_stop/mux_restart` now query this RPC instead of FS-scanning TEMP — closes #102. |
| `cleanStaleSockets` is now name-scoped | A daemon with `Name="aimux"` no longer touches `mcp-mux-*` sockets in TEMP. Foreign-prefix sockets are left alone; only own-prefix unreachable sockets are cleaned. |

**Migration for aimux:**

```go
eng, err := engine.New(engine.Config{
    Name:           "aimux",  // REQUIRED — empty returns error in v0.22.0
    Persistent:     true,     // now actually propagates to OwnerEntry (#103 fix)
    SessionHandler: srv.SessionHandler(),
    // Command/Args optional — SessionHandler topology runs in-process
})
```

After upgrade, `mcp-launcher persist` regression should report PASS where it
previously FAILed.

**Migration for engram:**

```go
eng, err := engine.New(engine.Config{
    Name: "engram",  // REQUIRED in v0.22.0
    // ...rest unchanged
})
```

**Cross-version coexistence:** v0.22 mcp-mux daemon coexists cleanly on the
same host with a v0.21 aimux. The v0.22 daemon's `cleanStaleSockets` only
matches own-prefix sockets, so the v0.21 aimux's `mcp-mux-*` files remain
intact. Operator-side, `mux_list` from the v0.22 mcp-mux no longer sees those
files — it asks the daemon, not the filesystem.

**Migration is non-incremental.** v0.21.x consumers cannot consume
muxcore/v0.22.0 without updating call sites for the three `serverid` functions
and adding `Name` to `engine.Config`. Pin via Go modules until ready.

**Tests landed in this release:**
- `TestNewRejectsEmptyName` (engine) — empty Name diagnostic
- `TestPersistentPropagatesToDaemonConfig` (engine) — propagation
- `TestSessionHandlerOwnerInheritsPersistent` (daemon) — R1 from #103
- `TestReaperRespectsConfigPersistent` (daemon) — R2 from #103
- `TestHandleListOwners` + `TestHandleListOwners_Truncated` (daemon) — RPC shape + 200-cap
- `TestCleanStaleSocketsNameScope` (daemon) — foreign-prefix preservation
- `TestCrossEngineIsolation` (mcpserver integration) — end-to-end FS partition proof
- `TestMuxStopRefusesForeignID` + `TestMuxRestartRefusesForeignID` (mcpserver) — operator footgun closed

**R3 mcp-launcher persist regression** is in-house tooling (`D:\Dev\mcp-launcher`)
— consumer-side verification step run during T019/T021 deployment phases.

---

### v0.21.10 — control flush before afterFn

v0.21.10 fixes conn flush before shutdown — explicit `conn.Close()` before
`afterFn()` ensures response reaches the caller. v0.21.9's `afterFn`
callback was necessary but insufficient: `defer conn.Close()` never ran
because Shutdown completed instantly post-handoff, killing the goroutine.

v0.21.9 introduced the `afterResponse func()` callback pattern.
**Breaking:** `DaemonHandler.HandleGracefulRestart` signature gains a
third return value `afterResponse func()`.

v0.21.8 fixes control socket conflict during graceful restart handoff.
`daemon.New()` bound the control socket before `loadSnapshot`/`tryHandoffReceive`
— predecessor still held it → successor failed → handoff never reached.
Fix: in handoff mode (env vars present), `loadSnapshot`/`tryHandoffReceive`
run first, then `retryControlBind` polls until predecessor releases the
socket. Non-handoff path unchanged. No breaking API changes.

v0.21.7 fixes `spawnSuccessor` hardcoding `"--daemon"` instead of using
`engine.Config.DaemonFlag` (default `"--muxcore-daemon"`). Both v0.21.7
and v0.21.8 are required for graceful restart to work end-to-end.

v0.20.4 is the multi-user hardening release on top of v0.20.3. Closes the
2 HIGH security findings from the 2026-04-18 PRC audit (S8-001 token
handshake enforcement, S5-001 Unix socket 0600 permissions). Ships FR-28
and FR-29 from the post-audit-remediation spec amendment. No breaking API
changes — two additive exports (`muxcore.ListenUnix`, `SessionManager.IsPreRegistered`).

For historical v0.19.3 concurrency fixes, see [muxcore/v0.19.3 release notes](https://github.com/thebtf/mcp-mux/releases/tag/muxcore%2Fv0.19.3).

| Fix | Where | Severity |
|-----|-------|----------|
| Owner Serve loop CPU spin after failed background spawn (BUG-001) | `owner/owner.go` | P1 |
| `ownerNotifier.Notify` held RLock across 30s write deadline (BUG-002) | `owner/owner.go` | P1 |
| JSON escape regression in `dispatchToSessionHandler` — invalid JSON on Windows paths/quotes (H1) | `owner/owner.go` | P1 |
| `daemon.cleanupDeadOwner` TOCTOU identity gap (BUG-003) | `daemon/daemon.go` | P1 |
| `control.Server handleConn` no read/write deadlines — silent-client DoS (BUG-004) | `control/server.go` | P1 |
| `daemon.Spawn` recursion on stuck placeholder + findSharedOwner argv collision | `daemon/daemon.go` | P2 |
| `drainOrphanedInflight` silent stdout write failures — CC hang with no log | `owner/resilient_client.go` | P2 |
| `findSharedOwner` lock-semantics doc ambiguity + stale-iterator hazard | `daemon/daemon.go` | P2 |

Every fix ships with at least one regression test. Full release notes:
https://github.com/thebtf/mcp-mux/releases/tag/muxcore/v0.19.3

v0.19.2 is a bug fix release on top of v0.19.1 — fixes a recursive goroutine
leak in `daemon.Spawn` when a concurrent placeholder wait times out. No API
changes, zero consumer code modifications required.

v0.19.1 was a refactor-only release on top of v0.19.0 — zero behaviour change.
Adds `engine.Config.SkipSnapshot` as an opt-in field (zero-value preserves the
prior hardcoded default). All v0.19.0 migration notes below still apply.

### Upgrade from v0.18.x

#### Breaking changes

| Change | Migration |
|--------|-----------|
| `DaemonControlPath(baseDir)` → `DaemonControlPath(baseDir, name)` | Add engine name as second arg. Empty string = "mcp-mux" (backward compat). engine.Config.Name handles this automatically. |
| `DaemonLockPath(baseDir)` → `DaemonLockPath(baseDir, name)` | Same as above. |

#### New: SessionHandler (replaces Handler for multi-session awareness)

```go
import muxcore "github.com/thebtf/mcp-mux/muxcore"

type MyHandler struct{}

func (h *MyHandler) HandleRequest(ctx context.Context, p muxcore.ProjectContext, req []byte) ([]byte, error) {
    // p.ID  — deterministic project ID (from worktree root hash)
    // p.Cwd — CC session working directory
    // p.Env — per-session env diff (API keys, config)
    return myServer.Handle(req)
}

// Optional: lifecycle hooks
func (h *MyHandler) OnProjectConnect(p muxcore.ProjectContext)    { /* new CC session */ }
func (h *MyHandler) OnProjectDisconnect(projectID string)         { /* CC session left */ }

// Optional: targeted notifications
func (h *MyHandler) SetNotifier(n muxcore.Notifier) { h.notifier = n }
// Then: h.notifier.Notify(projectID, notification) — to one session
//       h.notifier.Broadcast(notification)          — to all sessions

// Optional: client notification handling
func (h *MyHandler) HandleNotification(ctx context.Context, p muxcore.ProjectContext, data []byte) {
    // receives notifications/cancelled etc from CC
}

// Wire it:
e, _ := engine.New(engine.Config{
    Name:           "aimux",
    SessionHandler: &MyHandler{},
})
e.Run(ctx)
```

Legacy `Handler func(ctx, stdin, stdout)` still works. If both set, SessionHandler wins.

#### New: upgrade.Swap (binary update for engine consumers)

```go
import "github.com/thebtf/mcp-mux/muxcore/upgrade"

// Atomic binary swap: current → .old.{pid}, new → current
oldPath, err := upgrade.Swap(currentExe, newExe)

// Clean stale binaries (.old.*, .bak, ~~)
cleaned := upgrade.CleanStale(exePath)
```

For current consumers, prefer `engine.ApplyUpdateAndRestart` for the full live
update flow. `upgrade.Swap` remains the low-level atomic file rename primitive;
by itself it does not signal, wait for, or restart a daemon.

#### Bug fixes included in v0.18.0–v0.19.0

| Fix | Issue |
|-----|-------|
| Full env passthrough — remove diffEnv, pass complete session env | #50 |
| Stale daemon socket cleanup for engine consumers | #48 |
| Daemon CPU spin (60-80%) when owner shutdown with nil upstream | #46 |
| Crash circuit breaker (5 crashes/60s → spawn rejected) | #43 |
| CWD-aware dedup (no cross-project context leaks) | #42 |
| Per-engine daemon sockets (no collision between mcp-mux + aimux) | #43 |
| `_meta` injection for in-process handlers | #44 |
| SpawnUpstreamBackground HandlerFunc fix | #41 |

### For aimux

```bash
cd aimux && go get github.com/thebtf/mcp-mux/muxcore@v0.26.9
```

Key changes to adopt:
1. **SessionHandler** — replace `srv.StdioHandler()` with SessionHandler implementation to get per-CC-session ProjectContext
2. **Native update topology** — use `eng.RestartWithSuccessor(...)` when aimux keeps a stable launcher plus versioned engine store; use `eng.ApplyUpdateAndRestart(...)` only when `CurrentExe` is the replaceable engine path. Do not call `upgrade.Swap` as the whole update protocol.
3. **Native process management** — agents should use aimux's own `sessions`, `status`, and `upgrade` surfaces for aimux state. Default `mcp-mux mux_list` is scoped to the `mcp-mux` daemon namespace. When aimux consumes a muxcore version with opt-in daemon registry support, it may advertise via `engine.Config.Registry`; then `mux_engines` can show aimux and `mux_list(engine_name="aimux")` can list aimux owners read-only after status verification. This does not imply cross-engine stop/restart/update.
4. **Transparent subprocess respawn** — included automatically for pipe-backed owners. Do not duplicate it in aimux with direct stdio smokes, ad hoc binary names, or local retry loops; validate that the same open session survives an upstream crash/update and that the next call reaches the replacement upstream.
5. **v0.19.3 concurrency fixes** — included automatically, no code changes needed. The CPU-spin-on-failed-background-spawn (BUG-001) and ownerNotifier.Notify RLock hold (BUG-002) are both hidden behind existing aimux code paths and required no consumer-side changes.
6. **Circuit breaker** — included automatically, protects against upstream crash loops

### For engram

```bash
cd engram && go get github.com/thebtf/mcp-mux/muxcore@v0.26.9
```

Key changes:
1. **DaemonControlPath** — if you call it directly, add name parameter: `DaemonControlPath(baseDir, "engram")`
2. **v0.19.3 concurrency fixes** — included automatically
3. **SessionHandler** — optional. engram can stay on legacy `Handler` until multi-session support is needed
4. **Opt-in daemon registry** — optional read-only visibility. If engram adopts `engine.Config.Registry`, `mcp-mux serve` can show it through `mux_engines` and scoped `mux_list(engine_name="engram")`; default `mux_list` remains the `mcp-mux` namespace and engram's own management surface remains authoritative.
5. **Transparent subprocess respawn** — included automatically for pipe-backed owners. Remove or avoid product-local retry/reconnect workarounds for ordinary upstream process replacement; the provider acceptance is same-session crash error plus next-call generation replacement.

### v0.21.10 — Flush conn before afterFn (#99)

- v0.21.9 `afterFn` ran after `writeResponse` but `defer conn.Close()` never executed — post-handoff Shutdown completes instantly (0 owners), process exits, goroutine killed, kernel send buffer lost.
- **Fix:** explicit `conn.Close()` before `afterFn()` in `handleConn`. 1 line.
- **No breaking changes.** No API changes.

### v0.21.9 — Defer shutdown until after response write (#99)

- `HandleGracefulRestart` called `go d.Shutdown()` before the control handler wrote the response. On Windows AF_UNIX, data in the kernel send buffer is lost on process exit → caller sees `i/o timeout` instead of confirmation.
- **Fix:** `HandleGracefulRestart` returns `afterResponse func()` callback. Control server writes response first, then invokes callback.
- **Breaking:** `DaemonHandler.HandleGracefulRestart` signature: `(int) (string, func(), error)`. Consumers must update implementations and mocks.
- **Combined with v0.21.7 + v0.21.8:** all three fixes required for end-to-end graceful restart with confirmed response.

### v0.21.8 — Defer control socket binding in handoff mode (#99)

- `daemon.New()` bound the control socket **before** `loadSnapshot`/`tryHandoffReceive`. Predecessor still holds socket → successor fails → handoff never reached → 30s fallback.
- **Fix:** in handoff mode (env vars present), `loadSnapshot`/`tryHandoffReceive` run first, then `retryControlBind` polls (500ms × 60 = 30s max) until predecessor releases the socket via `Shutdown()`. Non-handoff path unchanged.
- **No breaking changes.** Internal constructor reordering only.
- **Combined with v0.21.7:** both fixes required for graceful restart to work end-to-end (v0.21.7 = DaemonFlag, v0.21.8 = socket ordering).

### v0.21.7 — Fix spawnSuccessor DaemonFlag hardcode (#99)

- `spawnSuccessor` hardcoded `"--daemon"` as the successor CLI flag. `engine.isDaemonMode()` checks `cfg.DaemonFlag` (default `"--muxcore-daemon"`). Mismatch caused successor to enter client mode — `tryHandoffReceive` never ran — graceful restart always fell back to kill-and-respawn after 30s.
- **Fix:** `daemon.Config` gains `DaemonFlag string` field. `engine.runDaemon` passes `e.cfg.DaemonFlag`. `spawnSuccessor` uses configured flag. Empty value defaults to `"--daemon"` for backward compat.
- **No breaking changes.** Additive field only.

### v0.21.1 — Shim reconnect token refresh (F2)

- **`HandleRefreshSessionToken(prevToken string) (newToken string, err error)`** added to `control.DaemonHandler` — lets a shim mint a fresh handshake token for the same owner after its original token is consumed, without triggering a full owner respawn.
- **`session.Manager` bound history** — `Bind` now records a 30-min TTL entry keyed by token; `RegisterReconnect(prev, ownerAlive)` looks up that history and returns a new pending token, or `ErrUnknownToken` / `ErrOwnerGone`.
- **`ResilientClientConfig.RefreshToken` + `MaxRefreshAttempts`** (default 3) — shim tries `HandleRefreshSessionToken` up to N times before falling back to the full `HandleSpawn` path. Fallback also fires immediately on `ErrOwnerGone`. Zero-value `RefreshToken` field preserves pre-F2 behaviour (skip refresh entirely).
- **Structured markers and counters** — `shim.reconnect.refresh_ok|refresh_fail|fallback_spawn` log markers; `shim_reconnect_refreshed`, `shim_reconnect_fallback_spawned`, `shim_reconnect_gave_up` counters exposed via `HandleStatus`. Note: `fallback_spawned` increments only when `HandleSpawn` is called with `ReconnectReason == "fallback_spawn"` (v0.21.x shims only); legacy shims that call bare `HandleSpawn` are invisible to all three counters.
- **Back-compat** — pre-v0.21.1 shims that have no knowledge of `refresh-token` are still rejected cleanly after their token is consumed; they recover via the existing `HandleSpawn` path, identical to a cold first-time spawn from the daemon's perspective.
- **Breaking change (internal API)** — `session.Manager.Bind` signature extended from `Bind(token string, session *Session)` to `Bind(token, ownerKey string, session *Session)`. Only call site is `owner.acceptLoop`; external consumers of muxcore do not call `Bind` directly.

### v0.21.2 — Engine accessors

- Adds read-only accessors to the `engine.Engine` type for observability and testing: `OwnerCount()`, `SessionCount()`, `HandleStatus()`, and `Entry(serverID)`. No breaking changes; all additive.

### v0.21.4 — Defensive ipc.Listen + upgrade --restart split-state fix

- **`ipc.Listen` now refuses to rebind an actively-served path.** Before removing the stale socket file and calling `sockperm.Listen`, `Listen` calls `IsAvailable`. If a live listener is detected, it returns `"ipc: listener already active at <path> (another process is serving)"`. Callers that previously relied on silent socket-steal semantics will now receive a loud error instead. This is a breaking behavioural change, but such reliance was always a bug — a second `Listen` on an active path would have silently disconnected all existing clients. The guard turns that into a diagnosable failure; the caller (snapshot restore, owner startup) can log and skip the conflicting owner rather than corrupting it.

- **`upgrade --restart` fallback-shutdown branch now waits for old daemon exit.** Before spawning the new daemon, `runUpgrade` mirrors the graceful-restart branch's 20×500 ms `isDaemonRunning` poll into both fallback sub-branches (`graceful-restart not available` and `graceful-restart failed`). Prevents the race where the new daemon rebinds per-owner sockets via `ipc.Listen` while the old daemon's Owner structs and `sessionMgr` are still live, causing shim handshake tokens registered in the new daemon to be routed to the old daemon's accept loop and rejected.

- **No API changes.** Both fixes are additive or strictly defensive. `ipc.Listen` signature is unchanged; callers that passed a stale-file path continue to work. The poll loop in `runUpgrade` is internal to the binary.

- **Bundled release:** muxcore/v0.21.4 + v0.21.4 binary.

- **Investigation:** `.agent/debug/upgrade-restart-split-state/investigation.md`

### v0.21.3 — OwnerConfig.UpstreamWriter (proposed in PR #93)

- `owner.OwnerConfig.UpstreamWriter io.Writer` — optional field that, when set, replaces the default subprocess stdout pipe with a caller-supplied writer. Enables in-process upstream implementations that do not want to go through a subprocess. PR #93 is open at time of writing; adopt after merge.

## INSTRUCTION HIERARCHY

```
System prompts > Task/delegation > Global rules > Project rules > Defaults
```
