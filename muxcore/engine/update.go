package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/upgrade"
)

const (
	defaultUpdateDrainTimeout    = 30 * time.Second
	defaultUpdateRestartTimeout  = 60 * time.Second
	defaultUpdateShutdownTimeout = 10 * time.Second
	defaultUpdateReadyTimeout    = 10 * time.Second
)

// UpdatePhase identifies the phase that failed during ApplyUpdateAndRestart.
type UpdatePhase string

const (
	UpdatePhaseValidate UpdatePhase = "validate"
	UpdatePhaseSwap     UpdatePhase = "swap"
	UpdatePhaseLock     UpdatePhase = "lock"
	UpdatePhaseRestart  UpdatePhase = "graceful_restart"
	UpdatePhaseWaitExit UpdatePhase = "wait_exit"
	UpdatePhaseStart    UpdatePhase = "start_replacement"
	UpdatePhaseReady    UpdatePhase = "wait_ready"
)

// UpdateAndRestartOptions configures ApplyUpdateAndRestart.
type UpdateAndRestartOptions struct {
	// CurrentExe is the replaceable engine executable path that should exist
	// after the swap. Do not pass a stable launcher path here unless that
	// launcher is deliberately replaceable while the product is running.
	CurrentExe string
	// StagedExe is the replacement binary to move into CurrentExe.
	StagedExe string
	// DrainTimeout is sent to the daemon's graceful-restart command.
	DrainTimeout time.Duration
	// RestartTimeout bounds the graceful-restart control RPC.
	RestartTimeout time.Duration
	// ShutdownTimeout bounds waiting for the old daemon to stop responding.
	ShutdownTimeout time.Duration
	// ReadyTimeout bounds waiting for the replacement daemon to answer ping.
	ReadyTimeout time.Duration
	// CleanStale removes stale old-binary artifacts after the restart attempt.
	CleanStale bool
	// ProceedWithoutLock allows restart to continue when the daemon namespace
	// lock cannot be acquired. The default is fail-closed.
	ProceedWithoutLock bool
}

// RestartWithSuccessorOptions configures RestartWithSuccessor.
type RestartWithSuccessorOptions struct {
	// SuccessorExe is the executable the daemon should use for the replacement
	// process. Use this for stable launcher or versioned engine store topologies
	// where the caller has already staged the engine and updated its active
	// pointer. Unlike ApplyUpdateAndRestart, this helper does not rename files.
	SuccessorExe string
	// DrainTimeout is sent to the daemon's graceful-restart command.
	DrainTimeout time.Duration
	// RestartTimeout bounds the graceful-restart control RPC.
	RestartTimeout time.Duration
	// ShutdownTimeout bounds waiting for the old daemon to stop responding.
	ShutdownTimeout time.Duration
	// ReadyTimeout bounds waiting for the replacement daemon to answer ping.
	ReadyTimeout time.Duration
	// ProceedWithoutLock allows restart to continue when the daemon namespace
	// lock cannot be acquired. The default is fail-closed.
	ProceedWithoutLock bool
}

// UpdateAndRestartResult reports each observable step of ApplyUpdateAndRestart.
type UpdateAndRestartResult struct {
	OldPath            string
	DaemonWasRunning   bool
	LockAcquired       bool
	GracefulRestarted  bool
	FallbackShutdown   bool
	ReplacementStarted bool
	ReplacementReady   bool
	CleanedStale       int
	Warnings           []string
}

// UpdateAndRestartError wraps a phase-specific failure and preserves the
// partial result so callers can report whether the binary swap already landed.
type UpdateAndRestartError struct {
	Phase  UpdatePhase
	Result UpdateAndRestartResult
	Err    error
}

func (e *UpdateAndRestartError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("apply update and restart failed during %s: %v", e.Phase, e.Err)
}

