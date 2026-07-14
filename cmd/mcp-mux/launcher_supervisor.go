package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

type launcherSupervisorConfig struct {
	LauncherPath        string
	InitialEnginePath   string
	Args                []string
	Stdin               io.Reader
	Stdout              io.Writer
	Stderr              io.Writer
	ResolveActiveEngine func(string) (string, bool)
	StartChild          func(launcherPath, enginePath string, args []string, stderr io.Writer) (*supervisedEngineChild, error)
	RespawnDelay        time.Duration
	ReplayTimeout       time.Duration
	EnginePollInterval  time.Duration
	DormantLease        time.Duration
}

type supervisedEngineChild struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	kill   func()
	waitCh <-chan error
}

type launcherStdinEvent struct {
	msg *jsonrpc.Message
	err error
}

type launcherChildOutput struct {
	data []byte
	err  error
}

const (
	launcherDormantReadyMethod  = "$/mcp-mux/launcher/dormant-ready"
	launcherCommitDormantMethod = "$/mcp-mux/launcher/commit-dormant"
	launcherDormantAckMethod    = "$/mcp-mux/launcher/dormant-ack"
	launcherDormantNackMethod   = "$/mcp-mux/launcher/dormant-nack"
	launcherDormantExitCode     = 75
	launcherDormantExitTimeout  = 2 * time.Second
	// The supervisor must not kill a replacement while the child is still
	// inside its own legal daemon-spawn budget. Doing so abandons a control
	// request after the daemon may already have created an owner reservation.
	defaultLauncherReplayTimeout = defaultDaemonSpawnTimeout + 5*time.Second
)

type launcherChildState uint8

