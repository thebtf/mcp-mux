package ipc

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func socketPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "mux-ipc-*.sock")
	if err != nil {
		t.Fatalf("create temp socket: %v", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	return path
}

func TestListenAndDial(t *testing.T) {
	path := socketPath(t)

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer ln.Close()

	// Accept in background
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	// Dial
	client, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer client.Close()

	server := <-accepted
	defer server.Close()

	// Named-pipe writes on Windows can wait until the peer has posted a read,
	// so arrange the read side before writing in each direction.
	serverRead := make(chan readResult, 1)
	go readOnce(server, serverRead)
	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatalf("client.Write() error: %v", err)
	}
	got := <-serverRead
	if got.err != nil {
		t.Fatalf("server.Read() error: %v", got.err)
	}
	if got.data != "hello\n" {
		t.Errorf("server received %q, want 'hello\\n'", got.data)
	}

	clientRead := make(chan readResult, 1)
	go readOnce(client, clientRead)
	if _, err := server.Write([]byte("world\n")); err != nil {
		t.Fatalf("server.Write() error: %v", err)
	}
	got = <-clientRead
	if got.err != nil {
		t.Fatalf("client.Read() error: %v", got.err)
	}
	if got.data != "world\n" {
		t.Errorf("client received %q, want 'world\\n'", got.data)
	}
}

type readResult struct {
	data string
	err  error
}

func readOnce(conn net.Conn, out chan<- readResult) {
	buf := make([]byte, 100)
	n, err := conn.Read(buf)
	if err != nil {
		out <- readResult{err: err}
		return
	}
	out <- readResult{data: string(buf[:n])}
}

func TestIsAvailable(t *testing.T) {
	path := socketPath(t)

	// Not available before listen
	if IsAvailable(path) {
		t.Error("IsAvailable() = true before listen")
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}

	// Accept connections in background (needed for IsAvailable to succeed)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// Available while listening
	if !IsAvailable(path) {
		t.Error("IsAvailable() = false while listening")
	}

	ln.Close()
}

func TestCleanup(t *testing.T) {
	path := socketPath(t)

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	ln.Close()

	Cleanup(path)

	// Socket file should be gone
	if IsAvailable(path) {
		t.Error("IsAvailable() = true after cleanup")
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	path := socketPath(t)

	// Create a stale file
	f, err := createFile(path)
	if err != nil {
		t.Fatalf("create stale file: %v", err)
	}
	f.Close()

	// Listen should succeed despite stale file
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() with stale file error: %v", err)
	}
	defer ln.Close()
}

func TestDialNonExistent(t *testing.T) {
	path := "/tmp/mux-test-nonexistent-" + t.Name() + ".sock"

	_, err := Dial(path)
	if err == nil {
		t.Error("Dial() to non-existent path expected error")
	}
}

func TestMultipleClients(t *testing.T) {
	path := socketPath(t)

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer ln.Close()

	// Accept connections
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Echo back
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}(conn)
		}
	}()

	// Connect two clients
	c1, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial 1 error: %v", err)
	}
	defer c1.Close()

	c2, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial 2 error: %v", err)
	}
	defer c2.Close()

	// Send from client 1
	c1.Write([]byte("from-c1"))
	buf := make([]byte, 100)
	n, err := c1.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("c1 read error: %v", err)
	}
	if string(buf[:n]) != "from-c1" {
		t.Errorf("c1 got %q, want 'from-c1'", string(buf[:n]))
	}

	// Send from client 2
	c2.Write([]byte("from-c2"))
	n, err = c2.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("c2 read error: %v", err)
	}
	if string(buf[:n]) != "from-c2" {
		t.Errorf("c2 got %q, want 'from-c2'", string(buf[:n]))
	}
}

// createFile is a helper to create a regular file at path.
func createFile(path string) (*os.File, error) {
	return os.Create(path)
}

func TestListen_RefusesWhenAlreadyActive(t *testing.T) {
	path := socketPath(t)

	// First Listen — must succeed.
	ln1, err := Listen(path)
	if err != nil {
		t.Fatalf("first Listen() error: %v", err)
	}
	defer ln1.Close()

	// Accept connections so IsAvailable's Dial can complete the handshake.
	go func() {
		for {
			conn, err := ln1.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// Second Listen on the same active path — must fail.
	ln2, err := Listen(path)
	if err == nil {
		ln2.Close()
		t.Fatal("second Listen() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listener already active") {
		t.Errorf("expected 'listener already active' in error, got: %v", err)
	}

	// First listener must still work after the refused second attempt.
	conn, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() after refused second Listen() error: %v", err)
	}
	conn.Close()
}

func TestListen_SucceedsOnStalePath(t *testing.T) {
	path := socketPath(t)

	// Write a stale file at path (nothing is listening on it).
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatalf("WriteFile stale: %v", err)
	}

	// Listen must succeed: IsAvailable returns false (nothing to connect to),
	// then Remove strips the stale file, then the real listener binds.
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() on stale path error: %v", err)
	}
	ln.Close()
}

func TestListen_RetriesTransientStaleRemoveFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows IPC uses named pipes without stale socket files")
	}
	path := socketPath(t)

	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatalf("WriteFile stale: %v", err)
	}

	origRemove := removeSocketFile
	origRename := renameSocketFile
	origSleep := sleepBeforeRemoveRetry
	origAttempts := staleSocketRemoveAttempts
	origDelay := staleSocketRemoveDelay
	t.Cleanup(func() {
		removeSocketFile = origRemove
		renameSocketFile = origRename
		sleepBeforeRemoveRetry = origSleep
		staleSocketRemoveAttempts = origAttempts
		staleSocketRemoveDelay = origDelay
	})

	removeErr := errors.New("Access is denied.")
	calls := 0
	removeSocketFile = func(p string) error {
		calls++
		if calls < 3 {
			return removeErr
		}
		return origRemove(p)
	}
	sleepBeforeRemoveRetry = func(time.Duration) {}
	staleSocketRemoveAttempts = 5
	staleSocketRemoveDelay = time.Millisecond

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() after transient stale remove failures error: %v", err)
	}
	ln.Close()

	if calls != 3 {
		t.Fatalf("remove calls = %d, want 3", calls)
	}
}

func TestListen_RetiresStaleSocketWhenRemoveDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows IPC uses named pipes without stale socket files")
	}
	path := socketPath(t)

	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatalf("WriteFile stale: %v", err)
	}

	origRemove := removeSocketFile
	origRename := renameSocketFile
	origSleep := sleepBeforeRemoveRetry
	origAttempts := staleSocketRemoveAttempts
	origDelay := staleSocketRemoveDelay
	t.Cleanup(func() {
		removeSocketFile = origRemove
		renameSocketFile = origRename
		sleepBeforeRemoveRetry = origSleep
		staleSocketRemoveAttempts = origAttempts
		staleSocketRemoveDelay = origDelay
	})

	removeErr := errors.New("Access is denied.")
	removeCalls := 0
	removeSocketFile = func(p string) error {
		removeCalls++
		if p == path {
			return removeErr
		}
		return origRemove(p)
	}
	sleepBeforeRemoveRetry = func(time.Duration) {}
	staleSocketRemoveAttempts = 3
	staleSocketRemoveDelay = time.Millisecond

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen() after retire fallback error: %v", err)
	}
	ln.Close()

	if removeCalls < 2 {
		t.Fatalf("remove calls = %d, want at least original + retired cleanup", removeCalls)
	}
}

func TestListen_BoundsPersistentStaleRemoveFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows IPC uses named pipes without stale socket files")
	}
	path := socketPath(t)

	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatalf("WriteFile stale: %v", err)
	}

	origRemove := removeSocketFile
	origRename := renameSocketFile
	origSleep := sleepBeforeRemoveRetry
	origAttempts := staleSocketRemoveAttempts
	origDelay := staleSocketRemoveDelay
	t.Cleanup(func() {
		removeSocketFile = origRemove
		renameSocketFile = origRename
		sleepBeforeRemoveRetry = origSleep
		staleSocketRemoveAttempts = origAttempts
		staleSocketRemoveDelay = origDelay
	})

	removeErr := errors.New("Access is denied.")
	calls := 0
	removeSocketFile = func(string) error {
		calls++
		return removeErr
	}
	renameSocketFile = func(string, string) error {
		return removeErr
	}
	sleepBeforeRemoveRetry = func(time.Duration) {}
	staleSocketRemoveAttempts = 3
	staleSocketRemoveDelay = time.Millisecond

	ln, err := Listen(path)
	if err == nil {
		ln.Close()
		t.Fatal("Listen() expected persistent stale remove error, got nil")
	}
	if !strings.Contains(err.Error(), "remove stale socket") {
		t.Fatalf("expected stale remove error, got: %v", err)
	}
	if calls != 3 {
		t.Fatalf("remove calls = %d, want 3", calls)
	}
}
