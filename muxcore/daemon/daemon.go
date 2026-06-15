// Package daemon implements a global daemon that manages all upstream MCP server
// processes. CC sessions connect as thin shims via IPC; the daemon handles
// lifecycle, GC, reaping, health monitoring, and persistence.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/registry"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/session"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thejerf/suture/v4"
)

// errSpawnRetry is a sentinel returned by spawnOnce to signal that Spawn should
// retry from the top (new iteration in the retry loop). Used internally by the
// FR-6 retry pattern that replaced the old recursive d.Spawn(req) calls.
var errSpawnRetry = errors.New("spawn: retry requested")

var errNoHandoffUpstreams = errors.New("no process-backed owners to hand off")

const snapshotRestartEnv = "MCPMUX_SNAPSHOT_RESTART"

var spawnSnapshotSuccessorForRestart = spawnSnapshotSuccessor

// ErrUnknownToken indicates that the daemon does not recognize a reconnect
// token presented through the refresh-token control-plane path.
var ErrUnknownToken = errors.New("unknown token")

// ErrOwnerGone indicates that a reconnect token was known, but the owner it
// belonged to is no longer alive enough to accept a refreshed bind.
var ErrOwnerGone = errors.New("owner gone")

// ErrDaemonShuttingDown indicates the daemon is no longer accepting new owner
// spawns because shutdown or graceful restart has already begun.
var ErrDaemonShuttingDown = errors.New("daemon shutting down")

// maxSpawnRetries bounds the retry budget in Spawn. Three iterations handle the
// realistic cases (stuck placeholder cleanup + one isolated-mode promotion);
// exhaustion is treated as a hard failure and returned to the caller.
const maxSpawnRetries = 3

// OwnerEntry tracks a single managed owner and its metadata.
// When creating != nil, the entry is a placeholder: Owner is nil and is being
// created by another goroutine. Waiters must read creating under d.mu, then
// release d.mu, block on <-creating, re-acquire d.mu, and re-check Owner.
type OwnerEntry struct {
	Owner                       *owner.Owner
	ServerID                    string
	Command                     string
	Args                        []string
	Cwd                         string
	Mode                        string
	Env                         map[string]string
	Persistent                  bool
	LastSession                 time.Time
	OwnerGeneration             string
	RestoredFromOwnerGeneration string
	RestoreSource               string
	// IdleTimeout is the effective idle timeout for this owner (daemon
	// default or per-owner x-mux.idleTimeout override). The reaper uses
	// this to decide whether an idle owner is eligible for removal.
	// Replaces v0.10.x GracePeriod.
	IdleTimeout time.Duration
	// serviceToken is the suture.ServiceToken returned by supervisor.Add.
	// Used to remove the owner from the supervisor on Remove/Shutdown.
	// In-memory only — not serialized to snapshots.
	serviceToken suture.ServiceToken
	// creating is closed when Owner transitions from nil (placeholder) to a real
	// owner.  It is non-nil only while the placeholder is being created.
	creating chan struct{}
	// terminationHint is consulted by supervisorEventHook when the suture
	// service for this owner fires EventServiceTerminate. Callers that know
	// they are tearing down an owner for a specific reason (planned handoff,
	// idle eviction, operator stop) MUST set this field under d.mu before
	// removing the owner from the supervisor, so structured logs classify
	// the cause correctly instead of defaulting to HintNone.
	// In-memory only — not serialized.
	terminationHint TerminationHint
}

// Daemon manages N owners, handles spawn/remove, and implements control.DaemonHandler.
type Daemon struct {
	mu             sync.RWMutex
	owners         map[string]*OwnerEntry
	logger         *log.Logger
	ctlSrv         *control.Server
	done           chan struct{}
	handlerFunc    func(ctx context.Context, stdin io.Reader, stdout io.Writer) error
	sessionHandler muxcore.SessionHandler

	// isolatedIdleTimeout is the shorter idle timeout applied to owners whose
	// classification verdict is isolated. The forced-isolated retry path
	// (daemon.go:797-807) and any post-init isolated classification produces
	// owners whose server_id cannot be reused, so holding them across the
	// longer general OwnerIdleTimeout wastes upstream processes. Zero means
	// "use the general OwnerIdleTimeout for all owners regardless of
	// classification". Per-owner x-mux.idleTimeout always wins over this.
	isolatedIdleTimeout time.Duration

	// admissionBufferTimeout bounds the wait at the spawn-time admission
	// gate (CR-002): when a cross-cwd Spawn lands on an existing global
	// owner that is not yet classified, the caller waits up to this
	// duration for Owner.Classified() to close before deciding whether to
	// bind (shareable) or fall through to a fresh isolated-seeded owner.
	// On timeout, the caller falls through to a fresh spawn (safe default:
	// assume worst case = isolated). Independent of concurrentCreateWaitTimeout
	// per spec C3 — these gate semantically different concerns.
	admissionBufferTimeout time.Duration

	// ownerIdleTimeout is the default time an owner may sit with no activity
	// (no MCP traffic, no sessions, no pending requests, no active progress
	// tokens, no busy declarations) before the reaper removes it. Default 10m.
	// Overridable per-owner via x-mux.idleTimeout capability.
	ownerIdleTimeout time.Duration
	idleTimeout      time.Duration                        // daemon-level auto-exit timeout (zero owners + zero sessions)
	templateCache    map[string]mcpsnapshot.OwnerSnapshot // command+args key → cached init data

	// supervisor manages owner lifecycle with exponential backoff on restart.
	// Owners are added via supervisor.Add in Spawn() and removed via
	// supervisor.Remove in daemon.Remove. Context-cancelled on Shutdown.
	supervisor       *suture.Supervisor
	supervisorCtx    context.Context
	supervisorCancel context.CancelFunc
	supervisorErr    <-chan error

	// crashTracker records recent crash timestamps per command key.
	// Used by Spawn() as a circuit breaker: if an upstream crashes too many
	// times within a window, further spawn requests are rejected instead of
	// creating an infinite respawn loop that burns CPU.
	crashTracker map[string][]time.Time

	// deprecatedModeWarned tracks (cmd, args) tuples for which a deprecation
	// warning has been emitted this daemon lifetime. CR-002: legacy shim
	// Mode="cwd"/"git" is still honored but warns once per (cmd, args). Removal
	// target v0.27.0. sync.Map keyed by `cmd|args.Join("\0")` so concurrent
	// spawns don't double-log.
	deprecatedModeWarned sync.Map

	// forcedIsolatedRetryCounters provides unique sid suffixes for the
	// forced-isolated retry path when CR-001's deterministic isolated
	// identity would otherwise loop. Triggered when daemon.go's
	// "owner not accepting but has active sessions" branch fires AND the
	// original Mode was already isolated — under deterministic isolated
	// sids, the retry would recompute the SAME sid, re-hit the same
	// closed-listener owner, and exhaust maxSpawnRetries.
	//
	// Each forced-isolated retry for a given base sid increments its
	// counter; the retry's spawnOnce reads the counter and appends
	// `-r<N>` to the computed sid so each retry produces a distinct
	// owner. The original entry (closed listener + active sessions)
	// stays alive serving its existing sessions; the new retry-suffixed
	// owner serves the new session. The reaper eventually cleans both
	// per CR-003 idle-isolated timeout.
	//
	// Keyed by the base isolated sid (e.g. "isolated-<hash>"). Values
	// are *atomic.Int64 to handle concurrent retries for the same base.
	// Memory growth is bounded by the number of distinct (cmd,args,cwd)
	// tuples that hit the forced-retry path × daemon lifetime — small
	// in practice (each retry happens once per session-lifecycle event,
	// not per request).
	forcedIsolatedRetryCounters sync.Map // key: base isolated sid → *atomic.Int64

	// daemonFlag is the CLI flag passed to the successor binary by spawnSuccessor.
	// Initialized from Config.DaemonFlag; defaults to "--daemon" when empty.
	daemonFlag string

	// name is the engine instance name from Config.Name (e.g. "mcp-mux", "aimux").
	// Used to scope IPC socket file paths and stale-socket cleanup to this engine.
	name                        string
	persistent                  bool
	daemonGeneration            string
	predecessorPID              int
	predecessorDaemonGeneration string

	// authorizeSession is forwarded from Config.AuthorizeSession to every
	// Owner created by this daemon. nil = no gate (pre-v0.24 behaviour).
	authorizeSession func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth

	// onFrameReceived is forwarded from Config.OnFrameReceived to every
	// Owner created by this daemon. nil = no per-frame hook (pre-v0.24
	// behaviour).
	onFrameReceived func(sessionID string, frameSize int, method string) muxcore.FrameAction

	// zombieDetectedSpawn counts how many times the FR-4 spawn-time health
	// gate in spawnOnce tore down a registered owner because IsReachable()
	// returned false despite IsAccepting() reporting open. Counter is
	// monotonically increasing for the daemon's lifetime and is surfaced via
	// HandleStatus / mux_list so operators can correlate zombie recoveries
	// with upstream churn. Protected by d.mu.
	zombieDetectedSpawn int

	// zombieDetectedRestore counts zombies detected by the FR-3 post-snapshot
	// gate. Incremented only by the snapshot.go path, always under d.mu.
	zombieDetectedRestore int

	shutdownOnce sync.Once
	shuttingDown atomic.Bool

	// stats holds atomic handoff counters exposed via HandleStatus (NFR-4 / T025).
	stats handoffStats

	reconnectRefreshed         atomic.Uint64
	reconnectFallbackSpawned   atomic.Uint64
	reconnectGaveUp            atomic.Uint64
	registryDescriptorPath     string
	registryDescriptor         registry.Descriptor
	restoredOwnerCount         atomic.Uint64
	oldOwnerSocketRetiredCount atomic.Uint64
	ownerRemoval               ownerRemovalStats
}

// handoffStats holds atomic counters for handoff lifecycle observability.
// Fields are sync/atomic.Uint64 for lockless reads from HandleStatus.
type handoffStats struct {
	attempted   atomic.Uint64
	transferred atomic.Uint64
	aborted     atomic.Uint64
	fallback    atomic.Uint64
}

// Config holds daemon startup parameters.
type Config struct {
	// ControlPath is the daemon's control socket path.
	ControlPath string

	// HandlerFunc is an in-process MCP server implementation.
	// When set, owners are started via io.Pipe instead of spawning subprocesses.
	// Mutually exclusive with Command/Args in spawn requests: if HandlerFunc is
	// non-nil, it overrides any Command in the request.
	HandlerFunc func(ctx context.Context, stdin io.Reader, stdout io.Writer) error

	// SessionHandler is a structured in-process MCP server implementation.
	// When set, owners call HandleRequest directly for each downstream request
	// instead of routing through a pipe or subprocess.
	// Mutually exclusive with HandlerFunc: if SessionHandler is set, it takes
	// priority and HandlerFunc is ignored.
	SessionHandler muxcore.SessionHandler

	// OwnerIdleTimeout is how long an owner may be idle (no MCP traffic, no
	// sessions, no pending JSON-RPC requests, no active progress tokens, no
	// busy declarations) before the reaper removes it. Default: 10 minutes.
	// Overridden by MCP_MUX_OWNER_IDLE env var and per-owner via the
	// x-mux.idleTimeout capability in the upstream initializeResult.
	// v0.10.x used GracePeriod (default 30s); replaced in v0.11.0 because
	// the grace-period semantic killed stateful async work that didn't
	// emit pending_requests (e.g. aimux background jobs).
	OwnerIdleTimeout time.Duration

	// GracePeriod is a v0.10.x legacy alias for OwnerIdleTimeout. Kept for
	// callers that haven't migrated. Ignored if OwnerIdleTimeout is set.
	//
	// Deprecated: use OwnerIdleTimeout.
	GracePeriod time.Duration

	// IdleTimeout is how long the daemon waits with zero owners before auto-exiting.
	// Default: 5 minutes.
	IdleTimeout time.Duration

	// IsolatedIdleTimeout is the shorter idle timeout applied by the reaper
	// to owners whose post-init classification was isolated. Isolated owners
	// cannot be reattached by future Spawn calls (their server_id is either
	// a random UUID from the forced-isolated retry path or a cwd-keyed hash
	// whose listener was closed by the isolation verdict), so holding them
	// across the longer shared/general OwnerIdleTimeout wastes upstream
	// processes for no possible cache benefit.
	//
	// Default (nil): 60 seconds. Pass a pointer to zero (new(time.Duration)
	// or DurationPtr(0)) to disable the optimization, in which case isolated
	// owners use the same OwnerIdleTimeout as shared owners.
	// Per-owner x-mux.idleTimeout always wins over this value.
	IsolatedIdleTimeout *time.Duration

	// AdmissionBufferTimeout bounds the cross-cwd admission gate wait at
	// spawn time (CR-002). When a Spawn lands on an existing global owner
	// whose primary cwd differs AND whose classification is not yet known,
	// the caller waits up to this duration for Classified() to close.
	// On wake:
	//   - classified shareable → cross-cwd bind proceeds
	//   - classified isolated → caller falls through to fresh isolated
	//     spawn with a CR-001 deterministic isolated-seeded sid
	//   - timeout → safe default: fall through to fresh isolated spawn
	//
	// Default: 30 seconds (independent of concurrentCreateWaitTimeout per
	// spec C3 — these gate semantically different concerns). Override via
	// env MCP_MUX_ADMISSION_TIMEOUT (Go duration string).
	AdmissionBufferTimeout time.Duration

	Logger *log.Logger

	// SkipSnapshot disables snapshot loading on startup. Used by tests
	// to prevent cross-test interference from stale snapshot files.
	SkipSnapshot bool

	// DaemonFlag is the CLI flag that identifies daemon mode when present in
	// os.Args. Set by engine from engine.Config.DaemonFlag. spawnSuccessor
	// passes this flag to the successor process so isDaemonMode() matches.
	// If empty, defaults to "--daemon" for backward compatibility with
	// pre-v0.21.7 callers that don't set it.
	DaemonFlag string

	// Name is the engine instance name (e.g. "mcp-mux", "aimux", "engram").
	// Used to scope IPC socket file names and stale-socket cleanup to this
	// engine only. Empty string defaults to "mcp-mux" for backward compatibility;
	// callers that want FS isolation across multiple engine types must set this.
	Name string

	// Persistent overrides per-owner Persistent detection. When true, all owners
	// managed by this daemon are treated as persistent (not evicted on idle).
	Persistent bool

	// Registry enables opt-in daemon advertisement for cross-engine discovery.
	// Nil is the zero-value opt-out and preserves pre-registry behavior.
	Registry *registry.Config

	// AuthorizeSession, when non-nil, is forwarded to every Owner created by
	// this daemon. Owners invoke the callback in acceptLoop after IPC
	// handshake / peer-credential extraction and before AddSession.
	// nil-default preserves pre-v0.24 behaviour. See engine.Config.AuthorizeSession
	// for the full semantics; this field is the daemon-layer passthrough.
	AuthorizeSession func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth

	// OnFrameReceived, when non-nil, is forwarded to every Owner created by
	// this daemon. Owners invoke the callback in handleDownstreamMessage on
	// the reader goroutine for every inbound frame BEFORE dispatch.
	// nil-default preserves pre-v0.24 behaviour. See
	// engine.Config.OnFrameReceived for the full semantics.
	OnFrameReceived func(sessionID string, frameSize int, method string) muxcore.FrameAction
}