const (
	launcherChildActive launcherChildState = iota
	launcherChildQuiescing
	launcherChildAwaitingDormantExit
	launcherChildDormant
)

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
		fmt.Fprintf(stderr, "mcp-mux launcher: stdio supervisor failed: %v\n", err)
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
		cfg.RespawnDelay = 100 * time.Millisecond
	}
	if cfg.ReplayTimeout == 0 {
		cfg.ReplayTimeout = defaultLauncherReplayTimeout
	}
	if cfg.EnginePollInterval == 0 {
		cfg.EnginePollInterval = 5 * time.Second
	}

	child, childOut, childEnginePath, err := startLauncherSupervisorChild(cfg, nil, nil, false, nil)
	if err != nil {
		return err
	}
	childWait := child.waitCh
	var childStdoutEOF bool
	var childExited bool
	var childExitErr error
	var dormantInvalid bool
	var enginePoll <-chan time.Time
	var engineTicker *time.Ticker
	if cfg.EnginePollInterval > 0 {
		engineTicker = time.NewTicker(cfg.EnginePollInterval)
		defer engineTicker.Stop()
		enginePoll = engineTicker.C
	}

	stdinCh := make(chan launcherStdinEvent, 16)
	go readLauncherSupervisorStdin(cfg.Stdin, stdinCh)

	inflight := make(map[string]string)
	var initializeRequest []byte
	var initializedNotification []byte
	state := launcherChildActive
	var pendingFrames [][]byte
	var dormantExitTimer *time.Timer
	var dormantExitC <-chan time.Time
	var dormantLeaseTimer *time.Timer
	var dormantLeaseC <-chan time.Time

	stopDormantExitTimer := func() {
		if dormantExitTimer == nil {
			return
		}
		dormantExitTimer.Stop()
		dormantExitTimer = nil
		dormantExitC = nil
	}
	resetDormantExitTimer := func() {
		stopDormantExitTimer()
		dormantExitTimer = time.NewTimer(launcherDormantExitTimeout)
		dormantExitC = dormantExitTimer.C
	}
	stopDormantLeaseTimer := func() {
		if dormantLeaseTimer != nil {
			dormantLeaseTimer.Stop()
			dormantLeaseTimer = nil
			dormantLeaseC = nil
		}
	}
	resetDormantLeaseTimer := func() {
		stopDormantLeaseTimer()
		if cfg.DormantLease > 0 {
			dormantLeaseTimer = time.NewTimer(cfg.DormantLease)
			dormantLeaseC = dormantLeaseTimer.C
		}
	}
	resetChildObservation := func() {
		childWait = child.waitCh
		childStdoutEOF = false
		childExited = false
		childExitErr = nil
		dormantInvalid = false
	}
	childTerminalAction := func() (enterDormant, restart bool) {
		if !childStdoutEOF || !childExited {
			return false, false
		}
		if state == launcherChildAwaitingDormantExit && !dormantInvalid && launcherChildExitCode(childExitErr) == launcherDormantExitCode {
			return true, false
		}
		return false, true
	}
	flushPending := func() error {
		for len(pendingFrames) > 0 {
			raw := pendingFrames[0]
			if err := writeLauncherSupervisorFrame(child.stdin, raw); err != nil {
				return err
			}
			if msg, parseErr := jsonrpc.Parse(raw); parseErr == nil && msg.IsRequest() {
				inflight[string(msg.ID)] = msg.Method
			}
			pendingFrames = pendingFrames[1:]
		}
		return nil
	}
	restartAndFlush := func() error {
		stopDormantExitTimer()
		stopDormantLeaseTimer()
		preserveInitialize := launcherHasInflightMethod(inflight, "initialize")
		launcherDrainInflight(cfg.Stdout, inflight, "initialize")
		if child != nil {
			child.kill()
			child.waitOrKill(2 * time.Second)
		}
		var restartErr error
		child, childOut, childEnginePath, restartErr = restartLauncherSupervisorChild(
			cfg, initializeRequest, initializedNotification, preserveInitialize, inflight,
		)
		if restartErr != nil {
			return restartErr
		}
		resetChildObservation()
		state = launcherChildActive
		return flushPending()
	}
	wakeAndFlush := func() error {
		stopDormantLeaseTimer()
		var startErr error
		child, childOut, childEnginePath, startErr = startLauncherSupervisorChild(
			cfg, initializeRequest, initializedNotification, false, inflight,
		)
		if startErr != nil {
			child, childOut, childEnginePath, startErr = restartLauncherSupervisorChild(
				cfg, initializeRequest, initializedNotification, false, inflight,
			)
		}
		if startErr != nil {
			return startErr
		}
		resetChildObservation()
		state = launcherChildActive
		if err := flushPending(); err != nil {
			return restartAndFlush()
		}
		return nil
	}

	for {
		select {
		case ev := <-stdinCh:
			if ev.err != nil {
				stopDormantLeaseTimer()
				child.closeStdin()
				child.waitOrKill(2 * time.Second)
				if ev.err == io.EOF {
					return nil
				}
				return fmt.Errorf("stdin: %w", ev.err)
			}

			msg := ev.msg
			if msg.IsRequest() && msg.Method == "initialize" {
				initializeRequest = cloneBytes(msg.Raw)
			}
			if msg.IsNotification() && msg.Method == "notifications/initialized" {
				initializedNotification = cloneBytes(msg.Raw)
			}
			if state != launcherChildActive {
				pendingFrames = append(pendingFrames, cloneBytes(msg.Raw))
				if state == launcherChildDormant {
					if err := wakeAndFlush(); err != nil {
						return err
					}
				}
				continue
			}
			if msg.IsRequest() {
				inflight[string(msg.ID)] = msg.Method
			}
			if err := writeLauncherSupervisorFrame(child.stdin, msg.Raw); err != nil {
				if err := restartAndFlush(); err != nil {
					return err
				}
			}

		case out := <-childOut:
			if out.err != nil {
				childOut = nil
				childStdoutEOF = errors.Is(out.err, io.EOF)
				enterDormant, restart := childTerminalAction()
				if enterDormant {
					stopDormantExitTimer()
					child = nil
					childEnginePath = ""
					state = launcherChildDormant
					resetDormantLeaseTimer()
					if len(pendingFrames) > 0 {
						if err := wakeAndFlush(); err != nil {
							return err
						}
					}
				} else if restart || state == launcherChildActive || !childStdoutEOF {
					if err := restartAndFlush(); err != nil {
						return err
					}
				}
				continue
			}

			raw := bytes.TrimRight(out.data, "\r\n")
			if msg, parseErr := jsonrpc.Parse(raw); parseErr == nil {
				if msg.IsNotification() {
					switch msg.Method {
					case launcherDormantReadyMethod:
						if state == launcherChildActive {
							state = launcherChildQuiescing
							resetDormantExitTimer()
							commit := []byte(fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"method\":%q}", launcherCommitDormantMethod))
							if err := writeLauncherSupervisorFrame(child.stdin, commit); err != nil {
								if err := restartAndFlush(); err != nil {
									return err
								}
							}
						}
						continue
					case launcherDormantAckMethod:
						if state == launcherChildQuiescing {
							state = launcherChildAwaitingDormantExit
							resetDormantExitTimer()
						}
						continue
					case launcherDormantNackMethod:
						if state == launcherChildQuiescing || state == launcherChildAwaitingDormantExit {
							stopDormantExitTimer()
							state = launcherChildActive
							if err := flushPending(); err != nil {
								if err := restartAndFlush(); err != nil {
									return err
								}
							}
						}
						continue
					}
				}
				if msg.IsResponse() {
					delete(inflight, string(msg.ID))
				}
			}
			if state == launcherChildAwaitingDormantExit {
				if _, err := cfg.Stdout.Write(out.data); err != nil {
					child.kill()
					return nil
				}
				dormantInvalid = true
				continue
			}
			if _, err := cfg.Stdout.Write(out.data); err != nil {
				child.kill()
				return nil
			}

		case <-dormantExitC:
			if err := restartAndFlush(); err != nil {
				return err
			}

		case <-dormantLeaseC:
			if state == launcherChildDormant {
				return nil
			}

		case exitErr := <-childWait:
			childWait = nil
			if child != nil {
				child.waitCh = nil
			}
			childExited = true
			childExitErr = exitErr
			enterDormant, restart := childTerminalAction()
			if enterDormant {
				stopDormantExitTimer()
				child = nil
				childOut = nil
				childEnginePath = ""
				state = launcherChildDormant
				resetDormantLeaseTimer()
				if len(pendingFrames) > 0 {
					if err := wakeAndFlush(); err != nil {
						return err
					}
				}
			} else if restart {
				if err := restartAndFlush(); err != nil {
					return err
				}
			} else if state == launcherChildActive {
				resetDormantExitTimer()
			}

		case <-enginePoll:
			if state != launcherChildActive {
				continue
			}
			activeEnginePath, ok := cfg.ResolveActiveEngine(cfg.LauncherPath)
			if !ok || activeEnginePath == "" || samePath(activeEnginePath, childEnginePath) {
				continue
			}
			preserveInitialize := launcherHasInflightMethod(inflight, "initialize")
			launcherDrainInflight(cfg.Stdout, inflight, "initialize")
			child.kill()
			child.waitOrKill(2 * time.Second)
			child, childOut, childEnginePath, err = restartLauncherSupervisorChild(
				cfg, initializeRequest, initializedNotification, preserveInitialize, inflight,
			)
			if err != nil {
				return err
			}
			resetChildObservation()
			launcherNotifyListChangedAfterEngineReplacement(cfg.Stdout, cfg.Stderr, cfg.Args)
		}
	}
}

