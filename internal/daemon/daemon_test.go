package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/internal/control"
	"github.com/thebtf/mcp-mux/internal/ipc"
)

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(os.Stderr, "[daemon-test] ", log.LstdFlags)
}

// shortSocketPath returns a short socket path suitable for all platforms.
// macOS limits Unix socket paths to 104 bytes; t.TempDir() paths are too long.
func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	f, err := os.CreateTemp("", "mux-test-*.sock")
	if err != nil {
		t.Fatalf("create temp socket path: %v", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path) // remove file so ipc.Listen can create socket
	t.Cleanup(func() { os.Remove(path) })
	return path
}

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	ctlPath := shortSocketPath(t, "daemon.ctl.sock")
	d, err := New(Config{
		ControlPath: ctlPath,
		GracePeriod: 1 * time.Second,
		IdleTimeout: 5 * time.Second,
		Logger:      testLogger(t),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d
}

func TestDaemonSpawnAndStatus(t *testing.T) {
	d := testDaemon(t)

	ipcPath, sid, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Cwd:     "",
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	if ipcPath == "" {
		t.Error("Spawn() returned empty ipcPath")
	}
	if sid == "" {
		t.Error("Spawn() returned empty serverID")
	}

	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount() = %d, want 1", d.OwnerCount())
	}

	// Status should include the server
	status := d.HandleStatus()
	if status["owner_count"] != 1 {
		t.Errorf("status owner_count = %v, want 1", status["owner_count"])
	}
	if !status["daemon"].(bool) {
		t.Error("status daemon should be true")
	}
}

func TestDaemonSpawnReusesExisting(t *testing.T) {
	d := testDaemon(t)

	req := control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	}

	ipc1, sid1, _, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() 1 error: %v", err)
	}

	ipc2, sid2, _, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() 2 error: %v", err)
	}

	if ipc1 != ipc2 {
		t.Errorf("second spawn returned different ipcPath: %s vs %s", ipc1, ipc2)
	}
	if sid1 != sid2 {
		t.Errorf("second spawn returned different serverID: %s vs %s", sid1, sid2)
	}
	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount() = %d, want 1 (should reuse)", d.OwnerCount())
	}
}

func TestDaemonRemove(t *testing.T) {
	d := testDaemon(t)

	_, sid, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	if err := d.Remove(sid); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}

	if d.OwnerCount() != 0 {
		t.Errorf("OwnerCount() = %d after Remove, want 0", d.OwnerCount())
	}

	// Remove again should fail
	if err := d.Remove(sid); err == nil {
		t.Error("Remove() should fail for non-existent server")
	}
}

func TestDaemonShutdownCleansAll(t *testing.T) {
	d := testDaemon(t)

	for i := 0; i < 3; i++ {
		_, _, _, err := d.Spawn(control.Request{
			Cmd:     "spawn",
			Command: "go",
			Args:    []string{"run", "../../testdata/mock_server.go"},
			Mode:    "isolated",
		})
		if err != nil {
			t.Fatalf("Spawn() %d error: %v", i, err)
		}
	}

	if d.OwnerCount() != 3 {
		t.Fatalf("OwnerCount() = %d, want 3", d.OwnerCount())
	}

	d.Shutdown()

	select {
	case <-d.Done():
		// ok
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown() did not complete in time")
	}

	if d.OwnerCount() != 0 {
		t.Errorf("OwnerCount() = %d after Shutdown, want 0", d.OwnerCount())
	}
}

func TestDaemonSetPersistent(t *testing.T) {
	d := testDaemon(t)

	_, sid, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	d.SetPersistent(sid, true)

	entry := d.Entry(sid)
	if entry == nil {
		t.Fatal("Entry() returned nil")
	}
	if !entry.Persistent {
		t.Error("Persistent should be true after SetPersistent(true)")
	}
}

