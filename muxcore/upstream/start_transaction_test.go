package upstream

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	startFailureRoleEnv    = "MCPMUX_START_FAILURE_ROLE"
	startFailurePIDFileEnv = "MCPMUX_START_FAILURE_PID_FILE"
)

func TestStartPostAuthorityFailureFinalizesProcessTree(t *testing.T) {
	switch os.Getenv(startFailureRoleEnv) {
	case "descendant":
		select {}
	case "leader":
		cmd := exec.Command(os.Args[0], "-test.run=^TestStartPostAuthorityFailureFinalizesProcessTree$")
		cmd.Env = append(os.Environ(), startFailureRoleEnv+"=descendant")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start descendant: %v", err)
		}
		pidFile := os.Getenv(startFailurePIDFileEnv)
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			t.Fatalf("write descendant pid: %v", err)
		}
		select {}
	}

	pidFile := t.TempDir() + string(os.PathSeparator) + "descendant.pid"
	injected := errors.New("injected post-authority failure")
	var leaderPID int
	var descendantPID int

	previousHook := startPostAuthorityHook
	startPostAuthorityHook = func(p *Process) error {
		leaderPID = p.PID()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(pidFile)
			if err == nil {
				text := strings.TrimSpace(string(data))
				if text == "" {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				pid, parseErr := strconv.Atoi(text)
				if parseErr != nil {
					return fmt.Errorf("parse descendant pid: %w", parseErr)
				}
				descendantPID = pid
				return injected
			}
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read descendant pid: %w", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return errors.New("descendant pid was not published")
	}
	t.Cleanup(func() { startPostAuthorityHook = previousHook })

	env := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	env[startFailureRoleEnv] = "leader"
	env[startFailurePIDFileEnv] = pidFile

	proc, err := Start(os.Args[0], []string{"-test.run=^TestStartPostAuthorityFailureFinalizesProcessTree$"}, env, "", nil)
	if proc != nil {
		t.Fatalf("Start() process = %#v, want nil on post-authority failure", proc)
	}
	if !errors.Is(err, injected) {
		t.Fatalf("Start() error = %v, want injected failure", err)
	}
	if leaderPID <= 0 || descendantPID <= 0 {
		t.Fatalf("captured pids = leader %d descendant %d, want both positive", leaderPID, descendantPID)
	}
	if upstreamTestProcessAlive(leaderPID) {
		killUpstreamTestProcess(leaderPID)
		t.Fatalf("leader pid %d still alive after Start returned", leaderPID)
	}
	if upstreamTestProcessAlive(descendantPID) {
		killUpstreamTestProcess(descendantPID)
		t.Fatalf("descendant pid %d still alive after Start returned", descendantPID)
	}
}

func TestRetirementProvenRequiresDoneAndTreeAuthority(t *testing.T) {
	done := make(chan struct{})
	p := &Process{pid: 42, Done: done, treeFinalized: true}
	if p.RetirementProven() {
		t.Fatal("tree finalizer success alone must not prove retirement before Done")
	}
	close(done)
	if !p.RetirementProven() {
		t.Fatal("Done plus tree authority finalization should prove retirement")
	}

	doneWithoutAuthority := make(chan struct{})
	close(doneWithoutAuthority)
	p = &Process{pid: 42, Done: doneWithoutAuthority}
	if p.RetirementProven() {
		t.Fatal("leader Done alone must not prove process-tree retirement")
	}

	p = &Process{pid: 42, Done: make(chan struct{}), detach: detachCommitted}
	if !p.RetirementProven() {
		t.Fatal("committed authority transfer should be terminal for predecessor owner")
	}
}

func TestStartPostAuthorityFailureRetainsUnprovenProcessAuthority(t *testing.T) {
	pidFile := t.TempDir() + string(os.PathSeparator) + "descendant.pid"
	setupErr := errors.New("injected post-authority failure")
	retirementErr := errors.New("injected unproven retirement")
	var captured *Process
	var leaderPID int
	var descendantPID int

	previousStartHook := startPostAuthorityHook
	previousFinalizer := finalizeFailedStartTree
	startPostAuthorityHook = func(p *Process) error {
		captured = p
		leaderPID = p.PID()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(pidFile)
			if err == nil {
				text := strings.TrimSpace(string(data))
				if text == "" {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				pid, parseErr := strconv.Atoi(text)
				if parseErr != nil {
					return fmt.Errorf("parse descendant pid: %w", parseErr)
				}
				descendantPID = pid
				return setupErr
			}
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read descendant pid: %w", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return errors.New("descendant pid was not published")
	}
	finalizeFailedStartTree = func(*Process) error { return retirementErr }
	t.Cleanup(func() {
		startPostAuthorityHook = previousStartHook
		finalizeFailedStartTree = previousFinalizer
		if captured != nil && !captured.RetirementProven() {
			_ = captured.finalizeOwnedTree()
		}
		if leaderPID > 0 && upstreamTestProcessAlive(leaderPID) {
			killUpstreamTestProcess(leaderPID)
		}
		if descendantPID > 0 && upstreamTestProcessAlive(descendantPID) {
			killUpstreamTestProcess(descendantPID)
		}
	})

	env := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	env[startFailureRoleEnv] = "leader"
	env[startFailurePIDFileEnv] = pidFile

	proc, err := Start(os.Args[0], []string{"-test.run=^TestStartPostAuthorityFailureFinalizesProcessTree$"}, env, "", nil)
	if proc == nil {
		t.Fatal("Start() process = nil, want retained unproven authority")
	}
	if proc != captured {
		t.Fatal("Start() did not return the installed Process authority")
	}
	if !errors.Is(err, setupErr) || !errors.Is(err, retirementErr) {
		t.Fatalf("Start() error = %v, want setup and retirement failures", err)
	}
	select {
	case <-proc.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process.Done did not report failed-start leader completion")
	}
	if proc.RetirementProven() {
		t.Fatal("leader completion without tree finalization proved retirement")
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("Close() retry after failed start: %v", err)
	}
	if !proc.RetirementProven() {
		t.Fatal("Close() retry did not prove failed-start retirement")
	}
	if upstreamTestProcessAlive(descendantPID) {
		t.Fatalf("descendant pid %d still alive after retry", descendantPID)
	}
}
