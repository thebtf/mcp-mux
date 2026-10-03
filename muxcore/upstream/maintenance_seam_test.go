package upstream

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type maintenanceBlockedStdin struct {
	entered, closed, done chan struct{}
	writeOnce, closeOnce  sync.Once
}

func (w *maintenanceBlockedStdin) Write([]byte) (int, error) {
	w.writeOnce.Do(func() { close(w.entered) })
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *maintenanceBlockedStdin) Close() error {
	w.closeOnce.Do(func() { close(w.closed); close(w.done) })
	return nil
}

func TestMaintenanceSoftCloseInterruptsBlockedStdin(t *testing.T) {
	stdin := &maintenanceBlockedStdin{entered: make(chan struct{}), closed: make(chan struct{}), done: make(chan struct{})}
	proc := &Process{stdin: stdin, Done: stdin.done}
	// Cleanup releases the real write barrier even when testing the old mutex
	// ordering, so a red result does not strand a goroutine.
	t.Cleanup(func() { _ = stdin.Close() })
	written := make(chan error, 1)
	go func() { written <- proc.WriteLine([]byte(`{"jsonrpc":"2.0","id":"blocked","method":"tools/call"}`)) }()
	select {
	case <-stdin.entered:
	case <-time.After(time.Second):
		t.Fatal("stdin write did not reach the blocking barrier")
	}
	retired := make(chan error, 1)
	go func() { _, err := proc.SoftClose(0); retired <- err }()
	select {
	case err := <-retired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("force retirement waited for stdin I/O instead of interrupting it")
	}
	select {
	case err := <-written:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("blocked write disposition: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retirement did not interrupt the blocked writer")
	}
	if !proc.TreesDead() {
		t.Fatal("interruptible retirement did not preserve completion proof")
	}
	if err := proc.WriteLine([]byte("late")); err == nil {
		t.Fatal("a new write was admitted after retirement")
	}
}
