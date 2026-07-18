//go:build linux

package supervisor

import (
	"testing"

	"golang.org/x/sys/unix"
)

func prepareCommandTreeTest(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatalf("enable child subreaper: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0); err != nil {
			t.Errorf("disable child subreaper: %v", err)
		}
	})
}
