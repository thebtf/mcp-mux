// Package owner implements the core multiplexer routing logic for mcp-mux.
// It manages a single upstream process and routes requests from multiple
// downstream sessions through it.
package owner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/listchanged"
	"github.com/thebtf/mcp-mux/muxcore/progress"
	"github.com/thebtf/mcp-mux/muxcore/remap"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/session"
	"github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
	"github.com/thejerf/suture/v4"
)

// Type aliases for session and snapshot types used throughout owner.
type (
	Session         = session.Session
	SessionManager  = session.Manager
	OwnerSnapshot   = snapshot.OwnerSnapshot
	SessionSnapshot = snapshot.SessionSnapshot
)

// Constructor aliases.
var (
	NewSession              = session.NewSession
	NewSessionWithRawWriter = session.NewSessionWithRawWriter
	NewSessionManager       = session.NewManager
)

// Version is the mcp-mux build version, included in status output.
// Auto-detected from Go build info (vcs.revision + vcs.modified).
// Override at build time via: -ldflags "-X github.com/thebtf/mcp-mux/muxcore/owner.Version=..."
var Version = initVersion()

func initVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 8 {
		rev = rev[:8]
	}
	return rev + dirty
}

func nextProactiveNamespace() string {
	return fmt.Sprintf("mux-init-%d-%d", os.Getpid(), proactiveOwnerSequence.Add(1))
}

// InflightRequest holds metadata about a request currently being processed by upstream.
// Used for observability: mux_list --verbose shows what's pending and for how long.
type InflightRequest struct {
	Method    string    `json:"method"`
	Tool      string    `json:"tool,omitempty"`
	SessionID int       `json:"session"`
	StartTime time.Time `json:"started_at"`
}

// proactiveRequest is claimed exactly once by its response or bound upstream death.
type proactiveRequest struct {
	method       string
	initResponse chan struct{}
	upstream     *upstream.Process
	upstreamDone <-chan struct{}
}

var proactiveOwnerSequence atomic.Uint64

// Owner is the multiplexer core. It manages a single upstream process and
// routes requests from multiple downstream sessions through it.
type Owner struct {
	upstream       *upstream.Process
	ipcPath        string
	cwd            string                                                             // primary working directory (from first spawn)
	cwdSet         map[string]bool                                                    // all known cwds (for multi-project roots/list)
	command        string                                                             // upstream command (for status/restart)
	args           []string                                                           // upstream args (for status/restart)
	env            map[string]string                                                  // upstream env captured at spawn (for background respawn)
	handlerFunc    func(ctx context.Context, stdin io.Reader, stdout io.Writer) error // in-process MCP handler (nil = subprocess)
	sessionHandler muxcore.SessionHandler                                             // structured in-process handler (nil = pipe or subprocess)
	// Cached *WithSessionMeta type assertions, populated once when
	// sessionHandler is wired in NewOwner / NewOwnerFromSnapshot /
	// NewOwnerFromHandoff. Hot-path dispatch reads these directly instead
	// of re-running the assertion on every frame (CodeRabbit nitpick on
	// PR #113). nil = handler does not implement the upgrade.
	notificationHandlerWithMeta muxcore.NotificationHandlerWithSessionMeta
	sessionHandlerWithMeta      muxcore.SessionHandlerWithSessionMeta
	upstreamWriter              io.Writer // injected writer (non-nil = skip subprocess, write directly; tests only)
	serverID                    string    // server identity hash
	listener                    net.Listener
	logger                      *log.Logger

	onZeroSessions       func(*Owner)
	onUpstreamExit       func(*Owner)
	onPersistentDetected func(*Owner)
	onPersistentResolved func(*Owner, bool)
	onCacheReady         func(*Owner, OwnerSnapshot) bool
	onCacheInvalidated   func(*Owner)

	// authorizeSession is the optional pre-dispatch session-admission gate
	// forwarded from engine.Config / daemon.Config / OwnerConfig. nil = no
	// gate (pre-v0.24 behaviour). Read-only after construction.
	authorizeSession func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth

	// onFrameReceived is the optional per-frame admission hook forwarded
	// from engine.Config. handleDownstreamMessage invokes it on every
	// inbound frame with a 1 ms budget. nil = no hook (pre-v0.24 behaviour).
	// Read-only after construction.
	onFrameReceived func(sessionID string, frameSize int, method string) muxcore.FrameAction

	mu                   sync.RWMutex
	sessions             map[int]*Session
	cachedInitSessions   map[int]bool // sessions that received a cached (replayed) initialize response
	initDone             bool
	initResp             []byte // cached initialize response (raw JSON-RPC)
	initProtocolVersion  string // protocolVersion from first initialize request (for fingerprint matching)
	toolList             []byte // cached tools/list response (raw JSON-RPC)
	promptList           []byte // cached prompts/list response (raw JSON-RPC)
	resourceList         []byte // cached resources/list response (raw JSON-RPC)
	resourceTemplateList []byte // cached resources/templates/list response (raw JSON-RPC)
	autoClassification   classify.SharingMode
	classificationSource string        // "capability" or "tools" — what determined classification
	classificationReason []string      // tool names that triggered isolation
	classified           chan struct{} // closed when autoClassification is first set
	classifiedOnce       sync.Once
	initReady            chan struct{} // closed when initialize response is cached or upstream dies
	initReadyOnce        sync.Once

	sessionMgr             *SessionManager
	tokenHandshake         bool   // true when daemon manages this owner (shims send token)
	initialAdmissionToken  string // creating shim token preserved when isolation revokes provisional fan-in
	rejectionLogger        *rejectionLogger
	progressOwners         map[string]int      // progressToken → session ID for targeted routing
	progressTokenRequestID map[string]string   // progressToken → remapped request ID that registered it
	requestToTokens        map[string][]string // remapped request ID → list of progress tokens

	progressTracker         *progress.Tracker // dedup state for synthetic progress emission
	upstreamDead            atomic.Bool       // set when upstream exits; prevents sending to dead pipe
	methodTags              sync.Map          // remapped request ID (string) -> method name
	proactiveNamespace      string            // unique across Owner construction and live handoff
	proactiveInitSeq        atomic.Uint64     // per-owner proactive handshake sequence
	proactiveRequests       sync.Map          // proactive response ID -> proactiveRequest
	inflightTracker         sync.Map          // remapped request ID (string) -> *InflightRequest
	timedOutIDs             sync.Map          // remapped request ID (string) -> struct{} — watchdog-claimed IDs, late upstream responses are dropped
	pendingRequests         atomic.Int64
	drainTimeout            time.Duration // from x-mux.drainTimeout capability; 0 = use default
	toolTimeoutNs           atomic.Int64  // from x-mux.toolTimeout capability; stored as nanoseconds for atomic access
	idleTimeoutNs           atomic.Int64  // from x-mux.idleTimeout capability; 0 = use daemon default
	progressIntervalNs      atomic.Int64  // from x-mux.progressInterval capability; stored as nanoseconds; 0 = use default (5s)
	lastActivityNs          atomic.Int64  // unix-nano of last inbound/outbound MCP message or session change
	listChangedAfterRefresh atomic.Bool
	busyMu                  sync.Mutex
	busyDeclarations        map[string]busyDeclaration // busy_id → declaration (long-running work signal)
	startTime               time.Time
	controlServer           *control.Server

	shutdownOnce            sync.Once
	teardownOnce            sync.Once
	removalMu               sync.Mutex
	closeListenerOnce       sync.Once
	isAccepting             atomic.Bool
	admissionFrozen         atomic.Bool
	pendingAdmissionsPurged atomic.Int64
	listenerDone            chan struct{} // closed when IPC listener is intentionally stopped
	done                    chan struct{}

	restartPins atomic.Int64
	// Lock order is upstreamEventMu, materializationMu, admissionMu, then mu.
	// Writers never hold upstreamEventMu across process Close/SoftClose; readers
	// may hold it across bounded session I/O. admissionMu serializes token bind +
	// session registration against coherent classification commits.
	upstreamEventMu                  sync.RWMutex
	materializationMu                sync.Mutex
	admissionMu                      sync.Mutex
	materializationPolicy            MaterializationPolicy
	materializationState             MaterializationState
	materializationTrigger           MaterializationTrigger
	materializationGeneration        uint64
	materializationAttempt           *materializationAttempt
	retiringProcess                  *upstream.Process
	materializationBlockedErr        error
	materializationStop              chan struct{}
	pendingDemands                   map[string]*localDemand
	pendingDemandOrder               []string
	persistentPending                bool
	persistentRequired               bool
	restartResumeTrigger             MaterializationTrigger
	materializationFinalizationProbe func(*upstream.Process) error
	beforeQueuedDemandWrite          func()
	beforeLocalDemandCancel          func()
	afterProactiveStore              func() // test-only registration race seam
	cacheStage                       *cacheStage
}

// OwnerConfig holds parameters for creating an Owner.
type OwnerConfig struct {
	// Command and Args for spawning the upstream MCP server.
	Command string
	Args    []string
	Env     map[string]string

	// Cwd is the working directory for the upstream process.
	// If empty, inherits from the current process.
	Cwd string

	// IPCPath for the Unix domain socket listener.
	IPCPath string

	// ControlPath for the control plane socket. If empty, control plane is disabled.
	ControlPath string

	// ServerID is the server identity hash. Used in callbacks to identify this owner.
	ServerID string

	// OnZeroSessions is called with the exact owner whose last session left.
	// If nil, the owner does not auto-shutdown on zero sessions.
	OnZeroSessions func(*Owner)

	// OnUpstreamExit is called with the exact owner whose controller cannot
	// recover the upstream. If nil, the owner auto-shuts down.
	OnUpstreamExit func(*Owner)

	// OnPersistentDetected latches a fresh generation's early persistent=true
	// declaration before coherent discovery commit.
	OnPersistentDetected func(*Owner)

	// OnCacheReady synchronously publishes one coherent cache/template snapshot.
	// Returning false rejects a stale owner generation and aborts the commit.
	OnCacheReady func(*Owner, OwnerSnapshot) bool

	// OnCacheInvalidated removes only this exact owner's daemon template.
	OnCacheInvalidated func(*Owner)

	// OnPersistentResolved receives a coherent generation's exact persistence
	// verdict for non-template consumers.
	OnPersistentResolved func(*Owner, bool)

	// MaterializationPolicy controls owners created without a live upstream.
	// The zero value is eager, preserving existing direct consumer behavior.
	MaterializationPolicy MaterializationPolicy
	// DeferInitialMaterialization lets the daemon register the exact OwnerEntry
	// before a fast upstream can publish its first cache generation.
	DeferInitialMaterialization bool

	// TokenHandshake enables reading the shim's handshake token from each new IPC
	// connection before the MCP session begins. Only set true when the owner is
	// managed by the global daemon — shims always send a token in that mode.
	// Legacy and test connections do not send a token; leave this false (default).
	TokenHandshake bool
	// PersistentPending protects cache-only restored owners from eviction until
	// live discovery resolves x-mux.persistent.
	PersistentPending bool
	// PersistentRequired is an external daemon policy that fresh discovery may
	// not downgrade.
	PersistentRequired bool

	// HandlerFunc is an in-process MCP server implementation.
	// When set, Owner runs the handler via io.Pipe instead of spawning a subprocess.
	// The handler receives stdin/stdout pipes and should speak JSON-RPC 2.0 on them.
	// Mutually exclusive with Command/Args: if HandlerFunc is set, Command is ignored.
	HandlerFunc func(ctx context.Context, stdin io.Reader, stdout io.Writer) error

	// SessionHandler is a structured in-process MCP server implementation.
	// When set, Owner calls HandleRequest directly for each downstream request
	// instead of routing through a pipe or subprocess.
	// Mutually exclusive with HandlerFunc and Command/Args.
	SessionHandler muxcore.SessionHandler

	// UpstreamWriter, when non-nil, overrides the subprocess upstream.
	// NewOwner will skip spawning Command and writeUpstream will route bytes
	// into this writer instead. Intended for tests that exercise pure
	// JSON-building methods (e.g. respondToRootsList) without the
	// subprocess-timing flake class introduced by "go run mock_server.go".
	//
	// Zero value (nil) preserves the normal subprocess-upstream path used by
	// production and by integration tests that need a real upstream lifecycle
	// (crash loop, reap, snapshot reattach).
	UpstreamWriter io.Writer

	// CachedClassification, if non-empty, skips the upstream classification
	// round-trip. Used by NewOwnerFromHandoff when restoring from daemon
	// handoff — the old daemon already classified the upstream, so the
	// successor adopts its result directly.
	CachedClassification classify.SharingMode

	// AdoptedSnapshot carries the predecessor's coherent cache into a live
	// process-backed handoff. It is in-memory only and adds no persistence field.
	AdoptedSnapshot *OwnerSnapshot

	// AuthorizeSession, when non-nil, gates every accepted session BEFORE
	// AddSession is called. acceptLoop invokes the callback with peer
	// credentials + project context and acts on the returned SessionAuth:
	// AuthDeny closes the connection with JSON-RPC -32000 and never adds
	// the session; AuthAllow stamps SessionMeta.TenantID + AuthorizedAt.
	// Panics are recovered and treated as AuthDeny{Reason: "authorize panic"}.
	// The callback's ctx is bound to the owner's lifecycle: cancelling
	// it (via Owner.Shutdown) interrupts in-flight authorization RPCs.
	// AuthDeny prevents per-session resource cost (session-table insert,
	// dispatch goroutines, frame routing state) but does NOT stop the
	// upstream process — upstream is per-OWNER and spawns at NewOwner
	// time, before the first session arrives. Use a daemon-level
	// pre-spawn check if you must gate upstream creation entirely.
	// nil-default preserves pre-v0.24 behaviour.
	AuthorizeSession func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth

	// OnFrameReceived, when non-nil, is invoked by handleDownstreamMessage
	// for every inbound frame BEFORE dispatch with a 1 ms callback budget
	// (overrun → FramePass; panic → FramePass). See
	// engine.Config.OnFrameReceived for the full semantics.
	// nil-default preserves pre-v0.24 behaviour.
	OnFrameReceived func(sessionID string, frameSize int, method string) muxcore.FrameAction

	// Logger for debug output. Uses log.Default() if nil.
	Logger *log.Logger
}

