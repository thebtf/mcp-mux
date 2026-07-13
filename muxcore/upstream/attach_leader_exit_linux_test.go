//go:build linux

package upstream

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWatchAttachedLeaderFallsBackWhenPidfdBlocked(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	originalPidfdOpen := pidfdOpen
	pidfdOpen = func(int, int) (int, error) { return -1, unix.EPERM }
	t.Cleanup(func() { pidfdOpen = originalPidfdOpen })

	done, err := watchAttachedLeader(nil, cmd.Process.Pid)
	if err != nil {
		t.Fatalf("watchAttachedLeader fallback: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_, _ = cmd.Process.Wait()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fallback watcher: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fallback watcher did not observe child exit")
	}
}

func TestReadProcIdentityCurrentProcess(t *testing.T) {
	identity, err := readProcIdentity(unix.Getpid())
	if err != nil {
		t.Fatalf("readProcIdentity: %v", err)
	}
	if identity.startTime == 0 {
		t.Fatal("start_time is zero")
	}
	if identity.state == 0 {
		t.Fatal("state is empty")
	}
}

func TestParseProcIdentityHandlesParenthesesInCommand(t *testing.T) {
	identity, err := parseProcIdentity([]byte(
		"123 (worker ) with spaces) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 4242 0",
	))
	if err != nil {
		t.Fatalf("parseProcIdentity: %v", err)
	}
	if identity.state != 'S' || identity.startTime != 4242 {
		t.Fatalf("identity = %+v, want state S start_time 4242", identity)
	}
}

func duplicateAttachHandles(stdinFD, stdoutFD, stderrFD, authorityFD uintptr) (uintptr, uintptr, uintptr, uintptr, error) {
	in, err := unix.Dup(int(stdinFD))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	out, err := unix.Dup(int(stdoutFD))
	if err != nil {
		_ = unix.Close(in)
		return 0, 0, 0, 0, err
	}
	errOut, err := unix.Dup(int(stderrFD))
	if err != nil {
		_ = unix.Close(in)
		_ = unix.Close(out)
		return 0, 0, 0, 0, err
	}
	return uintptr(in), uintptr(out), uintptr(errOut), authorityFD, nil
}

func processExitWaiter(pid int) (func(time.Duration) bool, func(), error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, nil, err
	}
	wait := func(timeout time.Duration) bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, int(timeout.Milliseconds()))
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err == nil && n == 1
		}
	}
	return wait, func() { _ = unix.Close(fd) }, nil
}
