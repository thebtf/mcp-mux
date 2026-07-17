package owner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerRespawnsUpstreamForLiveSession(t *testing.T) {
	ipcPath := testIPCPath(t)
	generationFile := t.TempDir() + string(os.PathSeparator) + "generation.txt"

	cmd, args, env := respawnHelperCommand(generationFile)
	o, err := NewOwner(OwnerConfig{
		Command: cmd,
		Args:    args,
		Env:     env,
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer o.Shutdown()

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	o.AddSession(session)

	sendReq(t, clientW, 1, "initialize", `{}`)
	assertResponseID(t, readResp(t, clientR), 1)

	sendReq(t, clientW, 2, "ping", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 2)
	initialGeneration := respawnGeneration(t, resp)
	if initialGeneration != 1 {
		t.Fatalf("initial ping response = %s, want generation 1", resp)
	}

	sendReq(t, clientW, 3, "tools/call", `{"name":"crash","arguments":{}}`)
	crashResp := readResp(t, clientR)
	assertResponseID(t, crashResp, 3)
	if !strings.Contains(string(crashResp), "upstream process exited") {
		t.Fatalf("crash response = %s, want explicit upstream-exit error", crashResp)
	}

	sendReq(t, clientW, 4, "ping", `{}`)
	resp = readRespWithID(t, clientR, 4)
	assertResponseID(t, resp, 4)
	replacementGeneration := respawnGeneration(t, resp)
	if replacementGeneration <= initialGeneration {
		t.Fatalf("post-respawn ping response = %s, want generation > %d from same session", resp, initialGeneration)
	}
}

func TestSnapshotBackgroundSpawnBlocksRequestRespawn(t *testing.T) {
	var starts atomic.Int32
	requestWaiting := make(chan struct{})
	initializeReceived := make(chan struct{})
	allowInitializeResponse := make(chan struct{})
	initializedReceived := make(chan error, 1)
	release := make(chan struct{})
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		if !scanner.Scan() {
			return scanner.Err()
		}
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return err
		}
		close(initializeReceived)
		<-allowInitializeResponse
		if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"background-gate","version":"1"}}}`+"\n", req.ID); err != nil {
			return err
		}

		if !scanner.Scan() {
			err := scanner.Err()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			initializedReceived <- err
			return err
		}
		var notification struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &notification); err != nil {
			initializedReceived <- err
			return err
		}
		if notification.Method != "notifications/initialized" {
			err := fmt.Errorf("first post-initialize message method = %q, want notifications/initialized", notification.Method)
			initializedReceived <- err
			return err
		}
		initializedReceived <- nil
		<-release
		return nil
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc: handler,
		IPCPath:     testIPCPath(t),
		Logger:      testLogger(t),
	}, OwnerSnapshot{})
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot() error: %v", err)
	}
	o.mu.Lock()
	o.beforeBackgroundSpawnWait = func() {
		close(requestWaiting)
	}
	spawnDone := o.backgroundSpawnCh
	o.mu.Unlock()
	if spawnDone == nil {
		t.Fatal("snapshot owner has no pending background-spawn channel")
	}

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	o.AddSession(session)

	respawnLocked := true
	initializeReleased := false
	o.respawnMu.Lock()
	t.Cleanup(func() {
		if respawnLocked {
			o.respawnMu.Unlock()
		}
		if !initializeReleased {
			close(allowInitializeResponse)
		}
		o.removeSession(session)
		session.Close()
		_ = clientR.Close()
		_ = clientW.Close()
		close(release)
		o.Shutdown()
	})

	ready := make(chan error, 1)
	go func() {
		ready <- o.ensureUpstreamReadyForRequest()
	}()

	select {
	case <-requestWaiting:
	case <-time.After(time.Second):
		t.Fatal("request path did not enter the pending background-spawn wait")
	}
	o.SpawnUpstreamBackground()
	waitForCondition(t, time.Second, func() bool {
		return starts.Load() == 1
	}, "background upstream did not start")
	select {
	case <-initializeReceived:
	case <-time.After(time.Second):
		t.Fatal("background upstream did not receive initialize")
	}

	select {
	case <-spawnDone:
		t.Fatal("background-spawn gate closed before the new upstream answered initialize")
	default:
	}
	select {
	case err := <-ready:
		t.Fatalf("request readiness returned before initialize completed: %v", err)
	default:
	}

	close(allowInitializeResponse)
	initializeReleased = true
	select {
	case err := <-initializedReceived:
		if err != nil {
			t.Fatalf("proactive initialization error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("background upstream did not receive notifications/initialized")
	}
	select {
	case <-spawnDone:
	case <-time.After(time.Second):
		t.Fatal("background-spawn gate did not close after notifications/initialized")
	}

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("ensureUpstreamReadyForRequest() error: %v", err)
		}
	case <-time.After(time.Second):
		o.respawnMu.Unlock()
		respawnLocked = false
		t.Fatal("request path started a competing respawn instead of waiting for the pending background spawn")
	}

	o.respawnMu.Unlock()
	respawnLocked = false
	if got := starts.Load(); got != 1 {
		t.Fatalf("upstream starts = %d, want 1", got)
	}
}

func respawnGeneration(t *testing.T, resp []byte) int {
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

func readRespWithID(t *testing.T, r io.Reader, id int) []byte {
	t.Helper()
	for i := 0; i < 10; i++ {
		resp := readResp(t, r)
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

func respawnHelperCommand(generationFile string) (string, []string, map[string]string) {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			env[k] = v
		}
	}
	env["MCP_MUX_RESPAWN_HELPER"] = "1"
	env["MCP_MUX_RESPAWN_GENERATION_FILE"] = generationFile
	return os.Args[0], []string{"-test.run=TestRespawnHelperProcess", "--"}, env
}

func TestRespawnHelperProcess(t *testing.T) {
	if os.Getenv("MCP_MUX_RESPAWN_HELPER") != "1" {
		return
	}
	generationFile := os.Getenv("MCP_MUX_RESPAWN_GENERATION_FILE")
	if generationFile == "" {
		fmt.Fprintln(os.Stderr, "MCP_MUX_RESPAWN_GENERATION_FILE is required")
		os.Exit(2)
	}
	generation := nextRespawnHelperGeneration(generationFile)
	runRespawnHelperServer(generation)
	os.Exit(0)
}

func nextRespawnHelperGeneration(path string) int {
	data, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o644)
	return n
}

func runRespawnHelperServer(generation int) {
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
			writeRespawnError(nil, -32700, err.Error())
			continue
		}
		switch req.Method {
		case "initialize":
			writeRespawnResult(req.ID, map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "respawn-helper",
					"version": fmt.Sprintf("generation-%d", generation),
				},
			})
		case "notifications/initialized":
			continue
		case "tools/list":
			writeRespawnResult(req.ID, map[string]any{
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
			writeRespawnResult(req.ID, map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("generation %d", generation)},
				},
				"generation": generation,
			})
		case "ping":
			writeRespawnResult(req.ID, map[string]any{"generation": generation})
		default:
			writeRespawnError(req.ID, -32601, "method not found")
		}
	}
}

func writeRespawnResult(id json.RawMessage, result any) {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	fmt.Fprintln(os.Stdout, string(data))
}

func writeRespawnError(id json.RawMessage, code int, message string) {
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