// NewOwnerFromSnapshot creates a cache-backed Owner without a live upstream.
// The caller chooses eager, persistent, or demand-driven materialization via
// OwnerConfig.MaterializationPolicy after registering the owner in the daemon.
func NewOwnerFromSnapshot(cfg OwnerConfig, snap OwnerSnapshot) (*Owner, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Start IPC listener (no upstream yet)
	ln, err := ipc.Listen(cfg.IPCPath)
	if err != nil {
		return nil, fmt.Errorf("owner from snapshot: listen %s: %w", cfg.IPCPath, err)
	}

	cwdSet := map[string]bool{}
	for _, c := range snap.CwdSet {
		cwdSet[c] = true
	}
	if cfg.Cwd != "" {
		cwdSet[cfg.Cwd] = true
	}
	o := &Owner{
		proactiveNamespace:     nextProactiveNamespace(),
		ipcPath:                cfg.IPCPath,
		cwd:                    cfg.Cwd,
		cwdSet:                 cwdSet,
		command:                cfg.Command,
		args:                   cfg.Args,
		env:                    cfg.Env,
		handlerFunc:            cfg.HandlerFunc,
		sessionHandler:         cfg.SessionHandler,
		upstreamWriter:         cfg.UpstreamWriter,
		serverID:               cfg.ServerID,
		listener:               ln,
		logger:                 logger,
		onZeroSessions:         cfg.OnZeroSessions,
		onUpstreamExit:         cfg.OnUpstreamExit,
		onPersistentDetected:   cfg.OnPersistentDetected,
		onPersistentResolved:   cfg.OnPersistentResolved,
		onCacheReady:           cfg.OnCacheReady,
		onCacheInvalidated:     cfg.OnCacheInvalidated,
		authorizeSession:       cfg.AuthorizeSession,
		onFrameReceived:        cfg.OnFrameReceived,
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		sessionMgr:             NewSessionManager(),
		tokenHandshake:         cfg.TokenHandshake,
		autoClassification:     snap.Classification,
		classificationSource:   snap.ClassificationSource,
		classificationReason:   snap.ClassificationReason,
		classified:             make(chan struct{}),
		initReady:              make(chan struct{}),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		startTime:              time.Now(),
		listenerDone:           make(chan struct{}),
		done:                   make(chan struct{}),
		materializationPolicy:  cfg.MaterializationPolicy,
		materializationState:   MaterializationCacheOnly,
		persistentRequired:     cfg.PersistentRequired,
		materializationStop:    make(chan struct{}),
		pendingDemands:         make(map[string]*localDemand),
		persistentPending:      cfg.PersistentPending,
	}
	if cfg.UpstreamWriter != nil || (cfg.SessionHandler != nil && cfg.HandlerFunc == nil) {
		o.materializationState = MaterializationReady
	}
	o.progressIntervalNs.Store(int64(5 * time.Second))

	if o.tokenHandshake {
		o.rejectionLogger = newRejectionLogger(logger)
	}
	if len(snap.BoundTokens) > 0 {
		imported := o.sessionMgr.ImportBoundHistory(boundTokenSnapshotsToSession(snap.BoundTokens))
		logger.Printf("owner restored %d reconnect token history entries from snapshot", imported)
	}
	// Pre-populate one coherent cache generation from the snapshot.
	o.hydrateSnapshotCache(snap)

	// Close initReady and classified immediately — caches already populated
	o.initReadyOnce.Do(func() { close(o.initReady) })
	o.classifiedOnce.Do(func() { close(o.classified) })

	o.cacheHandlerInterfaces()
	// Wire notifier into sessionHandler if it supports NotifierAware.
	if o.sessionHandler != nil {
		if na, ok := o.sessionHandler.(muxcore.NotifierAware); ok {
			na.SetNotifier(&ownerNotifier{owner: o})
		}
	}

	// Start control plane if configured
	if cfg.ControlPath != "" {
		ctlSrv, err := control.NewServer(cfg.ControlPath, o, logger)
		if err != nil {
			logger.Printf("warning: control server failed to start: %v", err)
		} else {
			o.controlServer = ctlSrv
		}
	}

	// Start accepting IPC connections (sessions get cached replay immediately)
	go o.acceptLoop()

	// Start synthetic progress reporter
	go o.runProgressReporter(doneContext(o.done))

	logger.Printf("owner restored from snapshot (cached: init=%v tools=%v prompts=%v resources=%v)",
		o.initDone, o.toolList != nil, o.promptList != nil, o.resourceList != nil)

	return o, nil
}

// hydrateSnapshotCache installs a previously committed cache generation before
// any owner goroutine starts. Callers must provide exclusive construction-time
// ownership of o.
func (o *Owner) hydrateSnapshotCache(snap OwnerSnapshot) {
	o.autoClassification = snap.Classification
	o.classificationSource = snap.ClassificationSource
	o.classificationReason = append([]string(nil), snap.ClassificationReason...)
	if snap.CachedInit != "" {
		if data, err := Base64Decode(snap.CachedInit); err == nil {
			o.initResp = data
			o.initDone = true
		}
	}
	if snap.CachedTools != "" {
		if data, err := Base64Decode(snap.CachedTools); err == nil {
			o.toolList = data
		}
	}
	if snap.CachedPrompts != "" {
		if data, err := Base64Decode(snap.CachedPrompts); err == nil {
			o.promptList = data
		}
	}
	if snap.CachedResources != "" {
		if data, err := Base64Decode(snap.CachedResources); err == nil {
			o.resourceList = data
		}
	}
	if snap.CachedResourceTemplates != "" {
		if data, err := Base64Decode(snap.CachedResourceTemplates); err == nil {
			o.resourceTemplateList = data
		}
	}
}

// SpawnUpstreamBackground starts the upstream process in a goroutine and runs
// proactive init to refresh caches. Called after NewOwnerFromSnapshot when the
// owner is registered in the daemon. If upstream fails, owner continues serving
// stale cached responses.

// NewOwner creates and starts a new Owner.
// It spawns the upstream process and starts the IPC listener.
// If cfg.HandlerFunc is set, the handler is run in-process via io.Pipe instead
// of spawning a subprocess.
func NewOwner(cfg OwnerConfig) (*Owner, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	ln, err := ipc.Listen(cfg.IPCPath)
	if err != nil {
		return nil, fmt.Errorf("owner: listen %s: %w", cfg.IPCPath, err)
	}

	o := &Owner{
		proactiveNamespace:     nextProactiveNamespace(),
		ipcPath:                cfg.IPCPath,
		cwd:                    cfg.Cwd,
		cwdSet:                 map[string]bool{serverid.CanonicalizePath(cfg.Cwd): true},
		command:                cfg.Command,
		args:                   cfg.Args,
		env:                    cfg.Env,
		handlerFunc:            cfg.HandlerFunc,
		sessionHandler:         cfg.SessionHandler,
		upstreamWriter:         cfg.UpstreamWriter,
		serverID:               cfg.ServerID,
		listener:               ln,
		logger:                 logger,
		onZeroSessions:         cfg.OnZeroSessions,
		onUpstreamExit:         cfg.OnUpstreamExit,
		onPersistentDetected:   cfg.OnPersistentDetected,
		onPersistentResolved:   cfg.OnPersistentResolved,
		onCacheReady:           cfg.OnCacheReady,
		onCacheInvalidated:     cfg.OnCacheInvalidated,
		authorizeSession:       cfg.AuthorizeSession,
		onFrameReceived:        cfg.OnFrameReceived,
		sessions:               make(map[int]*Session),
		cachedInitSessions:     make(map[int]bool),
		sessionMgr:             NewSessionManager(),
		tokenHandshake:         cfg.TokenHandshake,
		classified:             make(chan struct{}),
		initReady:              make(chan struct{}),
		progressOwners:         make(map[string]int),
		progressTokenRequestID: make(map[string]string),
		requestToTokens:        make(map[string][]string),
		progressTracker:        progress.NewTracker(),
		startTime:              time.Now(),
		listenerDone:           make(chan struct{}),
		done:                   make(chan struct{}),
		materializationPolicy:  cfg.MaterializationPolicy,
		materializationState:   MaterializationCacheOnly,
		persistentRequired:     cfg.PersistentRequired,
		materializationStop:    make(chan struct{}),
		pendingDemands:         make(map[string]*localDemand),
		persistentPending:      cfg.PersistentPending,
	}
	o.progressIntervalNs.Store(int64(5 * time.Second))

	if o.tokenHandshake {
		o.rejectionLogger = newRejectionLogger(logger)
	}
	o.cacheHandlerInterfaces()
	if o.sessionHandler != nil {
		if na, ok := o.sessionHandler.(muxcore.NotifierAware); ok {
			na.SetNotifier(&ownerNotifier{owner: o})
		}
	}
	if cfg.ControlPath != "" {
		ctlSrv, ctlErr := control.NewServer(cfg.ControlPath, o, logger)
		if ctlErr != nil {
			logger.Printf("warning: control server failed to start: %v", ctlErr)
		} else {
			o.controlServer = ctlSrv
			logger.Printf("control socket: %s", cfg.ControlPath)
		}
	}

	if cfg.UpstreamWriter != nil || (cfg.SessionHandler != nil && cfg.HandlerFunc == nil) {
		o.materializationState = MaterializationReady
	} else if !cfg.DeferInitialMaterialization {
		a := o.startMaterialization(MaterializationTriggerEager)
		<-a.started
		if a.startedErr != nil {
			o.Shutdown()
			return nil, fmt.Errorf("owner: start upstream: %w", a.startedErr)
		}
	}

	go o.acceptLoop()
	go o.runProgressReporter(doneContext(o.done))
	return o, nil
}

// SessionMgr returns the owner's SessionManager.
// Used for owner session state and observability.
func (o *Owner) SessionMgr() *SessionManager {
	return o.sessionMgr
}

// PendingAdmissionsPurged reports reservations revoked when admission closed.
// Daemon removal accounting combines this with any final manager cleanup.
func (o *Owner) PendingAdmissionsPurged() int {
	return int(o.pendingAdmissionsPurged.Load())
}

// PreRegister atomically reserves a token for a fresh shim while this owner is
// accepting fresh consumers. A classified-isolated owner keeps its listener
// alive for exact-token reconnects, but generic fresh admission remains closed.
// admissionMu precedes the owner and session-manager locks so classification
// commit can atomically freeze and revoke provisional fan-in.
func (o *Owner) PreRegister(token, cwd string, env map[string]string) bool {
	if o.admissionFrozen.Load() {
		return false
	}
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.admissionFrozen.Load() {
		return false
	}
	select {
	case <-o.listenerDone:
		return false
	default:
	}
	if o.autoClassification == classify.ModeIsolated {
		return false
	}
	o.sessionMgr.PreRegisterForOwner(token, o.serverID, cwd, env)
	return true
}

// PreRegisterInitial reserves the creating shim's token. Unlike PreRegister,
// it remains valid if proactive initialization classifies the new owner as
// isolated before Daemon.Spawn returns. Only one distinct initial token may be
// installed; later fresh consumers must use PreRegister and are rejected once
// isolation is known.
func (o *Owner) PreRegisterInitial(token, cwd string, env map[string]string) bool {
	deadline := time.Now().Add(materializationReadinessTimeout)
	for {
		o.admissionMu.Lock()
		o.mu.Lock()
		if o.admissionFrozen.Load() {
			o.mu.Unlock()
			o.admissionMu.Unlock()
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(time.Millisecond)
			continue
		}
		select {
		case <-o.listenerDone:
			o.mu.Unlock()
			o.admissionMu.Unlock()
			return false
		default:
		}
		if o.initialAdmissionToken != "" && o.initialAdmissionToken != token {
			o.mu.Unlock()
			o.admissionMu.Unlock()
			return false
		}
		o.initialAdmissionToken = token
		o.sessionMgr.PreRegisterForOwner(token, o.serverID, cwd, env)
		o.mu.Unlock()
		o.admissionMu.Unlock()
		return true
	}
}

func (o *Owner) revokePendingAdmission(token string) {
	if token != "" {
		o.sessionMgr.RemovePendingForOwnerToken(o.ServerID(), token)
	}
}

// readToken reads the handshake token sent by the shim immediately after connecting.
// The token is a hex string terminated by '\n'. Uses byte-by-byte reading to avoid
// consuming subsequent MCP messages that immediately follow the token line.
// Returns (token, conn) where conn may be wrapped with io.MultiReader if the first
// bytes were not a token (backward compat with old shims that send MCP directly).
func readToken(conn net.Conn) (string, io.Reader) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", conn
	}
	defer conn.SetReadDeadline(time.Time{}) // clear deadline after read

	buf := make([]byte, 0, 32)
	one := make([]byte, 1)
	for {
		_, err := conn.Read(one)
		if err != nil {
			// Timeout or error — return whatever we read prepended back
			if len(buf) > 0 {
				return "", io.MultiReader(bytes.NewReader(buf), conn)
			}
			return "", conn
		}
		if one[0] == '\n' {
			break
		}
		// First byte check: hex tokens are [0-9a-f]. If first byte is '{' or '[',
		// this is an MCP message from an old shim — prepend and return.
		if len(buf) == 0 && !isHexChar(one[0]) {
			return "", io.MultiReader(bytes.NewReader(one), conn)
		}
		buf = append(buf, one[0])
		if len(buf) > 64 { // tokens are 16 hex chars; >64 means something is wrong
			return "", io.MultiReader(bytes.NewReader(buf), conn)
		}
	}
	return string(buf), conn
}

// isHexChar returns true if b is a valid hex character [0-9a-f].
func isHexChar(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// Base64Decode decodes a base64-encoded string. Exported for snapshot loading.
func Base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// extractToolName extracts params.name from a tools/call JSON-RPC request.
// Returns empty string for non-tools/call requests or malformed params.
func extractToolName(raw []byte) string {
	var req struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &req) == nil && req.Params.Name != "" {
		return req.Params.Name
	}
	return ""
}

// AddSession registers a new downstream session and starts routing its messages.
// This is used for the owner's own stdio session (first client).
func (o *Owner) AddSession(s *Session) {
	o.admissionMu.Lock()
	o.addSessionLocked(s)
	o.admissionMu.Unlock()
	o.startRegisteredSession(s)
}

// addSessionLocked atomically publishes one admitted session. The caller holds
// admissionMu so an isolated classification commit cannot split token Bind
// from owner/session-manager registration.
func (o *Owner) addSessionLocked(s *Session) {
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	o.sessionMgr.RegisterSession(s, s.Cwd)
}

func (o *Owner) startRegisteredSession(s *Session) {
	o.touchActivity()
	o.logger.Printf("session %d connected (cwd: %q)", s.ID, s.Cwd)

	if o.sessionHandler != nil {
		if lc, ok := o.sessionHandler.(muxcore.ProjectLifecycle); ok {
			project := muxcore.ProjectContext{
				ID:  muxcore.ProjectContextID(s.Cwd),
				Cwd: s.Cwd,
				Env: s.Env,
			}
			go lc.OnProjectConnect(project)
		}
	}

	// Notify upstream that roots may have changed (new client = new potential root)
	o.sendRootsListChanged()
	go o.readSession(s)
}

// readSession reads messages from a session and forwards them to the upstream.
func (o *Owner) readSession(s *Session) {
	defer func() {
		o.removeSession(s)
		s.Close()
	}()

	for {
		msg, err := s.ReadMessage()
		if err != nil {
			if err != io.EOF {
				o.logger.Printf("session %d read error: %v", s.ID, err)
			}
			return
		}

		if err := o.handleDownstreamMessage(s, msg); err != nil {
			o.logger.Printf("session %d handle error: %v", s.ID, err)
		}
	}
}

