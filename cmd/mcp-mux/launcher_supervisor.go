package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/supervisor"
	"github.com/thebtf/mcp-mux/muxcore/supervisor/attest"
)

type launcherStartChildFunc func(
	context.Context,
	string,
	string,
	[]string,
	io.Writer,
) (supervisor.StartResult, error)

type launcherSupervisorConfig struct {
	LauncherPath        string
	InitialEnginePath   string
	Args                []string
	Stdin               io.ReadCloser
	Stdout              io.Writer
	Stderr              io.Writer
	ResolveActiveEngine func(string) (string, bool)
	StartChild          launcherStartChildFunc
	PrepareStart        func(context.Context, string, string, []string, io.Writer) error
	RespawnDelay        time.Duration
	StartTimeout        time.Duration
	ReplayTimeout       time.Duration
	EnginePollInterval  time.Duration
	DormantLease        time.Duration
}

const (
	launcherDormantExitTimeout   = 2 * time.Second
	launcherFinalizationTimeout  = 2 * time.Second
	defaultLauncherStartTimeout  = defaultDaemonSpawnTimeout
	defaultLauncherReplayTimeout = defaultDaemonSpawnTimeout + 5*time.Second
	defaultLauncherRespawnDelay  = 100 * time.Millisecond
	defaultLauncherEnginePoll    = 5 * time.Second
)

var launcherSupervisorEnsureDaemon = func(ctx context.Context, logger *log.Logger, launcherPath, enginePath string) error {
	return ensureDaemonWithinWithStarter(ctx, logger, 15*time.Second, func(startCtx context.Context) error {
		return startDaemonProcessFromContext(startCtx, launcherPath, enginePath)
	})
}

var launcherSupervisorCommandStart = startAttestedSupervisorCommand

func shouldSuperviseEngineProcess(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "status", "stop", "daemon", "upgrade":
		return false
	default:
		return true
	}
}

func runLauncherStdioSupervisor(cfg launcherSupervisorConfig) int {
	if err := runLauncherStdioSupervisorErr(cfg); err != nil {
		stderr := cfg.Stderr
		if stderr == nil {
			stderr = io.Discard
		}
		fmt.Fprintln(stderr, "mcp-mux launcher: stdio supervisor failed")
		return 1
	}
	return 0
}