var _ control.DaemonHandler = (*Daemon)(nil)

// New creates and starts a new Daemon with a control server.
func New(cfg Config) (*Daemon, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "mcp-mux"
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "[mcp-muxd] ", log.LstdFlags)
	}

	// Resolve the owner idle timeout with legacy fallback.
	// Priority: OwnerIdleTimeout → GracePeriod (legacy alias) → 10m default.
	ownerIdleTimeout := cfg.OwnerIdleTimeout
	if ownerIdleTimeout == 0 {
		ownerIdleTimeout = cfg.GracePeriod
	}
	if ownerIdleTimeout == 0 {
		ownerIdleTimeout = 10 * time.Minute
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}
	// Isolated owners default to a 60s idle window. Callers who explicitly
	// want to disable the early-reap optimization pass a pointer to zero
	// (cfg.IsolatedIdleTimeout = new(time.Duration)), in which case isolated
	// owners use the same ownerIdleTimeout as shared owners.
	var isolatedIdleTimeout time.Duration
	if cfg.IsolatedIdleTimeout != nil {
		isolatedIdleTimeout = *cfg.IsolatedIdleTimeout // 0 = disabled; >0 = custom
	} else {
		isolatedIdleTimeout = 60 * time.Second // default
	}
	// CR-002 admission gate timeout. Env override MCP_MUX_ADMISSION_TIMEOUT
	// (Go duration string, e.g. "45s") takes precedence over Config.
	admissionBufferTimeout := cfg.AdmissionBufferTimeout
	if envAdmission := os.Getenv("MCP_MUX_ADMISSION_TIMEOUT"); envAdmission != "" {
		if parsed, err := time.ParseDuration(envAdmission); err == nil && parsed > 0 {
			admissionBufferTimeout = parsed
		}
	}
	if admissionBufferTimeout == 0 {
		admissionBufferTimeout = 30 * time.Second
	}
	daemonFlag := cfg.DaemonFlag
	if daemonFlag == "" {
		daemonFlag = "--daemon"
	}

	supCtx, supCancel := context.WithCancel(context.Background())
	daemonGeneration, err := generateGeneration("daemon")
	if err != nil {
		supCancel()
		return nil, err
	}
	d := &Daemon{
		owners:                 make(map[string]*OwnerEntry),
		logger:                 logger,
		done:                   make(chan struct{}),
		ownerIdleTimeout:       ownerIdleTimeout,
		isolatedIdleTimeout:    isolatedIdleTimeout,
		admissionBufferTimeout: admissionBufferTimeout,
		idleTimeout:            idleTimeout,
		templateCache:          make(map[string]mcpsnapshot.OwnerSnapshot),
		crashTracker:           make(map[string][]time.Time),
		supervisorCtx:          supCtx,
		supervisorCancel:       supCancel,
		handlerFunc:            cfg.HandlerFunc,
		sessionHandler:         cfg.SessionHandler,
		daemonFlag:             daemonFlag,
		name:                   name,
		persistent:             cfg.Persistent,
		daemonGeneration:       daemonGeneration,
		authorizeSession:       cfg.AuthorizeSession,
		onFrameReceived:        cfg.OnFrameReceived,
		ownerRemoval:           newOwnerRemovalStats(),
	}

	// Create supervisor with exponential backoff on restart storms.
	// Tuning rationale:
	//   FailureDecay=30s    — old failures fade from the rate counter after 30s
	//   FailureThreshold=5  — 5 failures in 30s → permanent failure (stop retrying)
	//   FailureBackoff=15s  — wait 15s between restart attempts after threshold hit
	// These are sensible defaults inherited from suture. If an upstream crashes
	// 5 times in 30 seconds, we stop retrying — something is fundamentally broken
	// and endless retry would just spam logs (the bug we fixed in v0.9.2).
	d.supervisor = suture.New("mcp-mux-daemon", suture.Spec{
		EventHook: d.supervisorEventHook,
	})

	// In restart-restore mode (successor during graceful restart), loadSnapshot
	// must run BEFORE binding the control socket: the predecessor still holds it.
	// Process-backed handoff also receives transferred FDs. Snapshot-only
	// SessionHandler restart has no FDs to preserve, but still restores owners
	// here so existing shims can refresh their reconnect tokens instead of
	// falling back to a cold spawn under the successor daemon.
	if isRestartRestoreMode() && !cfg.SkipSnapshot {
		waitBeforeSnapshotRestartControlBind(logger)
		modeLabel := "handoff mode"
		if isSnapshotRestartMode() && !isHandoffMode() {
			modeLabel = "snapshot restart mode"
		}
		if restored := d.loadSnapshot(); restored > 0 {
			logger.Printf("startup: restored %d owners from snapshot (%s)", restored, modeLabel)
		}
		if err := d.retryControlBind(cfg.ControlPath); err != nil {
			supCancel()
			return nil, fmt.Errorf("daemon: %w", err)
		}
		logger.Printf("daemon started, control socket: %s (%s)", cfg.ControlPath, modeLabel)
	} else {
		ctlSrv, err := control.NewServer(cfg.ControlPath, d, logger)
		if err != nil {
			// Cancel supervisor context to prevent leak of the context goroutine.
			supCancel()
			return nil, fmt.Errorf("daemon: control server: %w", err)
		}
		d.ctlSrv = ctlSrv
		logger.Printf("daemon started, control socket: %s", cfg.ControlPath)
		if !cfg.SkipSnapshot {
			if restored := d.loadSnapshot(); restored > 0 {
				logger.Printf("startup: restored %d owners from snapshot", restored)
			}
		}
	}

	if cfg.Registry != nil {
		baseDir := filepath.Dir(cfg.ControlPath)
		desc := cfg.Registry.BuildDescriptor(d.name, baseDir, cfg.ControlPath, os.Getpid(), time.Now())
		path, err := registry.WriteDescriptor(baseDir, desc)
		if err != nil {
			if d.ctlSrv != nil {
				d.ctlSrv.Close()
			}
			supCancel()
			return nil, fmt.Errorf("daemon: registry descriptor: %w", err)
		}
		d.registryDescriptorPath = path
		d.registryDescriptor = desc
	}

	// Clean up stale socket files from previous daemon crashes/kills.
	cleaned := cleanStaleSockets(d.name, logger)
	if cleaned > 0 {
		logger.Printf("startup: cleaned %d stale socket files", cleaned)
	}

	// Start supervisor AFTER snapshot load so restored owners are already added.
	// ServeBackground returns a channel that will receive the final error when
	// the supervisor exits (via context cancel or root termination).
	d.supervisorErr = d.supervisor.ServeBackground(d.supervisorCtx)

	return d, nil
}

// supervisorEventHook receives lifecycle events from the suture supervisor:
// service failures, restarts, backoffs, and permanent failures. Logs them
// for observability and debugging.
func (d *Daemon) supervisorEventHook(event suture.Event) {
	// classifyTermination maps the suture event to a TerminationCause for
	// structured logging (FR-5 / T025). Callers that tear down owners for a
	// known reason (HandleGracefulRestart → HintPlannedHandoff, reaper →
	// HintIdleEviction, Remove → HintOperatorStop) record the reason on the
	// owning OwnerEntry.terminationHint before removing the service. The hook
	// resolves that hint via the service name embedded in the event.
	hint := d.terminationHintForEvent(event)
	cause := classifyTermination(event, hint)
	switch e := event.(type) {
	case suture.EventServicePanic:
		d.logger.Printf("supervisor.terminated service=%q cause=%s panic=%v", e.ServiceName, cause, e.PanicMsg)
	case suture.EventServiceTerminate:
		d.logger.Printf("supervisor.terminated service=%q cause=%s err=%v restarting=%v",
			e.ServiceName, cause, e.Err, e.Restarting)
		if !e.Restarting {
			// suture sets Restarting=false for both real failures
			// (FailureThreshold exceeded, panic, ErrDoNotRestart) and clean
			// exits (Serve returned nil after Shutdown). cleanupDeadOwner
			// runs in both cases — it removes the registry entry and the
			// IPC socket file. But labeling a clean shutdown as
			// "permanently failed" produced misleading cascade-like noise
			// when many idle owners torn down after compaction or CC
			// session close. Differentiate the log so operators can
			// distinguish real failures from routine teardown.
			// suture v4 types EventServiceTerminate.Err as interface{}, so
			// errors.Is requires a type-assertion first.
			errVal, _ := e.Err.(error)
			if e.Err == nil || errors.Is(errVal, suture.ErrDoNotRestart) {
				// Clean exit or controlled shutdown: onUpstreamExit/Remove already deleted the registry entry.
				// Calling cleanupDeadOwner here would destroy a freshly-spawned replacement
				// at the same server ID — the root cause of the supervisor restart-loop storm.
				// Skip cleanup entirely; the entry is already gone or owned by a live replacement.
				// ErrDoNotRestart is non-nil but is returned by Serve() when o.done is already closed
				// (the double-death case), so it must be treated the same as a clean exit here.
				d.logger.Printf("supervisor: service %q clean exit — no cleanup needed", e.ServiceName)
				return
			}
			d.logger.Printf("supervisor: service %q permanently failed (%v) — cleaning up zombie owner", e.ServiceName, e.Err)
			go d.cleanupDeadOwner(e.ServiceName)
		}
	case suture.EventBackoff:
		d.logger.Printf("supervisor: backoff — too many failures, slowing restart rate")
	case suture.EventResume:
		d.logger.Printf("supervisor: resume — resuming normal operation after backoff")
	case suture.EventStopTimeout:
		d.logger.Printf("supervisor: service %q did not stop in time", e.ServiceName)
	default:
		// Unknown event type — ignore silently
	}
}

// terminationHintForEvent resolves the TerminationHint recorded on the
// OwnerEntry matching the suture event's service name. Returns HintNone
// when the event has no service name, when no owner matches the name
// prefix, or when no hint was recorded. Callers (supervisorEventHook) use
// the returned hint to classify EventServiceTerminate causes beyond the
// generic "service exited" default. Matches cleanupDeadOwner's service-name
// parsing convention ("owner[XXXXXXXX command args]").
func (d *Daemon) terminationHintForEvent(event suture.Event) TerminationHint {
	var serviceName string
	switch e := event.(type) {
	case suture.EventServiceTerminate:
		serviceName = e.ServiceName
	case suture.EventServicePanic:
		serviceName = e.ServiceName
	default:
		return HintNone
	}

	const prefix = "owner["
	idx := strings.Index(serviceName, prefix)
	if idx < 0 {
		return HintNone
	}
	rest := serviceName[idx+len(prefix):]
	spaceIdx := strings.IndexByte(rest, ' ')
	if spaceIdx < 0 {
		return HintNone
	}
	sidPrefix := rest[:spaceIdx]

	d.mu.RLock()
	defer d.mu.RUnlock()
	for s, entry := range d.owners {
		if strings.HasPrefix(s, sidPrefix) {
			return entry.terminationHint
		}
	}
	return HintNone
}