// handleDownstreamMessage processes a message from a downstream session.
func (o *Owner) handleDownstreamMessage(s *Session, msg *jsonrpc.Message) error {
	// OnFrameReceived hook (FR-4 / NFR-2) — invoked synchronously on the
	// reader goroutine for every inbound frame, AFTER IsNotification /
	// IsRequest classification but BEFORE any dispatch / cache replay /
	// upstream forwarding. nil-default skips the hook entirely (byte-
	// identical to v0.23). Scope is inbound-only (CHK014): outbound
	// responses written by SessionHandler and synthetic notifications
	// generated by the Owner never enter this function.
	switch o.invokeFrameHook(s, msg) {
	case muxcore.FrameDrop:
		// Silent discard — no dispatch, no client response.
		return nil
	case muxcore.FrameError:
		// JSON-RPC 2.0 §4.1 forbids any response (including errors) to a
		// notification. When the hook returns FrameError for a notification
		// we degrade to silent FrameDrop semantics — emitting `{"id":null,
		// ...}` would produce a structurally valid JSON-RPC response but
		// violate the protocol's notification contract and confuse strict
		// clients. Documented as the "FrameError on notification → drop"
		// rule (Gemini code review #113).
		if msg.IsNotification() {
			o.logger.Printf("frame_hook_error_on_notification_dropped sid=%d method=%s", s.ID, msg.Method)
			return nil
		}
		// Request path: respond with -32004 ('rate limited') preserving
		// msg.ID so the client can correlate the failure with the
		// originating request.
		errBytes, marshalErr := buildJSONRPCErrorBytes(msg.ID, -32004, "rate limited")
		if marshalErr != nil {
			o.logger.Printf("frame_hook_error_marshal sid=%d method=%s err=%v", s.ID, msg.Method, marshalErr)
			return nil
		}
		return s.WriteRaw(errBytes)
	}
	// FramePass (zero value, default) — fall through to normal dispatch.

	switch {
	case msg.IsNotification():
		// Suppress notifications/initialized for sessions that received a cached initialize response
		if msg.Method == "notifications/initialized" {
			o.mu.RLock()
			wasCached := o.cachedInitSessions[s.ID]
			o.mu.RUnlock()
			if wasCached {
				o.logger.Printf("session %d: suppressing notifications/initialized (cached init)", s.ID)
				return nil
			}
		}
		// Remap requestId for cancellation notifications so upstream can find the right in-flight request
		if msg.Method == "notifications/cancelled" {
			return o.forwardCancelledNotification(s, msg)
		}
		// NotificationHandler dispatch for SessionHandler mode. Prefer the
		// *WithSessionMeta upgrade when implemented (FR-1, EC-7) — handlers
		// that satisfy both interfaces see ONLY the WithSessionMeta path.
		// Cached at owner construction (cacheHandlerInterfaces) so the
		// per-frame hot path skips a type assertion (CodeRabbit nitpick).
		if o.sessionHandler != nil {
			project := muxcore.ProjectContext{
				ID:  muxcore.ProjectContextID(s.Cwd),
				Cwd: s.Cwd,
				Env: s.Env,
			}
			if o.notificationHandlerWithMeta != nil {
				meta := s.Meta()
				go o.notificationHandlerWithMeta.HandleNotificationWithSessionMeta(context.Background(), project, meta, msg.Raw)
			} else if nh, ok := o.sessionHandler.(muxcore.NotificationHandler); ok {
				go nh.HandleNotification(context.Background(), project, msg.Raw)
			}
			return nil // notifications don't need forwarding to upstream
		}
		// Forward other notifications as-is to upstream.
		if !o.hasWritableUpstream() {
			return nil // snapshot owner — upstream not yet spawned, drop notification
		}
		return o.forwardNotificationIfReady(msg.Raw)

	case msg.IsRequest():
		// Replay from cache if available (avoids upstream round-trip).
		// Check cache BEFORE upstream-dead check: snapshot owners have nil upstream
		// but valid caches — they must serve from cache, not return error.
		if cached := o.getCachedResponse(msg.Method); cached != nil {
			// For initialize: verify protocolVersion matches before replaying
			if msg.Method == "initialize" && !o.initFingerprintMatches(msg.Raw) {
				o.logger.Printf("session %d: initialize fingerprint mismatch, forwarding to upstream", s.ID)
				// Fall through to forward to upstream
			} else {
				return o.replayFromCache(s, msg, cached)
			}
		}

		// SessionHandler dispatch: structured path without pipe/remap/_meta.
		// Must come BEFORE the upstream nil check so SessionHandler-only owners
		// (where o.upstream == nil by design) can serve requests normally.
		if o.sessionHandler != nil {
			return o.dispatchToSessionHandler(s, msg)
		}

		if !o.materializationReadyForRequests() {
			queued, err := o.enqueueLocalDemand(s, msg)
			if queued || err != nil {
				return err
			}
		}
		return o.forwardRequestNow(s, msg)

	case msg.IsResponse():
		// Client is responding to a server→client request (e.g., sampling/createMessage).
		// Forward as-is — the ID belongs to the upstream's request, no remapping needed.
		o.logger.Printf("session %d: forwarding client response to upstream (id=%s)", s.ID, string(msg.ID))
		return o.writeUpstream(msg.Raw)

	default:
		return fmt.Errorf("unexpected message type from downstream: %s", msg.Type)
	}
}

// dispatchToSessionHandler calls the SessionHandler directly instead of writing
// to the pipe. Runs in a goroutine for concurrent request handling.
func (o *Owner) dispatchToSessionHandler(s *Session, msg *jsonrpc.Message) error {
	project := muxcore.ProjectContext{
		ID:  muxcore.ProjectContextID(s.Cwd),
		Cwd: s.Cwd,
		Env: s.Env,
	}

	o.pendingRequests.Add(1)

	go func() {
		defer o.decrementPending()
		defer func() {
			if r := recover(); r != nil {
				o.logger.Printf("session %d: HandleRequest panic: %v", s.ID, r)
				if errResp, marshalErr := buildJSONRPCErrorBytes(msg.ID, -32603, "internal error: handler panic"); marshalErr == nil {
					s.WriteRaw(errResp)
				}
			}
		}()

		// Build context: cancel on session disconnect or owner shutdown, and
		// optionally apply the per-call tool timeout. The WithTimeout wrapping
		// MUST happen before the watcher goroutine is spawned — otherwise the
		// goroutine captures the outer ctx and races the parent's ctx
		// reassignment on the `ctx, timeoutCancel = context.WithTimeout(...)`
		// line (caught by -race as a data race on the ctx variable,
		// dispatch_test.go:TestDispatchToSessionHandler_Timeout on CI).
		// The semantic bug is real too: if toolTimeout fires before s.Done()
		// or o.done, the watcher is still blocking on the OLD ctx.Done() and
		// leaks until one of the other two triggers.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if timeout := o.toolTimeoutNs.Load(); timeout > 0 {
			var timeoutCancel context.CancelFunc
			ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(timeout))
			defer timeoutCancel()
		}

		go func() {
			select {
			case <-s.Done():
				cancel()
			case <-o.done:
				cancel()
			case <-ctx.Done():
			}
		}()

		// Prefer the *WithSessionMeta upgrade when implemented (FR-2, EC-7) —
		// handlers that satisfy both interfaces see ONLY the WithSessionMeta
		// path. Cached at owner construction (cacheHandlerInterfaces) so
		// the per-frame hot path skips a type assertion (CodeRabbit
		// nitpick on PR #113).
		var resp []byte
		var err error
		if o.sessionHandlerWithMeta != nil {
			resp, err = o.sessionHandlerWithMeta.HandleRequestWithSessionMeta(ctx, project, s.Meta(), msg.Raw)
		} else {
			resp, err = o.sessionHandler.HandleRequest(ctx, project, msg.Raw)
		}
		if err != nil {
			// Route through buildJSONRPCErrorBytes so error messages with JSON-special
			// characters (quotes, backslashes, newlines, Windows paths, control bytes)
			// are properly escaped and produce valid JSON for the CC client.
			var errMsg string
			if ctx.Err() != nil {
				errMsg = "request timeout"
			} else {
				errMsg = err.Error()
			}
			if errBytes, marshalErr := buildJSONRPCErrorBytes(msg.ID, -32603, errMsg); marshalErr == nil {
				resp = errBytes
			} else {
				o.logger.Printf("session %d: failed to marshal error response: %v", s.ID, marshalErr)
				resp = nil
			}
		}

		if resp != nil {
			s.WriteRaw(resp)
		}
	}()

	return nil
}

// sendProactiveInit sends an initialize request to the upstream immediately after
// starting, without waiting for a session to connect. This enables the init-ready
// gate: Daemon.Spawn blocks until init is cached, but init requires a request.
// The response is cached by handleUpstreamMessage → cacheResponse("initialize"),
// which closes initReady. Subsequent sessions get instant cached replay.
//
// Also sends notifications/initialized and tools/list to pre-populate caches.
func (o *Owner) sendProactiveInit(proc *upstream.Process, afterInitialized func()) {
	sequence := o.proactiveInitSeq.Add(1)
	initID := fmt.Sprintf("%s-%d-0", o.proactiveNamespace, sequence)
	initKey := strconv.Quote(initID)
	initResponse := make(chan struct{})
	if !o.registerProactive(initKey, proactiveRequest{method: "initialize", initResponse: initResponse, upstream: proc, upstreamDone: proc.Done}) {
		o.markCurrentUpstreamDead(proc)
		if afterInitialized != nil {
			afterInitialized()
		}
		return
	}
	initReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"mcp-mux","version":"1.0.0"}}}`, initKey)

	complete := func() {}
	if afterInitialized != nil {
		complete = sync.OnceFunc(afterInitialized)
	}
	if err := proc.WriteLine([]byte(initReq)); err != nil {
		if _, claimed := o.claimProactive(initKey); claimed {
			o.decrementPending()
		}
		o.failProactiveGeneration(proc)
		o.logger.Printf("proactive init: write failed: %v", err)
		complete()
		return
	}
	o.logger.Printf("proactive init: sent initialize request")

	go func() {
		defer complete()
		select {
		case <-initResponse:
		case <-proc.Done:
			o.markCurrentUpstreamDead(proc)
			return
		case <-o.done:
			return
		}

		if err := proc.WriteLine([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
			o.failProactiveGeneration(proc)
			o.logger.Printf("proactive init: notifications/initialized failed: %v", err)
			return
		}
		complete()
		toolsID := fmt.Sprintf("%s-%d-1", o.proactiveNamespace, sequence)
		toolsKey := strconv.Quote(toolsID)
		if !o.registerProactive(toolsKey, proactiveRequest{method: "tools/list", upstream: proc, upstreamDone: proc.Done}) {
			return
		}
		toolsReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/list","params":{}}`, toolsKey)
		if err := proc.WriteLine([]byte(toolsReq)); err != nil {
			if _, claimed := o.claimProactive(toolsKey); claimed {
				o.decrementPending()
			}
			o.failProactiveGeneration(proc)
			o.logger.Printf("proactive init: tools/list failed: %v", err)
		}
	}()
}

// readUpstream reads responses from one upstream generation and routes them to
// the correct session.
func (o *Owner) readUpstream(proc *upstream.Process) {
	if proc == nil {
		return // SessionHandler-only mode — no upstream to read
	}
	defer func() {
		o.markCurrentUpstreamDead(proc)
	}()
	for {
		line, err := proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				o.logger.Printf("upstream read error: %v", err)
			}
			return
		}

		o.touchActivity()
		msg, err := jsonrpc.Parse(line)
		if err != nil {
			o.logger.Printf("upstream parse error: %v", err)
			continue
		}

		if err := o.handleUpstreamMessageFrom(proc, msg); err != nil {
			o.logger.Printf("upstream handle error: %v", err)
		}
	}
}

func (o *Owner) monitorUpstreamExit(proc *upstream.Process) {
	select {
	case <-proc.Done:
	case <-o.materializationStop:
		return
	}
	if !o.isCurrentUpstream(proc) {
		o.monitorMaterializedProcessExit(proc)
		return
	}
	o.logger.Printf("upstream exited: %v", proc.ExitErr)
	o.initReadyOnce.Do(func() {
		if o.initReady != nil {
			close(o.initReady)
		}
	})
	o.monitorMaterializedProcessExit(proc)
}

func (o *Owner) isCurrentUpstream(proc *upstream.Process) bool {
	o.mu.RLock()
	current := o.upstream
	o.mu.RUnlock()
	return current == proc
}

func (o *Owner) notifyUpstreamExit() {
	if o.onUpstreamExit != nil {
		o.onUpstreamExit(o)
		return
	}
	o.Shutdown()
}

func (o *Owner) failProactiveGeneration(proc *upstream.Process) {
	o.markCurrentUpstreamDead(proc)
	_ = proc.Close()
}

func (o *Owner) markCurrentUpstreamDead(proc *upstream.Process) bool {
	o.drainProactiveRequests(proc)
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.RLock()
	current := o.upstream == proc
	o.mu.RUnlock()
	if !current {
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		return false
	}
	o.upstreamDead.Store(true)
	responses := o.claimInflightRequests()
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	o.deliverInflightResponses(responses)
	return true
}

func (o *Owner) spawnReplacementUpstream(launch LaunchContext) (*upstream.Process, error) {
	if o.handlerFunc != nil {
		o.logger.Printf("upstream respawn: in-process handler")
		return upstream.NewProcessFromHandler(context.Background(), o.handlerFunc), nil
	}
	if o.command == "" {
		return nil, errors.New("upstream command is empty")
	}
	return upstream.Start(o.command, o.args, launch.Env, launch.Cwd, o.logger)
}

// isProactiveID identifies only this Owner's proactive namespace. A response
// for another Owner, including a predecessor from live handoff, is never ours.
func (o *Owner) isProactiveID(id json.RawMessage) bool {
	value, err := strconv.Unquote(string(id))
	return err == nil && o.proactiveNamespace != "" && strings.HasPrefix(value, o.proactiveNamespace+"-")
}

func (o *Owner) registerProactive(id string, request proactiveRequest) bool {
	o.pendingRequests.Add(1)
	o.proactiveRequests.Store(id, request)
	if o.afterProactiveStore != nil {
		o.afterProactiveStore()
	}
	select {
	case <-request.upstreamDone:
		if _, claimed := o.claimProactive(id); claimed {
			o.decrementPending()
		}
		return false
	default:
		return true
	}
}

func (o *Owner) claimProactive(id string) (proactiveRequest, bool) {
	request, ok := o.proactiveRequests.LoadAndDelete(id)
	if !ok {
		return proactiveRequest{}, false
	}
	return request.(proactiveRequest), true
}

