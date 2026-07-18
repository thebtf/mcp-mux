//go:build darwin

package procgroup

import (
	"syscall"
	"testing"
)

func TestProcessGroupProbeTreatsZombieOnlyEPERMAsPending(t *testing.T) {
	if !processGroupProbePending(syscall.EPERM) {
		t.Fatal("Darwin EPERM probe must remain pending until ESRCH or timeout")
	}
	if processGroupProbePending(syscall.ESRCH) || processGroupProbePending(nil) {
		t.Fatal("only Darwin EPERM is a pending non-terminal probe")
	}
}