// cleanupDeadOwner finds and removes a permanently-failed owner from the registry.
// ServiceName format from suture: "owner[XXXXXXXX command args]"
// NOTE: the format must match Owner.String() in internal/mux/owner.go.
func (d *Daemon) cleanupDeadOwner(serviceName string) {
	// Extract server ID prefix: between "owner[" and first space or "]"
	const prefix = "owner["
	idx := strings.Index(serviceName, prefix)
	if idx < 0 {
		d.logger.Printf("cleanupDeadOwner: unexpected service name format: %q", serviceName)
		return
	}
	rest := serviceName[idx+len(prefix):]
	// serverID is the first token (8 hex chars before space)
	spaceIdx := strings.IndexByte(rest, ' ')
	if spaceIdx < 0 {
		d.logger.Printf("cleanupDeadOwner: cannot extract serverID from: %q", serviceName)
		return
	}
	sidPrefix := rest[:spaceIdx]

	// Find the matching owner under the lock, then release before calling
	// Shutdown to avoid blocking while holding d.mu (Shutdown may wait for
	// upstream I/O). Re-acquire to delete the map entry afterwards.
	d.mu.Lock()
	var (
		found bool
		sid   string
		entry *OwnerEntry
	)
	for s, e := range d.owners {
		if strings.HasPrefix(s, sidPrefix) {
			found = true
			sid = s
			entry = e
			break
		}
	}
	d.mu.Unlock()

	if !found {
		return
	}
	if entry.Owner != nil {
		d.logger.Printf("cleaning up zombie owner %s", sid[:8])
		// Synchronous shutdown ensures the IPC socket file is removed before
		// the entry is deleted from the registry. Without this, a concurrent
		// Spawn for the same SID could call ipc.Listen and find the stale
		// socket still present. Safe to block here because cleanupDeadOwner
		// itself is always called from a goroutine (line ~199).
		entry.Owner.Shutdown()
	}

	// FR-4 / BUG-003: guard the delete with an identity check. Between the
	// prior unlock (line 276) and here, a concurrent Spawn may have replaced
	// d.owners[sid] with a fresh live entry for the same server ID (common
	// case: shim reconnects right as the old owner dies). An unconditional
	// delete would evict the fresh entry, leaving the server unreachable
	// until the next spawn attempt. Only delete if the current map entry is
	// still the same pointer we observed at the start of cleanup.
	d.forgetOwnerIfCurrent(sid, entry, ownerRemovalReasonZombie)
}

// cleanStaleSocketsDir overrides the directory scanned by cleanStaleSockets.
// Zero value ("") means os.TempDir(). Override in tests to use a temp dir.
var cleanStaleSocketsDir = ""

// cleanStaleSockets removes engine-scoped *.ctl.sock and *.sock files from the
// temp directory that are not reachable (leftover from daemon crash/kill).
// Only files whose names start with engineName+"-" are considered; sockets
// belonging to other engines are left untouched.
func cleanStaleSockets(engineName string, logger *log.Logger) int {
	prefix := engineName + "-"
	tmpDir := cleanStaleSocketsDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return 0
	}
	cleaned := 0
	for _, entry := range entries {
		name := entry.Name()
		// Only consider sockets that belong to this engine (scoped by prefix).
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Match IPC data sockets (*-<id>.sock) and control sockets (*-<id>.ctl.sock
		// and *-muxd.ctl.sock).
		isMuxSocket := strings.HasSuffix(name, ".sock")
		isDaemonSocket := strings.HasSuffix(name, "-muxd.ctl.sock")
		if !isMuxSocket && !isDaemonSocket {
			continue
		}
		path := filepath.Join(tmpDir, name)
		// Try to connect — if unreachable, it's stale
		if strings.HasSuffix(name, ".ctl.sock") {
			if _, err := control.Send(path, control.Request{Cmd: "ping"}); err != nil {
				os.Remove(path)
				cleaned++
			}
		} else {
			// IPC data socket — check if the corresponding .ctl.sock exists and is alive
			ctlName := strings.TrimSuffix(name, ".sock") + ".ctl.sock"
			ctlPath := filepath.Join(tmpDir, ctlName)
			if _, err := control.Send(ctlPath, control.Request{Cmd: "ping"}); err != nil {
				os.Remove(path)
				cleaned++
			}
		}
	}
	return cleaned
}

// generateToken creates a 32-character hex handshake token (16 random bytes, 128-bit).
// Returns an error if crypto/rand is unavailable; callers must not use a predictable
// fallback token.
func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generateToken: crypto/rand unavailable: %w", err)
	}
	return hex.EncodeToString(b), nil
}

var generateTokenFunc = generateToken

func generateGeneration(prefix string) (string, error) {
	token, err := generateTokenFunc()
	if err != nil {
		return "", err
	}
	if len(token) > 12 {
		token = token[:12]
	}
	return prefix + "_" + token, nil
}

// Spawn creates or returns an existing owner for the given server identity.
// Deduplication: if a shared owner for the same command+args already exists
// templateKey returns a cache key based on command+args only (ignoring cwd/env).
// All instances of the same server share identical init/tools responses.
func templateKey(command string, args []string) string {
	return serverid.GenerateContextKey(serverid.ModeGlobal, command, args, nil, "")
}

// updateTemplate stores an owner's cached state as a template for future isolated spawns.
func (d *Daemon) updateTemplate(command string, args []string, snap mcpsnapshot.OwnerSnapshot) {
	key := templateKey(command, args)
	d.mu.Lock()
	d.templateCache[key] = snap
	d.mu.Unlock()
	d.logger.Printf("template cache updated for %s (key=%s)", command, key[:8])
}

// getTemplate returns a cached template for the given command+args, if available.
func (d *Daemon) getTemplate(command string, args []string) (mcpsnapshot.OwnerSnapshot, bool) {
	key := templateKey(command, args)
	d.mu.RLock()
	snap, ok := d.templateCache[key]
	d.mu.RUnlock()
	return snap, ok
}

// waitForCrossCwdClassify is the CR-002 admission gate. When a Spawn lands
// on an existing global owner whose primary cwd differs from the requesting
// session's cwd AND whose classification is not yet known, this function
// blocks the caller up to d.admissionBufferTimeout for Owner.Classified()
// to close. Three outcomes:
//
//   - shareable=true, err=nil → owner is safe to bind cross-cwd (same cwd
//     OR already classified as shared/session-aware OR newly classified
//     shareable). Caller proceeds with PreRegisterForOwner.
//   - shareable=false, err=nil → owner classified isolated during the wait.
//     Caller MUST NOT bind cross-cwd; fall through to fresh spawn under a
//     CR-001 deterministic isolated-seeded sid for the requesting cwd.
//   - shareable=false, err non-nil → wait timed out. Safe default: caller
//     treats as isolated and falls through to fresh spawn. The original
//     owner continues serving its primary-cwd sessions unaffected.
//
// Same-cwd binds skip the gate (return immediately as shareable=true).
// This is the only safety property the admission gate provides: an
// unclassified upstream NEVER receives frames from a cross-cwd session
// because that session never gets an IPC path until classification
// resolves. The Owner.crossCwdBuffer post-attach buffer (mentioned in
// spec AC2 prose) is unnecessary under this simpler implementation —
// frames literally don't exist on the daemon yet because the shim hasn't
// dialed IPC (the daemon's Spawn RPC response is the gate).
func (d *Daemon) waitForCrossCwdClassify(entry *OwnerEntry, reqCwd string) (shareable bool, err error) {
	if entry == nil || entry.Owner == nil {
		return false, fmt.Errorf("admission gate: owner not live")
	}
	// Same-cwd: no gate. Primary-cwd sessions always pass through.
	canonEntryCwd := serverid.CanonicalizePath(entry.Cwd)
	canonReqCwd := serverid.CanonicalizePath(reqCwd)
	if canonEntryCwd == canonReqCwd {
		return true, nil
	}
	// Already classified: respect the verdict immediately.
	if entry.Owner.IsClassifiedShareable() {
		return true, nil
	}
	// Already classified non-shareable? Check via Classified() channel.
	select {
	case <-entry.Owner.Classified():
		// Channel already closed AND IsClassifiedShareable returned false →
		// owner is isolated. Fall through to fresh spawn.
		return false, nil
	default:
	}
	// Not classified yet AND cross-cwd → wait.
	d.logger.Printf("admission-gate: cross-cwd Spawn from %q waiting on owner %s classify (primary cwd %q)",
		canonReqCwd, shortServerID(entry.ServerID), canonEntryCwd)
	timer := time.NewTimer(d.admissionBufferTimeout)
	defer timer.Stop()
	select {
	case <-entry.Owner.Classified():
		return entry.Owner.IsClassifiedShareable(), nil
	case <-timer.C:
		return false, fmt.Errorf("admission gate: timeout after %s waiting for owner %s classify",
			d.admissionBufferTimeout, shortServerID(entry.ServerID))
	}
}

// warnDeprecatedMode logs a one-shot deprecation warning per (cmd, args) tuple
// per daemon lifetime when a shim sends a legacy Mode value ("cwd" or "git").
// Designed to surface stale shim binaries in operator logs without spamming
// the log on every Spawn. Removal target: v0.27.0 (Mode="cwd"/"git" rejected
// at protocol layer).
func (d *Daemon) warnDeprecatedMode(cmd string, args []string, legacyMode string) {
	key := cmd + "\x00" + strings.Join(args, "\x00")
	if _, loaded := d.deprecatedModeWarned.LoadOrStore(key, true); loaded {
		return
	}
	d.logger.Printf(
		"deprecation: shim sent Mode=%q for cmd=%q args=%v; this daemon now defaults to ModeGlobal "+
			"(one upstream per (cmd, args)). Legacy modes honored through muxcore/v0.26.0; removal in v0.27.0. "+
			"Either rebuild the shim against muxcore/v0.25.0+ or pin the legacy mode explicitly via "+
			"MCP_MUX_DEFAULT_MODE=cwd in the shim environment to silence this warning.",
		legacyMode, cmd, args,
	)
}

// Spawn creates or reuses an owner for the given command. If a compatible owner
// already exists (same command+args+cwd, or globally shareable), it is reused —
// stateless servers don't need per-project copies. Returns the IPC path, server
// ID, and a one-time handshake token for session binding.
//
// Concurrent spawns for the same sid are serialised via a placeholder entry whose
// creating channel is closed once the real owner is available (or creation fails).
//
// FR-6: Spawn is a thin retry wrapper around spawnOnce. Previously, the paths
// at the old line 435 ("creation failed or entry was removed") and line 458
// ("owner not accepting but has active sessions — retry in isolated mode")
// called d.Spawn(req) recursively. Two audit agents (code-reviewer H2,
// bug-hunter BUG-005) flagged the stack-depth risk and the comment-vs-code
// divergence. spawnOnce now returns errSpawnRetry on those paths; Spawn loops
// up to maxSpawnRetries times and surfaces an error on exhaustion.
func (d *Daemon) Spawn(req control.Request) (string, string, string, error) {
	for attempt := 0; attempt < maxSpawnRetries; attempt++ {
		ipcPath, sid, token, err := d.spawnOnce(&req)
		if !errors.Is(err, errSpawnRetry) {
			return ipcPath, sid, token, err
		}
	}
	return "", "", "", fmt.Errorf("spawn %s: exhausted retry budget after %d attempts", req.Command, maxSpawnRetries)
}