// decrementPending atomically decrements pendingRequests, clamping at zero.
// Without the clamp, a late proactive-init response arriving after upstream
// death (its original Add(1) happened before death, the matching Add(-1)
// fires via drainInflight and again via the actual response path) drove the
// counter into negative territory. That showed up as `pending_requests: -1`
// in mux_list / status output. Cosmetic but misleading — this guard keeps
// the counter semantically accurate.
func (o *Owner) decrementPending() {
	for {
		cur := o.pendingRequests.Load()
		if cur <= 0 {
			return
		}
		if o.pendingRequests.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// handleUpstreamMessage routes a synthetic/test message without generation
// staging. Live process readers call handleUpstreamMessageFrom.
func (o *Owner) handleUpstreamMessage(msg *jsonrpc.Message) error {
	return o.handleUpstreamMessageFrom(nil, msg)
}

func (o *Owner) handleUpstreamMessageFrom(proc *upstream.Process, msg *jsonrpc.Message) error {
	if proc == nil {
		return o.handleUpstreamMessageFromLocked(proc, msg)
	}
	o.upstreamEventMu.RLock()
	defer o.upstreamEventMu.RUnlock()
	if !o.acceptsProcessEvent(proc) {
		o.logger.Printf("dropping stale upstream message pid=%d", proc.PID())
		return nil
	}
	return o.handleUpstreamMessageFromLocked(proc, msg)
}

// handleUpstreamMessageFromLocked handles one live process event while its
// caller holds upstreamEventMu for reading. Synthetic test messages pass nil.
func (o *Owner) handleUpstreamMessageFromLocked(proc *upstream.Process, msg *jsonrpc.Message) error {
	if msg.IsNotification() {
		if proc != nil && o.stageMaterializationListChanged(proc, msg.Method) {
			return nil
		}
		// Route progress notifications to owning session instead of broadcast.
		//
		// If the token is NOT in progressOwners (e.g. the originating request
		// has already completed and its token was cleared, or the upstream
		// server invented a token instead of echoing the client-supplied one),
		// we DROP the notification. Broadcasting to all sessions was the old
		// fallback and is actively harmful: the MCP client receives a
		// notifications/progress with a token it has no record of and logs
		// "Received a progress notification for an unknown token" — which
		// some clients (Claude Code) treat as a transport-level protocol
		// error and tear the stdio connection down. Observed in production
		// with netcoredbg-mcp debugger long-polls: the first unknown-token
		// progress arriving after a tool completed caused CC to close the
		// stdio transport, which in turn killed the shim, which in turn
		// required a manual `/mcp` reconnect. Dropping silently (with a log
		// line) is strictly safer than broadcasting.
		if msg.Method == "notifications/progress" {
			if err := o.routeProgressNotification(msg.Raw); err != nil {
				o.logger.Printf("drop notifications/progress: %v (preventing transport tear-down in MCP client)", err)
			}
			return nil
		}
		// x-mux busy protocol: upstream declares long-running background work
		// so the reaper does not idle-kill it. Consumed at the mux layer —
		// not forwarded to sessions (the busy signal is a mux contract, not
		// an MCP-client concern).
		if msg.Method == "notifications/x-mux/busy" {
			o.handleBusyNotification(msg.Raw)
			return nil
		}
		if msg.Method == "notifications/x-mux/idle" {
			o.handleIdleNotification(msg.Raw)
			return nil
		}
		return o.broadcast(msg.Raw)
	}

	if msg.IsRequest() {
		return o.handleUpstreamRequest(msg)
	}
	if !msg.IsResponse() {
		return fmt.Errorf("unexpected message type from upstream: %s", msg.Type)
	}
	if request, claimed := o.claimProactive(string(msg.ID)); claimed {
		o.decrementPending()
		if request.upstream != proc || !o.isCurrentUpstream(request.upstream) {
			return nil
		}
		select {
		case <-request.upstreamDone:
			return nil
		default:
		}
		o.cacheResponse(request.method, msg.Raw)
		if request.initResponse != nil {
			close(request.initResponse)
		}
		return nil
	}
	if o.isProactiveID(msg.ID) {
		if methodRaw, ok := o.methodTags.LoadAndDelete(string(msg.ID)); ok {
			o.decrementPending()
			o.sessionMgr.CompleteRequest(string(msg.ID))
			o.clearProgressTokensForRequest(string(msg.ID))
			o.cacheResponseFromLocked(proc, methodRaw.(string), msg.Raw)
		}
		return nil
	}

	// If the watchdog already claimed this request as timed-out, drop the
	// late upstream response to prevent duplicate delivery to the session.
	if _, timedOut := o.timedOutIDs.LoadAndDelete(string(msg.ID)); timedOut {
		o.logger.Printf("dropping late response for timed-out request id=%s", string(msg.ID))
		if methodRaw, ok := o.methodTags.LoadAndDelete(string(msg.ID)); ok {
			o.cacheResponseFromLocked(proc, methodRaw.(string), msg.Raw)
		}
		o.clearProgressTokensForRequest(string(msg.ID))
		return nil
	}

	// Ordinary responses must win their inflight claim before they can affect
	// pending state, caches, progress ownership, or a downstream session.
	if _, claimed := o.inflightTracker.LoadAndDelete(string(msg.ID)); !claimed {
		return nil
	}
	o.decrementPending()
	o.sessionMgr.CompleteRequest(string(msg.ID))
	o.clearProgressTokensForRequest(string(msg.ID))

	if methodRaw, ok := o.methodTags.LoadAndDelete(string(msg.ID)); ok {
		o.cacheResponseFromLocked(proc, methodRaw.(string), msg.Raw)
	}

	// Deremap the ID to find the target session
	result, err := remap.Deremap(msg.ID)
	if err != nil {
		return fmt.Errorf("deremap response id: %w", err)
	}

	o.logger.Printf("response routing: id=%s → session %d (original id=%s)",
		string(msg.ID), result.SessionID, string(result.OriginalID))

	// Replace the remapped ID with the original
	restored, err := jsonrpc.ReplaceID(msg.Raw, result.OriginalID)
	if err != nil {
		return fmt.Errorf("restore id: %w", err)
	}

	// Send to the correct session
	o.mu.RLock()
	session, ok := o.sessions[result.SessionID]
	o.mu.RUnlock()

	if !ok {
		o.logger.Printf("session %d not found for response (may have disconnected)", result.SessionID)
		return nil
	}

	if err := session.WriteRaw(restored); err != nil {
		o.logger.Printf("session %d: response write error: %v", result.SessionID, err)
		return err
	}
	o.logger.Printf("session %d: response delivered (%d bytes)", result.SessionID, len(restored))
	return nil
}

// handleUpstreamRequest handles server→client requests from the upstream.
// Most requests (roots/list, sampling, elicitation) are forwarded to the active
// session — CC itself responds with correct per-session data.
func (o *Owner) handleUpstreamRequest(msg *jsonrpc.Message) error {
	switch msg.Method {
	case "roots/list":
		// Forward to the active session — CC knows its own roots.
		// Falls back to local respondToRootsList if no active session.
		return o.routeToLastActiveSession(msg)
	case "ping":
		// Respond locally with empty result — no client involvement needed
		o.logger.Printf("upstream sent ping, responding locally")
		return o.respondToPing(msg.ID)
	default:
		// Route to the last active session (e.g., sampling/createMessage, elicitation/create)
		return o.routeToLastActiveSession(msg)
	}
}

// respondToRootsList sends a roots/list response to the upstream with ALL known cwds.
// Used as fallback when no active session can handle roots/list directly.
// In normal operation, roots/list is forwarded to the active session (CC answers).
func (o *Owner) respondToRootsList(id json.RawMessage) error {
	type rootEntry struct {
		URI  string `json:"uri"`
		Name string `json:"name,omitempty"`
	}

	o.mu.RLock()
	var roots []rootEntry
	for cwd := range o.cwdSet {
		if cwd != "" {
			roots = append(roots, rootEntry{URI: pathToFileURI(cwd), Name: filepath.Base(cwd)})
		}
	}
	o.mu.RUnlock()

	// Fallback if no cwds registered
	if len(roots) == 0 {
		fallback, _ := os.Getwd()
		roots = []rootEntry{{URI: pathToFileURI(fallback), Name: filepath.Base(fallback)}}
	}

	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Roots []rootEntry `json:"roots"`
		} `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
	}
	resp.Result.Roots = roots

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal roots response: %w", err)
	}

	return o.writeUpstream(data)
}

// respondToPing sends an empty result to the upstream in response to a ping.
func (o *Owner) respondToPing(id json.RawMessage) error {
	resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(id))
	return o.writeUpstream([]byte(resp))
}

// routeToLastActiveSession forwards a server→client request to the most recently
// active session with an in-flight request. Uses SessionManager.ResolveCallback()
// for causal routing. Falls back to local responses when no session is available.
func (o *Owner) routeToLastActiveSession(msg *jsonrpc.Message) error {
	ctx := o.sessionMgr.ResolveCallback()
	if ctx == nil {
		o.logger.Printf("no active session for server request %s", msg.Method)
		switch msg.Method {
		case "roots/list":
			// Fallback: answer locally from cwdSet when no session can handle it
			o.logger.Printf("roots/list fallback: responding locally with cwdSet")
			return o.respondToRootsList(msg.ID)
		case "elicitation/create":
			return o.respondToElicitationCancel(msg.ID)
		default:
			return o.respondWithError(msg.ID, -32603, "no active session available")
		}
	}

	session := ctx.Session
	o.logger.Printf("routing server request %s to session %d (async)", msg.Method, session.ID)
	// Use async SendNotification to prevent blocking readUpstream.
	// Server→client requests (roots/list, sampling) go through the notification
	// channel so upstream can continue producing output. The session's drain
	// goroutine delivers it to CC, and CC responds when ready.
	session.SendNotification(msg.Raw)
	return nil
}

// respondToElicitationCancel sends an elicitation cancel response to upstream.
func (o *Owner) respondToElicitationCancel(id json.RawMessage) error {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Action string `json:"action"`
		} `json:"result"`
	}{JSONRPC: "2.0", ID: id}
	resp.Result.Action = "cancel"
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal elicitation cancel: %w", err)
	}
	return o.writeUpstream(data)
}

// respondWithError sends a JSON-RPC error response to upstream.
func (o *Owner) respondWithError(id json.RawMessage, code int, message string) error {
	data, err := buildJSONRPCErrorBytes(id, code, message)
	if err != nil {
		return fmt.Errorf("marshal error response: %w", err)
	}
	return o.writeUpstream(data)
}

// buildJSONRPCErrorBytes constructs a JSON-RPC 2.0 error response as bytes.
// Uses json.Marshal on a struct to guarantee valid escaping for any message
// content — plain strings, quotes, backslashes, newlines, Windows paths,
// and control characters all round-trip correctly.
//
// Callers that need the response bytes (e.g., to send to a downstream session
// rather than upstream) use this directly; callers that want the full
// write-to-upstream side effect use respondWithError instead.
func buildJSONRPCErrorBytes(id json.RawMessage, code int, message string) ([]byte, error) {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	resp.Error.Code = code
	resp.Error.Message = message
	return json.Marshal(resp)
}

// routeProgressNotification sends a notifications/progress to the session that
// owns the progressToken, instead of broadcasting to all sessions.
//
// Uses the session's async notification channel (SendNotification) rather than
// synchronous WriteRaw. This matches how broadcast() and server-to-client
// requests are delivered elsewhere in the codebase, and prevents a single slow
// session from stalling the upstream reader loop for up to the write-deadline
// (30 s). Progress notifications are strictly informational — dropping under
// backpressure is preferable to blocking the upstream multiplexer.
func (o *Owner) routeProgressNotification(raw []byte) error {
	var notif struct {
		Params struct {
			ProgressToken json.RawMessage `json:"progressToken"`
			Total         *float64        `json:"total"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &notif); err != nil {
		return fmt.Errorf("parse progress notification: %w", err)
	}
	if notif.Params.ProgressToken == nil {
		return fmt.Errorf("no progressToken in notification")
	}

	token := string(notif.Params.ProgressToken)

	o.mu.RLock()
	sessionID, ok := o.progressOwners[token]
	session := o.sessions[sessionID]
	o.mu.RUnlock()

	if !ok || session == nil {
		return fmt.Errorf("no owner for progressToken %s", token)
	}

	session.SendNotification(raw)

	// Record that real progress arrived so the synthetic reporter can back off.
	o.recordRealProgress(token, notif.Params.Total != nil)
	return nil
}

