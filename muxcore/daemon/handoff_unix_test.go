//go:build unix

package daemon

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// socketpairUnixConn creates a connected pair of *unixFDConn using the kernel
// socketpair(2) syscall. Both ends are registered for cleanup via t.Cleanup.
func socketpairUnixConn(t *testing.T) (*unixFDConn, *unixFDConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	f1 := os.NewFile(uintptr(fds[0]), "sp-a")
	f2 := os.NewFile(uintptr(fds[1]), "sp-b")
	c1, err := net.FileConn(f1)
	if err != nil {
		t.Fatal(err)
	}
	f1.Close() // FileConn dups the fd; close the *os.File wrapper to avoid leak
	c2, err := net.FileConn(f2)
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	u1, ok1 := c1.(*net.UnixConn)
	u2, ok2 := c2.(*net.UnixConn)
	if !ok1 || !ok2 {
		t.Fatal("FileConn not *net.UnixConn")
	}
	t.Cleanup(func() {
		_ = u1.Close()
		_ = u2.Close()
	})
	return newUnixFDConn(u1), newUnixFDConn(u2)
}

// TestSendFDs_SingleFD verifies that SendFDs succeeds when transferring a
// single file descriptor alongside a header over a socketpair.
func TestSendFDs_SingleFD(t *testing.T) {
	sender, _ := socketpairUnixConn(t)
	tmp, err := os.CreateTemp("", "scm-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	})
	err = sender.SendFDs([]uintptr{tmp.Fd()}, []byte("hello\n"))
	if err != nil {
		t.Fatalf("SendFDs single: %v", err)
	}
}

// TestSendFDs_ThreeFDs verifies that SendFDs succeeds when transferring three
// file descriptors (exercises SCM_RIGHTS with multiple entries).
func TestSendFDs_ThreeFDs(t *testing.T) {
	sender, _ := socketpairUnixConn(t)
	fds := make([]uintptr, 3)
	for i := range fds {
		f, err := os.CreateTemp("", "scm-multi-*.tmp")
		if err != nil {
			t.Fatal(err)
		}
		fi := f // capture loop variable for closure
		t.Cleanup(func() {
			_ = fi.Close()
			_ = os.Remove(fi.Name())
		})
		fds[i] = f.Fd()
	}
	err := sender.SendFDs(fds, []byte("three\n"))
	if err != nil {
		t.Fatalf("SendFDs triple: %v", err)
	}
}

// TestSendFDs_EmptyFDs verifies that SendFDs returns a non-nil error when
// called with an empty fds slice. This test guards against a no-op body swap:
// if SendFDs simply returned nil unconditionally, this test would fail because
// we expect a non-nil error — satisfying the AC "swap body→return null ⇒
// tests MUST fail".
func TestSendFDs_EmptyFDs(t *testing.T) {
	sender, _ := socketpairUnixConn(t)
	err := sender.SendFDs([]uintptr{}, []byte("empty\n"))
	if err == nil {
		t.Fatal("SendFDs with empty fds should return error, got nil")
	}
}