func runLauncherStdioSupervisorErr(cfg launcherSupervisorConfig) error {
	if cfg.Stdin == nil {
		return errors.New("stdin is nil")
	}
	if cfg.Stdout == nil {
		return errors.New("stdout is nil")
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if cfg.ResolveActiveEngine == nil {
		cfg.ResolveActiveEngine = resolveActiveEngine
	}
	if cfg.StartChild == nil {
		cfg.StartChild = startDefaultSupervisedEngineChild
	}
	if cfg.RespawnDelay == 0 {
		cfg.RespawnDelay = defaultLauncherRespawnDelay
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = defaultLauncherStartTimeout
	}
	if cfg.ReplayTimeout == 0 {
		cfg.ReplayTimeout = defaultLauncherReplayTimeout
	}
	if cfg.EnginePollInterval == 0 {
		cfg.EnginePollInterval = defaultLauncherEnginePoll
	}

	resolve := func(context.Context) (supervisor.EngineRef, error) {
		enginePath, ok := cfg.ResolveActiveEngine(cfg.LauncherPath)
		if !ok || enginePath == "" {
			return supervisor.EngineRef{}, errors.New("launcher active engine path is unavailable")
		}
		return supervisor.EngineRef{ID: canonicalLauncherEnginePath(enginePath)}, nil
	}
	start := func(ctx context.Context, requested supervisor.EngineRef) (supervisor.StartResult, error) {
		if cfg.PrepareStart != nil {
			if err := cfg.PrepareStart(ctx, cfg.LauncherPath, requested.ID, cfg.Args, cfg.Stderr); err != nil {
				return supervisor.StartResult{}, err
			}
		}
		return cfg.StartChild(ctx, cfg.LauncherPath, requested.ID, cfg.Args, cfg.Stderr)
	}

	return supervisor.Run(context.Background(), supervisor.Config{
		HostIn:                   cfg.Stdin,
		HostOut:                  cfg.Stdout,
		Resolve:                  resolve,
		Start:                    start,
		Observe:                  observeLauncherSupervisor,
		ReplacementNotifications: launcherReplacementNotifications(cfg.Args),
		RetryDelay:               cfg.RespawnDelay,
		ReconcileInterval:        cfg.EnginePollInterval,
		StartTimeout:             cfg.StartTimeout,
		ReplayTimeout:            cfg.ReplayTimeout,
		GracefulStopTimeout:      launcherFinalizationTimeout,
		OutputDrainTimeout:       launcherFinalizationTimeout,
		DormantExitTimeout:       launcherDormantExitTimeout,
		DormantLease:             cfg.DormantLease,
	})
}

func launcherReplacementNotifications(args []string) []supervisor.ListChangedKind {
	if len(args) == 0 || args[0] != "serve" {
		return nil
	}
	return []supervisor.ListChangedKind{
		supervisor.ListChangedTools,
		supervisor.ListChangedPrompts,
	}
}

func launcherChildUsesDaemon(args []string) bool {
	return len(args) > 0 && args[0] != "serve" && os.Getenv("MCP_MUX_NO_DAEMON") != "1"
}

func prepareSupervisedEngineStart(ctx context.Context, launcherPath, enginePath string, args []string, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if launcherPath == "" || enginePath == "" {
		return errors.New("launcher or active engine path is empty")
	}
	activeEnginePath, ok := resolveActiveEngine(launcherPath)
	if !ok || !samePath(activeEnginePath, enginePath) || !authorizeInstalledEnginePath(launcherPath, enginePath) {
		return errors.New("active engine path is not authorized")
	}
	if !launcherChildUsesDaemon(args) {
		return nil
	}
	if stderr == nil {
		stderr = io.Discard
	}
	logger := log.New(stderr, "[mcp-mux:launcher] ", log.LstdFlags|log.Lmicroseconds)
	if err := launcherSupervisorEnsureDaemon(ctx, logger, launcherPath, enginePath); err != nil {
		return err
	}
	return ctx.Err()
}

func launcherSupervisorEnv(launcherPath string, args []string) []string {
	env := launcherEnv(launcherPath)
	if launcherChildUsesDaemon(args) {
		env = setEnv(env, envLauncherOwnsDaemon, "1")
	}
	return env
}

func observeLauncherSupervisor(event supervisor.Event) {
	launcherTracef(
		"stdio_supervisor generation=%d state=%s reason=%s fallback=%t",
		event.Generation,
		event.State,
		event.Reason,
		event.Fallback,
	)
}

func canonicalLauncherEnginePath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func launcherSupervisorStartResult(child *supervisor.CommandChild, admission *attest.Parent, actualPath string) supervisor.StartResult {
	result := supervisor.StartResult{
		Actual: supervisor.EngineRef{ID: canonicalLauncherEnginePath(actualPath)},
	}
	if child != nil {
		result.Child = child
	}
	if admission != nil {
		result.Admission = admission
	}
	return result
}

func startDefaultSupervisedEngineChild(
	ctx context.Context,
	launcherPath, enginePath string,
	args []string,
	stderr io.Writer,
) (supervisor.StartResult, error) {
	requested := supervisor.EngineRef{ID: canonicalLauncherEnginePath(enginePath)}
	fallback := supervisor.EngineRef{ID: canonicalLauncherEnginePath(launcherPath)}
	return supervisor.StartWithFallback(ctx, requested, fallback, func(ctx context.Context, ref supervisor.EngineRef) (supervisor.StartResult, error) {
		executablePath := enginePath
		if ref != requested {
			executablePath = launcherPath
		}
		child, admission, err := launcherSupervisorCommandStart(ctx, launcherPath, executablePath, args, stderr)
		return launcherSupervisorStartResult(child, admission, executablePath), err
	})
}

func startAttestedSupervisorCommand(
	ctx context.Context,
	launcherPath, executablePath string,
	args []string,
	stderr io.Writer,
) (*supervisor.CommandChild, *attest.Parent, error) {
	if samePath(executablePath, launcherPath) {
		child, err := supervisor.StartCommand(ctx, supervisor.Command{
			Path:   executablePath,
			Args:   append([]string(nil), args...),
			Env:    launcherSupervisorEnv(launcherPath, args),
			Stderr: stderr,
		})
		return child, nil, err
	}
	activePath, ok := resolveActiveEngine(launcherPath)
	if !ok || !samePath(activePath, executablePath) || !authorizeInstalledEnginePath(launcherPath, executablePath) {
		return nil, nil, errors.New("active engine path is not authorized")
	}
	env := launcherSupervisorEnv(launcherPath, args)
	admission, attestationErr := launcherAttestationStart()
	if attestationErr != nil {
		launcherTracef("attestation unavailable")
		admission = nil
	} else {
		advertisement := admission.Advertisement()
		env = setEnv(env, envLauncherProtocol, advertisement.Version+":"+fmt.Sprint(advertisement.ParentPID))
		env = setEnv(env, envLauncherAttestation, advertisement.Endpoint)
	}

	child, err := supervisor.StartCommand(ctx, supervisor.Command{
		Path:   executablePath,
		Args:   append([]string(nil), args...),
		Env:    env,
		Stderr: stderr,
	})
	if err != nil {
		var closeErr error
		if admission != nil {
			closeErr = admission.Close()
		}
		if errors.Is(err, supervisor.ErrStartRollbackUnproven) || closeErr != nil {
			return child, admission, errors.Join(supervisor.ErrStartRollbackUnproven, err, closeErr)
		}
		return nil, nil, err
	}
	if admission == nil {
		return child, nil, nil
	}
	if err := admission.BindChildPID(child.PID()); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), launcherFinalizationTimeout)
		stopErr := child.Stop(stopCtx)
		cancel()
		closeErr := admission.Close()
		if stopErr != nil || closeErr != nil {
			return child, admission, errors.Join(supervisor.ErrStartRollbackUnproven, err, stopErr, closeErr)
		}
		return nil, nil, fmt.Errorf("bind launcher attestation child: %w", err)
	}
	return child, admission, nil
}
