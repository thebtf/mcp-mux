package upstream

import (
	"os"
	"strconv"
	"testing"
	"time"
)

func TestMaintenanceTreeDeathExcludesCommittedTransfer(t *testing.T) {
	exe := os.Args[0]
	proc, err := Start(exe, []string{"-test.run=^TestAttachedTimeoutTreeHelper$"}, map[string]string{attachedLeaderHelperEnv: "timeout-leader"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	line, err := proc.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	descendant, err := strconv.Atoi(string(line))
	if err != nil {
		t.Fatal(err)
	}
	waitChild, closeWaiter, err := processExitWaiter(descendant)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWaiter()
	if proc.TreesDead() {
		t.Fatal("live process tree reported dead")
	}
	pid, in, out, stderr, authority, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatal(err)
	}
	in, out, stderr, authority, err = duplicateAttachHandles(in, out, stderr, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.CommitDetach(); err != nil {
		t.Fatal(err)
	}
	if !proc.RetirementProven() || proc.TreesDead() {
		t.Fatal("transferred tree confused with dead tree")
	}
	adopted, err := AttachFromFDsWithAuthority(pid, in, out, stderr, authority, exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminateProcessTree(adopted); _ = adopted.Close() })
	_, _ = adopted.SoftClose(0)
	if !adopted.TreesDead() {
		t.Fatal("whole-tree termination did not establish death")
	}
	if !waitChild(5 * time.Second) {
		t.Fatal("descendant survived maintenance retirement")
	}
	if proc.TreesDead() {
		t.Fatal("predecessor lost the distinction between transfer and death")
	}
}