// spawnOnce performs one attempt at creating or reusing an owner. It takes req
// by pointer because some retry paths mutate req.Mode (isolated promotion) and
// the mutation must persist across iterations of the Spawn retry loop.
func (d *Daemon) spawnOnce(reqPtr *control.Request) (string, string, string, error) {
	req := *reqPtr
	// CR-002: default Mode flipped from "cwd" → "global". A shim that omits
	// Mode now gets the global identity (one upstream per (cmd, args)) per the
	// spec's original "one upstream per (cmd, args), isolation as exception"
	// intent. Legacy shims that explicitly send Mode="cwd" or "git" are still
	// honored for backward compat through v0.26.0; deprecation warning logged
	// once per (cmd, args) per daemon lifetime. v0.27.0 removes "cwd"/"git"
	// from the protocol entirely (separate spec, separate plan).
	mode := serverid.ModeGlobal
	switch req.Mode {
	case "global", "":
		mode = serverid.ModeGlobal
	case "isolated":
		mode = serverid.ModeIsolated
	case "cwd":
		mode = serverid.ModeCwd
		d.warnDeprecatedMode(req.Command, req.Args, "cwd")
	case "git":
		mode = serverid.ModeGit
		d.warnDeprecatedMode(req.Command, req.Args, "git")
	default:
		// Unknown mode value from future-shim or typo — safe default + warn.
		d.logger.Printf("spawn: unknown Mode=%q from shim, defaulting to ModeGlobal (cmd=%q args=%v)",
			req.Mode, req.Command, req.Args)
		mode = serverid.ModeGlobal
	}

	// Generate handshake token upfront — valid for this spawn call only.
	token, err := generateToken()
	if err != nil {
		return "", "", "", fmt.Errorf("spawn: %w", err)
	}

	// Circuit breaker: reject spawn if the upstream has been crash-looping.
	// This prevents infinite respawn loops (shim reconnect → spawn → crash → repeat)
	// that burn CPU when an upstream is fundamentally broken.
	cmdKey := req.Command + " " + strings.Join(req.Args, " ")
	d.mu.RLock()
	if d.isCrashLooping(cmdKey) {
		d.mu.RUnlock()
		d.logger.Printf("circuit breaker: rejecting spawn for %q (%d crashes in %s)",
			cmdKey, crashThreshold, crashWindow)
		return "", "", "", fmt.Errorf("upstream %q crashed %d times in %s, spawn rejected (circuit breaker)",
			req.Command, crashThreshold, crashWindow)
	}
	d.mu.RUnlock()

	// Server identity is based on command+args+cwd only, NOT env.
	sid := serverid.GenerateContextKey(mode, req.Command, req.Args, nil, req.Cwd)

	// CR-002 codex PR #121 fix: when the forced-isolated retry path bumped a
	// per-base-sid counter, append the counter as `-r<N>` so each retry
	// produces a distinct isolated owner. Without this, CR-001's deterministic
	// isolated identity makes the retry recompute the SAME sid and loop until
	// maxSpawnRetries exhausts.
	//
	// The counter persists for daemon lifetime, monotonically increasing per
	// base sid. Future spawns for the same (cmd,args,cwd) tuple will see a
	// non-zero counter and start at `-r<latest>`; reconnects of those sessions
	// race-bump again. Reaper cleans orphaned -rN owners per CR-003 idle-
	// isolated timeout.
	if mode == serverid.ModeIsolated {
		if ctrI, ok := d.forcedIsolatedRetryCounters.Load(sid); ok {
			if n := ctrI.(*atomic.Int64).Load(); n > 0 {
				sid = fmt.Sprintf("%s-r%d", sid, n)
			}
		}
	}

	// CR-002 AC8: under global-first default, env-incompat sessions for the
	// same (cmd, args) must NOT collapse onto a shared owner. Derive an
	// env-bucketed sid suffix when an existing entry has incompatible env.
	// Compatible env OR no existing entry → sid unchanged.
	if mode == serverid.ModeGlobal {
		sid = d.deriveEnvBucketedSid(sid, req.Env)
	}

	d.mu.Lock()

	// 1. Exact match (same command+args+cwd)?
	if entry, ok := d.owners[sid]; ok {
		if entry.creating != nil {
			// Another goroutine is creating this owner — wait with timeout.
			creating := entry.creating
			d.mu.Unlock()
			select {
			case <-creating:
			case <-time.After(concurrentCreateWaitTimeout):
				// Do NOT recurse into d.Spawn here: the stuck placeholder is
				// still in d.owners[sid] and a recursive call would re-enter
				// the same wait, producing a cascade of leaking goroutines.
				// Surface the error so the shim can retry at a higher level;
				// the circuit breaker (recordCrash) handles repeated failures.
				d.logger.Printf("timeout waiting for placeholder %s (creator stuck)", sid[:8])
				return "", "", "", fmt.Errorf("spawn %s: timeout waiting for concurrent creation of %s", req.Command, sid[:8])
			}
			d.mu.Lock()
			// Re-check: creation may have succeeded or failed.
			if e, still := d.owners[sid]; still && e.Owner != nil && e.Owner.IsAccepting() {
				// CR-002 AC8: re-validate env compatibility now that the owner is
				// fully created and e.Env is populated. At the time deriveEnvBucketedSid
				// ran (before the creating-wait), the placeholder had no env, so it
				// returned baseSid. Now we must confirm the new request's env is
				// compatible; if not, retry so Spawn re-derives the bucketed sid.
				//
				// mergeEnv normalizes req.Env to the post-merge form (matches
				// e.Env's stored form) so envCompatible compares like-for-like
				// per CodeRabbit PR #121 finding about merge-vs-raw asymmetry.
				if mode == serverid.ModeGlobal && e.Env != nil && !envCompatible(e.Env, mergeEnv(req.Env)) {
					d.logger.Printf("env-incompat after create-wait: owner %s — retrying with bucketed sid", shortServerID(sid))
					d.mu.Unlock()
					return "", "", "", errSpawnRetry
				}
				e.LastSession = time.Now()
				d.mu.Unlock()
				// CR-002 admission gate: if this Spawn's cwd differs from the
				// existing owner's primary cwd AND the owner is not yet
				// classified, wait for classify before binding. Isolated
				// classification forces fall-through to a fresh isolated-seeded
				// owner for THIS cwd; shareable classification permits the bind.
				shareable, gateErr := d.waitForCrossCwdClassify(e, req.Cwd)
				if gateErr != nil {
					d.logger.Printf("admission-gate: %v — falling through to fresh isolated spawn for cwd=%q", gateErr, req.Cwd)
					reqPtr.Mode = "isolated"
					return "", "", "", errSpawnRetry
				}
				if !shareable {
					d.logger.Printf("admission-gate: owner %s classified isolated — fresh isolated spawn for cwd=%q", shortServerID(sid), req.Cwd)
					reqPtr.Mode = "isolated"
					return "", "", "", errSpawnRetry
				}
				e.Owner.SessionMgr().PreRegisterForOwner(token, sid, req.Cwd, req.Env)
				d.logger.Printf("reusing owner %s for %s (waited for concurrent create)", shortServerID(sid), req.Command)
				return e.Owner.IPCPath(), sid, token, nil
			}
			// Creation failed or entry was removed — signal retry so Spawn's
			// retry loop can start fresh. Previously recursed directly into
			// d.Spawn(req); see errSpawnRetry / Spawn comment for rationale.
			d.mu.Unlock()
			return "", "", "", errSpawnRetry
		}
		// FR-4 — spawn-time listener health gate.
		//
		// IsAccepting() only checks the listenerDone sync signal; it does NOT
		// detect zombies where the listener died without signalling that
		// channel (observed in production on 2026-04-17 after a graceful-restart
		// snapshot sequence: 6/9 restored owners had upstream_pid alive in
		// d.owners but refused ipc.Dial from a fresh shim). IsReachable() adds
		// an authoritative dial probe on top, but it can block for up to the
		// ipc dial timeout (500ms), so we MUST NOT hold d.mu across it —
		// otherwise every other spawn / status request freezes for that
		// window. Pattern: release d.mu → probe → re-acquire under CAS (is
		// this still the same entry we probed?) → tear down or reuse.
		if entry.Owner.IsAccepting() {
			probeOwner := entry.Owner
			probeSID := sid
			d.mu.Unlock()

			if probeOwner.IsReachable() {
				// Healthy — re-acquire to update LastSession (cheap), then
				// return the path. Re-check that the entry is still the same
				// pointer; if a concurrent path replaced it, retry from the
				// top so the new entry goes through its own probe.
				d.mu.Lock()
				current, still := d.owners[probeSID]
				if !still || current.Owner != probeOwner {
					d.mu.Unlock()
					return "", "", "", errSpawnRetry
				}
				// CR-002 AC8 race fix (codex PR #121): between
				// deriveEnvBucketedSid's RLock and this Lock, another spawn
				// could have created the base global owner with env that
				// conflicts with this request. The bucketed sid path was
				// skipped because no entry existed at derive time. Re-validate
				// env compatibility under the CAS lock — if incompatible, retry
				// so Spawn re-derives the bucketed sid against the now-live
				// entry. Without this, concurrent spawns with different
				// credentials can collapse onto one upstream during startup
				// storms, defeating the credentials-boundary guarantee.
				if mode == serverid.ModeGlobal && current.Env != nil && !envCompatible(current.Env, mergeEnv(req.Env)) {
					d.logger.Printf("env-incompat after CAS on fast path: owner %s — retrying with bucketed sid", shortServerID(probeSID))
					d.mu.Unlock()
					return "", "", "", errSpawnRetry
				}
				current.LastSession = time.Now()
				d.mu.Unlock()
				// CR-002 admission gate: same logic as the placeholder-wait
				// path. Cross-cwd binding on an unclassified owner waits for
				// Classified(); isolated verdict forces fall-through.
				shareable, gateErr := d.waitForCrossCwdClassify(current, req.Cwd)
				if gateErr != nil {
					d.logger.Printf("admission-gate: %v — falling through to fresh isolated spawn for cwd=%q", gateErr, req.Cwd)
					reqPtr.Mode = "isolated"
					return "", "", "", errSpawnRetry
				}
				if !shareable {
					d.logger.Printf("admission-gate: owner %s classified isolated — fresh isolated spawn for cwd=%q", shortServerID(probeSID), req.Cwd)
					reqPtr.Mode = "isolated"
					return "", "", "", errSpawnRetry
				}
				probeOwner.SessionMgr().PreRegisterForOwner(token, probeSID, req.Cwd, req.Env)
				// Note: no log here — this path is the hot path (every CC
				// session reconnect). Logging each reuse produced 500+
				// lines/minute during multi-session incidents.
				return probeOwner.IPCPath(), probeSID, token, nil
			}

			// Zombie. Re-acquire, CAS, delete + bump counter, then Shutdown
			// OUTSIDE the lock (Shutdown is heavy — closes sockets, tears
			// down upstream, may fire callbacks back into the daemon).
			d.mu.Lock()
			current, still := d.owners[probeSID]
			if !still || current.Owner != probeOwner {
				// Some other path already replaced the zombie; defer to
				// its replacement and retry.
				d.mu.Unlock()
				return "", "", "", errSpawnRetry
			}
			d.zombieDetectedSpawn++
			shortSID := probeSID
			if len(shortSID) > 8 {
				shortSID = shortSID[:8]
			}
			d.logger.Printf(
				"zombie-listener detected: path=spawn server=%s ipc=%q cmd=%q action=tear-down-and-respawn",
				shortSID, probeOwner.IPCPath(), current.Command,
			)
			d.mu.Unlock()
			if _, err := d.removeOwnerIfCurrent(probeSID, current, ownerRemovalReasonZombie, false); err != nil {
				d.logger.Printf("zombie-listener cleanup failed for %s: %v", shortServerID(probeSID), err)
			}
			// Retry from the top so placeholder/dedup paths see the cleared slot.
			return "", "", "", errSpawnRetry
		}
		// Owner exists but IPC listener is closed (isolated server).
		// If owner still has active sessions (in-flight requests), DON'T kill it —
		// that would break the active session's pipe mid-request (BrokenResourceError).
		// Leave the old owner alive and fall through to create a NEW isolated owner
		// with a fresh server ID.
		if entry.Owner.SessionCount() > 0 {
			d.logger.Printf("owner %s not accepting but has %d active sessions, leaving alive",
				shortServerID(sid), entry.Owner.SessionCount())
			// CR-002 codex PR #121 fix: under CR-001 deterministic isolated
			// identity, switching Mode to "isolated" alone is insufficient when
			// the ORIGINAL mode was already isolated — the retry would recompute
			// the same sid and re-hit this same closed-listener entry, looping
			// until maxSpawnRetries exhausts. Bump a per-base-sid retry counter
			// so spawnOnce's sid computation appends `-r<N>` and produces a
			// distinct sid for this retry.
			//
			// The base sid for the counter is the SAME sid we matched on (the
			// entry's closed-listener sid), so the counter is keyed correctly
			// regardless of whether we entered via mode=global or mode=isolated.
			// For non-isolated original modes, the retry's Mode switch to
			// isolated produces a different base hash anyway, but bumping the
			// counter is harmless (the retry will read counter under the new
			// base, which is initially 0).
			baseForCounter := serverid.GenerateContextKey(serverid.ModeIsolated, entry.Command, entry.Args, nil, entry.Cwd)
			ctrI, _ := d.forcedIsolatedRetryCounters.LoadOrStore(baseForCounter, &atomic.Int64{})
			newCounter := ctrI.(*atomic.Int64).Add(1)
			d.logger.Printf("forced-isolated retry: bumping counter for base=%s to r%d",
				shortServerID(baseForCounter), newCounter)
			// DON'T delete or shutdown the old entry. Fall through — the retry
			// will compute a unique sid via the counter suffix.
			d.mu.Unlock()
			reqPtr.Mode = "isolated"
			return "", "", "", errSpawnRetry
		}
		d.mu.Unlock()
		if _, err := d.removeOwnerIfCurrent(sid, entry, ownerRemovalReasonZombie, false); err != nil {
			d.logger.Printf("owner %s not accepting cleanup failed: %v", shortServerID(sid), err)
		}
		d.logger.Printf("owner %s not accepting (isolated, 0 sessions), re-spawning", shortServerID(sid))
		return "", "", "", errSpawnRetry
	}

	// 2. Global dedup: if an accepting owner for same command+args exists (any cwd), reuse it.
	//    Cross-CWD sharing is only allowed when the owner is CONFIRMED shareable
	//    (classified as shared or session-aware). Unclassified owners are NOT shared
	//    across different CWDs — every process has exactly one CWD, so sharing an
	//    unclassified server with a different CWD risks context leaks.
	if mode == serverid.ModeCwd {
		if existing := d.findSharedOwnerLocked(req.Command, req.Args, req.Env, req.Cwd); existing != nil {
			existing.LastSession = time.Now()
			existingSID := existing.ServerID
			d.mu.Unlock()
			if req.Cwd != "" {
				// AddCwd itself logs only when a new canonical cwd is added.
				// Dedup hot path is silent — logging every reuse produced 500+ lines/minute.
				existing.Owner.AddCwd(req.Cwd)
			}
			existing.Owner.SessionMgr().PreRegisterForOwner(token, existingSID, req.Cwd, req.Env)
			return existing.Owner.IPCPath(), existingSID, token, nil
		}
	}

	// Reserve the slot with a placeholder before releasing d.mu.
	// Any concurrent goroutine that arrives for the same sid will wait on the
	// creating channel instead of racing to spawn a duplicate owner.
	ownerGeneration, err := generateGeneration("owner")
	if err != nil {
		d.mu.Unlock()
		return "", "", "", err
	}
	placeholder := &OwnerEntry{
		ServerID:        sid,
		Command:         req.Command,
		Args:            req.Args,
		Cwd:             req.Cwd,
		OwnerGeneration: ownerGeneration,
		RestoreSource:   "fresh",
		creating:        make(chan struct{}),
	}
	d.owners[sid] = placeholder
	d.mu.Unlock()

	ipcPath := serverid.IPCPath("", d.name, sid)

	// Pass full session env to the owner. Shim-supplied vars WIN; daemon env
	// fills gaps. Rationale: some shims are launched by tools that strip
	// inherited vars (observed: CC sessions started in certain worktree paths
	// arrive with 18-25 vars instead of 130+, missing GITHUB_PERSONAL_ACCESS_TOKEN
	// and other credentials). Session-aware upstreams (e.g. pr-review-mcp) then
	// fail with "No GitHub token available for session ...". Merging from the
	// daemon's own os.Environ() (which was snapshotted at daemon start from the
	// user env) fills the gap without overriding anything the shim did send.
	sessionEnv := mergeEnv(req.Env)
	if len(sessionEnv) > 0 {
		// Log presence (NOT values) of common credential keys so env-passthrough
		// regressions remain visible. `shim_vars` is the pre-merge count — that
		// is the one that regresses when CC sends a short env. `total` reflects
		// what the upstream actually sees after the daemon-env fallback.
		d.logger.Printf("owner %s: session env shim_vars=%d total=%d (github_pat=%v gh_token=%v openai_key=%v anthropic_key=%v)",
			sid[:8], len(req.Env), len(sessionEnv),
			sessionEnv["GITHUB_PERSONAL_ACCESS_TOKEN"] != "",
			sessionEnv["GH_TOKEN"] != "" || sessionEnv["GITHUB_TOKEN"] != "",
			sessionEnv["OPENAI_API_KEY"] != "",
			sessionEnv["ANTHROPIC_API_KEY"] != "")
	}

	// Build the shared owner config (used by both template and fresh paths).
	controlPath := serverid.ControlPath("", d.name, sid)
	ownerCfg := owner.OwnerConfig{
		Command:          req.Command,
		Args:             req.Args,
		Env:              sessionEnv,
		Cwd:              req.Cwd,
		IPCPath:          ipcPath,
		ControlPath:      controlPath,
		ServerID:         sid,
		TokenHandshake:   true, // daemon-managed owners: shims send a handshake token
		HandlerFunc:      d.handlerFunc,
		SessionHandler:   d.sessionHandler,
		AuthorizeSession: d.authorizeSession,
		OnFrameReceived:  d.onFrameReceived,
		OnZeroSessions: func(serverID string) {
			d.onZeroSessions(serverID)
		},
		OnUpstreamExit: func(serverID string) {
			d.onUpstreamExit(serverID)
		},
		OnPersistentDetected: func(serverID string) {
			d.SetPersistent(serverID, true)
		},
		OnCacheReady: func(serverID string) {
			d.mu.RLock()
			entry, ok := d.owners[serverID]
			d.mu.RUnlock()
			if !ok || entry.Owner == nil {
				return
			}
			snap := entry.Owner.ExportSnapshot()
			d.updateTemplate(req.Command, req.Args, snap)
		},
		Logger: log.New(d.logger.Writer(), fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags|log.Lmicroseconds),
	}

	// Try template-based spawn: if the daemon has seen this server before,
	// create the owner from cached init data (instant response to CC) and
	// start the real upstream process in the background.
	var o *owner.Owner
	var ownerErr error
	fromTemplate := false
	if tmpl, ok := d.getTemplate(req.Command, req.Args); ok {
		// Adapt template for this specific owner instance
		tmpl.ServerID = sid
		tmpl.Cwd = req.Cwd
		tmpl.CwdSet = []string{req.Cwd}
		tmpl.Env = sessionEnv
		tmpl.Mode = req.Mode

		o, ownerErr = owner.NewOwnerFromSnapshot(ownerCfg, tmpl)
		if ownerErr != nil {
			d.logger.Printf("template spawn failed for %s: %v, falling back to fresh spawn", sid[:8], ownerErr)
			o = nil // fall through to fresh spawn
		} else {
			fromTemplate = true
			d.logger.Printf("spawned owner %s from template cache (instant init) for %s", sid[:8], req.Command)
		}
	}

	// Fresh spawn: no template available, or template spawn failed.
	if o == nil {
		o, ownerErr = owner.NewOwner(ownerCfg)
		if ownerErr != nil {
			// Remove the placeholder and unblock any waiters.
			d.mu.Lock()
			if d.owners[sid] == placeholder {
				d.deleteOwnerEntryLocked(sid)
			}
			close(placeholder.creating)
			d.mu.Unlock()
			return "", "", "", fmt.Errorf("spawn %s: %w", req.Command, ownerErr)
		}
		d.logger.Printf("spawned owner %s for %s %v (cold start)", sid[:8], req.Command, req.Args)
	}

	// Register owner with the supervisor for lifecycle management.
	// Suture will call owner.Serve(ctx) in its own goroutine and handle
	// restart with exponential backoff if Serve returns an error.
	serviceToken := d.supervisor.Add(o)

	// Promote the placeholder to a real entry and signal waiters.
	// Store the merged env (not raw req.Env) so snapshot save and dedup
	// checks see the same credential-complete view that the upstream and
	// session already got. Otherwise a daemon restart would round-trip
	// trimmed env through the snapshot and re-surface the original bug.
	d.mu.Lock()
	placeholder.Owner = o
	placeholder.Mode = req.Mode
	placeholder.Env = sessionEnv
	placeholder.LastSession = time.Now()
	placeholder.IdleTimeout = d.ownerIdleTimeout
	placeholder.serviceToken = serviceToken
	placeholder.Persistent = d.persistent
	close(placeholder.creating)
	placeholder.creating = nil // no longer a placeholder
	d.mu.Unlock()

	// For template-spawned owners, start the upstream process in the background.
	// The owner already serves cached responses; upstream refreshes caches when ready.
	if fromTemplate {
		o.SpawnUpstreamBackground()
	}

	// PreRegister with the MERGED env (not raw req.Env) so the session — bound
	// to this token on handshake — sees daemon-filled credentials too.
	// owner.go:~815 gates muxEnv injection on `len(s.Env) > 0` and sends s.Env
	// as _meta.muxEnv; session-aware upstreams (pr-review-mcp etc.) look up
	// GITHUB_PERSONAL_ACCESS_TOKEN here. Without the merge, a trimmed shim
	// env would leave muxEnv missing the token even though the owner/upstream
	// process has it via mergeEnv above.
	o.SessionMgr().PreRegisterForOwner(token, sid, req.Cwd, sessionEnv)
	return ipcPath, sid, token, nil
}

