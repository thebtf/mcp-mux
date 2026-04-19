//go:build darwin

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHandoffDarwin_Successor runs the successor side of the handoff.
// Invoked by TestHandoffDarwin_LaunchdStyleSpawn via exec.Command with a
// clean env and magic env var DAEMON_HANDOFF_SUCCESSOR_SOCKET set.
//
// NOTE: this test only runs when the env var is present. In a normal
// `go test ./daemon/...` invocation it short-circuits.
func TestHandoffDarwin_Successor(t *testing.T) {
	socketPath := os.Getenv("DAEMON_HANDOFF_SUCCESSOR_SOCKET")
	if socketPath == "" {
		t.Skip("not invoked as successor child")
	}
	token := os.Getenv("DAEMON_HANDOFF_SUCCESSOR_TOKEN")

	// Hold briefly so the parent has a window to snapshot our PGID before
	// we exit. Without this, the json.Decoder read path completes handoff
	// so fast that the successor process disappears before the parent's
	// syscall.Getpgid fires (observed on macos-latest CI under Go 1.25.9).
	time.Sleep(300 * time.Millisecond)

	// Dial the socket opened by the parent (old daemon side).
	conn, err := dialHandoffUnix(socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Run receiveHandoff. We don't verify contents here — the parent side
	// verifies transferred/aborted counts. We only signal success/failure
	// via test exit status (go test binary exit 0 on PASS).
	received, err := receiveHandoff(context.Background(), conn, token)
	if err != nil {
		t.Fatalf("receiveHandoff: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 received upstream, got %d", len(received))
	}
}

// TestHandoffDarwin_LaunchdStyleSpawn spawns a child test process via
// exec.Command with a clean environment — simulating launchd delivering
// the successor daemon. Asserts the SCM_RIGHTS handoff completes across
// that cross-parentage boundary, and that the successor is NOT in the
// parent's process group.
func TestHandoffDarwin_LaunchdStyleSpawn(t *testing.T) {
	// Skip if running as a child (prevents infinite recursion).
	if os.Getenv("DAEMON_HANDOFF_SUCCESSOR_SOCKET") != "" {
		t.Skip("this invocation is the successor child")
	}

	tmp := t.TempDir()
	// macOS has a 104-byte unix socket path limit. t.TempDir() on darwin
	// returns a ~90-byte path already, leaving no room for the filename.
	// Use os.CreateTemp("", ...) which places the socket in /tmp/ (short).
	sockFile, err := os.CreateTemp("", "handoff-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	socketPath := sockFile.Name()
	_ = sockFile.Close()
	_ = os.Remove(socketPath) // listenHandoffUnix creates the socket
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	token := "darwin-launchd-test-token"

	// Open a file whose FD we'll transfer.
	tmpFile, err := os.CreateTemp(tmp, "fd-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer tmpFile.Close()

	upstreams := []HandoffUpstream{
		{
			ServerID: "s1",
			Command:  "test",
			PID:      os.Getpid(),
			StdinFD:  tmpFile.Fd(),
			StdoutFD: tmpFile.Fd(),
		},
	}

	// Find our own test binary.
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// Spawn successor: run this same binary with -test.run targeting the
	// successor test, and with a CLEAN env containing only our magic vars
	// plus essentials (PATH, HOME).
	cleanEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"DAEMON_HANDOFF_SUCCESSOR_SOCKET=" + socketPath,
		"DAEMON_HANDOFF_SUCCESSOR_TOKEN=" + token,
	}

	cmd := exec.Command(testBinary, "-test.run=^TestHandoffDarwin_Successor$", "-test.v")
	cmd.Env = cleanEnv
	// Detach: new session + new process group. This matches launchd's
	// no-tty spawn pattern and also gives us the PGID check below.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Capture stdout+stderr for diagnostic printing on failure.
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	// Start the listener goroutine BEFORE spawning the child so the socket
	// is bound before the child attempts to dial.
	type listenResult struct {
		conn fdConn
		err  error
	}
	listenCh := make(chan listenResult, 1)
	go func() {
		conn, err := listenHandoffUnix(socketPath, 10*time.Second)
		listenCh <- listenResult{conn, err}
	}()

	// Give the listener a moment to bind before spawning the child.
	time.Sleep(50 * time.Millisecond)

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}

	// Verify successor is NOT in our process group.
	parentPGID, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("parent getpgid: %v", err)
	}
	// Give the kernel a moment to finalize the new process group.
	time.Sleep(50 * time.Millisecond)
	successorPGID, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("successor getpgid: %v", err)
	}
	if successorPGID == parentPGID {
		_ = cmd.Process.Kill()
		t.Fatalf("successor PGID=%d matches parent PGID=%d — Setsid did not take effect",
			successorPGID, parentPGID)
	}

	// Wait for the accepted connection from the successor child.
	lr := <-listenCh
	if lr.err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("listenHandoffUnix: %v\n\nchild output:\n%s", lr.err, outBuf.String())
	}
	defer lr.conn.Close()

	// Now run the old-daemon side: perform the handoff over the accepted conn.
	result, err := performHandoff(context.Background(), lr.conn, token, upstreams)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("performHandoff: %v\n\nchild output:\n%s", err, outBuf.String())
	}
	if len(result.Transferred) != 1 {
		_ = cmd.Process.Kill()
		t.Errorf("Transferred: %v, want [s1]\n\nchild output:\n%s",
			result.Transferred, outBuf.String())
	}

	// Wait for successor to exit cleanly.
	waitErr := cmd.Wait()
	if waitErr != nil {
		t.Errorf("successor exited with error: %v\n\nchild output:\n%s",
			waitErr, outBuf.String())
	}
}