func (e *UpdateAndRestartError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type daemonLock interface {
	Close() error
}

type fileDaemonLock struct {
	f *os.File
}

type daemonIdentity struct {
	pid        int
	generation string
}

func (id daemonIdentity) isZero() bool {
	return id.pid == 0 && id.generation == ""
}

func (id daemonIdentity) same(other daemonIdentity) bool {
	if id.generation != "" && other.generation != "" {
		return id.generation == other.generation
	}
	if id.pid != 0 && other.pid != 0 {
		return id.pid == other.pid
	}
	return false
}

func (l *fileDaemonLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := unlockDaemonFile(l.f)
	closeErr := l.f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

var (
	engineUpgradeSwap            = upgrade.Swap
	engineCleanStale             = upgrade.CleanStale
	engineControlSend            = control.Send
	engineControlSendWithTimeout = control.SendWithTimeout
	engineIsDaemonRunning        = isDaemonRunning
	engineStartDaemonExecutable  = startDaemonExecutable
	engineWaitForDaemonReady     = waitForDaemonReady
	engineWaitForDaemonExit      = waitForDaemonExit
	engineWaitForReplacement     = waitForDaemonReplacementOrExit
	engineDaemonIdentity         = daemonIdentityFromStatus
	enginePrepareControlSocket   = prepareControlSocketForReplacement
	engineControlSocketAvailable = ipc.IsAvailable
	engineAcquireDaemonLock      = acquireDaemonLock
)

// ApplyUpdateAndRestart swaps in a staged binary and restarts this engine's
// daemon namespace using muxcore's graceful-restart control path. It is intended
// for embedded consumers that use a fixed replaceable engine path and need the
// same restart choreography as the mcp-mux binary without copying cmd/mcp-mux
// code.
func (e *MuxEngine) ApplyUpdateAndRestart(ctx context.Context, opts UpdateAndRestartOptions) (result UpdateAndRestartResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = opts.withDefaults()
	if err := ctx.Err(); err != nil {
		return result, phaseError(UpdatePhaseValidate, result, err)
	}
	if opts.CurrentExe == "" {
		return result, phaseError(UpdatePhaseValidate, result, errors.New("CurrentExe is required"))
	}
	if opts.StagedExe == "" {
		return result, phaseError(UpdatePhaseValidate, result, errors.New("StagedExe is required"))
	}

	oldPath, err := engineUpgradeSwap(opts.CurrentExe, opts.StagedExe)
	if err != nil {
		return result, phaseError(UpdatePhaseSwap, result, err)
	}
	result.OldPath = oldPath
	defer func() {
		if opts.CleanStale {
			result.CleanedStale = engineCleanStale(opts.CurrentExe)
			var updateErr *UpdateAndRestartError
			if errors.As(err, &updateErr) {
				updateErr.Result.CleanedStale = result.CleanedStale
			}
		}
	}()

	ctlPath := e.ControlSocketPath()
	if !engineIsDaemonRunning(ctlPath) {
		return result, nil
	}
	return e.restartDaemonWithSuccessor(ctx, RestartWithSuccessorOptions{
		SuccessorExe:       opts.CurrentExe,
		DrainTimeout:       opts.DrainTimeout,
		RestartTimeout:     opts.RestartTimeout,
		ShutdownTimeout:    opts.ShutdownTimeout,
		ReadyTimeout:       opts.ReadyTimeout,
		ProceedWithoutLock: opts.ProceedWithoutLock,
	}, result)
}

// RestartWithSuccessor restarts this engine's daemon namespace with an explicit
// successor executable without moving binaries on disk. It is intended for
// stable launcher and versioned engine store consumers that update their active
// engine pointer themselves, then need muxcore's standard graceful-restart,
// shutdown fallback, replacement start, and readiness choreography.
func (e *MuxEngine) RestartWithSuccessor(ctx context.Context, opts RestartWithSuccessorOptions) (result UpdateAndRestartResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = opts.withDefaults()
	if err := ctx.Err(); err != nil {
		return result, phaseError(UpdatePhaseValidate, result, err)
	}
	if opts.SuccessorExe == "" {
		return result, phaseError(UpdatePhaseValidate, result, errors.New("SuccessorExe is required"))
	}
	return e.restartDaemonWithSuccessor(ctx, opts, result)
}

func (e *MuxEngine) restartDaemonWithSuccessor(ctx context.Context, opts RestartWithSuccessorOptions, result UpdateAndRestartResult) (UpdateAndRestartResult, error) {
	ctlPath := e.ControlSocketPath()
	if !engineIsDaemonRunning(ctlPath) {
		return result, nil
	}
	result.DaemonWasRunning = true
	beforeRestart, identityErr := engineDaemonIdentity(ctlPath)
	if identityErr != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not read daemon identity before restart: %v", identityErr))
	}

	lockPath := serverid.DaemonLockPath(e.cfg.BaseDir, e.cfg.Namespace)
	lock, err := engineAcquireDaemonLock(lockPath)
	if err != nil {
		if !opts.ProceedWithoutLock {
			return result, phaseError(UpdatePhaseLock, result, err)
		}
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not acquire daemon lock %s: %v", lockPath, err))
	} else {
		result.LockAcquired = true
		defer func() {
			if closeErr := lock.Close(); closeErr != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("could not release daemon lock %s: %v", lockPath, closeErr))
			}
		}()
	}

	if err := ctx.Err(); err != nil {
		return result, phaseError(UpdatePhaseRestart, result, err)
	}
	resp, err := engineControlSendWithTimeout(ctlPath, control.Request{
		Cmd:            "graceful-restart",
		DrainTimeoutMs: durationMillis(opts.DrainTimeout),
		SuccessorExe:   opts.SuccessorExe,
	}, opts.RestartTimeout)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("graceful-restart unavailable: %v", err))
		result.FallbackShutdown = true
	} else if !resp.OK {
		result.Warnings = append(result.Warnings, fmt.Sprintf("graceful-restart failed: %s", resp.Message))
		result.FallbackShutdown = true
	} else {
		result.GracefulRestarted = true
	}

	if result.FallbackShutdown {
		if shutdownResp, shutdownErr := engineControlSend(ctlPath, control.Request{Cmd: "shutdown"}); shutdownErr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("shutdown fallback send failed: %v", shutdownErr))
		} else if !shutdownResp.OK {
			result.Warnings = append(result.Warnings, fmt.Sprintf("shutdown fallback failed: %s", shutdownResp.Message))
		}
	}

	successorAlreadyStarted := false
	if result.GracefulRestarted && !beforeRestart.isZero() {
		replaced, waitErr := engineWaitForReplacement(ctx, ctlPath, beforeRestart, opts.ShutdownTimeout)
		if waitErr != nil {
			return result, phaseError(UpdatePhaseWaitExit, result, waitErr)
		}
		successorAlreadyStarted = replaced
	} else {
		waitErr := engineWaitForDaemonExit(ctx, ctlPath, opts.ShutdownTimeout)
		if waitErr != nil {
			return result, phaseError(UpdatePhaseWaitExit, result, waitErr)
		}
	}

	controlPrepared := false
	replacementActive := func() bool {
		if beforeRestart.isZero() {
			return false
		}
		current, err := engineDaemonIdentity(ctlPath)
		return err == nil && !current.same(beforeRestart)
	}
	prepareControlSocket := func() (bool, error) {
		if controlPrepared {
			return false, nil
		}
		if replacementActive() {
			return true, nil
		}
		if err := enginePrepareControlSocket(ctx, ctlPath, opts.ShutdownTimeout); err != nil {
			if replacementActive() {
				return true, nil
			}
			return false, err
		}
		controlPrepared = true
		return false, nil
	}

	if successorAlreadyStarted {
		result.ReplacementStarted = true
		if readyErr := engineWaitForDaemonReady(ctx, ctlPath, opts.ReadyTimeout); readyErr == nil {
			result.ReplacementReady = true
			return result, nil
		} else {
			return result, phaseError(UpdatePhaseReady, result, readyErr)
		}
	}

	if result.GracefulRestarted {
		active, err := prepareControlSocket()
		if err != nil {
			return result, phaseError(UpdatePhaseStart, result, err)
		}
		if active {
			result.ReplacementStarted = true
			if readyErr := engineWaitForDaemonReady(ctx, ctlPath, opts.ReadyTimeout); readyErr == nil {
				result.ReplacementReady = true
				return result, nil
			} else {
				return result, phaseError(UpdatePhaseReady, result, readyErr)
			}
		}
		if readyErr := engineWaitForDaemonReady(ctx, ctlPath, opts.ReadyTimeout); readyErr == nil {
			result.ReplacementReady = true
			result.ReplacementStarted = true
			return result, nil
		} else {
			result.Warnings = append(result.Warnings, fmt.Sprintf("graceful successor not ready: %v", readyErr))
		}
	}

	if err := ctx.Err(); err != nil {
		return result, phaseError(UpdatePhaseStart, result, err)
	}
	active, err := prepareControlSocket()
	if err != nil {
		return result, phaseError(UpdatePhaseStart, result, err)
	}
	if active {
		result.ReplacementStarted = true
		if readyErr := engineWaitForDaemonReady(ctx, ctlPath, opts.ReadyTimeout); readyErr != nil {
			return result, phaseError(UpdatePhaseReady, result, readyErr)
		}
		result.ReplacementReady = true
		return result, nil
	}
	if err := engineStartDaemonExecutable(opts.SuccessorExe, e.cfg.DaemonFlag); err != nil {
		return result, phaseError(UpdatePhaseStart, result, err)
	}
	result.ReplacementStarted = true

	if err := engineWaitForDaemonReady(ctx, ctlPath, opts.ReadyTimeout); err != nil {
		return result, phaseError(UpdatePhaseReady, result, err)
	}
	result.ReplacementReady = true
	return result, nil
}