// Remove shuts down and removes an owner by server ID.
func (d *Daemon) Remove(serverID string) error {
	_, err := d.removeOwner(serverID, ownerRemovalReasonOperatorHard, false)
	return err
}

// SoftRemove performs a graceful shutdown of the named owner, giving the upstream
// up to 30 seconds to exit cleanly via stdin close before escalating to SIGTERM/SIGKILL.
//
// Use Remove (hard kill) for operator-requested restarts; use SoftRemove for idle
// eviction so upstreams can flush caches, close files, and exit with code 0 (US3).
func (d *Daemon) SoftRemove(serverID string) error {
	_, err := d.removeOwner(serverID, ownerRemovalReasonOperatorSoft, true)
	return err
}

// HandleSpawn implements control.DaemonHandler.
func (d *Daemon) HandleSpawn(req control.Request) (string, string, string, error) {
	if d.shuttingDown.Load() {
		return "", "", "", ErrDaemonShuttingDown
	}
	ipcPath, serverID, token, err := d.Spawn(req)
	if err == nil && req.ReconnectReason == "fallback_spawn" {
		d.reconnectFallbackSpawned.Add(1)
	}
	return ipcPath, serverID, token, err
}

// HandleRemove implements control.DaemonHandler.
func (d *Daemon) HandleRemove(serverID string) error {
	return d.Remove(serverID)
}

// HandleShutdown implements control.CommandHandler.
func (d *Daemon) HandleShutdown(drainTimeoutMs int) string {
	d.shuttingDown.Store(true)
	go d.Shutdown()
	return "daemon shutting down"
}

// HandleReconnectGiveUp records that a shim exhausted its reconnect budget and
// could not recover.
func (d *Daemon) HandleReconnectGiveUp(reason string) error {
	d.reconnectGaveUp.Add(1)
	return nil
}

// handoffAcceptTimeout is the maximum time the old daemon waits for the
// successor daemon to dial the handoff socket. Declared as var so tests can
// override it without recompiling with build tags.
var handoffAcceptTimeout = 30 * time.Second

// handoffTotalTimeout is the maximum time allocated to the entire performHandoff
// protocol exchange (hello/ready/transfer/done/ack sequence). Declared as var
// for the same test-override reason as handoffAcceptTimeout.
var handoffTotalTimeout = 30 * time.Second

// controlBindRetryInterval is the pause between successive control-socket bind
// attempts in handoff mode. Declared as var so tests can shrink it.
var controlBindRetryInterval = 500 * time.Millisecond

// controlBindMaxAttempts caps the number of bind retries in handoff mode.
// At 500 ms/attempt this gives up to 30 s for the predecessor to release the
// socket. Declared as var so tests can override it.
var controlBindMaxAttempts = 60

// snapshotRestartControlBindDelay gives the predecessor process a short window
// to fully release Windows AF_UNIX socket reparse points before the snapshot
// successor starts touching the fixed daemon control path.
var snapshotRestartControlBindDelay = 1 * time.Second

// isHandoffMode reports whether this process was launched as a successor daemon
// during a graceful restart. Both env vars must be present for the handoff
// protocol to proceed.
func isHandoffMode() bool {
	return os.Getenv("MCPMUX_HANDOFF_TOKEN_PATH") != "" &&
		os.Getenv("MCPMUX_HANDOFF_SOCKET") != ""
}

func isSnapshotRestartMode() bool {
	return os.Getenv(snapshotRestartEnv) == "1"
}

func isRestartRestoreMode() bool {
	return isHandoffMode() || isSnapshotRestartMode()
}

func waitBeforeSnapshotRestartControlBind(logger *log.Logger) {
	if !isSnapshotRestartMode() || isHandoffMode() || snapshotRestartControlBindDelay <= 0 {
		return
	}
	if logger != nil {
		logger.Printf("snapshot_restart.control_bind_delay delay=%s", snapshotRestartControlBindDelay)
	}
	time.Sleep(snapshotRestartControlBindDelay)
}

// retryControlBind polls for the control socket to become available and binds
// it. Used in handoff mode where the predecessor daemon still holds the socket
// when the successor starts. The predecessor calls Shutdown() after completing
// the handoff protocol, releasing the socket file; retryControlBind detects
// that and completes the bind.
func (d *Daemon) retryControlBind(socketPath string) error {
	for i := range controlBindMaxAttempts {
		ctlSrv, err := control.NewServer(socketPath, d, d.logger)
		if err == nil {
			d.ctlSrv = ctlSrv
			if i > 0 {
				d.logger.Printf("handoff.control_bind retries=%d", i)
			}
			return nil
		}
		if i == 0 {
			d.logger.Printf("handoff.control_bind_wait predecessor still holds socket, retrying: %v", err)
		} else if (i+1)%10 == 0 {
			d.logger.Printf("handoff.control_bind_retry attempt=%d err=%v", i+1, err)
		}
		time.Sleep(controlBindRetryInterval)
	}
	return fmt.Errorf("daemon: control socket not available after %d attempts (%v)",
		controlBindMaxAttempts, time.Duration(controlBindMaxAttempts)*controlBindRetryInterval)
}

