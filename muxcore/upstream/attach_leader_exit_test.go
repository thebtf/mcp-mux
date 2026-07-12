//go:build windows || linux

package upstream

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
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

	pid, stdinFD, stdoutFD, authorityFD, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}
	stdinFD, stdoutFD, authorityFD, err = duplicateAttachHandles(stdinFD, stdoutFD, authorityFD)
	if err != nil {
		t.Fatalf("duplicate handoff handles: %v", err)
	}
	if err := proc.CommitDetach(); err != nil {
		t.Fatalf("CommitDetach: %v", err)
	}
	adopted, err := AttachFromFDsWithAuthority(pid, stdinFD, stdoutFD, authorityFD, exe)
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
	if !waitDescendant(5 * time.Second) {
		t.Fatalf("descendant pid %d survived attached leader exit", descendantPID)
	}
}
