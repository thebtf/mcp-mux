package owner

import (
	"fmt"
	"log"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/progress"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

// NewOwnerFromHandoff creates an Owner backed by an already-running upstream
// process received via daemon handoff. Instead of spawning a new upstream process
// (as NewOwner does), it wraps the transferred file descriptors from payload
// and starts the standard IPC listener and monitoring goroutines.
//
// If cfg.CachedClassification is non-empty, the classification round-trip is
// skipped and the provided mode is used directly (the old daemon already
// classified the upstream; this propagates the result to the successor).
//
// Returns an error if the FD attachment fails or the IPC listener cannot bind.
func NewOwnerFromHandoff(cfg OwnerConfig, payload HandoffPayload) (*Owner, error) {
	proc, err := upstream.AttachFromFDs(payload.PID, payload.StdinFD, payload.StdoutFD, payload.Command)
	if err != nil {
		return nil, fmt.Errorf("owner: NewOwnerFromHandoff: attach FDs: %w", err)
	}
	return newOwnerWithProcess(cfg, payload, proc)
}

// newOwnerWithProcess is the shared constructor body for NewOwnerFromHandoff.
// It accepts a pre-constructed *upstream.Process so that unit tests can inject
// a handler-based process (via upstream.NewProcessFromHandler) without needing
// real transferred OS file descriptors. Production code always passes the result
// of upstream.AttachFromFDs.
func newOwnerWithProcess(cfg OwnerConfig, payload HandoffPayload, proc *upstream.Process) (*Owner, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	ln, err := ipc.Listen(cfg.IPCPath)
	if err != nil {
		if proc != nil {
			proc.Close()
		}
		return nil, fmt.Errorf("owner: NewOwnerFromHandoff: listen %s: %w", cfg.IPCPath, err)
	}

	// Resolve identity: prefer payload fields, fall back to cfg.
	cwd := payload.Cwd
	if cwd == "" {
		cwd = cfg.Cwd
	}
	srvID := payload.ServerID
	if srvID == "" {
		srvID = cfg.ServerID
	}

	o := &Owner{
		upstream:               proc,
		ipcPath:                cfg.IPCPath,
		cwd:                    cwd,
		cwdSet:                 map[string]bool{serverid.CanonicalizePath(cwd): true},
		command:                payload.Command,
		args:                   payload.Args,
		env:                    cfg.Env,
		handlerFunc:            cfg.HandlerFunc,
		sessionHandler:         cfg.SessionHandler,
		serverID:               srvID,
		listener:               ln,
		logger:                 logger,
		onZeroSessions:         cfg.OnZeroSessions,
		onUpstreamExit:         cfg.OnUpstreamExit,
		onPersistentDetected:   cfg.OnPersistentDetected,
		onCacheReady:           cfg.OnCacheReady,
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
	}
	o.progressIntervalNs.Store(int64(5 * time.Second))

	if o.tokenHandshake {
		o.rejectionLogger = newRejectionLogger(logger)
	}

	// Apply cached classification if provided — skip the classify round-trip.
	if cfg.CachedClassification != "" {
		o.autoClassification = cfg.CachedClassification
		o.classificationSource = "handoff"
		o.classificationReason = nil
		o.classifiedOnce.Do(func() { close(o.classified) })
	}

	// Wire notifier into sessionHandler if it supports NotifierAware.
	if o.sessionHandler != nil {
		if na, ok := o.sessionHandler.(muxcore.NotifierAware); ok {
			na.SetNotifier(&ownerNotifier{owner: o})
		}
	}

	// Start control plane if configured.
	if cfg.ControlPath != "" {
		ctlSrv, err := control.NewServer(cfg.ControlPath, o, logger)
		if err != nil {
			logger.Printf("warning: control server failed to start: %v", err)
		} else {
			o.controlServer = ctlSrv
		}
	}

	// Start goroutines — identical set to NewOwner.
	go o.readUpstream()
	go o.sendProactiveInit()
	go o.acceptLoop()
	go o.runProgressReporter(doneContext(o.done))

	// Monitor upstream exit. Attached processes are NOT managed by suture
	// (NewOwnerFromHandoff bypasses Serve()), so we need our own watcher.
	// Select between proc.Done (real exit) and o.done (owner shutdown) so the
	// goroutine never leaks in tests that use handler-based stubs whose
	// proc.Done only closes via context cancellation.
	go func() {
		select {
		case <-proc.Done:
			logger.Printf("handoff upstream exited (pid=%d, cmd=%s): %v", payload.PID, payload.Command, proc.ExitErr)
			o.initReadyOnce.Do(func() {
				if o.initReady != nil {
					close(o.initReady)
				}
			})
			if o.onUpstreamExit != nil {
				o.onUpstreamExit(o.serverID)
			} else {
				o.Shutdown()
			}
		case <-o.done:
			// Owner shutdown — bail without touching initReady or onUpstreamExit.
			return
		}
	}()

	logger.Printf("owner reattached from handoff (pid=%d, server=%s)", payload.PID, srvID)
	return o, nil
}