// trackProgressToken extracts _meta.progressToken from a request and records
// which session owns it, enabling targeted progress notification routing.
// requestID is the remapped (upstream-facing) request ID; it is stored so that
// clearProgressTokensForRequest can clean up when the response arrives.
func (o *Owner) trackProgressToken(sessionID int, requestID string, raw []byte) {
	var req struct {
		Params struct {
			Meta struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	if req.Params.Meta.ProgressToken == nil {
		return
	}
	token := string(req.Params.Meta.ProgressToken)
	o.mu.Lock()
	o.progressOwners[token] = sessionID
	o.progressTokenRequestID[token] = requestID
	o.requestToTokens[requestID] = append(o.requestToTokens[requestID], token)
	o.mu.Unlock()
}

// clearProgressTokensForRequest removes all progress tokens associated with the
// given remapped request ID. Called when:
//   - the upstream response for that request arrives (normal completion),
//   - notifications/cancelled is received for that request (client abort), or
//   - the owning session is removed (session died before the call completed).
//
// Must be called with o.mu held for write, or acquire it internally.
// This variant acquires the lock itself (safe to call from any goroutine).
func (o *Owner) clearProgressTokensForRequest(requestID string) {
	o.mu.Lock()
	tokens := o.requestToTokens[requestID]
	for _, token := range tokens {
		delete(o.progressOwners, token)
		delete(o.progressTokenRequestID, token)
	}
	delete(o.requestToTokens, requestID)
	o.mu.Unlock()

	// Clean up dedup state via the Tracker so no stale suppression remains.
	o.progressTracker.Cleanup(tokens)
}

// sendRootsListChanged notifies the upstream that roots have changed.
func (o *Owner) sendRootsListChanged() {
	if o.upstream == nil {
		return // snapshot owner — upstream not yet spawned
	}
	notification := `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`
	if err := o.forwardNotificationIfReady([]byte(notification)); err != nil {
		o.logger.Printf("failed to send roots/list_changed: %v", err)
	}
}

// pathToFileURI converts a filesystem path to a file:// URI.
func pathToFileURI(p string) string {
	// Normalize path separators
	p = filepath.ToSlash(p)
	// Windows paths like C:/foo → file:///C:/foo
	if len(p) >= 2 && p[1] == ':' {
		return "file:///" + p
	}
	// Unix paths like /home/user → file:///home/user
	return "file://" + p
}

// broadcast sends a message to all connected sessions.
func (o *Owner) broadcast(data []byte) error {
	// Invalidate caches for list-changed notifications before forwarding
	o.invalidateCacheIfNeeded(data)

	o.mu.RLock()
	sessions := make([]*Session, 0, len(o.sessions))
	for _, s := range o.sessions {
		sessions = append(sessions, s)
	}
	o.mu.RUnlock()

	// Async broadcast: queue notifications via SendNotification (non-blocking).
	// Each session has a drain goroutine that writes to IPC at its own pace.
	// This prevents the upstream reader goroutine from deadlocking when a
	// session's IPC write is slow — upstream can keep producing output.
	for _, s := range sessions {
		s.SendNotification(data)
	}
	return nil
}

// broadcastListChanged sends tools/prompts/resources list_changed notifications
// to all connected sessions. Called after upstream restarts to prompt CC to
// re-fetch its tool/prompt/resource lists. Uses async delivery (SendNotification)
// so the caller is never blocked by a slow session.
func (o *Owner) broadcastListChanged() {
	notifications := []string{
		`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`,
		`{"jsonrpc":"2.0","method":"notifications/prompts/list_changed"}`,
		`{"jsonrpc":"2.0","method":"notifications/resources/list_changed"}`,
	}
	o.mu.RLock()
	sessions := make([]*Session, 0, len(o.sessions))
	for _, s := range o.sessions {
		sessions = append(sessions, s)
	}
	o.mu.RUnlock()

	if len(sessions) == 0 {
		return
	}

	for _, s := range sessions {
		for _, notif := range notifications {
			s.SendNotification([]byte(notif))
		}
	}
	o.logger.Printf("broadcast list_changed to %d sessions", len(sessions))
}

// ownerNotifier implements muxcore.Notifier by routing notifications through
// the Owner's session manager.
type ownerNotifier struct {
	owner *Owner
}

// Notify dispatches notifications for a project with best-effort semantics.
// best effort; failures logged at session level.
func (n *ownerNotifier) Notify(projectID string, notification []byte) error {
	// Collect ALL sessions matching projectID under RLock, then release before
	// SendNotification to avoid blocking on slow IPC consumers.
	// Holding o.mu.RLock() during SendNotification would still stall
	// goroutines needing o.mu.Lock() (addSession, removeSession, cacheResponse,
	// progress token cleanup), so lock scopes stay short.
	//
	// In Shared mode multiple CC sessions can share the same Cwd (same project),
	// so we must notify ALL of them, not just the first one found (which would be
	// random due to Go map iteration order).
	n.owner.mu.RLock()
	var targets []*Session
	for _, s := range n.owner.sessions {
		if muxcore.ProjectContextID(s.Cwd) == projectID {
			targets = append(targets, s)
		}
	}
	n.owner.mu.RUnlock()
	if len(targets) == 0 {
		return fmt.Errorf("no session found for project %s", projectID)
	}
	for _, s := range targets {
		s.SendNotification(notification)
	}
	return nil
}

func (n *ownerNotifier) Broadcast(notification []byte) {
	n.owner.mu.RLock()
	sessions := make([]*Session, 0, len(n.owner.sessions))
	for _, s := range n.owner.sessions {
		sessions = append(sessions, s)
	}
	n.owner.mu.RUnlock()
	for _, s := range sessions {
		s.SendNotification(notification)
	}
}

// removeSession removes a session from the owner.
func (o *Owner) removeSession(s *Session) {
	o.mu.Lock()
	delete(o.sessions, s.ID)
	remaining := len(o.sessions)
	var removedTokens []string
	// Clean up progress tokens owned by this session (FIX 1: session died
	// before its tool call completed — prevent permanent reaper veto).
	for token, ownerID := range o.progressOwners {
		if ownerID == s.ID {
			reqID := o.progressTokenRequestID[token]
			delete(o.progressOwners, token)
			delete(o.progressTokenRequestID, token)
			delete(o.requestToTokens, reqID)
			removedTokens = append(removedTokens, token)
		}
	}
	o.mu.Unlock()
	o.cancelLocalDemandsForSession(s.ID)
	if remaining == 0 {
		o.cancelDisposableMaterializationForZeroSessions()
	}
	if len(removedTokens) > 0 {
		o.progressTracker.Cleanup(removedTokens)
	}

	o.sessionMgr.RemoveSession(s.ID)
	o.touchActivity()
	o.logger.Printf("session %d disconnected (%d remaining)", s.ID, remaining)

	if o.sessionHandler != nil {
		if lc, ok := o.sessionHandler.(muxcore.ProjectLifecycle); ok {
			go lc.OnProjectDisconnect(muxcore.ProjectContextID(s.Cwd))
		}
	}

	if remaining > 0 {
		// Notify upstream that roots may have changed (client left)
		o.sendRootsListChanged()
	} else if o.onZeroSessions != nil {
		o.onZeroSessions(o)
	}
}

// acceptLoop accepts new IPC connections and creates sessions for them.
//
// If listener.Close() is called WITHOUT closing listenerDone, Accept() returns
// "use of closed network connection" forever — previously this caused a tight
// spin loop (711 errors/sec observed) that killed the daemon with log spam.
// Now we detect the "closed" error class and exit cleanly; other errors get a
// small backoff to prevent CPU saturation.
func (o *Owner) acceptLoop() {
	o.isAccepting.Store(true)
	defer o.isAccepting.Store(false)

	for {
		conn, err := o.listener.Accept()
		if err != nil {
			select {
			case <-o.done:
				return // full shutdown
			case <-o.listenerDone:
				return // listener closed (auto-isolation or shutdown)
			default:
			}
			// Detect "listener closed" by error type — if we can't Accept again,
			// don't spin. This catches the case where listener.Close() was called
			// but listenerDone wasn't signaled (bug safety net).
			if errors.Is(err, net.ErrClosed) {
				o.logger.Printf("accept loop exiting: listener closed (%v)", err)
				return
			}
			o.logger.Printf("accept error: %v", err)
			time.Sleep(100 * time.Millisecond) // backoff to prevent CPU saturation
			continue
		}
		select {
		case <-o.done:
			conn.Close()
			return
		case <-o.listenerDone:
			conn.Close()
			return
		default:
		}

		var token string
		var pendingCwd string
		var pendingEnv map[string]string
		var reader io.Reader = conn
		if o.tokenHandshake {
			token, reader = readToken(conn)
			var ok bool
			pendingCwd, pendingEnv, ok = o.sessionMgr.LookupPendingForOwner(token, o.ServerID())
			if token == "" || !ok {
				peerPID := readPeerPID(conn)
				o.rejectionLogger.Log(o.logger, peerPID)
				conn.Close()
				continue
			}
		}
		// reader may be io.MultiReader if readToken prepended unconsumed bytes.
		// conn is always the writer and closer.
		s := NewSession(reader, conn)
		// Authorization gets a side-effect-free snapshot of pending project
		// context. Exact Bind is deferred until the admission commit below.
		if token != "" {
			s.Cwd = pendingCwd
			s.Env = pendingEnv
		}
		// io.MultiReader doesn't implement io.Closer — set closer to conn explicitly
		// so Close() can forcefully disconnect the IPC connection.
		s.SetCloser(conn)

		// Populate cached SessionMeta with OS-level peer credentials before
		// dispatch (FR-5/FR-6). Unconditional — token-handshake-disabled
		// callers (EC-6) still receive Conn populated; AuthorizeSession
		// optionally mutates TenantID + AuthorizedAt from this baseline.
		// peerCreds normalises -1 sentinels to 0.
		info := peerCreds(conn)
		s.SetMeta(muxcore.SessionMeta{Conn: info})
		o.logger.Printf("peer_creds_extracted sid=%d pid=%d uid=%d platform=%s",
			s.ID, info.PeerPid, info.PeerUid, info.Platform)

		// AuthorizeSession gate (FR-3). Single-shot per session; runs after
		// peer-creds extraction and BEFORE AddSession. nil = no gate
		// (byte-identical to v0.23 dispatch path).
		//
		// Shutdown handling (CHK015 / EC-11): if owner.done fires before
		// the callback runs, refuse the session without invoking.
		// Panic guard: any panic inside the callback is recovered and
		// treated as AuthDeny{Reason: "authorize panic"} — the daemon
		// continues accepting subsequent connections.
		if o.authorizeSession != nil {
			select {
			case <-o.done:
				o.logger.Printf("auth_skipped_shutdown sid=%d", s.ID)
				o.revokePendingAdmission(token)
				conn.Close()
				s.Close()
				continue
			default:
			}

			project := muxcore.ProjectContext{
				ID:  muxcore.ProjectContextID(s.Cwd),
				Cwd: s.Cwd,
				Env: s.Env,
			}
			verdict := invokeAuthorize(o, o.authorizeSession, info, project, s.ID, o.logger)

			// Post-callback shutdown re-check (CHK015 amendment / CodeRabbit
			// MAJOR review on PR #113). The pre-callback check at the top
			// of the authorize block closes the gap up to the call, but a
			// long-running callback (external RPC, slow tenant lookup) can
			// outlast o.done. Without this re-check, AuthAllow would still
			// reach AddSession during teardown, registering a session in a
			// shutting-down owner.
			select {
			case <-o.done:
				o.logger.Printf("auth_aborted_shutdown sid=%d", s.ID)
				o.revokePendingAdmission(token)
				conn.Close()
				s.Close()
				continue
			default:
			}

			if verdict.Decision == muxcore.AuthDeny {
				// Apply the documented "empty Reason → stable fallback"
				// contract from session_auth.go SessionAuth godoc. Without
				// this fallback the JSON-RPC error.message would be an
				// empty string and consumers' deny-categorisation logic
				// (which keys off Reason) would silently bucket every
				// such denial together. CodeRabbit MINOR on PR #113.
				reason := verdict.Reason
				if reason == "" {
					reason = "session not authorized"
				}
				if errBytes, marshalErr := buildJSONRPCErrorBytes(nil, -32000, reason); marshalErr == nil {
					// Write the JSON-RPC -32000 error directly to the conn
					// (not through s.WriteRaw — the session is being torn
					// down, no need for the buffered-writer/bufio path).
					_, _ = conn.Write(append(errBytes, '\n'))
				} else {
					o.logger.Printf("auth_deny_marshal_error sid=%d err=%v", s.ID, marshalErr)
				}
				o.logger.Printf("auth_deny sid=%d reason=%q", s.ID, reason)
				o.revokePendingAdmission(token)
				conn.Close()
				s.Close()
				continue
			}

			// AuthAllow: stamp tenant + authorized timestamp on cached meta.
			// Re-SetMeta with the full payload (Conn + TenantID + AuthorizedAt).
			s.SetMeta(muxcore.SessionMeta{
				Conn:         info,
				TenantID:     verdict.TenantID,
				AuthorizedAt: time.Now(),
			})
			o.logger.Printf("auth_allow sid=%d tenant=%q", s.ID, verdict.TenantID)
		}

		o.admissionMu.Lock()
		select {
		case <-o.done:
			o.admissionMu.Unlock()
			o.logger.Printf("admission_aborted_shutdown sid=%d", s.ID)
			o.revokePendingAdmission(token)
			s.Close()
			continue
		default:
		}
		if token != "" && !o.sessionMgr.Bind(token, o.ServerID(), s) {
			o.admissionMu.Unlock()
			peerPID := readPeerPID(conn)
			o.rejectionLogger.Log(o.logger, peerPID)
			s.Close()
			continue
		}
		o.addSessionLocked(s)
		o.admissionMu.Unlock()
		o.startRegisteredSession(s)
	}
}

// invokeAuthorize calls cb under a defer/recover guard with an owner-bound
// context. Cancellation propagates from o.done, so a long-running callback
// (external RPC, slow tenant lookup) can observe shutdown via ctx.Done()
// and abort early instead of running to completion against a teardown.
// Panic is logged with the panic value and converted to
// AuthDeny{Reason: "authorize panic"}; the daemon continues accepting
// subsequent connections.
func invokeAuthorize(
	o *Owner,
	cb func(ctx context.Context, conn muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth,
	info muxcore.ConnInfo,
	project muxcore.ProjectContext,
	sid int,
	logger *log.Logger,
) (verdict muxcore.SessionAuth) {
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("auth_panic sid=%d recovered=%v", sid, r)
			verdict = muxcore.SessionAuth{Decision: muxcore.AuthDeny, Reason: "authorize panic"}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-o.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return cb(ctx, info, project)
}

// frameHookBudget is the per-frame OnFrameReceived deadline (NFR-2).
// Overrun is treated as FramePass to keep the reader goroutine moving;
// the late callback completion is logged but its returned action is dropped.
const frameHookBudget = 1 * time.Millisecond

// cacheHandlerInterfaces resolves the optional *WithSessionMeta interface
// upgrades exactly once at owner construction. Hot-path dispatch
// (handleDownstreamMessage / dispatchToSessionHandler) reads the resulting
// fields directly instead of re-running the type assertion on every frame.
// Safe to call multiple times — repeated calls overwrite with the same
// values. Invoked after o.sessionHandler is wired by every constructor
// path (NewOwner, NewOwnerFromSnapshot, newOwnerWithProcess from handoff).
func (o *Owner) cacheHandlerInterfaces() {
	if o.sessionHandler == nil {
		o.notificationHandlerWithMeta = nil
		o.sessionHandlerWithMeta = nil
		return
	}
	if h, ok := o.sessionHandler.(muxcore.NotificationHandlerWithSessionMeta); ok {
		o.notificationHandlerWithMeta = h
	}
	if h, ok := o.sessionHandler.(muxcore.SessionHandlerWithSessionMeta); ok {
		o.sessionHandlerWithMeta = h
	}
}

// invokeFrameHook runs o.onFrameReceived under a 1 ms cancellation-free
// timeout (the callback continues running after timeout but its result is
// discarded — Go has no kill-goroutine primitive). Panics are recovered;
// timeout / panic / nil all map to FramePass (fail-open per FR-4 / NFR-2).
//
// Hot-path notes (Gemini code review #113):
//   - strconv.Itoa replaces fmt.Sprintf("%d", ...) — avoids fmt's reflection
//     pipeline and the *fmt.pp pool churn on every frame.
//   - time.NewTimer + Stop replaces time.After — time.After leaks a
//     unstoppable timer for `frameHookBudget` after the fast-path branch
//     wins, which under load creates GC pressure proportional to frame
//     rate. NewTimer + Stop returns the timer to the pool on the success
//     path.
//   - The done channel is buffered (cap=1) so a callback returning AFTER
//     the budget elapses does not deadlock its goroutine on send. The
//     verdict is silently dropped; we already logged frame_hook_timeout.
//   - We accept the goroutine-per-frame cost as the price of NFR-2's
//     hard 1 ms budget. A pure-synchronous variant (suggested by Gemini)
//     would remove the budget entirely, requiring a spec amendment.
func (o *Owner) invokeFrameHook(s *Session, msg *jsonrpc.Message) muxcore.FrameAction {
	if o.onFrameReceived == nil {
		return muxcore.FramePass
	}
	sid := strconv.Itoa(s.ID)
	method := msg.Method
	frameSize := len(msg.Raw)

	done := make(chan muxcore.FrameAction, 1) // buffered so a late return never blocks the goroutine
	go func() {
		defer func() {
			if r := recover(); r != nil {
				o.logger.Printf("frame_hook_panic sid=%s method=%s recovered=%v", sid, method, r)
				done <- muxcore.FramePass
			}
		}()
		done <- o.onFrameReceived(sid, frameSize, method)
	}()

	timer := time.NewTimer(frameHookBudget)
	select {
	case action := <-done:
		if !timer.Stop() {
			// Timer already fired; drain its channel so the runtime can
			// reclaim the timer immediately instead of holding it until GC.
			<-timer.C
		}
		return action
	case <-timer.C:
		o.logger.Printf("frame_hook_timeout sid=%s method=%s elapsed_ms_min=%d",
			sid, method, int64(frameHookBudget/time.Millisecond))
		return muxcore.FramePass
	}
}

// HandleShutdown implements control.CommandHandler.
// It performs a graceful drain: stops accepting new connections,
// waits for pending requests to complete (up to timeout), then shuts down.
func (o *Owner) HandleShutdown(drainTimeoutMs int) string {
	if drainTimeoutMs <= 0 {
		// Force shutdown — no drain
		go o.Shutdown()
		return "force shutdown initiated"
	}

	go func() {
		// Close IPC listener to stop new connections.
		// Use closeListener() to signal listenerDone — otherwise acceptLoop spins
		// in a tight error loop ("use of closed network connection") at CPU speed,
		// producing 700+ log lines/sec and eventually killing the daemon.
		o.closeListener()

		timeout := time.Duration(drainTimeoutMs) * time.Millisecond
		deadline := time.After(timeout)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-deadline:
				pending := o.pendingRequests.Load()
				if pending > 0 {
					o.logger.Printf("drain timeout: %d requests still pending, forcing shutdown", pending)
				}
				o.Shutdown()
				return
			case <-ticker.C:
				if o.pendingRequests.Load() <= 0 {
					o.logger.Printf("drain complete, shutting down")
					o.Shutdown()
					return
				}
			}
		}
	}()

	return fmt.Sprintf("draining (timeout %dms)", drainTimeoutMs)
}

// HandleStatus implements control.CommandHandler.
func (o *Owner) HandleStatus() map[string]any {
	return o.Status()
}

// evictExtraSessions disconnects all sessions except the first one.
// Called when an owner is reclassified as isolated after optimistic dedup
// connected multiple sessions. Evicted sessions get IPC EOF → their shims
// reconnect → daemon spawns separate owners for each.
func (o *Owner) evictExtraSessions() {
	o.mu.Lock()
	if len(o.sessions) <= 1 {
		o.mu.Unlock()
		return
	}

	// Find the lowest session ID (first connected) — keep it.
	keepID := -1
	for id := range o.sessions {
		if keepID == -1 || id < keepID {
			keepID = id
		}
	}

	// Collect sessions to evict.
	var evict []*Session
	for id, s := range o.sessions {
		if id != keepID {
			evict = append(evict, s)
		}
	}
	o.mu.Unlock()

	if len(evict) > 0 {
		o.logger.Printf("evicting %d extra sessions (keeping session %d) — server classified isolated after dedup", len(evict), keepID)
	}
	for _, s := range evict {
		s.Close() // triggers IPC EOF → shim reconnects → gets own owner
	}
}

func (o *Owner) resetCwdSetToPrimary() (string, int) {
	primaryCwd := serverid.CanonicalizePath(o.cwd)
	o.cwdSet = map[string]bool{primaryCwd: true}
	return primaryCwd, 1
}

// closeListener stops the IPC listener and removes the socket file.
// Safe to call multiple times (uses sync.Once).
// Nil-safe: owners constructed for tests may not have a listener.
func (o *Owner) closeListener() {
	o.closeListenerOnce.Do(func() {
		// Serialize teardown with token reservation and bind. Once listenerDone is
		// closed, purge every unconsumed reservation before admission can resume.
		o.admissionMu.Lock()
		o.mu.Lock()
		o.isAccepting.Store(false)
		close(o.listenerDone)
		o.mu.Unlock()
		o.pendingAdmissionsPurged.Add(int64(o.sessionMgr.RemovePendingForOwner(o.serverID)))
		o.admissionMu.Unlock()
		if o.listener != nil {
			o.listener.Close()
		}
		if o.ipcPath != "" {
			ipc.Cleanup(o.ipcPath)
		}
	})
}

// drainInflightRequests sends JSON-RPC error responses for all in-flight requests
// when the upstream process dies. Without this, clients wait forever for responses
// that will never come.
type drainedInflightResponse struct {
	session   *Session
	sessionID int
	payload   []byte
}

// claimInflightRequests atomically detaches generation-owned request state.
// It performs no downstream session I/O, so callers may hold upstreamEventMu.
func (o *Owner) claimInflightRequests() []drainedInflightResponse {
	entries := o.sessionMgr.DrainInflight()
	if len(entries) == 0 {
		return nil
	}
	o.logger.Printf("upstream died: sending error responses for %d in-flight requests", len(entries))

	responses := make([]drainedInflightResponse, 0, len(entries))
	for _, entry := range entries {
		if _, claimed := o.inflightTracker.LoadAndDelete(entry.RemappedID); !claimed {
			continue
		}
		o.decrementPending()
		remappedID := json.RawMessage(entry.RemappedID)
		if !json.Valid(remappedID) {
			remappedID = json.RawMessage(strconv.Quote(entry.RemappedID))
		}
		result, err := remap.Deremap(remappedID)
		if err != nil {
			o.logger.Printf("drainInflight: deremap error for %s: %v", entry.RemappedID, err)
			continue
		}
		o.mu.RLock()
		s, ok := o.sessions[result.SessionID]
		o.mu.RUnlock()
		if ok {
			responses = append(responses, drainedInflightResponse{
				session:   s,
				sessionID: result.SessionID,
				payload: []byte(fmt.Sprintf(
					`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"upstream process exited"}}`,
					string(result.OriginalID),
				)),
			})
		}

		o.methodTags.Delete(entry.RemappedID)
		o.timedOutIDs.Delete(entry.RemappedID)
		o.clearProgressTokensForRequest(entry.RemappedID)
	}
	return responses
}

func (o *Owner) drainProactiveRequests(proc *upstream.Process) {
	o.proactiveRequests.Range(func(key, value any) bool {
		request := value.(proactiveRequest)
		if request.upstream != proc {
			return true
		}
		if _, claimed := o.claimProactive(key.(string)); claimed {
			o.decrementPending()
		}
		return true
	})
}

func (o *Owner) deliverInflightResponses(responses []drainedInflightResponse) {
	for _, response := range responses {
		if err := response.session.WriteRaw(response.payload); err != nil {
			o.logger.Printf("drainInflight: write error to session %d: %v", response.sessionID, err)
		}
	}
}

func (o *Owner) drainInflightRequests() {
	o.deliverInflightResponses(o.claimInflightRequests())
}

// FinalizeForRemoval tears down admission and retries the owned process
// generation's whole-tree finalizer. Done is closed only after authority
// retirement is proven, so daemon state may safely be forgotten afterwards.
func (o *Owner) FinalizeForRemoval(soft bool, timeout time.Duration) (int, bool, error) {
	o.removalMu.Lock()
	defer o.removalMu.Unlock()
	select {
	case <-o.done:
		return 0, true, nil
	default:
	}
	if timeout <= 0 {
		timeout = materializationFinalizeTimeout
	}

	o.stopMaterialization()
	o.teardownExceptUpstream()

	o.materializationMu.Lock()
	attempt := o.materializationAttempt
	if attempt != nil {
		attempt.cancelMaterialization()
	}
	o.materializationMu.Unlock()
	if attempt != nil {
		timer := time.NewTimer(timeout)
		select {
		case <-attempt.done:
			timer.Stop()
		case <-timer.C:
			return 0, false, fmt.Errorf("owner finalization: materialization did not settle after %s", timeout)
		}
	}

	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	proc := o.retiringProcess
	if proc == nil {
		o.mu.RLock()
		proc = o.upstream
		o.mu.RUnlock()
	}
	if proc != nil {
		o.retiringProcess = proc
		o.materializationState = MaterializationFinalizing
		o.upstreamDead.Store(true)
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()

	exitCode := 0
	var closeErr error
	if proc != nil {
		if soft {
			exitCode, closeErr = proc.SoftClose(timeout)
		} else {
			closeErr = proc.Close()
		}
	}
	proofErr := error(nil)
	proven := proc == nil || proc.RetirementProven()
	if proc != nil && o.materializationFinalizationProbe != nil {
		proofErr = o.materializationFinalizationProbe(proc)
		proven = proofErr == nil
	}
	if !proven {
		if proofErr == nil {
			proofErr = errFinalizationUnproven
		}
		if closeErr != nil {
			proofErr = errors.Join(proofErr, closeErr)
		}
		o.upstreamEventMu.Lock()
		o.materializationMu.Lock()
		o.materializationState = MaterializationFinalizeBlocked
		o.materializationBlockedErr = proofErr
		o.retiringProcess = proc
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		return exitCode, false, proofErr
	}

	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.Lock()
	if o.upstream == proc {
		o.upstream = nil
	}
	o.upstreamDead.Store(true)
	o.mu.Unlock()
	if o.retiringProcess == proc {
		o.retiringProcess = nil
	}
	o.materializationBlockedErr = nil
	o.materializationState = MaterializationCacheOnly
	if o.cacheStage != nil && o.cacheStage.process == proc {
		o.cacheStage = nil
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	o.completeShutdown("owner shut down")
	return exitCode, true, closeErr
}

func (o *Owner) completeShutdown(message string) {
	o.shutdownOnce.Do(func() {
		o.logger.Printf("%s", message)
		close(o.done)
	})
}

// Shutdown finalizes the owner synchronously. A failed proof leaves Done open
// so a later daemon removal attempt can retry without losing authority.
func (o *Owner) Shutdown() {
	if _, finalized, err := o.FinalizeForRemoval(false, materializationFinalizeTimeout); !finalized {
		o.logger.Printf("owner shutdown blocked: %v", err)
	}
}

// SoftShutdown gives the upstream a bounded graceful exit before forceful
// whole-tree retirement. It still refuses terminal completion without proof.
func (o *Owner) SoftShutdown(timeout time.Duration) (int, error) {
	exitCode, finalized, err := o.FinalizeForRemoval(true, timeout)
	if !finalized && err == nil {
		err = errFinalizationUnproven
	}
	return exitCode, err
}

// Done returns a channel closed when the owner has shut down.
func (o *Owner) Done() <-chan struct{} {
	return o.done
}

// String implements fmt.Stringer to provide a compact identifier for
// the suture supervisor's logging. Without this, suture falls back to
// fmt.Sprintf("%#v", o) which dumps all fields — including raw []byte
// caches (initResp, toolList can be 10s of KB) — on every termination
// event. The reflected dump was 25 KB per log line and consumed 93%
// of log volume during supervisor restart cycles (see aimux→mcp-mux
// issue #5 comment 2026-04-10).
func (o *Owner) String() string {
	if o == nil {
		return "owner[<nil>]"
	}
	sid := o.serverID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	return fmt.Sprintf("owner[%s %s]", sid, o.command)
}

// Serve is the stable owner lifecycle service. Process exit and replacement are
// handled by the owner-local materialization controller; a cache-only owner is
// therefore healthy and must not cause suture restart/backoff cycles.
func (o *Owner) Serve(ctx context.Context) error {
	select {
	case <-ctx.Done():
		o.Shutdown()
		return ctx.Err()
	case <-o.done:
		return suture.ErrDoNotRestart
	}
}

// ServerID returns the server identity hash for this owner.
func (o *Owner) ServerID() string {
	return o.serverID
}

// IPCPath returns the IPC socket path for this owner.
func (o *Owner) IPCPath() string {
	return o.ipcPath
}

// IsAccepting returns true while the accept loop is running and the owner is
// not in shutdown/listener-close teardown. This is a lock-free signal only —
// it does NOT guarantee the listener is actually reachable from a fresh dial.
//
// Use IsReachable() when an authoritative liveness answer is required (e.g.
// when deciding whether to hand out an IPC path to a shim that will dial it).
// IsAccepting alone is insufficient: a listener can become unreachable while
// listenerDone is still open (e.g. a race where the accept goroutine returned
// on net.ErrClosed before closeListener signalled listenerDone, or where an
// OS-level teardown closed the socket fd out from under us). Those zombie
// states produce "dial: connection refused" for shims even though the owner
// entry is still registered in the daemon.
func (o *Owner) IsAccepting() bool {
	if o.admissionFrozen.Load() {
		return false
	}
	// If the accept loop has explicitly set isAccepting true, trust it.
	if o.isAccepting.Load() {
		return true
	}
	// Fallback: check whether listenerDone has been signalled. This covers
	// test owners and owners in the pre-acceptLoop window. If listenerDone
	// is closed the owner explicitly shut down; otherwise treat as accepting.
	select {
	case <-o.listenerDone:
		return false
	default:
		return true
	}
}

// IsReachable returns true if the IPC listener is both marked-open
// (listenerDone not signalled) AND actually accepting new connections right
// now as observed by an outbound ipc.Dial probe against the owner's own
// ipcPath. This is the authoritative liveness check — use it at any junction
// where a stale "accepting" answer would cause an external caller to dial a
// dead socket.
//
// The probe reuses ipc.Dial's existing 500ms timeout. Callers MUST NOT hold
// daemon-wide locks across this call; the probe can take up to 500ms on a
// hung peer and a daemon-wide lock would freeze every other spawn / status
// request for that window.
//
// Returns:
//   - false if listenerDone has been signalled (explicit closeListener — an
//     owner that deliberately retired its endpoint will return false here;
//     callers that need to distinguish "deliberately retired" from "zombie"
//     must pair IsReachable with IsAccepting to tell them apart). Classified
//     isolated owners keep the endpoint reachable for exact-token reconnect.
//   - true if ipcPath is empty (test owners / pre-bind SessionHandler-only
//     fixtures have no path to probe — they are treated as reachable so
//     unit-test flows are not short-circuited).
//   - otherwise, the result of ipc.IsAvailable(ipcPath) (dial-then-close
//     probe with the 500ms timeout from ipc.dialTimeout).
func (o *Owner) IsReachable() bool {
	// Fast path: explicit close always wins.
	select {
	case <-o.listenerDone:
		return false
	default:
	}
	if o.ipcPath == "" {
		// No path to probe (test owner, pre-bind). Treat as reachable — the
		// caller is responsible for its own liveness semantics.
		return true
	}
	return ipc.IsAvailable(o.ipcPath)
}

// InitReady returns a channel that is closed when the owner's upstream has responded
// to the first initialize request (response cached for replay) OR the upstream has
// exited. Used by Daemon.Spawn to block until the server is ready, preventing CC
// from timing out on slow-starting upstreams.
func (o *Owner) InitReady() <-chan struct{} {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.initReady
}

// InitSuccess returns true if the initialize response was successfully cached.
// Returns false if the upstream exited before responding to initialize.
// Must only be called after InitReady() is closed.
func (o *Owner) InitSuccess() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.initDone
}

// Classified returns a channel that is closed when the owner's auto-classification
// is determined (shared, isolated, or session-aware). Used by daemon's findSharedOwner
// to wait for classification before dedup decision.
func (o *Owner) Classified() <-chan struct{} {
	return o.classified
}

// IsClassifiedShareable returns true if the owner has been classified AND
// the classification allows cross-CWD sharing (shared or session-aware).
// Returns false if not yet classified or classified as isolated.
func (o *Owner) IsClassifiedShareable() bool {
	select {
	case <-o.classified:
	default:
		return false // not yet classified
	}
	o.mu.RLock()
	mode := o.autoClassification
	o.mu.RUnlock()
	return mode == classify.ModeShared || mode == classify.ModeSessionAware
}

// IsClassifiedIsolated returns true if the owner has been classified AND the
// classification is isolated. Returns false when classification is still
// pending OR when the mode is shared/session-aware. Used by the reaper to
// apply a shorter idle timeout to isolated owners, which reject fresh Spawn
// admission while retaining their authenticated listener for token reconnect.
func (o *Owner) IsClassifiedIsolated() bool {
	select {
	case <-o.classified:
	default:
		return false
	}
	o.mu.RLock()
	mode := o.autoClassification
	o.mu.RUnlock()
	return mode == classify.ModeIsolated
}

// MarkClassified forces the classified channel closed. Used by tests that need
// to bypass the normal init/tools handshake classification flow.
func (o *Owner) MarkClassified() {
	o.classifiedOnce.Do(func() {
		if o.classified != nil {
			close(o.classified)
		}
	})
}

// MarkClassifiedAs sets the owner's auto-classification to the given mode and
// closes the classified channel. Used by tests that need both an explicit
// classification verdict (shared / isolated / session-aware) AND the channel
// signal in one call, without running a real initialize + tools/list
// handshake. classificationSource is set to "test" only if it was empty.
//
// Production code populates autoClassification via classifyFromCapability or
// classifyFromToolList in this file; this helper exists exclusively for
// daemon-level race tests in the daemon package that cannot reach those
// private code paths.
func (o *Owner) MarkClassifiedAs(mode classify.SharingMode) {
	o.classifiedOnce.Do(func() {
		o.mu.Lock()
		o.autoClassification = mode
		if o.classificationSource == "" {
			o.classificationSource = "test"
		}
		o.mu.Unlock()
		if o.classified != nil {
			close(o.classified)
		}
	})
}

// AddCwd registers an additional project cwd for this owner.
// Used by dedup: when a second project reuses a shared owner,
// its cwd is added so roots/list includes all project roots.
//
// Paths are canonicalized (lowercased on Windows, symlinks resolved) to
// prevent memory leaks from case/slash variants being treated as distinct.
// Before normalization: D:\Dev\foo, D:/Dev/foo, d:\dev\foo all grew cwdSet.
// sendRootsListChanged is called ONLY when a new cwd is actually added,
// preventing log spam and upstream notification storms on duplicate calls.
func (o *Owner) AddCwd(cwd string) {
	if cwd == "" {
		return
	}
	canonical := serverid.CanonicalizePath(cwd)
	o.mu.Lock()
	if o.cwdSet[canonical] {
		o.mu.Unlock()
		return // already registered — silent no-op, no log, no notify
	}
	o.cwdSet[canonical] = true
	count := len(o.cwdSet)
	o.mu.Unlock()
	o.logger.Printf("added cwd: %s (now %d roots)", canonical, count)
	// Notify upstream ONLY on actual change
	o.sendRootsListChanged()
}

// Command returns the upstream command.
func (o *Owner) Command() string {
	return o.command
}

// Args returns the upstream command arguments.
func (o *Owner) Args() []string {
	return o.args
}

// SessionCount returns the number of connected sessions.
func (o *Owner) SessionCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.sessions)
}