// TestDaemonMultiSessionSharing verifies that two independent shim-style IPC
// clients can connect to a single daemon-managed owner and share it correctly:
//   - Session 1 primes the initialize cache via the upstream.
//   - Session 2 receives the cached initialize response (no extra upstream round-trip).
//   - Both sessions send tools/call requests; ID remapping delivers each response
//     only to its originating session.
//   - After session 1 disconnects, session 2 continues to function.
//   - SessionCount() reflects the live connection count throughout.
func TestDaemonMultiSessionSharing(t *testing.T) {
	d := testDaemon(t)

	// 1. Spawn owner for mock_server via daemon.
	ipcPath, sid, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}
	if ipcPath == "" || sid == "" {
		t.Fatal("Spawn() returned empty ipcPath or sid")
	}

	entry := d.Entry(sid)
	if entry == nil {
		t.Fatal("Entry() returned nil after Spawn")
	}
	owner := entry.Owner

	// waitSessions blocks until the owner reports exactly n sessions (or times out).
	waitSessions := func(n int, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if owner.SessionCount() == n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("timed out waiting for SessionCount = %d (got %d)", n, owner.SessionCount())
	}

	// 2. Connect two IPC clients to the owner's socket.
	// The IPC listener may not be ready immediately after Spawn returns, so retry briefly.
	var conn1, conn2 *ipcConn
	for i := 0; i < 50; i++ {
		c, dialErr := ipc.Dial(ipcPath)
		if dialErr == nil {
			conn1 = &ipcConn{conn: c, scanner: bufio.NewScanner(c)}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if conn1 == nil {
		t.Fatalf("failed to dial IPC path %s after retries", ipcPath)
	}
	defer conn1.conn.Close()

	for i := 0; i < 50; i++ {
		c, dialErr := ipc.Dial(ipcPath)
		if dialErr == nil {
			conn2 = &ipcConn{conn: c, scanner: bufio.NewScanner(c)}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if conn2 == nil {
		t.Fatalf("failed to dial second IPC connection to %s", ipcPath)
	}
	defer conn2.conn.Close()

	// Wait for both sessions to be registered by the owner's accept loop.
	waitSessions(2, 5*time.Second)
	if owner.SessionCount() != 2 {
		t.Fatalf("SessionCount = %d, want 2 after both clients connected", owner.SessionCount())
	}

	// 3. Session 1 sends initialize — upstream responds and response is cached.
	daemonSendReq(t, conn1, 1, "initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`)
	resp1 := daemonReadResp(t, conn1)
	daemonAssertID(t, resp1, 1)
	if !strings.Contains(string(resp1), "mock-server") {
		t.Errorf("session1 initialize response missing mock-server: %s", resp1)
	}

	// 4. Session 2 sends initialize — should receive cached response.
	daemonSendReq(t, conn2, 2, "initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`)
	resp2 := daemonReadResp(t, conn2)
	daemonAssertID(t, resp2, 2)
	if !strings.Contains(string(resp2), "mock-server") {
		t.Errorf("session2 initialize (from cache) response missing mock-server: %s", resp2)
	}

	// 5. Both sessions call tools/call echo with distinct messages and id=10.
	//    Identical client-side IDs are valid; ID remapping must deliver each
	//    response only to the correct session.
	daemonSendReq(t, conn1, 10, "tools/call", `{"name":"echo","arguments":{"message":"hello-from-session1"}}`)
	daemonSendReq(t, conn2, 10, "tools/call", `{"name":"echo","arguments":{"message":"hello-from-session2"}}`)

	got1 := daemonReadResp(t, conn1)
	got2 := daemonReadResp(t, conn2)

	daemonAssertID(t, got1, 10)
	daemonAssertID(t, got2, 10)

	if !strings.Contains(string(got1), "hello-from-session1") {
		t.Errorf("session1 tools/call response missing expected content: %s", got1)
	}
	if !strings.Contains(string(got2), "hello-from-session2") {
		t.Errorf("session2 tools/call response missing expected content: %s", got2)
	}

	// 6. Disconnect session 1 — session 2 must still work.
	conn1.conn.Close()
	waitSessions(1, 5*time.Second)
	if owner.SessionCount() != 1 {
		t.Errorf("SessionCount = %d after session1 disconnect, want 1", owner.SessionCount())
	}

	// 7. Session 2 sends another request to confirm the owner is still functional.
	daemonSendReq(t, conn2, 20, "ping", `{}`)
	pingResp := daemonReadResp(t, conn2)
	daemonAssertID(t, pingResp, 20)

	// 8. Verify final state: still exactly one owner in the daemon.
	if d.OwnerCount() != 1 {
		t.Errorf("OwnerCount = %d, want 1 (owner should persist while session2 is live)", d.OwnerCount())
	}
}

// ipcConn bundles a net.Conn with its line scanner for test helpers.
type ipcConn struct {
	conn interface {
		Write([]byte) (int, error)
		Close() error
	}
	scanner *bufio.Scanner
}

// daemonSendReq writes a JSON-RPC request line to an IPC connection.
func daemonSendReq(t *testing.T, c *ipcConn, id int, method, params string) {
	t.Helper()
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"%s","params":%s}`+"\n", id, method, params)
	if _, err := c.conn.Write([]byte(line)); err != nil {
		t.Fatalf("daemonSendReq write error: %v", err)
	}
}

// daemonReadResp reads one JSON-RPC response line from an IPC connection.
func daemonReadResp(t *testing.T, c *ipcConn) []byte {
	t.Helper()
	type result struct {
		line string
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		ok := c.scanner.Scan()
		ch <- result{line: c.scanner.Text(), ok: ok}
	}()
	select {
	case r := <-ch:
		if !r.ok {
			t.Fatal("daemonReadResp: connection closed before response")
		}
		return []byte(r.line)
	case <-time.After(15 * time.Second):
		t.Fatal("daemonReadResp: timeout waiting for response")
		return nil
	}
}

// TestDaemonStartAfterCrash verifies crash recovery: when a stale control socket
// file is left on disk (simulating a previous daemon crash), a new daemon can
// start successfully and respond to control commands.
//
// Flow exercised:
//
//	stale socket file exists
//	  → ipc.IsAvailable returns false (dial fails — nobody is listening)
//	  → ensureDaemon proceeds to start new daemon
//	  → daemon.New → control.NewServer → ipc.Listen removes stale file, binds
//	  → daemon is ready and responsive
func TestDaemonStartAfterCrash(t *testing.T) {
	ctlPath := shortSocketPath(t, "mcp-muxd.ctl.sock")

	// Step 1: Simulate a crashed daemon by leaving a stale socket file.
	f, err := os.Create(ctlPath)
	if err != nil {
		t.Fatalf("create stale socket file: %v", err)
	}
	f.Close()

	// Confirm the stale file is present and unconnectable.
	if _, err := os.Stat(ctlPath); err != nil {
		t.Fatalf("stale socket file should exist: %v", err)
	}
	if ipc.IsAvailable(ctlPath) {
		t.Fatal("stale socket should not be connectable")
	}

	// Step 2: Start a new daemon — this must succeed despite the stale file.
	d, err := New(Config{
		ControlPath: ctlPath,
		GracePeriod: 1 * time.Second,
		IdleTimeout: 5 * time.Second,
		Logger:      testLogger(t),
	})
	if err != nil {
		t.Fatalf("daemon.New() with stale socket: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	// Step 3: Verify the daemon is live — control socket must now be connectable.
	if !ipc.IsAvailable(ctlPath) {
		t.Fatal("daemon control socket should be available after start")
	}

	// Step 4: Verify the daemon responds correctly to a ping.
	resp, err := control.Send(ctlPath, control.Request{Cmd: "ping"})
	if err != nil {
		t.Fatalf("control.Send(ping) error: %v", err)
	}
	if !resp.OK {
		t.Errorf("ping response OK = false, message: %s", resp.Message)
	}
	if resp.Message != "pong" {
		t.Errorf("ping response message = %q, want %q", resp.Message, "pong")
	}

	// Step 5: Stale file was replaced — daemon should own the socket path now.
	if _, err := os.Stat(ctlPath); err != nil {
		t.Errorf("control socket path should exist after daemon start: %v", err)
	}
}

// daemonAssertID verifies that a JSON-RPC response carries the expected numeric id.
func daemonAssertID(t *testing.T, resp []byte, expectedID int) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(resp, &obj); err != nil {
		t.Fatalf("daemonAssertID: unmarshal error: %v (raw: %s)", err, resp)
	}
	idRaw, ok := obj["id"]
	if !ok {
		t.Fatalf("daemonAssertID: response has no id field: %s", resp)
	}
	var id int
	if err := json.Unmarshal(idRaw, &id); err != nil {
		t.Fatalf("daemonAssertID: unmarshal id: %v (raw: %s)", err, idRaw)
	}
	if id != expectedID {
		t.Errorf("daemonAssertID: id = %d, want %d (full: %s)", id, expectedID, resp)
	}
}

func TestEnvTransient(t *testing.T) {
	testCases := []struct {
		name string
		key  string
		want bool
	}{
		{name: "entrypoint", key: "CLAUDE_CODE_ENTRYPOINT", want: true},
		{name: "claude code prefix", key: "CLAUDE_CODE_USE_POWERSHELL_TOOL", want: true},
		{name: "claude auto prefix", key: "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", want: true},
		{name: "wt session", key: "WT_SESSION", want: true},
		{name: "wt profile", key: "WT_PROFILE_ID", want: true},
		{name: "session name", key: "SESSIONNAME", want: true},
		{name: "wsl env", key: "WSLENV", want: true},
		{name: "github token", key: "GITHUB_TOKEN", want: false},
		{name: "github personal access token", key: "GITHUB_PERSONAL_ACCESS_TOKEN", want: false},
		{name: "tavily api key", key: "TAVILY_API_KEY", want: false},
		{name: "path", key: "PATH", want: false},
		{name: "home", key: "HOME", want: false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := envTransient(tc.key)
			if got != tc.want {
				t.Fatalf("envTransient(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestEnvCompatible(t *testing.T) {
	testCases := []struct {
		name string
		a    map[string]string
		b    map[string]string
		want bool
	}{
		{name: "both empty", a: map[string]string{}, b: map[string]string{}, want: true},
		{name: "a empty b has keys", a: map[string]string{}, b: map[string]string{"GITHUB_TOKEN": "abc"}, want: true},
		{name: "a has keys b empty", a: map[string]string{"GITHUB_TOKEN": "abc"}, b: map[string]string{}, want: true},
		{name: "same key same value", a: map[string]string{"GITHUB_TOKEN": "abc"}, b: map[string]string{"GITHUB_TOKEN": "abc"}, want: true},
		{name: "same key different value semantic", a: map[string]string{"GITHUB_TOKEN": "abc"}, b: map[string]string{"GITHUB_TOKEN": "xyz"}, want: false},
		{name: "transient key different value", a: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}, b: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "ide"}, want: true},
		{name: "transient key semantic same", a: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli", "GITHUB_TOKEN": "abc"}, b: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "ide", "GITHUB_TOKEN": "abc"}, want: true},
		{name: "transient key semantic different", a: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli", "GITHUB_TOKEN": "abc"}, b: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "ide", "GITHUB_TOKEN": "xyz"}, want: false},
		{name: "overlapping partial", a: map[string]string{"GITHUB_TOKEN": "abc", "PATH": "/usr"}, b: map[string]string{"GITHUB_TOKEN": "abc"}, want: true},
		{name: "real world same token", a: map[string]string{"GITHUB_TOKEN": "abc", "CLAUDE_CODE_ENTRYPOINT": "cli"}, b: map[string]string{"GITHUB_TOKEN": "abc", "CLAUDE_CODE_ENTRYPOINT": "ide"}, want: true},
		{name: "real world different token", a: map[string]string{"GITHUB_TOKEN": "abc", "CLAUDE_CODE_ENTRYPOINT": "cli"}, b: map[string]string{"GITHUB_TOKEN": "xyz", "CLAUDE_CODE_ENTRYPOINT": "cli"}, want: false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := envCompatible(tc.a, tc.b)
			if got != tc.want {
				t.Fatalf("envCompatible(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestFindSharedOwnerEnvFiltering(t *testing.T) {
	d := testDaemon(t)
	req := control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		Mode:    "cwd",
		Env:     map[string]string{"GITHUB_TOKEN": "abc"},
	}

	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	entry := findSharedOwnerLocked(d, req.Command, req.Args, map[string]string{"GITHUB_TOKEN": "abc"})
	if entry == nil {
		t.Fatal("findSharedOwner() returned nil for matching semantic env")
	}
	if entry.ServerID != sid {
		t.Fatalf("findSharedOwner() serverID = %q, want %q", entry.ServerID, sid)
	}

	conflict := findSharedOwnerLocked(d, req.Command, req.Args, map[string]string{"GITHUB_TOKEN": "xyz"})
	if conflict != nil {
		t.Fatalf("findSharedOwner() returned %q for conflicting semantic env", conflict.ServerID)
	}

	transientMatch := findSharedOwnerLocked(d, req.Command, req.Args, map[string]string{
		"GITHUB_TOKEN":           "abc",
		"CLAUDE_CODE_ENTRYPOINT": "cli",
	})
	if transientMatch == nil {
		t.Fatal("findSharedOwner() returned nil when only transient env differed")
	}
	if transientMatch.ServerID != sid {
		t.Fatalf("findSharedOwner() with transient env returned %q, want %q", transientMatch.ServerID, sid)
	}
}

func TestCleanStaleSockets(t *testing.T) {
	const staleSocketCount = 3

	paths := make([]string, 0, staleSocketCount)
	for i := 0; i < staleSocketCount; i++ {
		f, err := os.CreateTemp(os.TempDir(), "mcp-mux-*-test.ctl.sock")
		if err != nil {
			t.Fatalf("CreateTemp() error: %v", err)
		}
		path := f.Name()
		if err := f.Close(); err != nil {
			t.Fatalf("Close() error: %v", err)
		}
		t.Cleanup(func() { os.Remove(path) })
		paths = append(paths, path)
	}

	cleaned := cleanStaleSockets(testLogger(t))
	if cleaned != staleSocketCount {
		t.Fatalf("cleanStaleSockets() = %d, want %d", cleaned, staleSocketCount)
	}

	for _, path := range paths {
		_, err := os.Stat(path)
		if err == nil {
			t.Fatalf("cleanStaleSockets() did not remove %q", path)
		}
		if !os.IsNotExist(err) {
			t.Fatalf("Stat(%q) error = %v, want not exist", path, err)
		}
	}
}

func findSharedOwnerLocked(d *Daemon, command string, args []string, env map[string]string) *OwnerEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.findSharedOwner(command, args, env)
}
