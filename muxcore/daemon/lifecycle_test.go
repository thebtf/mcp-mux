package daemon

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

// testdataPath returns the absolute path to a file under the project's testdata directory.
// Tests in this package execute with cwd = muxcore/daemon, so testdata is two levels up.
func testdataPath(t *testing.T, name string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("testdataPath: Getwd: %v", err)
	}
	return filepath.Join(cwd, "..", "..", "testdata", name)
}

// dialIPC retries ipc.Dial until the socket is available or the timeout elapses.
// If token is non-empty, sends it as the handshake line immediately after connecting
// (required for daemon-owned owners that have TokenHandshake enabled).
// Returns a connected ipcConn or fails the test.
func dialIPC(t *testing.T, ipcPath string, token ...string) *ipcConn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(ipcPath)
		if err == nil {
			if len(token) > 0 && token[0] != "" {
				if _, werr := fmt.Fprintf(c, "%s\n", token[0]); werr != nil {
					t.Fatalf("dialIPC: send token: %v", werr)
				}
			}
			return &ipcConn{conn: c, scanner: bufio.NewScanner(c)}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("dialIPC: timed out connecting to %s", ipcPath)
	return nil
}

// waitEOF blocks until the IPC scanner returns false (EOF or connection closed)
// or the timeout elapses.  Returns true when EOF is detected.
func waitEOF(c *ipcConn, timeout time.Duration) bool {
	ch := make(chan struct{}, 1)
	go func() {
		// Drain until Scan returns false — either EOF or a closed connection.
		for c.scanner.Scan() {
			// Consume any in-flight lines; we only care about the eventual EOF.
		}
		ch <- struct{}{}
	}()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestUpstreamCrashDisconnectsSession verifies that when the upstream process
// exits unexpectedly, the daemon calls Owner.Shutdown(), which closes all
// active sessions via Session.Close, and the connected IPC client receives EOF
// within 5 seconds.
//
// Because mock_server_crash exits ~200 ms after responding, there is a race
// between the initialize response arriving and the session being torn down.
// The test accepts both outcomes — receiving the response OR an immediate EOF —
// and then waits for the final EOF that signals the session was closed.
func TestUpstreamCrashDisconnectsSession(t *testing.T) {
	d := testDaemon(t)

	// Spawn an owner using mock_server_crash — it responds to initialize then exits.
	ipcPath, _, tok, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server_crash.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	// Connect one IPC client.
	client := dialIPC(t, ipcPath, tok)
	defer client.conn.Close()

	// Send initialize so the upstream processes at least one request.
	// We do NOT use daemonReadReq here because the session may close before
	// the response arrives — instead waitEOF drains all remaining lines.
	daemonSendReq(t, client, 1, "initialize",
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`)

	// mock_server_crash exits after ~200 ms.
	// Path: upstream exit → onUpstreamExit → Shutdown() → Session.Close() → net.Conn.Close().
	// The scanner must eventually return false (EOF), signalling the session was closed.
	if !waitEOF(client, 5*time.Second) {
		t.Fatal("IPC client did not receive EOF within 5s after upstream crash")
	}
}

// TestDaemonShutdownDisconnectsAllSessions verifies that Daemon.Shutdown() closes
// all active IPC sessions: both connected clients must receive EOF within 5 seconds.
func TestDaemonShutdownDisconnectsAllSessions(t *testing.T) {
	d := testDaemon(t)

	ipcPath, _, tok1, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	_, _, tok2, err2 := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err2 != nil {
		t.Fatalf("Spawn() second error: %v", err2)
	}

	// Connect two IPC clients.
	client1 := dialIPC(t, ipcPath, tok1)
	defer client1.conn.Close()

	client2 := dialIPC(t, ipcPath, tok2)
	defer client2.conn.Close()

	// Prime initialize on both sessions so the owner is fully initialised.
	daemonSendReq(t, client1, 1, "initialize",
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c1","version":"0"}}`)
	daemonReadResp(t, client1)

	daemonSendReq(t, client2, 2, "initialize",
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c2","version":"0"}}`)
	daemonReadResp(t, client2)

	// Trigger daemon shutdown asynchronously so we can observe the effect.
	go d.Shutdown()

	// Both clients must receive EOF within 5 seconds.
	eof1 := make(chan bool, 1)
	eof2 := make(chan bool, 1)
	go func() { eof1 <- waitEOF(client1, 5*time.Second) }()
	go func() { eof2 <- waitEOF(client2, 5*time.Second) }()

	if !<-eof1 {
		t.Error("client1 did not receive EOF within 5s after Shutdown()")
	}
	if !<-eof2 {
		t.Error("client2 did not receive EOF within 5s after Shutdown()")
	}
}

// TestOwnerSurvivesSingleSessionDisconnect verifies that when one of two active
// IPC sessions disconnects, the owner remains alive and the surviving session
// can continue sending requests and receiving valid responses.
func TestOwnerSurvivesSingleSessionDisconnect(t *testing.T) {
	d := testDaemon(t)

	ipcPath, sid, tok1, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	_, _, tok2, err2 := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err2 != nil {
		t.Fatalf("Spawn() second error: %v", err2)
	}

	entry := d.Entry(sid)
	if entry == nil {
		t.Fatal("Entry() returned nil after Spawn")
	}
	owner := entry.Owner

	// waitSessionCount polls until the owner reports exactly n sessions.
	waitSessionCount := func(n int, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if owner.SessionCount() == n {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("timed out waiting for SessionCount=%d (got %d)", n, owner.SessionCount())
	}

	// Connect two clients.
	client1 := dialIPC(t, ipcPath, tok1)
	client2 := dialIPC(t, ipcPath, tok2)
	defer client2.conn.Close()

	waitSessionCount(2, 5*time.Second)

	// Send initialize on both sessions.
	daemonSendReq(t, client1, 1, "initialize",
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c1","version":"0"}}`)
	daemonReadResp(t, client1)

	daemonSendReq(t, client2, 2, "initialize",
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c2","version":"0"}}`)
	daemonReadResp(t, client2)

	// Disconnect client1 — owner must survive.
	client1.conn.Close()
	waitSessionCount(1, 5*time.Second)

	// Daemon must still have exactly one owner.
	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount = %d after client1 disconnect, want 1", d.OwnerCount())
	}

	// client2 must still be able to send requests and receive valid responses.
	daemonSendReq(t, client2, 10, "ping", `{}`)
	pingResp := daemonReadResp(t, client2)
	daemonAssertID(t, pingResp, 10)
}

// TestDedup_SharedServersReusedAcrossCwd verifies the global dedup path:
// two spawns of the same command+args from different cwds (mode="cwd") return
// the same ipc_path and server_id, and the daemon keeps exactly one owner.
func TestDedup_SharedServersReusedAcrossCwd(t *testing.T) {
	d := testDaemon(t)

	// Use the absolute path to mock_server.go so the process can start from any cwd.
	mockServer := testdataPath(t, "mock_server.go")
	// Use os.TempDir() subdirs as distinct cwds (not t.TempDir()) so Windows doesn't
	// try to remove them while a spawned go-run process may still hold a handle.
	cwdA := filepath.Join(os.TempDir(), "mux-test-dedup-a")
	cwdB := filepath.Join(os.TempDir(), "mux-test-dedup-b")
	for _, dir := range []string{cwdA, cwdB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	t.Cleanup(func() {
		os.RemoveAll(cwdA)
		os.RemoveAll(cwdB)
	})

	// First spawn — cwdA.
	ipcPath1, sid1, probeTok, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Mode:    "cwd",
		Cwd:     cwdA,
	})
	if err != nil {
		t.Fatalf("Spawn() cwdA error: %v", err)
	}

	// Prime the owner: send initialize + tools/list so auto_classification is
	// populated ("shared") before the second spawn's findSharedOwner check runs.
	probe := dialIPC(t, ipcPath1, probeTok)
	daemonSendReq(t, probe, 1, "initialize",
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}`)
	daemonReadResp(t, probe)
	daemonSendReq(t, probe, 2, "tools/list", `{}`)
	daemonReadResp(t, probe)
	probe.conn.Close()

	// Second spawn — same command, different cwd (cwdB).
	// findSharedOwner must find the first owner because its classification is not "isolated".
	ipcPath2, sid2, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Mode:    "cwd",
		Cwd:     cwdB,
	})
	if err != nil {
		t.Fatalf("Spawn() cwdB error: %v", err)
	}

	if ipcPath1 != ipcPath2 {
		t.Errorf("dedup: expected same ipc_path, got\n  cwdA: %s\n  cwdB: %s",
			ipcPath1, ipcPath2)
	}
	if sid1 != sid2 {
		t.Errorf("dedup: expected same server_id, got\n  cwdA: %s\n  cwdB: %s",
			sid1, sid2)
	}
	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount = %d, want 1 (shared owner reused across cwds)", d.OwnerCount())
	}
}

// TestDedup_IsolatedServersDistinctByCwd verifies the post-CR-001 contract for
// mode="isolated": identity is deterministic on (cmd, args, cwd), so two
// isolated spawns from DIFFERENT cwds produce two distinct owners (cross-cwd
// isolation preserved), while two isolated spawns from the SAME (cmd, args,
// cwd) reuse one owner (see companion TestDedup_IsolatedSameContextReuses).
//
// Replaces pre-CR-001 TestDedup_IsolatedServersNotShared which asserted
// "isolated always gets a fresh sid regardless of inputs" — that encoded the
// random-UUID accumulation bug (Engram #244 Bug 2). Under the new contract,
// "isolated" still prevents cross-CWD sharing, but allows same-context reuse.
func TestDedup_IsolatedServersDistinctByCwd(t *testing.T) {
	// TempDirs allocated BEFORE testDaemon so the daemon's Shutdown cleanup
	// (t.Cleanup LIFO) terminates upstream subprocesses BEFORE TempDir rm-rf
	// runs on Windows — otherwise Windows file locks on the subprocess cwd
	// fail the rm-rf and mark the test FAIL post-assertions.
	cwd1 := t.TempDir()
	cwd2 := t.TempDir()

	d := testDaemon(t)

	mockServer := testdataPath(t, "mock_server.go")

	ipcPath1, sid1, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd1,
		Mode:    "isolated",
	})
	if err != nil {
		t.Fatalf("Spawn() isolated-cwd1 error: %v", err)
	}

	ipcPath2, sid2, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd2,
		Mode:    "isolated",
	})
	if err != nil {
		t.Fatalf("Spawn() isolated-cwd2 error: %v", err)
	}

	if ipcPath1 == ipcPath2 {
		t.Errorf("isolated servers from different cwds must not share ipc_path: both returned %s", ipcPath1)
	}
	if sid1 == sid2 {
		t.Errorf("isolated servers from different cwds must not share server_id: both returned %s", sid1)
	}
	if d.OwnerCount() != 2 {
		t.Errorf("OwnerCount = %d, want 2 for two isolated spawns from distinct cwds", d.OwnerCount())
	}
}

// TestDedup_IsolatedSameContextReuses asserts the reuse half of the post-CR-001
// contract: two isolated spawns with identical (cmd, args, cwd) hit the same
// owner. This is the half that fixes Engram #244 Bug 2's accumulation pattern
// — every reconnect of the same isolated upstream from the same project now
// rebinds instead of spawning a fresh subprocess.
func TestDedup_IsolatedSameContextReuses(t *testing.T) {
	cwd := t.TempDir()

	d := testDaemon(t)

	mockServer := testdataPath(t, "mock_server.go")

	_, sid1, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd,
		Mode:    "isolated",
	})
	if err != nil {
		t.Fatalf("Spawn() isolated-1 error: %v", err)
	}

	_, sid2, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd,
		Mode:    "isolated",
	})
	if err != nil {
		t.Fatalf("Spawn() isolated-2 error: %v", err)
	}

	if sid1 != sid2 {
		t.Errorf("isolated servers with identical (cmd, args, cwd) must reuse owner: got %s vs %s", sid1, sid2)
	}
	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount = %d, want 1 for two isolated spawns of identical context", d.OwnerCount())
	}
}