func startDefaultSupervisedEngineChild(launcherPath, enginePath string, args []string, stderr io.Writer) (*supervisedEngineChild, error) {
	cmd := newLauncherEnvCommand(launcherPath, enginePath, args)
	cmd.Stderr = stderr
	child, err := startSupervisedChildCommand(cmd)
	if err == nil {
		return child, nil
	}

	fmt.Fprintf(stderr, "mcp-mux launcher: start active engine %s: %v\n", enginePath, err)
	fmt.Fprintln(stderr, "mcp-mux launcher: starting stable launcher binary as supervised engine fallback")
	fallback := newLauncherEnvCommand(launcherPath, launcherPath, args)
	fallback.Stderr = stderr
	child, fallbackErr := startSupervisedChildCommand(fallback)
	if fallbackErr != nil {
		return nil, fmt.Errorf("start active engine %s: %v; start stable launcher fallback %s: %w", enginePath, err, launcherPath, fallbackErr)
	}
	return child, nil
}

func startSupervisedChildCommand(cmd *exec.Cmd) (*supervisedEngineChild, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := startLauncherEnvCommand(cmd); err != nil {
		_ = stdin.Close()
		return nil, err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	return &supervisedEngineChild{
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		waitCh: waitCh,
		kill: func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		},
	}, nil
}

func startLauncherSupervisorChild(cfg launcherSupervisorConfig, initializeRequest, initializedNotification []byte, forwardInitializeResponse bool, inflight map[string]string) (*supervisedEngineChild, <-chan launcherChildOutput, string, error) {
	enginePath := cfg.InitialEnginePath
	if active, ok := cfg.ResolveActiveEngine(cfg.LauncherPath); ok && active != "" {
		enginePath = active
	}

	child, err := cfg.StartChild(cfg.LauncherPath, enginePath, cfg.Args, cfg.Stderr)
	if err != nil {
		return nil, nil, "", fmt.Errorf("start engine child: %w", err)
	}

	var prelude [][]byte
	if len(initializeRequest) > 0 {
		response, preserved, err := replayLauncherSupervisorHandshake(child, initializeRequest, initializedNotification, cfg.ReplayTimeout)
		if err != nil {
			child.kill()
			child.waitOrKill(2 * time.Second)
			return nil, nil, "", fmt.Errorf("replay initialize into replacement engine: %w", err)
		}
		prelude = preserved
		if forwardInitializeResponse {
			prelude = append(prelude, response)
			if msg, parseErr := jsonrpc.Parse(bytes.TrimRight(response, "\r\n")); parseErr == nil && msg.IsResponse() && inflight != nil {
				delete(inflight, string(msg.ID))
			}
		}
	}

	out := make(chan launcherChildOutput, 16)
	go func() {
		for _, line := range prelude {
			out <- launcherChildOutput{data: line}
		}
		pumpLauncherSupervisorStdout(child.stdout, out)
	}()
	return child, out, enginePath, nil
}

