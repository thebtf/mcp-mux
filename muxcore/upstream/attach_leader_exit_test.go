//go:build windows || linux

package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const attachedLeaderHelperEnv = "MCPMUX_TEST_ATTACHED_LEADER_HELPER"

func TestAttachedLeaderExitFinalizesDescendant(t *testing.T) {
	switch os.Getenv(attachedLeaderHelperEnv) {
	case "descendant":
		time.Sleep(30 * time.Second)
		return
	case "leader":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(exe, "-test.run=^TestAttachedLeaderExitFinalizesDescendant$")
		child.Env = append(os.Environ(), attachedLeaderHelperEnv+"=descendant")
		child.Stdout = os.Stdout
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(child.Process.Pid); err != nil {
			t.Fatal(err)
		}
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_, _ = os.Stderr.WriteString("stderr-after-handoff\n")
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	proc, err := Start(exe, []string{"-test.run=^TestAttachedLeaderExitFinalizesDescendant$"}, map[string]string{
		attachedLeaderHelperEnv: "leader",
	}, "", nil)
	if err != nil {
		t.Fatalf("Start leader: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })

	line, err := proc.ReadLine()
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	descendantPID, err := strconv.Atoi(string(line))
	if err != nil {
		t.Fatalf("parse descendant pid %q: %v", line, err)
	}
	waitDescendant, closeWaiter, err := processExitWaiter(descendantPID)
	if err != nil {
		t.Fatalf("watch descendant: %v", err)
	}
	defer closeWaiter()

	pid, stdinFD, stdoutFD, stderrFD, authorityFD, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}
	stdinFD, stdoutFD, stderrFD, authorityFD, err = duplicateAttachHandles(stdinFD, stdoutFD, stderrFD, authorityFD)
	if err != nil {
		t.Fatalf("duplicate handoff handles: %v", err)
	}
	if err := proc.CommitDetach(); err != nil {
		t.Fatalf("CommitDetach: %v", err)
	}
	var adoptedStderr bytes.Buffer
	adopted, err := AttachFromFDsWithAuthority(
		pid, stdinFD, stdoutFD, stderrFD, authorityFD, exe, log.New(&adoptedStderr, "", 0),
	)
	if err != nil {
		t.Fatalf("AttachFromFDsWithAuthority: %v", err)
	}
	t.Cleanup(func() { _ = adopted.Close() })
	if err := adopted.WriteLine([]byte("exit")); err != nil {
		t.Fatalf("release leader: %v", err)
	}

	select {
	case <-adopted.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("attached upstream Done did not close after leader exit")
	}
	if !strings.Contains(adoptedStderr.String(), "stderr-after-handoff") {
		t.Fatalf("successor did not drain transferred stderr: %q", adoptedStderr.String())
	}
	if !waitDescendant(5 * time.Second) {
		t.Fatalf("descendant pid %d survived attached leader exit", descendantPID)
	}
}

func TestAttachedSoftCloseTimeoutTerminatesTransferredTree(t *testing.T) {
	testAttachedTimeoutTermination(t, true)
}

func TestAttachedCloseTimeoutTerminatesTransferredTree(t *testing.T) {
	testAttachedTimeoutTermination(t, false)
}

func testAttachedTimeoutTermination(t *testing.T, soft bool) {
	t.Helper()
	var command string
	var args []string
	if runtime.GOOS == "windows" {
		command = "ping"
		args = []string{"-n", "30", "127.0.0.1"}
	} else {
		command = "sleep"
		args = []string{"30"}
	}

	proc, err := Start(command, args, nil, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	waitLeader, closeWaiter, err := processExitWaiter(proc.PID())
	if err != nil {
		t.Fatalf("watch leader: %v", err)
	}
	defer closeWaiter()

	pid, stdinFD, stdoutFD, stderrFD, authorityFD, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}
	stdinFD, stdoutFD, stderrFD, authorityFD, err = duplicateAttachHandles(stdinFD, stdoutFD, stderrFD, authorityFD)
	if err != nil {
		t.Fatalf("duplicate handoff handles: %v", err)
	}
	if err := proc.CommitDetach(); err != nil {
		t.Fatalf("CommitDetach: %v", err)
	}
	adopted, err := AttachFromFDsWithAuthority(pid, stdinFD, stdoutFD, stderrFD, authorityFD, command, nil)
	if err != nil {
		t.Fatalf("AttachFromFDsWithAuthority: %v", err)
	}
	t.Cleanup(func() {
		_ = terminateProcessTree(adopted)
		_ = adopted.Close()
	})

	if soft {
		exitCode, err := adopted.SoftClose(100 * time.Millisecond)
		if err == nil {
			t.Fatal("SoftClose error = nil, want forced termination after timeout")
		}
		if exitCode == 0 {
			t.Fatalf("SoftClose exitCode = %d, want non-zero forced termination", exitCode)
		}
	} else {
		adopted.SetDrainTimeout(100 * time.Millisecond)
		if err := adopted.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	select {
	case <-adopted.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("attached process Done did not close after forced termination")
	}
	if !waitLeader(5 * time.Second) {
		t.Fatalf("attached leader pid %d survived SoftClose timeout escalation", pid)
	}
}