func (opts UpdateAndRestartOptions) withDefaults() UpdateAndRestartOptions {
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = defaultUpdateDrainTimeout
	}
	if opts.RestartTimeout <= 0 {
		opts.RestartTimeout = defaultUpdateRestartTimeout
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultUpdateShutdownTimeout
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = defaultUpdateReadyTimeout
	}
	return opts
}

func (opts RestartWithSuccessorOptions) withDefaults() RestartWithSuccessorOptions {
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = defaultUpdateDrainTimeout
	}
	if opts.RestartTimeout <= 0 {
		opts.RestartTimeout = defaultUpdateRestartTimeout
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultUpdateShutdownTimeout
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = defaultUpdateReadyTimeout
	}
	return opts
}

func phaseError(phase UpdatePhase, result UpdateAndRestartResult, err error) error {
	return &UpdateAndRestartError{Phase: phase, Result: result, Err: err}
}

func durationMillis(d time.Duration) int {
	ms := d / time.Millisecond
	if ms <= 0 {
		return 1
	}
	return int(ms)
}

func acquireDaemonLock(lockPath string) (daemonLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockDaemonFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &fileDaemonLock{f: f}, nil
}

func startDaemonExecutable(exe, daemonFlag string) error {
	cmd := exec.Command(exe, daemonFlag)
	closeStdio, err := attachDetachedStdio(cmd)
	if err != nil {
		return err
	}
	defer closeStdio()
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon process: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}
	return nil
}