// spawnSuccessor forks the current binary as a detached background process,
// injecting MCPMUX_HANDOFF_TOKEN_PATH and MCPMUX_HANDOFF_SOCKET so the successor
// daemon can locate the handoff socket and authenticate (FR-11). The caller does
// NOT wait for the process — it runs independently and will dial back.
func spawnSuccessor(tokenPath, socketPath, daemonFlag, successorExe string) error {
	exe, err := successorExecutableFor(successorExe)
	if err != nil {
		return fmt.Errorf("handoff: resolve executable: %w", err)
	}
	cmd := exec.Command(exe, daemonFlag)
	cmd.Env = append(os.Environ(),
		"MCPMUX_HANDOFF_TOKEN_PATH="+tokenPath,
		"MCPMUX_HANDOFF_SOCKET="+socketPath,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	// setSuccessorDetached is platform-specific (handoff_socket_unix.go / _windows.go).
	// It mirrors engine.setDetached; importing engine from daemon would be a cycle.
	setSuccessorDetached(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("handoff: spawn successor: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("handoff: release successor: %w", err)
	}
	return nil
}

func spawnSnapshotSuccessor(successorExe, daemonFlag string) error {
	exe, err := successorExecutableFor(successorExe)
	if err != nil {
		return fmt.Errorf("snapshot restart: resolve executable: %w", err)
	}
	cmd := exec.Command(exe, daemonFlag)
	cmd.Env = append(os.Environ(), snapshotRestartEnv+"=1")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	setSuccessorDetached(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("snapshot restart: spawn successor: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("snapshot restart: release successor: %w", err)
	}
	return nil
}

func successorExecutable() (string, error) {
	return successorExecutableFor("")
}

func successorExecutableFor(explicitOverride string) (string, error) {
	if explicit := strings.TrimSpace(explicitOverride); explicit != "" {
		return explicit, nil
	}
	if explicit := strings.TrimSpace(os.Getenv("MCPMUX_SUCCESSOR_EXE")); explicit != "" {
		return explicit, nil
	}
	if pointer := strings.TrimSpace(os.Getenv("MCPMUX_ACTIVE_ENGINE_FILE")); pointer != "" {
		data, err := os.ReadFile(pointer)
		if err == nil {
			target := strings.TrimSpace(string(data))
			if target != "" {
				if filepath.IsAbs(target) {
					return filepath.Clean(target), nil
				}
				return filepath.Clean(filepath.Join(filepath.Dir(pointer), target)), nil
			}
		}
	}
	return os.Executable()
}

// collectHandoffUpstreams calls ShutdownForHandoff on every live owner and
// returns the HandoffUpstream list. Owners that fail to detach are logged and
// skipped; per-upstream atomicity (FR-7) is enforced inside performHandoff.
// Must NOT hold d.mu while calling ShutdownForHandoff (it may block).
func (d *Daemon) collectHandoffUpstreams() []HandoffUpstream {
	d.mu.RLock()
	entries := make([]*OwnerEntry, 0, len(d.owners))
	for _, e := range d.owners {
		if e.Owner != nil {
			entries = append(entries, e)
		}
	}
	d.mu.RUnlock()

	var upstreams []HandoffUpstream
	for _, e := range entries {
		payload, err := e.Owner.ShutdownForHandoff()
		if err != nil {
			d.logger.Printf("handoff: owner %s ShutdownForHandoff error: %v (skipping)",
				e.ServerID[:8], err)
			continue
		}
		upstreams = append(upstreams, HandoffUpstream{
			ServerID: payload.ServerID,
			Command:  payload.Command,
			PID:      payload.PID,
			StdinFD:  payload.StdinFD,
			StdoutFD: payload.StdoutFD,
		})
	}
	return upstreams
}

func (d *Daemon) hasHandoffUpstreamOwners() bool {
	d.mu.RLock()
	owners := make([]*owner.Owner, 0, len(d.owners))
	for _, e := range d.owners {
		if e.Owner != nil {
			owners = append(owners, e.Owner)
		}
	}
	d.mu.RUnlock()

	for _, o := range owners {
		if o.HasHandoffUpstream() {
			return true
		}
	}
	return false
}

// attemptHandoff runs the old-daemon side of the two-daemon handoff protocol.
// Returns nil if the protocol completed (transferred or FR-7 per-upstream abort).
// Returns non-nil on any protocol-level failure — the caller logs "handoff.fallback"
// and falls back to the legacy kill-and-respawn path (FR-8).
//
// FR-8 fallback trigger set (all route through the returned error):
//   - Token write failure (crypto/rand unavailable, disk full, permission denied)
//   - Socket bind / accept failure (path collision, permission denied)
//   - Accept timeout exceeded (successor never connected)
//   - Successor spawn failure (os.Executable error, exec.Start error)
//   - ErrTokenMismatch (successor presented wrong token — FR-11)
//   - ErrProtocolVersionMismatch (binary skew — FR-6)
//   - Any other performHandoff protocol error (conn drop, JSON decode failure)
func (d *Daemon) attemptHandoff(successorExe string) error {
	d.stats.attempted.Add(1)
	startedAt := time.Now()
	baseDir := os.TempDir()

	if !d.hasHandoffUpstreamOwners() {
		return errNoHandoffUpstreams
	}

	// Write handoff token (FR-11). Token is deleted via defer on both paths.
	token, tokenPath, err := writeHandoffToken(baseDir)
	if err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	defer deleteHandoffToken(tokenPath) //nolint:errcheck

	// Compute socket path before starting the listener goroutine; the path
	// is also passed to the successor via MCPMUX_HANDOFF_SOCKET.
	socketPath := handoffSocketPath(baseDir)

	d.logger.Printf("handoff.start socket=%s token_path=%s", socketPath, tokenPath)

	// Start listening BEFORE spawning successor so the socket/pipe exists
	// when the successor process starts and tries to dial. listenHandoff
	// blocks until accept OR timeout — must run in a goroutine.
	type connResult struct {
		conn fdConn
		err  error
	}
	connCh := make(chan connResult, 1)
	go func() {
		conn, err := listenHandoff(socketPath, handoffAcceptTimeout)
		connCh <- connResult{conn, err}
	}()

	// Spawn successor with handoff credentials.
	if spawnErr := spawnSuccessor(tokenPath, socketPath, d.daemonFlag, successorExe); spawnErr != nil {
		// Do NOT drain connCh here — the listener goroutine will self-terminate
		// once handoffAcceptTimeout expires (buffered channel prevents goroutine
		// leak; the accepted conn, if any arrives despite the spawn failure, is
		// closed when connCh is garbage-collected after nobody reads it).
		return fmt.Errorf("spawn successor: %w", spawnErr)
	}

	// Wait for successor to connect (or timeout to expire).
	cr := <-connCh
	if cr.err != nil {
		return fmt.Errorf("accept: %w", cr.err)
	}
	conn := cr.conn
	defer conn.Close() //nolint:errcheck

	// Collect HandoffUpstream list by detaching all live owners.
	// ShutdownForHandoff detaches FDs from the Owner; they are no longer managed
	// by any Owner after this call and must be closed explicitly by this function.
	// Failing to close them would exhaust file descriptors and prevent upstreams
	// from receiving EOF on their pipes if the handoff protocol fails.
	upstreams := d.collectHandoffUpstreams()
	defer func() {
		for _, u := range upstreams {
			if u.StdinFD > 2 {
				_ = os.NewFile(u.StdinFD, "").Close()
			}
			if u.StdoutFD > 2 {
				_ = os.NewFile(u.StdoutFD, "").Close()
			}
		}
	}()

	// Run handoff protocol with 30s deadline. The existing _ = ctx reservation
	// in performHandoff (T009 integration) becomes meaningful here.
	ctx, cancel := context.WithTimeout(context.Background(), handoffTotalTimeout)
	defer cancel()

	result, err := performHandoff(ctx, conn, token, upstreams)
	if err != nil {
		return fmt.Errorf("protocol error: %w", err)
	}

	d.stats.transferred.Add(uint64(len(result.Transferred)))
	d.stats.aborted.Add(uint64(len(result.Aborted)))
	d.logger.Printf("handoff.complete transferred=%d aborted=%d phase=%s duration_ms=%d",
		len(result.Transferred), len(result.Aborted), result.Phase,
		time.Since(startedAt).Milliseconds())
	return nil
}

// HandleGracefulRestart implements control.DaemonHandler.
// Serializes a state snapshot (used as FR-8 fallback seed and successor
// cold-start seed), then attempts FD-passing handoff to the successor daemon
// (FR-1/FR-2/FR-3). Process-backed handoff failures emit the "handoff.fallback"
// log line and fall through to legacy kill-and-respawn (FR-8). SessionHandler-
// only configurations have no upstream FDs to hand off and use snapshot-only
// restart as their healthy path.
func (d *Daemon) HandleGracefulRestart(drainTimeoutMs int) (string, func(), error) {
	return d.HandleGracefulRestartWithOptions(control.GracefulRestartOptions{DrainTimeoutMs: drainTimeoutMs})
}

// HandleGracefulRestartWithOptions implements control.GracefulRestartOptionsHandler.
func (d *Daemon) HandleGracefulRestartWithOptions(opts control.GracefulRestartOptions) (string, func(), error) {
	d.shuttingDown.Store(true)
	if successorExe := strings.TrimSpace(opts.SuccessorExe); successorExe != "" {
		d.logger.Printf("graceful-restart: successor_exe=%q", successorExe)
	}

	// Serialize snapshot first — needed for FR-8 fallback and successor seed
	// regardless of whether handoff succeeds.
	snapshotPath, err := d.SerializeSnapshot()
	if err != nil {
		d.shuttingDown.Store(false)
		return "", nil, fmt.Errorf("snapshot: %w", err)
	}

	// Tag owners BEFORE attemptHandoff. collectHandoffUpstreams (inside
	// attemptHandoff) detaches owners → supervisor fires termination events
	// → event hook acquires d.mu. Tagging after attemptHandoff deadlocks
	// because this goroutine also needs d.mu (#99).
	d.mu.Lock()
	for _, entry := range d.owners {
		entry.terminationHint = HintPlannedHandoff
	}
	d.mu.Unlock()

	// Attempt FD-passing handoff. Callers never see a handoff error — FR-8 is
	// silent to the control-plane caller (it still gets a valid snapshot path).
	if handoffErr := d.attemptHandoff(opts.SuccessorExe); handoffErr != nil {
		if errors.Is(handoffErr, errNoHandoffUpstreams) {
			return snapshotPath, d.afterSnapshotOnlyRestart(opts.SuccessorExe), nil
		}
		d.stats.fallback.Add(1)
		d.logger.Printf("handoff.fallback reason=%v — using legacy shutdown+respawn", handoffErr)
	}

	return snapshotPath, func() { go d.Shutdown() }, nil
}

func (d *Daemon) afterSnapshotOnlyRestart(successorExe string) func() {
	return func() {
		go func() {
			d.shutdown(func() {
				d.logger.Printf("snapshot_restart.spawn_successor exe=%q flag=%q", successorExe, d.daemonFlag)
				if err := spawnSnapshotSuccessorForRestart(successorExe, d.daemonFlag); err != nil {
					d.logger.Printf("snapshot_restart.successor_spawn_failed reason=no_process_backed_owners err=%v", err)
					return
				}
				d.logger.Printf("snapshot_restart.successor_spawned reason=no_process_backed_owners")
			})
		}()
	}
}

// HandleRefreshSessionToken implements control.DaemonHandler.
func (d *Daemon) HandleRefreshSessionToken(prevToken string) (string, error) {
	if prevToken == "" {
		d.logger.Printf("shim.reconnect.refresh_fail reason=unknown_token")
		return "", ErrUnknownToken
	}

	entry, ownerKey := d.lookupReconnectOwner(prevToken)
	if entry == nil || entry.Owner == nil {
		d.logger.Printf("shim.reconnect.refresh_fail reason=unknown_token")
		return "", ErrUnknownToken
	}

	newToken, err := entry.Owner.SessionMgr().RegisterReconnect(prevToken, d.ownerIsAccepting)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrUnknownToken):
			d.logger.Printf("shim.reconnect.refresh_fail reason=unknown_token")
			return "", ErrUnknownToken
		case errors.Is(err, session.ErrOwnerGone):
			d.logger.Printf("shim.reconnect.refresh_fail reason=owner_gone")
			return "", ErrOwnerGone
		default:
			d.logger.Printf("shim.reconnect.refresh_fail reason=internal")
			return "", err
		}
	}

	d.reconnectRefreshed.Add(1)
	d.logger.Printf("shim.reconnect.refresh_ok owner=%s", shortServerID(ownerKey))
	return newToken, nil
}

// HandleStatus implements control.CommandHandler.
func (d *Daemon) HandleStatus() map[string]any {
	d.mu.RLock()
	defer d.mu.RUnlock()

	servers := make([]map[string]any, 0, len(d.owners))
	for sid, entry := range d.owners {
		if entry.Owner == nil {
			continue // placeholder still being created
		}
		s := entry.Owner.Status()
		s["server_id"] = sid
		s["persistent"] = entry.Persistent
		s["owner_generation"] = entry.OwnerGeneration
		if entry.RestoredFromOwnerGeneration != "" {
			s["restored_from_owner_generation"] = entry.RestoredFromOwnerGeneration
		}
		if entry.RestoreSource != "" {
			s["restore_source"] = entry.RestoreSource
		} else {
			s["restore_source"] = "fresh"
		}
		s["last_session"] = entry.LastSession.Format(time.RFC3339)
		// Prefer the per-owner override from x-mux.idleTimeout capability
		// (set via Owner.SetIdleTimeout after init); fall back to the
		// daemon-wide default captured at spawn time.
		effectiveIdleTimeout := entry.IdleTimeout
		if override := entry.Owner.IdleTimeout(); override > 0 {
			effectiveIdleTimeout = override
		}
		s["idle_timeout_s"] = effectiveIdleTimeout.Seconds()
		if !entry.Owner.LastActivity().IsZero() {
			s["last_activity"] = entry.Owner.LastActivity().Format(time.RFC3339)
		}
		s["active_progress_tokens"] = entry.Owner.ActiveProgressTokens()
		s["busy"] = entry.Owner.HasActiveBusyWork()
		servers = append(servers, s)
	}

	return map[string]any{
		"daemon":                          true,
		"engine_name":                     d.name,
		"shutting_down":                   d.shuttingDown.Load(),
		"pid":                             os.Getpid(),
		"daemon_generation":               d.daemonGeneration,
		"reaped_owner_count":              d.ownerRemoval.ByReason[ownerRemovalReasonIdle],
		"owner_removal":                   d.ownerRemoval.statusMap(),
		"owner_count":                     len(servers), // excludes placeholders still being created
		"servers":                         servers,
		"owner_idle_timeout":              d.ownerIdleTimeout.String(),
		"idle_timeout":                    d.idleTimeout.String(),
		"shim_reconnect_refreshed":        d.reconnectRefreshed.Load(),
		"shim_reconnect_fallback_spawned": d.reconnectFallbackSpawned.Load(),
		"shim_reconnect_gave_up":          d.reconnectGaveUp.Load(),
		"zombie_detections_spawn":         d.zombieDetectedSpawn,
		"zombie_detections_restore":       d.zombieDetectedRestore,
		"handoff": map[string]any{
			"attempted":                      d.stats.attempted.Load(),
			"transferred":                    d.stats.transferred.Load(),
			"aborted":                        d.stats.aborted.Load(),
			"fallback":                       d.stats.fallback.Load(),
			"predecessor_pid":                d.predecessorPID,
			"predecessor_daemon_generation":  d.predecessorDaemonGeneration,
			"successor_daemon_generation":    d.daemonGeneration,
			"restored_owner_count":           d.restoredOwnerCount.Load(),
			"old_owner_socket_retired_count": d.oldOwnerSocketRetiredCount.Load(),
		},
	}
}