func restartLauncherSupervisorChild(cfg launcherSupervisorConfig, initializeRequest, initializedNotification []byte, forwardInitializeResponse bool, inflight map[string]string) (*supervisedEngineChild, <-chan launcherChildOutput, string, error) {
	for {
		time.Sleep(cfg.RespawnDelay)
		child, childOut, enginePath, err := startLauncherSupervisorChild(cfg, initializeRequest, initializedNotification, forwardInitializeResponse, inflight)
		if err == nil {
			fmt.Fprintln(cfg.Stderr, "mcp-mux launcher: replacement engine attached; stdio transport preserved")
			return child, childOut, enginePath, nil
		}
		fmt.Fprintf(cfg.Stderr, "mcp-mux launcher: replacement engine start failed: %v; retrying\n", err)
	}
}

func launcherNotifyListChangedAfterEngineReplacement(stdout, stderr io.Writer, args []string) {
	if len(args) == 0 || args[0] != "serve" {
		return
	}
	for _, method := range []string{
		"notifications/tools/list_changed",
		"notifications/prompts/list_changed",
	} {
		if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","method":%q}`+"\n", method); err != nil {
			fmt.Fprintf(stderr, "mcp-mux launcher: failed to send %s after engine replacement: %v\n", method, err)
			return
		}
	}
}

func readLauncherSupervisorStdin(r io.Reader, out chan<- launcherStdinEvent) {
	scanner := jsonrpc.NewScanner(r)
	for {
		msg, err := scanner.Scan()
		if err != nil {
			out <- launcherStdinEvent{err: err}
			return
		}
		out <- launcherStdinEvent{msg: msg}
	}
}

func pumpLauncherSupervisorStdout(r *bufio.Reader, out chan<- launcherChildOutput) {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			data := cloneBytes(line)
			out <- launcherChildOutput{data: data}
		}
		if err != nil {
			out <- launcherChildOutput{err: err}
			return
		}
	}
}

func replayLauncherSupervisorHandshake(child *supervisedEngineChild, initializeRequest, initializedNotification []byte, timeout time.Duration) ([]byte, [][]byte, error) {
	request, err := jsonrpc.Parse(bytes.TrimSpace(initializeRequest))
	if err != nil || !request.IsRequest() || request.Method != "initialize" {
		return nil, nil, errors.New("cached initialize request is invalid")
	}
	initializeID := string(request.ID)
	if err := writeLauncherSupervisorFrame(child.stdin, initializeRequest); err != nil {
		return nil, nil, fmt.Errorf("write initialize: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var preserved [][]byte
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil, fmt.Errorf("read initialize response: timeout after %s", timeout)
		}
		line, err := readLauncherSupervisorLineWithTimeout(child.stdout, remaining)
		if err != nil {
			return nil, nil, fmt.Errorf("read initialize response: %w", err)
		}
		msg, parseErr := jsonrpc.Parse(bytes.TrimRight(line, "\r\n"))
		if parseErr == nil && msg.IsResponse() && string(msg.ID) == initializeID {
			if len(initializedNotification) > 0 {
				if err := writeLauncherSupervisorFrame(child.stdin, initializedNotification); err != nil {
					return nil, nil, fmt.Errorf("write initialized notification: %w", err)
				}
			}
			return line, preserved, nil
		}
		preserved = append(preserved, line)
	}
}

func readLauncherSupervisorLineWithTimeout(r *bufio.Reader, timeout time.Duration) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadBytes('\n')
		ch <- result{line: line, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return res.line, nil
	case <-timer.C:
		return nil, fmt.Errorf("timeout after %s", timeout)
	}
}

func writeLauncherSupervisorFrame(w io.Writer, raw []byte) error {
	_, err := fmt.Fprintf(w, "%s\n", raw)
	return err
}

func launcherChildExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func launcherDrainInflight(stdout io.Writer, inflight map[string]string, preserveMethod string) {
	if len(inflight) == 0 {
		return
	}
	ids := make([]string, 0, len(inflight))
	for id, method := range inflight {
		if preserveMethod != "" && method == preserveMethod {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		delete(inflight, id)
		_, _ = fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"mcp-mux engine restarted, request lost during launcher reconnect"}}`+"\n", id)
	}
}

func launcherHasInflightMethod(inflight map[string]string, method string) bool {
	for _, got := range inflight {
		if got == method {
			return true
		}
	}
	return false
}

func (c *supervisedEngineChild) closeStdin() {
	if c != nil && c.stdin != nil {
		_ = c.stdin.Close()
	}
}

func (c *supervisedEngineChild) waitOrKill(timeout time.Duration) {
	if c == nil || c.waitCh == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.waitCh:
	case <-timer.C:
		c.kill()
	}
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