func waitForDaemonReady(ctx context.Context, ctlPath string, timeout time.Duration) error {
	return waitForDaemonCondition(ctx, timeout, func() bool {
		return engineIsDaemonRunning(ctlPath)
	}, fmt.Sprintf("daemon did not start within %s", timeout))
}

func waitForDaemonExit(ctx context.Context, ctlPath string, timeout time.Duration) error {
	return waitForDaemonCondition(ctx, timeout, func() bool {
		return !engineIsDaemonRunning(ctlPath)
	}, fmt.Sprintf("daemon did not exit within %s", timeout))
}

func waitForDaemonReplacementOrExit(ctx context.Context, ctlPath string, previous daemonIdentity, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = defaultUpdateShutdownTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(daemonPollInterval)
	defer ticker.Stop()
	var lastErr error

	for {
		current, err := engineDaemonIdentity(ctlPath)
		if err == nil {
			if !current.same(previous) {
				return true, nil
			}
		} else if !engineIsDaemonRunning(ctlPath) {
			return false, nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			if lastErr != nil {
				return false, fmt.Errorf("daemon did not exit or hand off within %s: %w", timeout, lastErr)
			}
			return false, fmt.Errorf("daemon did not exit or hand off within %s", timeout)
		case <-ticker.C:
		}
	}
}

func daemonIdentityFromStatus(ctlPath string) (daemonIdentity, error) {
	resp, err := engineControlSend(ctlPath, control.Request{Cmd: "status"})
	if err != nil {
		return daemonIdentity{}, err
	}
	if resp == nil {
		return daemonIdentity{}, errors.New("status returned nil response")
	}
	if !resp.OK {
		return daemonIdentity{}, fmt.Errorf("status failed: %s", resp.Message)
	}
	var data struct {
		PID        int    `json:"pid"`
		Generation string `json:"daemon_generation"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return daemonIdentity{}, fmt.Errorf("decode status identity: %w", err)
	}
	id := daemonIdentity{pid: data.PID, generation: data.Generation}
	if id.isZero() {
		return daemonIdentity{}, errors.New("status missing pid and daemon_generation")
	}
	return id, nil
}

func prepareControlSocketForReplacement(ctx context.Context, ctlPath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultUpdateShutdownTimeout
	}
	if ctlPath == "" {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(daemonPollInterval)
	defer ticker.Stop()
	for {
		if !engineIsDaemonRunning(ctlPath) && !engineControlSocketAvailable(ctlPath) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("control socket %s was still active", ctlPath)
		case <-ticker.C:
		}
	}
}

func waitForDaemonCondition(ctx context.Context, timeout time.Duration, done func() bool, timeoutMsg string) error {
	if timeout <= 0 {
		timeout = defaultUpdateReadyTimeout
	}
	if done() {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(daemonPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if done() {
				return nil
			}
			return errors.New(timeoutMsg)
		case <-ticker.C:
			if done() {
				return nil
			}
		}
	}
}
