package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/thebtf/mcp-mux/muxcore/supervisor"
)

var testLauncherProtocol = supervisor.ProtocolV2()

var (
	launcherDormantReadyMethod  = launcherTestControlMethod(supervisor.ControlDormantReady)
	launcherCommitDormantMethod = launcherTestControlMethod(supervisor.ControlCommitDormant)
	launcherDormantAckMethod    = launcherTestControlMethod(supervisor.ControlDormantAck)
	launcherDormantNackMethod   = launcherTestControlMethod(supervisor.ControlDormantNack)
	launcherDormantExitCode     = testLauncherProtocol.DormantExitCode()
)

func launcherTestControlMethod(control supervisor.Control) string {
	var envelope struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(testLauncherProtocol.Frame(control), &envelope); err != nil {
		panic(err)
	}
	return envelope.Method
}

func startSupervisedChildCommand(ctx context.Context, cmd *exec.Cmd) (*supervisor.CommandChild, error) {
	return supervisor.StartCommand(ctx, supervisor.Command{
		Path:   cmd.Path,
		Args:   append([]string(nil), cmd.Args[1:]...),
		Env:    append([]string(nil), cmd.Env...),
		Dir:    cmd.Dir,
		Stderr: cmd.Stderr,
	})
}

func runTestLauncherStdioSupervisor(cfg launcherSupervisorConfig) int {
	if err := runLauncherStdioSupervisorErr(cfg); err != nil {
		if cfg.Stderr != nil {
			fmt.Fprintln(cfg.Stderr, err)
		}
		return 1
	}
	return 0
}

type verifiedTestAdmission struct{}

func (verifiedTestAdmission) Verified() bool { return true }
func (verifiedTestAdmission) Close() error   { return nil }