// PendingRequests returns the number of in-flight requests.
func (o *Owner) PendingRequests() int64 {
	return o.pendingRequests.Load()
}

// ActiveProgressTokens returns the number of tracked progressTokens —
// long-running tool calls that are still streaming notifications/progress
// from upstream. While non-zero, the reaper must not kill this owner.
func (o *Owner) ActiveProgressTokens() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.progressOwners)
}

// UpstreamDead returns true if the upstream process has exited.
func (o *Owner) UpstreamDead() bool {
	return o.upstreamDead.Load()
}

// LastActivity returns the unix-nano timestamp of the last activity seen by
// the owner (inbound upstream message, outbound session→upstream message, or
// a session connect/disconnect event).
func (o *Owner) LastActivity() time.Time {
	ns := o.lastActivityNs.Load()
	if ns == 0 {
		return o.startTime
	}
	return time.Unix(0, ns)
}

// touchActivity records the current time as the last activity timestamp.
// Called on every MCP message (both directions), session add/remove, and
// upstream init. The reaper uses this to decide whether an owner is idle.
//
// Throttled to one atomic write per second: the reaper only needs
// second-level precision, and avoiding a store on every streaming message
// reduces cache-line contention on high-traffic owners (e.g. LLM responses).
func (o *Owner) touchActivity() {
	now := time.Now().UnixNano()
	last := o.lastActivityNs.Load()
	if now-last >= int64(time.Second) {
		o.lastActivityNs.Store(now)
	}
}

