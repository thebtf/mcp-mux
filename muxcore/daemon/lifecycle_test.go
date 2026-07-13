package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestUpstreamCrashRespawnsSession verifies that when an upstream exits
// unexpectedly while a session is still connected, muxcore keeps the IPC
// session alive, returns an explicit error for the lost in-flight request, and
// respawns a replacement upstream for the next request.
func TestUpstreamCrashRespawnsSession(t *testing.T) {
	d := testDaemon(t)
	generationFile := t.TempDir() + string(os.PathSeparator) + "generation.txt"
	cmd, args, env := daemonRespawnHelperCommand(generationFile)

	ipcPath, _, tok, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: cmd,
		Args:    args,
		Env:     env,
		Mode:    "global",
	})
	if err != nil {
		t.Fatalf("Spawn() error: %v", err)
	}

	// Connect one IPC client.
	client := dialIPC(t, ipcPath, tok)
	defer client.conn.Close()

	daemonSendReq(t, client, 1, "initialize",
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`)
	daemonReadRespWithID(t, client, 1)

	daemonSendReq(t, client, 2, "ping", `{}`)
	resp := daemonReadRespWithID(t, client, 2)
	initialGeneration := daemonRespawnGeneration(t, resp)
	if initialGeneration != 1 {
		t.Fatalf("initial ping response = %s, want generation 1", resp)
	}

	daemonSendReq(t, client, 3, "tools/call", `{"name":"crash","arguments":{}}`)
	crashResp := daemonReadRespWithID(t, client, 3)
	if !strings.Contains(string(crashResp), "upstream process exited") {
		t.Fatalf("crash response = %s, want explicit upstream-exit error", crashResp)
	}

	daemonSendReq(t, client, 4, "ping", `{}`)
	resp = daemonReadRespWithID(t, client, 4)
	replacementGeneration := daemonRespawnGeneration(t, resp)
	if replacementGeneration <= initialGeneration {
		t.Fatalf("post-respawn ping response = %s, want generation > %d from same IPC session", resp, initialGeneration)
	}
}

func daemonRespawnGeneration(t *testing.T, resp []byte) int {
	t.Helper()
	var obj struct {
		Result struct {
			Generation int `json:"generation"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &obj); err != nil {
		t.Fatalf("unmarshal generation response: %v (raw: %s)", err, resp)
	}
	return obj.Result.Generation
}

func daemonReadRespWithID(t *testing.T, c *ipcConn, id int) []byte {
	t.Helper()
	for i := 0; i < 10; i++ {
		resp := daemonReadResp(t, c)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(resp, &obj); err != nil {
			t.Fatalf("unmarshal response: %v (raw: %s)", err, resp)
		}
		if got := string(obj["id"]); got == strconv.Itoa(id) {
			return resp
		}
		if obj["id"] == nil && obj["method"] != nil {
			continue
		}
		t.Fatalf("unexpected response while waiting for id %d: %s", id, resp)
	}
	t.Fatalf("did not receive response id %d", id)
	return nil
}

func daemonRespawnHelperCommand(generationFile string) (string, []string, map[string]string) {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			env[k] = v
		}
	}
	env["MCP_MUX_DAEMON_RESPAWN_HELPER"] = "1"
	env["MCP_MUX_DAEMON_RESPAWN_GENERATION_FILE"] = generationFile
	return os.Args[0], []string{"-test.run=TestDaemonRespawnHelperProcess", "--"}, env
}

func TestDaemonRespawnHelperProcess(t *testing.T) {
	if os.Getenv("MCP_MUX_DAEMON_RESPAWN_HELPER") != "1" {
		return
	}
	generationFile := os.Getenv("MCP_MUX_DAEMON_RESPAWN_GENERATION_FILE")
	if generationFile == "" {
		fmt.Fprintln(os.Stderr, "MCP_MUX_DAEMON_RESPAWN_GENERATION_FILE is required")
		os.Exit(2)
	}
	generation := nextDaemonRespawnHelperGeneration(generationFile)
	runDaemonRespawnHelperServer(generation)
	os.Exit(0)
}

func nextDaemonRespawnHelperGeneration(path string) int {
	data, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o644)
	return n
}

func runDaemonRespawnHelperServer(generation int) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			writeDaemonRespawnError(nil, -32700, err.Error())
			continue
		}
		switch req.Method {
		case "initialize":
			writeDaemonRespawnResult(req.ID, map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{
					"name":    "daemon-respawn-helper",
					"version": fmt.Sprintf("generation-%d", generation),
				},
			})
		case "notifications/initialized":
			continue
		case "tools/list":
			writeDaemonRespawnResult(req.ID, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "crash",
						"description": "exit without a response",
						"inputSchema": map[string]any{"type": "object"},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name == "crash" {
				os.Exit(42)
			}
			writeDaemonRespawnResult(req.ID, map[string]any{"generation": generation})
		case "ping":
			writeDaemonRespawnResult(req.ID, map[string]any{"generation": generation})
		default:
			writeDaemonRespawnError(req.ID, -32601, "method not found")
		}
	}
}

func writeDaemonRespawnResult(id json.RawMessage, result any) {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	fmt.Fprintln(os.Stdout, string(data))
}

func writeDaemonRespawnError(id json.RawMessage, code int, message string) {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
	fmt.Fprintln(os.Stdout, string(data))
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

// TestDedup_ExplicitIsolatedSameContextGetsFreshOwner keeps explicit isolation
// private per fresh consumer. Reconnects return through refresh-token rather
// than a fresh Spawn, so a new Spawn must not attach to an existing owner.
func TestDedup_ExplicitIsolatedSameContextGetsFreshOwner(t *testing.T) {
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

	if sid1 == sid2 {
		t.Errorf("fresh isolated servers with identical (cmd, args, cwd) must not reuse owner: %s", sid1)
	}
	if d.OwnerCount() != 2 {
		t.Errorf("OwnerCount = %d, want 2 for two fresh isolated spawns of identical context", d.OwnerCount())
	}
}
