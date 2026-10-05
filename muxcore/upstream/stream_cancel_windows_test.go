//go:build windows

package upstream

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestStreamReadCancelReaderLifetime(t *testing.T) {
	cancel, clear, cleanup, err := prepareStreamReadCancel(nil)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_ = cleanup()
		}
	}()
	if err := cancel(); err != nil {
		t.Fatalf("cancel idle live reader: %v", err)
	}
	cleaned = true
	if err := cleanup(); err != nil {
		t.Fatalf("retire reader cancellation authority: %v", err)
	}

	// Hold the exact cleanup-before-done window open. A stale reading snapshot
	// must not use the closed thread handle, but cleanup alone is not a join.
	if err := cancel(); err != nil {
		t.Fatalf("cancel completed reader: %v", err)
	}
	control := newStreamReadControl()
	control.bind(cancel, clear, nil)
	control.reading = true
	if err := control.quiesce(10 * time.Millisecond); err == nil || err.Error() != "stream reader did not quiesce" {
		t.Fatalf("quiesce without reader join = %v, want missing completion proof", err)
	}
	close(control.done)
	if err := control.quiesce(time.Second); err != nil {
		t.Fatalf("quiesce joined reader with stale reading snapshot: %v", err)
	}
}

func TestStreamReadQuiesceRejectsUnownedInvalidHandle(t *testing.T) {
	control := newStreamReadControl()
	control.bind(func() error {
		close(control.done)
		return windows.ERROR_INVALID_HANDLE
	}, nil, nil)
	control.reading = true
	if err := control.quiesce(time.Second); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("unproven cancellation handle error = %v, want ERROR_INVALID_HANDLE even after join", err)
	}
}
