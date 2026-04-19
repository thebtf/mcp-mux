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

// TestRecvFDs_PairedWithSender completes the SCM_RIGHTS roundtrip —
// sender from T009, receiver from T010, verifies the FD arrives valid.
// AC4 guard: if RecvFDs returned nil, nil, nil unconditionally, len(fds)!=1
// would fire and t.Fatalf would catch the broken implementation.
func TestRecvFDs_PairedWithSender(t *testing.T) {
	sender, receiver := socketpairUnixConn(t)

	// Open a temp file; sender transfers its FD to receiver.
	tmp, err := os.CreateTemp("", "scm-recv-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	tmpName := tmp.Name()
	t.Cleanup(func() { _ = os.Remove(tmpName); _ = tmp.Close() })

	content := []byte("hello from sender")
	if _, err := tmp.Write(content); err != nil {
		t.Fatal(err)
	}

	// Capture fd before starting the goroutine to avoid a data race with
	// the cleanup's tmp.Close() call, which may run concurrently.
	tmpFD := tmp.Fd()

	done := make(chan error, 1)
	go func() {
		done <- sender.SendFDs([]uintptr{tmpFD}, []byte("header-bytes\n"))
	}()

	fds, header, err := receiver.RecvFDs()
	if err != nil {
		t.Fatalf("RecvFDs: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("SendFDs: %v", err)
	}

	if len(fds) != 1 {
		t.Fatalf("expected 1 fd, got %d", len(fds))
	}
	if string(header) != "header-bytes\n" {
		t.Errorf("header mismatch: %q", header)
	}

	// Verify the received FD is a valid, readable duplicate of the temp file.
	received := os.NewFile(fds[0], "received")
	defer received.Close()
	if _, err := received.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	got := make([]byte, len(content))
	if _, err := received.Read(got); err != nil {
		t.Fatalf("read via received fd: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("received fd content %q != sent content %q", got, content)
	}
}

// TestRecvFDs_TripleRoundtrip sends + receives 3 FDs in a single SCM_RIGHTS
// message and verifies the count. AC4 guard: if RecvFDs returned nil FDs,
// len(recv)!=3 fires immediately.
func TestRecvFDs_TripleRoundtrip(t *testing.T) {
	sender, receiver := socketpairUnixConn(t)
	files := make([]*os.File, 3)
	fds := make([]uintptr, 3)
	for i := range files {
		f, err := os.CreateTemp("", "scm-triple-*.tmp")
		if err != nil {
			t.Fatal(err)
		}
		// Capture fd and name before registering cleanup: t.Cleanup may run
		// concurrently with the sender goroutine, and f.Close() races with
		// any later f.Fd() call.
		fds[i] = f.Fd()
		fName := f.Name()
		t.Cleanup(func() { _ = os.Remove(fName); _ = f.Close() })
		files[i] = f
	}

	done := make(chan error, 1)
	go func() { done <- sender.SendFDs(fds, []byte("triple\n")) }()

	recv, header, err := receiver.RecvFDs()
	if err != nil {
		t.Fatalf("RecvFDs: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("SendFDs: %v", err)
	}

	if len(recv) != 3 {
		t.Fatalf("expected 3 fds, got %d", len(recv))
	}
	if string(header) != "triple\n" {
		t.Errorf("header: %q", header)
	}

	// Clean up the duplicated FDs on the receiver side.
	for _, f := range recv {
		_ = syscall.Close(int(f))
	}
}
