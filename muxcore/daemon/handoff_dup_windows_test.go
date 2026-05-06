//go:build windows

package daemon

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func dupFDForHandoff(t *testing.T, f *os.File) uintptr {
	t.Helper()
	var dup windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		windows.Handle(f.Fd()),
		windows.CurrentProcess(),
		&dup,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatalf("duplicate handoff handle: %v", err)
	}
	return uintptr(dup)
}