// writeUpstream is the single choke point for sending a line to upstream
// stdin. Wrapping upstream.WriteLine lets us touch activity on every
// outbound message without scattering the call across 15+ sites.
//
// SessionHandler-only owners never call writeUpstream — requests dispatch
// directly via dispatchToSessionHandler, and notification forwarding
// returns early when o.upstream == nil.
//
// Writer-injection mode (upstreamWriter != nil): bytes are written directly
// to the injected writer followed by a newline, mirroring upstream.WriteLine
// semantics. Used by tests to capture output without a subprocess.
func (o *Owner) writeUpstream(data []byte) error {
	_, err := o.writeUpstreamFromCurrent(data)
	return err
}

// writeUpstreamFromCurrent returns the exact process generation used for the
// write so a failure cannot retire a replacement generation.
func (o *Owner) writeUpstreamFromCurrent(data []byte) (*upstream.Process, error) {
	o.touchActivity()
	o.mu.RLock()
	writer := o.upstreamWriter
	up := o.upstream
	o.mu.RUnlock()
	if writer != nil {
		if _, err := writer.Write(data); err != nil {
			return nil, fmt.Errorf("upstream writer: write: %w", err)
		}
		if _, err := writer.Write([]byte("\n")); err != nil {
			return nil, fmt.Errorf("upstream writer: write newline: %w", err)
		}
		return nil, nil
	}
	if up == nil {
		return nil, errors.New("upstream writer unavailable")
	}
	return up, up.WriteLine(data)
}

func (o *Owner) hasWritableUpstream() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.upstream != nil || o.upstreamWriter != nil
}

// busyDeclaration is a signal from upstream that it is doing long-running
// work without an active JSON-RPC request or progress token. Used by the
// reaper to exempt such owners from idle-based kill.
type busyDeclaration struct {
	StartedAt     time.Time
	EstimatedEnd  time.Time // StartedAt + estimatedDuration
	HardExpiresAt time.Time // safety cap: StartedAt + estimatedDuration * 2
	Task          string
	SessionID     int // session that declared the work (-1 if unattributed)
}

// HasActiveBusyWork returns true if the owner has any unexpired busy
// declarations. The reaper must not kill an owner while this is true.
// Expired declarations are garbage-collected inside the call.
func (o *Owner) HasActiveBusyWork() bool {
	now := time.Now()
	o.busyMu.Lock()
	defer o.busyMu.Unlock()
	active := false
	for id, d := range o.busyDeclarations {
		if now.After(d.HardExpiresAt) {
			delete(o.busyDeclarations, id)
			continue
		}
		active = true
	}
	return active
}

// RegisterBusy records a long-running work declaration from upstream.
// The reaper will not kill the owner while any non-expired declaration
// exists. estimatedDuration of 0 falls back to 10 minutes; values above
// 24 hours are clamped to prevent overflow and forgotten declarations from
// living indefinitely.
const maxBusyDuration = 24 * time.Hour

func (o *Owner) RegisterBusy(id string, startedAt time.Time, estimatedDuration time.Duration, task string, sessionID int) {
	if estimatedDuration <= 0 {
		estimatedDuration = 10 * time.Minute
	}
	if estimatedDuration > maxBusyDuration {
		estimatedDuration = maxBusyDuration
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	d := busyDeclaration{
		StartedAt:     startedAt,
		EstimatedEnd:  startedAt.Add(estimatedDuration),
		HardExpiresAt: startedAt.Add(estimatedDuration * 2),
		Task:          task,
		SessionID:     sessionID,
	}
	o.busyMu.Lock()
	if o.busyDeclarations == nil {
		o.busyDeclarations = map[string]busyDeclaration{}
	}
	o.busyDeclarations[id] = d
	o.busyMu.Unlock()
	o.touchActivity()
}

// ClearBusy removes a busy declaration (upstream signalled the long-running
// work is finished).
func (o *Owner) ClearBusy(id string) {
	o.busyMu.Lock()
	delete(o.busyDeclarations, id)
	o.busyMu.Unlock()
	o.touchActivity()
}

// IdleTimeout returns the owner-specific idle timeout override from
// x-mux.idleTimeout capability, or 0 if not declared (caller uses default).
func (o *Owner) IdleTimeout() time.Duration {
	return time.Duration(o.idleTimeoutNs.Load())
}

// SetIdleTimeout stores the x-mux.idleTimeout override. Called by the
// cache-init path after parsing the upstream initializeResult.
func (o *Owner) SetIdleTimeout(d time.Duration) {
	if d < 0 {
		return
	}
	o.idleTimeoutNs.Store(int64(d))
}

// Status returns a JSON-serializable status summary.
func (o *Owner) Status() map[string]any {
	o.mu.RLock()
	sessionIDs := make([]int, 0, len(o.sessions))
	for id := range o.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	classification := string(o.autoClassification)
	classificationSource := o.classificationSource
	reason := o.classificationReason
	hasCachedInit := o.initResp != nil
	hasCachedTools := o.toolList != nil
	hasCachedPrompts := o.promptList != nil
	hasCachedResources := o.resourceList != nil
	cwds := make([]string, 0, len(o.cwdSet))
	for c := range o.cwdSet {
		cwds = append(cwds, c)
	}
	primaryCwd := o.cwd
	upstreamPresent := o.upstream != nil || o.upstreamWriter != nil || (o.sessionHandler != nil && o.handlerFunc == nil)
	upstreamPID := 0
	if o.upstream != nil {
		upstreamPID = o.upstream.PID()
	}
	o.mu.RUnlock()
	matState, matTrigger, matPolicy, matGeneration, pendingDemandCount, persistentPending, finalizationError := o.materializationStatus()

	status := map[string]any{
		"mux_version":                Version,
		"upstream_pid":               upstreamPID,
		"ipc_path":                   o.ipcPath,
		"command":                    o.command,
		"args":                       o.args,
		"cwd":                        primaryCwd,
		"cwd_set":                    cwds,
		"sessions":                   sessionIDs,
		"session_count":              len(sessionIDs),
		"pending_requests":           o.pendingRequests.Load(),
		"uptime_seconds":             time.Since(o.startTime).Seconds(),
		"cached_init":                hasCachedInit,
		"cached_tools":               hasCachedTools,
		"cached_prompts":             hasCachedPrompts,
		"cached_resources":           hasCachedResources,
		"materialization_state":      string(matState),
		"materialization_trigger":    string(matTrigger),
		"materialization_policy":     string(matPolicy),
		"materialization_generation": matGeneration,
		"pending_demand_count":       pendingDemandCount,
		"persistent_pending":         persistentPending,
		"restart_pin_count":          o.restartPins.Load(),
		"cache_ready":                hasCachedInit && hasCachedTools,
		"upstream_live":              upstreamPresent && !o.upstreamDead.Load(),
		"finalization_error":         finalizationError,
	}

	if classification != "" {
		status["auto_classification"] = classification
		status["classification_source"] = classificationSource
		if len(reason) > 0 {
			status["classification_reason"] = reason
		}
	}

	// Include inflight request details when requests are pending
	var inflight []map[string]any
	var oldestMs int64
	o.inflightTracker.Range(func(key, value any) bool {
		req := value.(*InflightRequest)
		inflight = append(inflight, map[string]any{
			"method":          req.Method,
			"tool":            req.Tool,
			"session":         req.SessionID,
			"started_at":      req.StartTime.UTC().Format(time.RFC3339Nano),
			"elapsed_seconds": time.Since(req.StartTime).Seconds(),
		})
		age := time.Since(req.StartTime).Milliseconds()
		if age > oldestMs {
			oldestMs = age
		}
		return true
	})
	if len(inflight) > 0 {
		status["inflight"] = inflight
	}
	if oldestMs > 0 {
		status["oldest_request_age_ms"] = oldestMs
	}

	return status
}

// ExportSnapshot captures the owner's serializable state for graceful restart.
// Thread-safe: acquires RLock. Cached responses are base64-encoded.
func (o *Owner) ExportSnapshot() OwnerSnapshot {
	o.materializationMu.Lock()
	o.mu.RLock()
	snap := o.exportSnapshotLocked()
	o.applyMaterializationSnapshotLocked(&snap)
	o.mu.RUnlock()
	o.materializationMu.Unlock()
	o.appendBoundTokenSnapshots(&snap)
	return snap
}

// ExportSnapshotState captures owner metadata, discovery caches, and active
// sessions under one owner read lock. Daemon restart serialization uses this
// view so it cannot combine cache/classification facts from one instant with
// a session set from another.
func (o *Owner) ExportSnapshotState() (OwnerSnapshot, []SessionSnapshot) {
	o.materializationMu.Lock()
	o.mu.RLock()
	snap := o.exportSnapshotLocked()
	o.applyMaterializationSnapshotLocked(&snap)
	sessions := o.exportSessionsLocked()
	o.mu.RUnlock()
	o.materializationMu.Unlock()
	o.appendBoundTokenSnapshots(&snap)
	return snap, sessions
}

func (o *Owner) applyMaterializationSnapshotLocked(snap *OwnerSnapshot) {
	snap.Persistent = snap.Persistent || o.persistentPending
	if a := o.materializationAttempt; a != nil {
		snap.Cwd = a.launch.Cwd
		snap.Env = cloneSnapshotEnv(a.launch.Env)
	}
}

func (o *Owner) exportSnapshotLocked() OwnerSnapshot {
	cwds := make([]string, 0, len(o.cwdSet))
	for cwd := range o.cwdSet {
		cwds = append(cwds, cwd)
	}
	snap := OwnerSnapshot{
		Command:              o.command,
		Args:                 append([]string(nil), o.args...),
		Cwd:                  o.cwd,
		Env:                  cloneSnapshotEnv(o.env),
		CwdSet:               cwds,
		Classification:       o.autoClassification,
		ClassificationSource: o.classificationSource,
		ClassificationReason: append([]string(nil), o.classificationReason...),
	}
	if o.initResp != nil {
		snap.CachedInit = base64Encode(o.initResp)
	}
	if o.toolList != nil {
		snap.CachedTools = base64Encode(o.toolList)
	}
	if o.promptList != nil {
		snap.CachedPrompts = base64Encode(o.promptList)
	}
	if o.resourceList != nil {
		snap.CachedResources = base64Encode(o.resourceList)
	}
	if o.resourceTemplateList != nil {
		snap.CachedResourceTemplates = base64Encode(o.resourceTemplateList)
	}
	return snap
}

func (o *Owner) appendBoundTokenSnapshots(snap *OwnerSnapshot) {
	for _, hist := range o.sessionMgr.ExportBoundHistory() {
		snap.BoundTokens = append(snap.BoundTokens, snapshot.BoundTokenSnapshot{
			Token:    hist.Token,
			OwnerKey: hist.OwnerKey,
			Cwd:      hist.Cwd,
			Env:      cloneSnapshotEnv(hist.Env),
			BoundAt:  hist.BoundAt,
			LastUsed: hist.LastUsed,
		})
	}
}

func cloneSnapshotEnv(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func boundTokenSnapshotsToSession(entries []snapshot.BoundTokenSnapshot) []session.BoundHistorySnapshot {
	if len(entries) == 0 {
		return nil
	}
	out := make([]session.BoundHistorySnapshot, 0, len(entries))
	for _, entry := range entries {
		out = append(out, session.BoundHistorySnapshot{
			Token:    entry.Token,
			OwnerKey: entry.OwnerKey,
			Cwd:      entry.Cwd,
			Env:      entry.Env,
			BoundAt:  entry.BoundAt,
			LastUsed: entry.LastUsed,
		})
	}
	return out
}

// ExportSessions returns snapshot metadata for all active sessions.
func (o *Owner) ExportSessions() []SessionSnapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.exportSessionsLocked()
}

func (o *Owner) exportSessionsLocked() []SessionSnapshot {
	sessions := make([]SessionSnapshot, 0, len(o.sessions))
	for _, s := range o.sessions {
		sessions = append(sessions, SessionSnapshot{
			MuxSessionID: s.MuxSessionID,
			Cwd:          s.Cwd,
			Env:          cloneSnapshotEnv(s.Env),
		})
	}
	return sessions
}

// getCachedResponse returns the cached response for the given method, or nil.
func (o *Owner) getCachedResponse(method string) []byte {
	o.mu.RLock()
	defer o.mu.RUnlock()

	switch method {
	case "initialize":
		return o.initResp
	case "tools/list":
		return o.toolList
	case "prompts/list":
		return o.promptList
	case "resources/list":
		return o.resourceList
	case "resources/templates/list":
		return o.resourceTemplateList
	default:
		return nil
	}
}

// replayFromCache sends a cached response to the session with the client's request ID.
func (o *Owner) replayFromCache(s *Session, msg *jsonrpc.Message, cached []byte) error {
	replaced, err := jsonrpc.ReplaceID(cached, msg.ID)
	if err != nil {
		return fmt.Errorf("replay %s: replace id: %w", msg.Method, err)
	}

	// Track sessions that received a cached initialize so we can suppress
	// the subsequent notifications/initialized they send to upstream.
	if msg.Method == "initialize" {
		o.mu.Lock()
		o.cachedInitSessions[s.ID] = true
		o.mu.Unlock()
	}

	o.logger.Printf("session %d: replaying cached %s response", s.ID, msg.Method)
	return s.WriteRaw(replaced)
}

// cacheResponse stores a raw JSON-RPC response for later replay.
func (o *Owner) cacheResponse(method string, raw []byte) {
	if o.cacheResponseState(method, raw) {
		o.broadcastListChanged()
	}
}

// cacheResponseState commits cache and classification state without downstream
// session I/O. The caller may hold upstreamEventMu to linearize a live process
// response against process-generation replacement.
func (o *Owner) cacheResponseState(method string, raw []byte) bool {
	cached := make([]byte, len(raw))
	copy(cached, raw)

	o.mu.Lock()
	switch method {
	case "initialize":
		o.initResp = cached
		o.initDone = true
		if injected, ok := listchanged.InjectInitializeCapability(cached); ok {
			o.initResp = injected
			o.logger.Printf("injected listChanged:true into cached initialize response")
		}
	case "tools/list":
		o.toolList = cached
	case "prompts/list":
		o.promptList = cached
	case "resources/list":
		o.resourceList = cached
	case "resources/templates/list":
		o.resourceTemplateList = cached
	}
	o.mu.Unlock()

	o.logger.Printf("cached %s response (%d bytes)", method, len(cached))

	if method == "initialize" {
		o.initReadyOnce.Do(func() {
			if o.initReady != nil {
				close(o.initReady)
			}
		})
		o.classifyFromCapabilities(cached)
		o.checkPersistent(cached)
		o.parseDrainTimeout(cached)
		o.parseToolTimeout(cached)
		o.parseIdleTimeout(cached)
		if sec := classify.ParseProgressInterval(cached); sec > 0 {
			o.progressIntervalNs.Store(int64(time.Duration(sec) * time.Second))
			o.logger.Printf("using x-mux.progressInterval: %ds", sec)
		}
	}
	if method != "tools/list" {
		return false
	}
	o.classifyFromToolList(cached)
	// Publish synchronously while a live response still owns its generation
	// lease, so template publication cannot cross successor installation.
	if o.onCacheReady != nil && o.initDone {
		o.onCacheReady(o, o.ExportSnapshot())
	}
	if !o.listChangedAfterRefresh.Swap(false) {
		return false
	}
	o.mu.Lock()
	o.promptList = nil
	o.resourceList = nil
	o.resourceTemplateList = nil
	o.mu.Unlock()
	return true
}

// forwardCancelledNotification remaps the requestId in a notifications/cancelled
// notification so upstream can match it to the in-flight remapped request.
func (o *Owner) forwardCancelledNotification(s *Session, msg *jsonrpc.Message) error {
	// Parse the raw notification into a generic map
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(msg.Raw, &obj); err != nil {
		// Fallback: forward as-is
		return o.forwardNotificationIfReady(msg.Raw)
	}

	paramsRaw, hasParams := obj["params"]
	if !hasParams {
		return o.forwardNotificationIfReady(msg.Raw)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return o.forwardNotificationIfReady(msg.Raw)
	}

	requestIDRaw, hasRequestID := params["requestId"]
	if !hasRequestID {
		return o.forwardNotificationIfReady(msg.Raw)
	}
	if o.cancelLocalDemand(s, requestIDRaw) {
		return nil
	}

	// Remap the requestId to the upstream-facing ID
	remappedRequestID := remap.Remap(s.ID, requestIDRaw)
	params["requestId"] = remappedRequestID

	newParams, err := json.Marshal(params)
	if err != nil {
		return o.forwardNotificationIfReady(msg.Raw)
	}

	obj["params"] = newParams
	remapped, err := json.Marshal(obj)
	if err != nil {
		return o.forwardNotificationIfReady(msg.Raw)
	}

	// Clean up progress tokens for the cancelled request (FIX 1).
	o.clearProgressTokensForRequest(string(remappedRequestID))

	o.logger.Printf("session %d: forwarding notifications/cancelled with remapped requestId", s.ID)
	return o.forwardNotificationIfReady(remapped)
}

// invalidateCacheIfNeeded checks if a notification from upstream signals that a
// cached list has changed, and clears the relevant cache entry.
func (o *Owner) invalidateCacheIfNeeded(data []byte) {
	msg, err := jsonrpc.Parse(data)
	if err != nil || !msg.IsNotification() {
		return
	}
	invalidated := false
	o.mu.Lock()
	switch msg.Method {
	case "notifications/tools/list_changed":
		o.toolList = nil
		invalidated = true
		o.logger.Printf("cache invalidated: tools/list")
	case "notifications/prompts/list_changed":
		o.promptList = nil
		invalidated = true
		o.logger.Printf("cache invalidated: prompts/list")
	case "notifications/resources/list_changed":
		o.resourceList = nil
		o.resourceTemplateList = nil
		invalidated = true
		o.logger.Printf("cache invalidated: resources/list + templates")
	}
	o.mu.Unlock()
	if invalidated && o.onCacheInvalidated != nil {
		o.onCacheInvalidated(o)
	}
}

// captureInitFingerprint extracts protocolVersion from an initialize request
// and stores it for fingerprint matching against later clients.
func (o *Owner) captureInitFingerprint(raw []byte) {
	var req struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.Params.ProtocolVersion == "" {
		return
	}

	o.mu.Lock()
	if o.initProtocolVersion == "" {
		o.initProtocolVersion = req.Params.ProtocolVersion
		o.logger.Printf("captured init fingerprint: protocolVersion=%s", req.Params.ProtocolVersion)
	}
	o.mu.Unlock()
}

// initFingerprintMatches checks if a new initialize request has the same
// protocolVersion as the cached one. If no fingerprint was captured, matches by default.
func (o *Owner) initFingerprintMatches(raw []byte) bool {
	o.mu.RLock()
	cached := o.initProtocolVersion
	o.mu.RUnlock()

	if cached == "" {
		return true // no fingerprint captured — allow replay
	}

	var req struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return true // can't parse — allow replay
	}

	return req.Params.ProtocolVersion == cached
}

