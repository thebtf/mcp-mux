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
		cfg.ReplayTimeout = 5 * time.Second
	}

	child, childOut, err := startLauncherSupervisorChild(cfg, nil, nil, false, nil)
	if err != nil {
		return err
	}

	stdinCh := make(chan launcherStdinEvent, 16)
	go readLauncherSupervisorStdin(cfg.Stdin, stdinCh)

	inflight := make(map[string]string)
	var initializeRequest []byte
	var initializedNotification []byte

	for {
		select {
		case ev := <-stdinCh:
			if ev.err != nil {
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
			if msg.IsRequest() {
				inflight[string(msg.ID)] = msg.Method
			}
			if err := writeLauncherSupervisorFrame(child.stdin, msg.Raw); err != nil {
				preserveInitialize := launcherHasInflightMethod(inflight, "initialize")
				launcherDrainInflight(cfg.Stdout, inflight, "initialize")
				child.kill()
				child, childOut, err = restartLauncherSupervisorChild(cfg, initializeRequest, initializedNotification, preserveInitialize, inflight)
				if err != nil {
					return err
				}
			}

		case out := <-childOut:
			if out.err != nil {
				preserveInitialize := launcherHasInflightMethod(inflight, "initialize")
				launcherDrainInflight(cfg.Stdout, inflight, "initialize")
				child.kill()
				child, childOut, err = restartLauncherSupervisorChild(cfg, initializeRequest, initializedNotification, preserveInitialize, inflight)
				if err != nil {
					return err
				}
				continue
			}

			raw := bytes.TrimRight(out.data, "\r\n")
			if msg, parseErr := jsonrpc.Parse(raw); parseErr == nil && msg.IsResponse() {
				delete(inflight, string(msg.ID))
			}
			if _, err := cfg.Stdout.Write(out.data); err != nil {
				child.kill()
				return nil
			}
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
	if err := cmd.Start(); err != nil {
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

func startLauncherSupervisorChild(cfg launcherSupervisorConfig, initializeRequest, initializedNotification []byte, forwardInitializeResponse bool, inflight map[string]string) (*supervisedEngineChild, <-chan launcherChildOutput, error) {
	enginePath := cfg.InitialEnginePath
	if active, ok := cfg.ResolveActiveEngine(cfg.LauncherPath); ok && active != "" {
		enginePath = active
	}

	child, err := cfg.StartChild(cfg.LauncherPath, enginePath, cfg.Args, cfg.Stderr)
	if err != nil {
		return nil, nil, fmt.Errorf("start engine child: %w", err)
	}

	if len(initializeRequest) > 0 {
		response, err := replayLauncherSupervisorHandshake(child, initializeRequest, initializedNotification, cfg.ReplayTimeout)
		if err != nil {
			child.kill()
			child.waitOrKill(2 * time.Second)
			return nil, nil, fmt.Errorf("replay initialize into replacement engine: %w", err)
		}
		if forwardInitializeResponse {
			if _, err := cfg.Stdout.Write(response); err != nil {
				child.kill()
				child.waitOrKill(2 * time.Second)
				return nil, nil, fmt.Errorf("forward replayed initialize response: %w", err)
			}
			if msg, parseErr := jsonrpc.Parse(bytes.TrimRight(response, "\r\n")); parseErr == nil && msg.IsResponse() && inflight != nil {
				delete(inflight, string(msg.ID))
			}
		}
	}

	out := make(chan launcherChildOutput, 16)
	go pumpLauncherSupervisorStdout(child.stdout, out)
	return child, out, nil
}

func restartLauncherSupervisorChild(cfg launcherSupervisorConfig, initializeRequest, initializedNotification []byte, forwardInitializeResponse bool, inflight map[string]string) (*supervisedEngineChild, <-chan launcherChildOutput, error) {
	for {
		time.Sleep(cfg.RespawnDelay)
		child, childOut, err := startLauncherSupervisorChild(cfg, initializeRequest, initializedNotification, forwardInitializeResponse, inflight)
		if err == nil {
			fmt.Fprintln(cfg.Stderr, "mcp-mux launcher: replacement engine attached; stdio transport preserved")
			return child, childOut, nil
		}
		fmt.Fprintf(cfg.Stderr, "mcp-mux launcher: replacement engine start failed: %v; retrying\n", err)
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

func replayLauncherSupervisorHandshake(child *supervisedEngineChild, initializeRequest, initializedNotification []byte, timeout time.Duration) ([]byte, error) {
	if err := writeLauncherSupervisorFrame(child.stdin, initializeRequest); err != nil {
		return nil, fmt.Errorf("write initialize: %w", err)
	}
	response, err := readLauncherSupervisorLineWithTimeout(child.stdout, timeout)
	if err != nil {
		return nil, fmt.Errorf("read initialize response: %w", err)
	}
	if len(initializedNotification) > 0 {
		if err := writeLauncherSupervisorFrame(child.stdin, initializedNotification); err != nil {
			return nil, fmt.Errorf("write initialized notification: %w", err)
		}
	}
	return response, nil
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