// HandleListOwners returns a snapshot of all active owners, capped at 200 entries,
// sorted by server_id ascending for deterministic output. Placeholder entries
// (Owner == nil) are excluded. Satisfies control.DaemonHandler.
func (d *Daemon) HandleListOwners(req control.Request) (control.ListOwnersResponse, error) {
	const maxOwners = 200
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Collect server IDs, skipping placeholder entries.
	sids := make([]string, 0, len(d.owners))
	for sid, entry := range d.owners {
		if entry.Owner == nil {
			continue
		}
		sids = append(sids, sid)
	}
	sort.Strings(sids)

	truncated := false
	if len(sids) > maxOwners {
		sids = sids[:maxOwners]
		truncated = true
	}

	owners := make([]control.OwnerInfo, 0, len(sids))
	for _, sid := range sids {
		entry := d.owners[sid]
		if entry == nil || entry.Owner == nil {
			continue
		}
		s := entry.Owner.Status()

		cwd, _ := s["cwd"].(string)
		muxVer, _ := s["mux_version"].(string)
		classification, _ := s["auto_classification"].(string)
		classificationSource, _ := s["classification_source"].(string)

		sessions := statusInt(s["session_count"])
		pending := statusInt(s["pending_requests"])
		upstreamPID := statusInt(s["upstream_pid"])

		var cwdSet []string
		switch v := s["cwd_set"].(type) {
		case []string:
			cwdSet = v
		case []any:
			for _, c := range v {
				if cs, ok := c.(string); ok {
					cwdSet = append(cwdSet, cs)
				}
			}
		}

		var classificationReason []string
		switch v := s["classification_reason"].(type) {
		case []string:
			classificationReason = append(classificationReason, v...)
		case []any:
			for _, r := range v {
				if rs, ok := r.(string); ok {
					classificationReason = append(classificationReason, rs)
				}
			}
		}

		cachedInit, _ := s["cached_init"].(bool)
		cachedTools, _ := s["cached_tools"].(bool)
		cachedPrompts, _ := s["cached_prompts"].(bool)
		cachedResources, _ := s["cached_resources"].(bool)

		owners = append(owners, control.OwnerInfo{
			ServerID:             sid,
			EngineName:           d.name,
			Command:              entry.Command,
			Args:                 entry.Args,
			Cwd:                  cwd,
			CwdSet:               cwdSet,
			Sessions:             sessions,
			Pending:              pending,
			UpstreamPID:          upstreamPID,
			Classification:       classification,
			ClassificationSource: classificationSource,
			ClassificationReason: classificationReason,
			MuxVersion:           muxVer,
			Persistent:           entry.Persistent,
			CachedInit:           cachedInit,
			CachedTools:          cachedTools,
			CachedPrompts:        cachedPrompts,
			CachedResources:      cachedResources,
		})
	}

	return control.ListOwnersResponse{Owners: owners, Truncated: truncated}, nil
}

func statusInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if parsed, err := n.Int64(); err == nil {
			return int(parsed)
		}
	}
	return 0
}

func (d *Daemon) ownerIsAccepting(serverID string) bool {
	d.mu.RLock()
	entry, ok := d.owners[serverID]
	d.mu.RUnlock()
	if !ok || entry == nil || entry.Owner == nil {
		return false
	}
	if !entry.Owner.IsAccepting() {
		return false
	}
	select {
	case <-entry.Owner.Done():
		return false
	default:
		return true
	}
}

func (d *Daemon) lookupReconnectOwner(prevToken string) (*OwnerEntry, string) {
	d.mu.RLock()
	entries := make([]*OwnerEntry, 0, len(d.owners))
	for _, entry := range d.owners {
		entries = append(entries, entry)
	}
	d.mu.RUnlock()

	for _, entry := range entries {
		if entry == nil || entry.Owner == nil {
			continue
		}
		ownerKey, _, _, ok := entry.Owner.SessionMgr().LookupHistory(prevToken)
		if ok {
			return entry, ownerKey
		}
	}
	return nil, ""
}

func shortServerID(serverID string) string {
	if len(serverID) > 8 {
		return serverID[:8]
	}
	return serverID
}

// SetPersistent marks an owner as persistent (survives zero-session periods).
func (d *Daemon) SetPersistent(serverID string, persistent bool) {
	d.mu.Lock()
	if entry, ok := d.owners[serverID]; ok {
		entry.Persistent = persistent
		d.logger.Printf("owner %s persistent=%v", serverID[:8], persistent)
	}
	d.mu.Unlock()
}

// findSharedOwnerLocked looks for an accepting owner that matches the requested
// command+args and is compatible with the caller's env and cwd for shared reuse.
// Dedup is optimistic: unclassified owners are assumed shareable. If an owner
// later classifies as isolated, it closes its IPC listener — extra sessions get
// EOF and reconnect with their own owner.
//
// Lock semantics (FR-8 / BUG-007): this function MUST be called with d.mu
// Lock-held (write lock, not RLock). It drops d.mu via d.mu.Unlock() while
// waiting for an in-flight placeholder to resolve, then re-acquires with
// d.mu.Lock(). Calling it under RLock is a panic (Unlock on an RLock-held mutex).
//
// Match semantics: command and args are compared field-by-field, NOT by
// joining on spaces — that would make ("sh -c", ["ls"]) collide with
// ("sh", ["-c", "ls"]) and produce false positives for shells and wrappers.
//
// Placeholder handling: the first scan pass skips entries still being created
// (Owner == nil). If the scan finds no concrete match but one or more matching
// placeholders exist, the function waits for the first matching placeholder
// (releases the lock during the wait), then starts a fresh scan from scratch
// to avoid stale-iteration hazards. The outer loop is bounded by the number
// of wait cycles (at most one — placeholders only exist while someone is
// actively creating an owner; after a full wait-and-resolve cycle, either a
// live entry exists or no placeholder remains).
func (d *Daemon) findSharedOwnerLocked(command string, args []string, env map[string]string, reqCwd string) *OwnerEntry {
	canonReqCwd := serverid.CanonicalizePath(reqCwd)

	// envCompatible must compare like-for-like: owner.Env is post-mergeEnv
	// (populated at spawn time via mergeEnv(req.Env)), so the request side
	// must also be merged. Without this, codex PR #121 P1's credential-
	// asymmetry guard fires on every cross-session lookup because the raw
	// request env lacks os.Environ-supplied credentials (SSH_AUTH_SOCK,
	// system-level GITHUB_TOKEN, etc.) that the owner inherited.
	// mergeEnv is idempotent on already-merged input — safe to apply here
	// whether the caller already merged or not.
	mergedEnv := mergeEnv(env)

	// Wait budget: at most one placeholder-wait cycle. Multiple matching
	// placeholders in flight is pathological; one wait is sufficient for
	// the common concurrent-spawn case and bounds the wall-clock cost.
	const maxWaits = 1
	waitsDone := 0

	for {
		// Phase 1: scan live entries for a concrete match.
		var (
			match        *OwnerEntry
			placeholder  chan struct{}
			classifyWait <-chan struct{} // in-flight classification of a matching entry
		)
		for _, entry := range d.owners {
			if entry.Command != command {
				continue
			}
			if !argsEqual(entry.Args, args) {
				continue
			}
			if entry.Owner == nil {
				// Placeholder — remember the first one; skip for now, may
				// come back to it if no live match is found below.
				if placeholder == nil {
					placeholder = entry.creating
				}
				continue
			}
			// Skip owners with incompatible env — different API keys, tokens, etc.
			if !envCompatible(entry.Env, mergedEnv) {
				continue
			}
			if !entry.Owner.IsAccepting() {
				continue
			}

			// CWD-aware dedup: every process has exactly one CWD. Sharing an upstream
			// across sessions with different CWDs is only safe when the server has been
			// confirmed CWD-independent (classified as shared or session-aware).
			// Unclassified servers are NOT shared across CWDs — this prevents
			// cross-project context leaks for CWD-dependent servers.
			canonEntryCwd := serverid.CanonicalizePath(entry.Cwd)
			cwdMatch := canonReqCwd == canonEntryCwd

			if !cwdMatch {
				// Different CWD — only share if owner is confirmed shareable.
				if !entry.Owner.IsClassifiedShareable() {
					// If classification is still in flight, remember its channel.
					// If the owner ends up shareable we'll adopt it on rescan;
					// if isolated we'll skip it next time and fall through to
					// creating our own owner. This closes the race where a
					// second CWD arrives BEFORE the first owner classifies,
					// currently leaving two owners for the same command.
					select {
					case <-entry.Owner.Classified():
						// Already classified and !shareable → truly skip.
					default:
						if classifyWait == nil {
							classifyWait = entry.Owner.Classified()
						}
					}
					continue
				}
			}

			// Non-blocking classification check: if already classified, respect it.
			select {
			case <-entry.Owner.Classified():
				// Classification known — re-check IsAccepting (may have closed listener)
				if !entry.Owner.IsAccepting() {
					continue
				}
			default:
				// Not yet classified. Same CWD → safe to share optimistically.
				// Different CWD → already filtered above (IsClassifiedShareable).
			}
			match = entry
			break
		}

		if match != nil {
			return match
		}

		// No concrete match. If a placeholder or in-flight classification was
		// seen and we have wait budget, release the lock, wait for it (or
		// time out), re-acquire, and rescan. Otherwise give up.
		if placeholder == nil && classifyWait == nil {
			return nil
		}
		if waitsDone >= maxWaits {
			return nil
		}
		waitsDone++

		d.mu.Unlock()
		// Build a select that blocks on whichever signals we collected.
		// Using nil channels in a select case blocks forever, so a single
		// select with nil branches safely waits only on non-nil ones.
		// Invariant: at least one of placeholder/classifyWait is non-nil (guard
		// at line ~998 ensures this). Nil-channel cases in a select are never
		// selected, so time.After is the fallback for the non-nil branch.
		select {
		case <-placeholder:
			// Creation resolved (success or failure) — rescan.
		case <-classifyWait:
			// Classification resolved — rescan; owner may now be shareable.
		case <-time.After(concurrentCreateWaitTimeout):
			// Timed out. Re-acquire and return — caller will create new.
			d.mu.Lock()
			return nil
		}
		d.mu.Lock()
		// Loop: fresh scan on the now-mutated map.
	}
}

// argsEqual compares two argv slices element-by-element. Used by findSharedOwner
// instead of joining on spaces, which would collide on different tokenizations
// of the same command line (e.g. "sh -c" + "ls" vs "sh" + "-c ls").
func argsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mergeEnv combines a shim-supplied env with the daemon's own os.Environ().
// Shim-supplied entries win on key collision (per-session credentials and cwd
// vars must override anything inherited by the daemon). Any key missing from
// the shim env is filled from the daemon env.
//
// Why: some shim launch paths arrive with a drastically trimmed environment
// (observed in CC sessions started in certain worktree layouts: ~18-25 vars
// instead of the usual 130+), missing GITHUB_PERSONAL_ACCESS_TOKEN and other
// credentials. Session-aware upstreams (pr-review-mcp, etc.) then surface
// "No GitHub token available for session ..." errors and CC marks the server
// `failed` in `/mcp`. Filling from the daemon's own env (captured from the
// user environment at daemon start) restores the missing credentials without
// overriding anything the shim did send. shim-supplied nil map → daemon env
// returned directly.
func mergeEnv(shimEnv map[string]string) map[string]string {
	merged := make(map[string]string, len(shimEnv)+64)
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			merged[e[:i]] = e[i+1:]
		}
	}
	for k, v := range shimEnv {
		merged[k] = v
	}
	return merged
}

// envCompatible returns true if two env maps have no conflicting values
// for semantically significant keys (API tokens, config paths, etc.).
// Transient per-session vars (CLAUDE_CODE_*, WT_SESSION, etc.) are ignored.
//
// Credential-bearing keys (GITHUB_TOKEN, OPENAI_API_KEY, anything matching
// envCredentialKey()) additionally MUST match by presence: if a key exists
// in one side but not the other, the envs are NOT compatible. Per codex
// PR #121 P1: without this, a first session spawning an owner with
// GITHUB_TOKEN=abc and a later session without GITHUB_TOKEN would share
// the same owner, leaking the first session's credential into the second.
// Plain value-conflict checks miss this because the second map has no
// key to conflict against.
func envCompatible(a, b map[string]string) bool {
	for k, va := range a {
		if envTransient(k) {
			continue
		}
		vb, ok := b[k]
		if !ok {
			if envCredentialKey(k) {
				return false
			}
			continue
		}
		if va != vb {
			return false
		}
	}
	// Symmetric check: a credential key present in b but missing in a
	// must also split. The loop over a alone cannot see keys unique to b.
	for k := range b {
		if envTransient(k) {
			continue
		}
		if _, ok := a[k]; ok {
			continue
		}
		if envCredentialKey(k) {
			return false
		}
	}
	return true
}

// envCredentialKey returns true if the key likely carries an authentication
// secret whose presence asymmetry across two env maps must split owners
// under global-first identity. Heuristic suffix/exact match — exhaustive
// enumeration is impractical, so the rule errs toward over-splitting on
// keys that LOOK like credentials. False positives waste one owner per
// uniquely-named "fake credential" var; false negatives leak real
// credentials across sessions. The trade-off prefers the former.
func envCredentialKey(key string) bool {
	upper := strings.ToUpper(key)
	switch {
	case strings.HasSuffix(upper, "_TOKEN"):
		return true
	case strings.HasSuffix(upper, "_KEY"):
		return true
	case strings.HasSuffix(upper, "_API_KEY"):
		return true
	case strings.HasSuffix(upper, "_SECRET"):
		return true
	case strings.HasSuffix(upper, "_PASSWORD"):
		return true
	case strings.HasSuffix(upper, "_PASSWD"):
		return true
	case strings.HasSuffix(upper, "_CREDENTIALS"):
		return true
	case strings.HasSuffix(upper, "_AUTH"):
		return true
	// Exact-match keys that don't fit the suffix pattern.
	case upper == "GH_TOKEN" || upper == "GITHUB_TOKEN":
		return true
	case upper == "GITHUB_PERSONAL_ACCESS_TOKEN":
		return true
	case upper == "OPENAI_API_KEY" || upper == "ANTHROPIC_API_KEY":
		return true
	case upper == "TAVILY_API_KEY" || upper == "GOOGLE_API_KEY":
		return true
	case upper == "AWS_ACCESS_KEY_ID" || upper == "AWS_SECRET_ACCESS_KEY":
		return true
	case upper == "AWS_SESSION_TOKEN":
		return true
	case upper == "SSH_AUTH_SOCK" || upper == "SSH_AGENT_PID":
		return true
	case upper == "DOCKER_AUTH_CONFIG":
		return true
	}
	return false
}

