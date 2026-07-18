package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/supervisor"
	"github.com/thebtf/mcp-mux/muxcore/supervisor/attest"
)

func main() {
	mode := flag.String("mode", "", "old or new launcher")
	engine := flag.String("engine", "", "engine fixture path")
	engineMode := flag.String("engine-mode", "", "engine fixture mode")
	pidFile := flag.String("pid-file", "", "child PID ledger")
	flag.Parse()
	if *engine == "" || *pidFile == "" {
		fmt.Fprintln(os.Stderr, "engine and pid-file are required")
		os.Exit(2)
	}
	var err error
	switch *mode {
	case "old":
		err = runOld(*engine, *engineMode, *pidFile)
	case "new":
		err = runNew(*engine, *engineMode, *pidFile)
	default:
		err = fmt.Errorf("unknown launcher mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runOld(engine, engineMode, pidFile string) error {
	cmd := exec.Command(engine)
	cmd.Env = append(os.Environ(), "MCP_MUX_COMPAT_ENGINE_MODE="+engineMode)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := recordPID(pidFile, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return err
	}
	return cmd.Wait()
}

func runNew(engine, engineMode, pidFile string) error {
	return supervisor.Run(context.Background(), supervisor.Config{
		HostIn:  os.Stdin,
		HostOut: os.Stdout,
		Resolve: func(context.Context) (supervisor.EngineRef, error) {
			return supervisor.EngineRef{ID: engine}, nil
		},
		Start: func(ctx context.Context, requested supervisor.EngineRef) (supervisor.StartResult, error) {
			parent, err := attest.StartParent(ctx, attest.ParentConfig{Lifetime: 2 * time.Second, IOTimeout: time.Second})
			if err != nil {
				return supervisor.StartResult{}, err
			}
			advertisement := parent.Advertisement()
			env := append(os.Environ(),
				"MCP_MUX_COMPAT_ENGINE_MODE="+engineMode,
				"MCP_MUX_COMPAT_ATTEST_VERSION="+advertisement.Version,
				"MCP_MUX_COMPAT_ATTEST_PARENT="+strconv.Itoa(advertisement.ParentPID),
				"MCP_MUX_COMPAT_ATTEST_ENDPOINT="+advertisement.Endpoint,
			)
			child, err := supervisor.StartCommand(ctx, supervisor.Command{Path: engine, Env: env, Stderr: os.Stderr})
			if err != nil {
				_ = parent.Close()
				return supervisor.StartResult{}, err
			}
			if err := parent.BindChildPID(child.PID()); err != nil {
				stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				stopErr := child.Stop(stopCtx)
				cancel()
				closeErr := parent.Close()
				return supervisor.StartResult{}, fmt.Errorf("bind child: %v; stop: %v; close: %v", err, stopErr, closeErr)
			}
			if err := recordPID(pidFile, child.PID()); err != nil {
				stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				stopErr := child.Stop(stopCtx)
				cancel()
				closeErr := parent.Close()
				return supervisor.StartResult{}, fmt.Errorf("record child: %v; stop: %v; close: %v", err, stopErr, closeErr)
			}
			return supervisor.StartResult{Child: child, Actual: requested, Admission: parent}, nil
		},
		RetryDelay:          10 * time.Millisecond,
		ReplayTimeout:       2 * time.Second,
		GracefulStopTimeout: 2 * time.Second,
		OutputDrainTimeout:  2 * time.Second,
		DormantExitTimeout:  2 * time.Second,
	})
}

func recordPID(path string, pid int) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.WriteString(file, strconv.Itoa(pid)+"\n")
	return err
}