// classifyFromCapabilities extracts x-mux capability from the cached initialize
// response and sets the classification. Has priority over tool-name classification.
// Skipped if already classified (snapshot restore, duplicate proactive init).
func (o *Owner) classifyFromCapabilities(initJSON []byte) {
	o.mu.RLock()
	alreadyClassified := o.classificationSource != ""
	o.mu.RUnlock()
	if alreadyClassified {
		return
	}

	mode, ok := classify.ClassifyCapabilities(initJSON)
	if !ok {
		return // no x-mux capability — will fall back to tool classification
	}

	o.mu.Lock()
	o.autoClassification = mode
	o.classificationSource = "capability"
	o.classificationReason = nil
	o.mu.Unlock()

	o.classifiedOnce.Do(func() {
		if o.classified != nil {
			close(o.classified)
		}
	})
	o.logger.Printf("x-mux capability: %s", mode)

	if mode == classify.ModeIsolated {
		o.mu.Lock()
		primaryCwd, cwdCount := o.resetCwdSetToPrimary()
		removed := o.sessionMgr.RemovePendingForOwnerExcept(o.serverID, o.initialAdmissionToken)
		o.mu.Unlock()

		if primaryCwd != "" {
			o.logger.Printf("reset cwd_set to primary cwd: %s (now %d roots)", primaryCwd, cwdCount)
		}
		o.logger.Printf("restricting IPC listener to reconnect tokens — server declares isolated via x-mux (revoked_pending=%d)", removed)
		o.evictExtraSessions()
	}
}

// classifyFromToolList runs the auto-classifier on a cached tools/list response.
// Skipped if classification already determined (capability has priority, snapshot
// restores classification, duplicate proactive init responses are ignored).
func (o *Owner) classifyFromToolList(toolsJSON []byte) {
	o.mu.RLock()
	alreadyClassified := o.classificationSource != ""
	classSrc := o.classificationSource // capture inside lock for safe use after unlock
	o.mu.RUnlock()
	if alreadyClassified {
		o.logger.Printf("skipping tool classification — already classified via %s", classSrc)
		return
	}

	mode, matched := classify.ClassifyTools(toolsJSON)

	o.mu.Lock()
	o.autoClassification = mode
	o.classificationSource = "tools"
	o.classificationReason = matched
	o.mu.Unlock()

	o.classifiedOnce.Do(func() {
		if o.classified != nil {
			close(o.classified)
		}
	})

	if mode == classify.ModeIsolated {
		o.mu.Lock()
		primaryCwd, cwdCount := o.resetCwdSetToPrimary()
		removed := o.sessionMgr.RemovePendingForOwnerExcept(o.serverID, o.initialAdmissionToken)
		o.mu.Unlock()

		o.logger.Printf("auto-classification: ISOLATED (matched: %v)", matched)
		if primaryCwd != "" {
			o.logger.Printf("reset cwd_set to primary cwd: %s (now %d roots)", primaryCwd, cwdCount)
		}
		o.logger.Printf("restricting IPC listener to reconnect tokens — server requires per-session isolation (revoked_pending=%d)", removed)
		// Disconnect extra sessions that were dedup'd before classification.
		// Keep only the first session — others will reconnect and get their own owner.
		o.evictExtraSessions()
	} else {
		o.logger.Printf("auto-classification: SHARED")
	}
}

// checkPersistent checks if the upstream declares x-mux.persistent: true
// and notifies the daemon via callback.
func (o *Owner) checkPersistent(initJSON []byte) {
	o.ResolvePersistent(classify.ParsePersistent(initJSON))
	persistent := classify.ParsePersistent(initJSON)
	if persistent {
		o.logger.Printf("x-mux capability: persistent=true")
	}
	if o.onPersistentResolved != nil {
		o.onPersistentResolved(o, persistent)
		return
	}
	if persistent && o.onPersistentDetected != nil {
		o.onPersistentDetected(o)
	}
}

// parseToolTimeout extracts x-mux.toolTimeout from the init response
// and stores it atomically for use during tools/call watchdog.
func (o *Owner) parseToolTimeout(initJSON []byte) {
	seconds := classify.ParseToolTimeout(initJSON)
	if seconds > 0 {
		timeout := time.Duration(seconds) * time.Second
		o.toolTimeoutNs.Store(int64(timeout))
		o.logger.Printf("x-mux capability: toolTimeout=%ds", seconds)
	}
}

// parseIdleTimeout extracts x-mux.idleTimeout from the init response
// and stores it atomically for use by the daemon reaper. Overrides
// the daemon-wide default (MCP_MUX_OWNER_IDLE or 10m).
func (o *Owner) parseIdleTimeout(initJSON []byte) {
	seconds := classify.ParseIdleTimeout(initJSON)
	if seconds > 0 {
		timeout := time.Duration(seconds) * time.Second
		o.SetIdleTimeout(timeout)
		o.logger.Printf("x-mux capability: idleTimeout=%ds", seconds)
	}
}

// startToolWatchdog launches a goroutine that fires after toolTimeout.
// If the inflight request is still tracked when the watchdog fires, it
// synthesizes a JSON-RPC error response and sends it to the session,
// preventing eternal hangs when upstream deadlocks during a tool call.
//
// Race design (prevents double response delivery):
//   - The watchdog and handleUpstreamMessage both call LoadAndDelete on
//     inflightTracker. Whichever wins delivers the response.
//   - Watchdog wins → records ID in timedOutIDs → handleUpstreamMessage
//     checks timedOutIDs and drops the late response.
//   - handleUpstreamMessage wins → watchdog's LoadAndDelete misses → exits.
//
// Uses time.NewTimer (with defer Stop) instead of time.After to avoid
// goroutine leaks when the tool call completes naturally before timeout.
//
// Only fires for tools/call requests — other methods (initialize, tools/list)
// have their own timing semantics (cached replays are instant).
func (o *Owner) startToolWatchdog(remappedID string, originalID json.RawMessage, s *Session, method string) {
	timeoutNs := o.toolTimeoutNs.Load()
	if timeoutNs <= 0 {
		return
	}
	timeout := time.Duration(timeoutNs)
	// Copy originalID to avoid aliasing the caller's buffer after this goroutine returns.
	origIDCopy := make(json.RawMessage, len(originalID))
	copy(origIDCopy, originalID)

	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-o.done:
			return
		}
		// Try to claim the inflight. If another path already claimed it, exit.
		if _, ok := o.inflightTracker.LoadAndDelete(remappedID); !ok {
			return
		}
		// Mark this ID as timed-out so handleUpstreamMessage drops any
		// late upstream response (prevents duplicate delivery to session).
		o.timedOutIDs.Store(remappedID, struct{}{})
		o.methodTags.Delete(remappedID)
		o.sessionMgr.CompleteRequest(remappedID)
		o.decrementPending()
		errResp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"mux: %s timed out after %s (upstream did not respond)"}}`,
			string(origIDCopy), method, timeout,
		)
		if err := s.WriteRaw([]byte(errResp)); err != nil {
			o.logger.Printf("watchdog: session %d write error: %v", s.ID, err)
			return
		}
		o.logger.Printf("watchdog: %s (id=%s) timed out after %s, sent error to session %d",
			method, remappedID, timeout, s.ID)
	}()
}

// parseDrainTimeout extracts x-mux.drainTimeout from the init response
// and stores it for use during graceful shutdown.
func (o *Owner) parseDrainTimeout(initJSON []byte) {
	seconds := classify.ParseDrainTimeout(initJSON)
	if seconds > 0 {
		o.drainTimeout = time.Duration(seconds) * time.Second
		o.logger.Printf("x-mux capability: drainTimeout=%ds", seconds)
		// Propagate to upstream process so Close() uses the declared timeout
		o.mu.RLock()
		up := o.upstream
		o.mu.RUnlock()
		if up != nil {
			up.SetDrainTimeout(o.drainTimeout)
		}
	}
}

// DrainTimeout returns the upstream's declared drain timeout, or 0 if not declared.
func (o *Owner) DrainTimeout() time.Duration {
	return o.drainTimeout
}