// semanticEnvHash returns a stable 8-hex-char hash of env entries that
// envCompatible would consider semantically significant (non-transient
// keys, sorted alphabetically with their values). Used as a sid suffix
// when env-incompat sessions need distinct owners under global-first
// identity (CR-002 AC8). Empty / nil env returns "00000000".
func semanticEnvHash(env map[string]string) string {
	if len(env) == 0 {
		return "00000000"
	}
	h := sha256.New()
	keys := make([]string, 0, len(env))
	for k := range env {
		if envTransient(k) {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "00000000"
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(env[k]))
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// deriveEnvBucketedSid (CR-002 AC8) preserves the credentials-partition
// invariant of pre-CR-002 cwd-keyed identity under the new global-first
// default. When a Spawn lands on a global sid whose existing entry has
// env semantically incompatible with the new request (e.g., different
// GITHUB_TOKEN), this function returns a derived sid of the form
// `{baseSid}-env-{8hex}` so the two sessions land on distinct owners.
//
// Compatible env OR no existing entry → return baseSid unchanged
// (the standard d.owners[sid] hit/spawn path runs unmodified).
//
// The check uses envCompatible() with the existing entry's stored env
// (which is post-mergeEnv from the original spawn). The new request's
// env is raw shim env; envCompatible's asymmetric semantics correctly
// detect conflicts even across the merged-vs-raw asymmetry because the
// daemon's own env keys are stable across spawns — only shim-supplied
// overrides differ.
func (d *Daemon) deriveEnvBucketedSid(baseSid string, reqEnv map[string]string) string {
	d.mu.RLock()
	entry, ok := d.owners[baseSid]
	var existingEnv map[string]string
	if ok && entry.Owner != nil {
		existingEnv = entry.Env
	}
	d.mu.RUnlock()

	// CodeRabbit PR #121 fix: normalize the new request's env via the same
	// mergeEnv used at spawn time so envCompatible compares like-for-like.
	// Without this normalization, the existing entry's stored env (post-
	// mergeEnv) and the raw shim env have asymmetric coverage: a shim that
	// previously sent GITHUB_TOKEN=A and now omits it entirely would NOT
	// trigger an env-bucket split (envCompatible iterates existing keys
	// and req-missing keys produce no conflict), so credential-rotated
	// sessions could silently reuse an owner holding the old token.
	// Normalizing both sides closes that asymmetry.
	normalizedReqEnv := mergeEnv(reqEnv)

	// Also compute the bucket suffix from the normalized env so two
	// requests with the same effective env (after daemon merge) land on
	// the same bucketed sid even if one shim sent the key and another
	// inherited it from daemon defaults.
	envHash := semanticEnvHash(normalizedReqEnv)

	if existingEnv == nil {
		// No owner at baseSid. Before returning baseSid, check whether a
		// bucketed sid for this request's env already exists in d.owners
		// (can happen when the base owner was evicted but the bucketed owner
		// is still live). If so, return the bucketed sid to avoid creating a
		// duplicate base-sid owner that races with the existing bucketed owner.
		bucketedSid := baseSid + "-env-" + envHash
		d.mu.RLock()
		_, bucketedExists := d.owners[bucketedSid]
		d.mu.RUnlock()
		if bucketedExists {
			return bucketedSid
		}
		return baseSid // no existing entry — base sid is free
	}
	if envCompatible(existingEnv, normalizedReqEnv) {
		return baseSid // compatible — reuse base sid
	}
	derived := baseSid + "-env-" + envHash
	d.logger.Printf("env-incompat under global-first: deriving sid=%s for cmd=%q (base=%s)",
		derived, "...", shortServerID(baseSid))
	return derived
}

// envTransient returns true for env vars that are per-session/transient
// and should NOT affect dedup decisions.
func envTransient(key string) bool {
	switch {
	// Claude Code-specific session vars
	case strings.HasPrefix(key, "CLAUDE_CODE_"):
		return true
	case strings.HasPrefix(key, "CLAUDE_AUTO"):
		return true
	case key == "CLAUDE_CODE_ENTRYPOINT":
		return true
	// Windows Terminal / WSL session vars
	case strings.HasPrefix(key, "WT_"):
		return true
	case key == "SESSIONNAME" || key == "WSLENV":
		return true
	// CR-002 codex PR #121 fix: cwd-derived vars MUST be transient so
	// env-bucketing under global-first does not fragment owners across
	// per-session working directories. Without this, two sessions with
	// identical credentials from different cwds get different env-bucket
	// sids because PWD/OLDPWD/INIT_CWD differ — defeating the global-first
	// dedup goal and recreating per-cwd owner fan-out the spec explicitly
	// targets (Engram #244 Bug 1).
	case key == "PWD" || key == "OLDPWD" || key == "INIT_CWD":
		return true
	// npm launch-noise vars (per-script/cwd, not semantic to MCP identity).
	// Per CodeRabbit PR #121: narrow from blanket `npm_*` prefix so
	// credential-bearing keys like npm_config_registry / npm_config_token
	// REMAIN part of identity — two sessions with different registries
	// must NOT collapse onto a shared owner.
	case strings.HasPrefix(key, "npm_lifecycle_"):
		return true
	case strings.HasPrefix(key, "npm_package_"):
		return true
	case key == "npm_execpath" || key == "npm_node_execpath" || key == "npm_command":
		return true
	// Terminal/shell-derived vars (per-shim-launch transients, not semantic
	// to the upstream MCP server's identity).
	case key == "TERM" || key == "TERM_PROGRAM" || key == "TERM_PROGRAM_VERSION":
		return true
	case key == "COLORTERM" || key == "LINES" || key == "COLUMNS":
		return true
	case key == "_" || key == "SHLVL" || key == "SHELL":
		return true
	// SSH session metadata (per-connection transients). Per CodeRabbit
	// PR #121: do NOT match SSH_AUTH_SOCK / SSH_AGENT_PID — those bind
	// credentials to the upstream and must stay in identity.
	case key == "SSH_CLIENT" || key == "SSH_CONNECTION" || key == "SSH_TTY":
		return true
	// X11/Wayland display vars (per-session transients on Linux desktops).
	case key == "DISPLAY" || key == "XAUTHORITY" || key == "WAYLAND_DISPLAY":
		return true
	}
	return false
}

// Shutdown gracefully stops all owners and the daemon.
func (d *Daemon) Shutdown() {
	d.shutdown(nil)
}

func (d *Daemon) shutdown(beforeDone func()) {
	d.shutdownOnce.Do(func() {
		d.shuttingDown.Store(true)
		d.logger.Printf("daemon shutting down...")

		// Cancel supervisor context first — prevents suture from restarting
		// services as we're tearing down owners one by one. We wait briefly
		// for the supervisor goroutine to exit before proceeding.
		if d.supervisorCancel != nil {
			d.supervisorCancel()
		}
		if d.supervisorErr != nil {
			select {
			case <-d.supervisorErr:
				// Supervisor exited cleanly
			case <-time.After(2 * time.Second):
				d.logger.Printf("supervisor did not exit within 2s, proceeding anyway")
			}
		}

		// Close control server
		if d.ctlSrv != nil {
			d.ctlSrv.Close()
		}
		if d.registryDescriptorPath != "" {
			if _, err := registry.RemoveDescriptorIfOwned(d.registryDescriptorPath, d.registryDescriptor); err != nil {
				d.logger.Printf("registry: remove descriptor %s: %v", d.registryDescriptorPath, err)
			}
		}

		// Shutdown all owners
		d.mu.Lock()
		entries := make([]*OwnerEntry, 0, len(d.owners))
		for _, e := range d.owners {
			entries = append(entries, e)
		}
		d.owners = make(map[string]*OwnerEntry)
		d.mu.Unlock()

		for _, e := range entries {
			if e.Owner != nil {
				e.Owner.Shutdown()
			}
		}

		if beforeDone != nil {
			beforeDone()
		}

		d.logger.Printf("daemon stopped (%d owners shut down)", len(entries))
		close(d.done)
	})
}

// Done returns a channel closed when the daemon has shut down.
func (d *Daemon) Done() <-chan struct{} {
	return d.done
}

// OwnerCount returns the number of fully-created owners (excludes placeholders).
func (d *Daemon) OwnerCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := 0
	for _, e := range d.owners {
		if e.Owner != nil {
			n++
		}
	}
	return n
}

// Entry returns the OwnerEntry for the given server ID, or nil.
func (d *Daemon) Entry(serverID string) *OwnerEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.owners[serverID]
}

// onZeroSessions is called by an owner when its last session disconnects.
func (d *Daemon) onZeroSessions(serverID string) {
	d.mu.Lock()
	entry, ok := d.owners[serverID]
	if ok {
		entry.LastSession = time.Now()
	}
	d.mu.Unlock()

	if ok {
		d.logger.Printf("owner %s: zero sessions (grace period starts)", serverID[:8])
	}
	// Reaper will handle cleanup after grace period
}

// onUpstreamExit is called by an owner when its upstream process exits.
func (d *Daemon) onUpstreamExit(serverID string) {
	d.mu.Lock()
	entry, ok := d.owners[serverID]

	// If the current entry is a placeholder for a different pending owner
	// creation (entry.Owner == nil), this callback is from a PRIOR owner
	// whose entry was already replaced in the registry — typically via the
	// FR-4 zombie-spawn tear-down path which deletes the entry under d.mu
	// and then calls Shutdown outside the lock, after which a concurrent
	// shim can install a fresh placeholder at the same serverID. Acting on
	// the placeholder here would panic on entry.Owner.Shutdown() and would
	// incorrectly delete the placeholder belonging to a completely different
	// spawn goroutine. Skip cleanly — the prior owner's tear-down path is
	// already draining it, and the placeholder will resolve on its own.
	if ok && entry.Owner == nil {
		d.mu.Unlock()
		return
	}

	if ok {
		// Record crash for circuit breaker before any other action.
		cmdKey := entry.Command + " " + strings.Join(entry.Args, " ")
		d.recordCrash(cmdKey)

		if entry.Persistent {
			d.mu.Unlock()
			d.logger.Printf("owner %s: upstream exited, will re-spawn (persistent)", serverID[:8])
			// Reaper handles re-spawn for persistent owners
			return
		}
		d.mu.Unlock()
		d.forgetOwnerIfCurrent(serverID, entry, ownerRemovalReasonUpstreamExit)
		entry.Owner.Shutdown()
		d.logger.Printf("owner %s: upstream exited, removed", shortServerID(serverID))
		return
	}
	d.mu.Unlock()
}

// crashWindow is the time window for crash counting. If an upstream crashes
// crashThreshold times within this window, further spawns are rejected.
const crashWindow = 60 * time.Second
const crashThreshold = 5

// concurrentCreateWaitTimeout is the maximum time a Spawn / findSharedOwner
// goroutine will wait for another goroutine that is currently creating the
// same owner entry (i.e. holds the creating channel). Beyond this, the waiter
// returns a timeout error (in Spawn) or nil (in findSharedOwner).
//
// Note: Spawn does NOT recurse on timeout. The stuck placeholder is still in
// d.owners[sid], so a recursive call would re-enter the same wait, producing a
// cascade of leaked goroutines. Surfacing the error lets the shim retry at a
// higher level and allows the circuit breaker to engage on repeated failures.
//
// Declared as var (not const) so tests can override it. Production code never
// writes to it — treat it as an effective constant in all non-test paths.
var concurrentCreateWaitTimeout = 30 * time.Second

// recordCrash adds a crash timestamp for the given command key.
// Must be called with d.mu held.
func (d *Daemon) recordCrash(cmdKey string) {
	now := time.Now()
	d.crashTracker[cmdKey] = append(d.crashTracker[cmdKey], now)

	// Trim old entries outside the window.
	cutoff := now.Add(-crashWindow)
	crashes := d.crashTracker[cmdKey]
	i := 0
	for i < len(crashes) && crashes[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		d.crashTracker[cmdKey] = crashes[i:]
	}
}

// isCrashLooping returns true if the command has crashed too many times recently.
// Must be called with d.mu held (at least RLock).
func (d *Daemon) isCrashLooping(cmdKey string) bool {
	crashes := d.crashTracker[cmdKey]
	if len(crashes) < crashThreshold {
		return false
	}
	// Count crashes within the window.
	cutoff := time.Now().Add(-crashWindow)
	count := 0
	for _, t := range crashes {
		if !t.Before(cutoff) {
			count++
		}
	}
	return count >= crashThreshold
}

// StatusJSON returns the daemon status as a JSON-encoded byte slice.
func (d *Daemon) StatusJSON() (json.RawMessage, error) {
	status := d.HandleStatus()
	return json.Marshal(status)
}
