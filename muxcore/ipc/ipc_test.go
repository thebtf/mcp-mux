package ipc

import (
	"io"
	"net"
	"os"
	"strings"
	"testing"
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

	// Send data client → server
	_, err = client.Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("client.Write() error: %v", err)
	}

	buf := make([]byte, 100)
	n, err := server.Read(buf)
	if err != nil {
		t.Fatalf("server.Read() error: %v", err)
	}
	if string(buf[:n]) != "hello\n" {
		t.Errorf("server received %q, want 'hello\\n'", string(buf[:n]))
	}

	// Send data server → client
	_, err = server.Write([]byte("world\n"))
	if err != nil {
		t.Fatalf("server.Write() error: %v", err)
	}

	n, err = client.Read(buf)
	if err != nil {
		t.Fatalf("client.Read() error: %v", err)
	}
	if string(buf[:n]) != "world\n" {
		t.Errorf("client received %q, want 'world\\n'", string(buf[:n]))
	}
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

