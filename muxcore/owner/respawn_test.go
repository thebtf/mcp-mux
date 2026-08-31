package owner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
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

func TestModernOwnerRespawnsAfterTerminalResponseWithoutLegacyTraffic(t *testing.T) {
	const terminalRequest = `{"jsonrpc":"2.0","id":101,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	const freshRequest = `{"jsonrpc":"2.0","id":102,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

	type observedFrame struct {
		generation int
		raw        string
	}

	var generations atomic.Int32
	var cachePublishes atomic.Int32
	started := make(chan int, 2)
	frames := make(chan observedFrame, 8)
	terminalResponseWritten := make(chan struct{}, 1)
	releaseGenerationOne := make(chan struct{})
	handler := func(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := int(generations.Add(1))
		select {
		case started <- generation:
		case <-ctx.Done():
			return ctx.Err()
		}

		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			raw := string(append([]byte(nil), scanner.Bytes()...))
			select {
			case frames <- observedFrame{generation: generation, raw: raw}:
			case <-ctx.Done():
				return ctx.Err()
			}

			var request struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal([]byte(raw), &request); err != nil {
				return err
			}
			if len(request.ID) == 0 {
				continue
			}
			if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"result":{"generation":%d}}`+"\n", request.ID, generation); err != nil {
				return err
			}
			if generation == 1 && string(request.ID) == "101" {
				select {
				case terminalResponseWritten <- struct{}{}:
				default:
				}
				select {
				case <-releaseGenerationOne:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwner(OwnerConfig{
		HandlerFunc: handler,
		IPCPath:     testIPCPath(t),
		ProtocolEra: era.EraModern20260728,
		OnCacheReady: func(*Owner, OwnerSnapshot) bool {
			cachePublishes.Add(1)
			return true
		},
		Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner(modern): %v", err)
	}
	t.Cleanup(o.Shutdown)
	t.Cleanup(func() {
		select {
		case <-releaseGenerationOne:
		default:
			close(releaseGenerationOne)
		}
	})

	select {
	case generation := <-started:
		if generation != 1 {
			t.Fatalf("initial upstream generation = %d, want 1", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("modern generation 1 did not start")
	}
	select {
	case frame := <-frames:
		t.Fatalf("modern generation 1 received legacy bootstrap traffic: %s", frame.raw)
	case <-time.After(100 * time.Millisecond):
	}

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	session := NewSession(serverR, serverW)
	o.AddSession(session)

	if _, err := fmt.Fprintln(clientW, terminalRequest); err != nil {
		t.Fatalf("write terminal host request: %v", err)
	}
	select {
	case frame := <-frames:
		if frame.generation != 1 || frame.raw != terminalRequest {
			t.Fatalf("generation 1 first upstream frame = %+v, want exact terminal host request", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("generation 1 did not receive terminal host request")
	}
	assertResponseID(t, readResp(t, clientR), 101)
	select {
	case <-terminalResponseWritten:
	case <-time.After(time.Second):
		t.Fatal("generation 1 did not complete the terminal host response")
	}
	select {
	case generation := <-started:
		t.Fatalf("generation %d started before generation 1 was allowed to exit", generation)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseGenerationOne)
	select {
	case generation := <-started:
		if generation != 2 {
			t.Fatalf("replacement upstream generation = %d, want 2", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("modern generation 2 did not start after generation 1 exited")
	}
	if o.protocolEra != era.EraModern20260728 {
		t.Fatalf("replacement owner era = %v, want %v", o.protocolEra, era.EraModern20260728)
	}
	select {
	case frame := <-frames:
		t.Fatalf("generation 2 received bootstrap, cache replay, or list-change traffic before host demand: %s", frame.raw)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := fmt.Fprintln(clientW, freshRequest); err != nil {
		t.Fatalf("write fresh generation 2 host request: %v", err)
	}
	select {
	case frame := <-frames:
		if frame.generation != 2 || frame.raw != freshRequest {
			t.Fatalf("generation 2 first upstream frame = %+v, want exact fresh host request", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("generation 2 did not receive fresh host request")
	}
	assertResponseID(t, readResp(t, clientR), 102)
	if got := generations.Load(); got != 2 {
		t.Fatalf("upstream generations = %d, want exactly 2", got)
	}
	if cached := o.getCachedResponse("tools/list"); cached != nil {
		t.Fatalf("modern respawn cached tools/list response: %s", cached)
	}
	if got := cachePublishes.Load(); got != 0 {
		t.Fatalf("modern respawn published legacy cache %d time(s)", got)
	}
}

func TestLifecycleConstructorsRejectUnsafeModernOrNativeBoundary(t *testing.T) {
	assertSnapshotRejected := func(t *testing.T, cfg OwnerConfig, snap OwnerSnapshot) {
		t.Helper()
		ipcPath := testIPCPath(t)
		listener, err := ipc.Listen(ipcPath)
		if err != nil {
			t.Fatalf("occupy lifecycle IPC path: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })

		cfg.IPCPath = ipcPath
		cfg.TokenHandshake = true
		cfg.Logger = testLogger(t)
		o, err := NewOwnerFromSnapshot(cfg, snap)
		if o != nil {
			o.Shutdown()
			t.Fatal("NewOwnerFromSnapshot returned an owner across an unsafe lifecycle boundary")
		}
		if !errors.Is(err, era.AdmissionUnsafeLifecycleBoundary) {
			t.Fatalf("NewOwnerFromSnapshot() error = %v, want AdmissionUnsafeLifecycleBoundary before listener or snapshot hydration", err)
		}
	}
	assertHandoffRejected := func(t *testing.T, cfg OwnerConfig, payload HandoffPayload) {
		t.Helper()
		cfg.IPCPath = testIPCPath(t)
		cfg.TokenHandshake = true
		cfg.Logger = testLogger(t)
		o, err := NewOwnerFromHandoff(cfg, payload)
		if o != nil {
			o.Shutdown()
			t.Fatal("NewOwnerFromHandoff returned an owner across an unsafe lifecycle boundary")
		}
		if !errors.Is(err, era.AdmissionUnsafeLifecycleBoundary) {
			t.Fatalf("NewOwnerFromHandoff() error = %v, want AdmissionUnsafeLifecycleBoundary before FD attach or token hydration", err)
		}
	}

	t.Run("snapshot", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cfg  OwnerConfig
			snap OwnerSnapshot
		}{
			{name: "modern-config-era", cfg: OwnerConfig{ProtocolEra: era.EraModern20260728, ServerID: "legacy-snapshot"}, snap: lifecycleBoundarySnapshot("legacy-snapshot")},
			{name: "native-config-id", cfg: OwnerConfig{ServerID: "native-config-snapshot"}, snap: lifecycleBoundarySnapshot("legacy-snapshot")},
			{name: "native-snapshot-id", cfg: OwnerConfig{ServerID: "legacy-snapshot"}, snap: lifecycleBoundarySnapshot("native-snapshot")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assertSnapshotRejected(t, tc.cfg, tc.snap)
			})
		}
	})

	t.Run("handoff", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			cfg     OwnerConfig
			payload HandoffPayload
		}{
			{name: "modern-config-era", cfg: OwnerConfig{ProtocolEra: era.EraModern20260728, ServerID: "legacy-handoff", AdoptedSnapshot: ptrLifecycleBoundarySnapshot("legacy-handoff")}, payload: HandoffPayload{PID: 0, ServerID: "legacy-handoff", Command: "must-not-attach"}},
			{name: "native-config-id", cfg: OwnerConfig{ServerID: "native-config-handoff", AdoptedSnapshot: ptrLifecycleBoundarySnapshot("legacy-handoff")}, payload: HandoffPayload{PID: 0, ServerID: "legacy-handoff", Command: "must-not-attach"}},
			{name: "native-payload-id", cfg: OwnerConfig{ServerID: "legacy-handoff", AdoptedSnapshot: ptrLifecycleBoundarySnapshot("legacy-handoff")}, payload: HandoffPayload{PID: 0, ServerID: "native-payload-handoff", Command: "must-not-attach"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assertHandoffRejected(t, tc.cfg, tc.payload)
			})
		}
	})
}

func TestLegacyLifecycleConstructorsRemainAdmissible(t *testing.T) {
	t.Run("snapshot", func(t *testing.T) {
		snap := lifecycleBoundarySnapshot("legacy-snapshot-control")
		o, err := NewOwnerFromSnapshot(OwnerConfig{
			IPCPath:        testIPCPath(t),
			ServerID:       snap.ServerID,
			TokenHandshake: true,
			Logger:         testLogger(t),
		}, snap)
		if err != nil {
			t.Fatalf("NewOwnerFromSnapshot(legacy): %v", err)
		}
		t.Cleanup(o.Shutdown)
		if o.protocolEra != era.EraLegacy {
			t.Fatalf("legacy snapshot owner era = %v, want %v", o.protocolEra, era.EraLegacy)
		}
		if cached := o.getCachedResponse("initialize"); cached == nil {
			t.Fatal("legacy snapshot constructor did not preserve cached initialize response")
		}
		if history := o.SessionMgr().ExportBoundHistory(); len(history) != 1 || history[0].Token != "lifecycle-bound-token" {
			t.Fatalf("legacy snapshot constructor bound-token history = %+v, want lifecycle-bound-token", history)
		}
	})

	t.Run("handoff", func(t *testing.T) {
		generationFile := t.TempDir() + string(os.PathSeparator) + "legacy-handoff-generation.txt"
		cmd, args, env := respawnHelperCommand(generationFile)
		predecessor, err := NewOwner(OwnerConfig{
			Command:  cmd,
			Args:     args,
			Env:      env,
			IPCPath:  testIPCPath(t),
			ServerID: "legacy-handoff-control",
			Logger:   testLogger(t),
		})
		if err != nil {
			t.Fatalf("NewOwner(predecessor): %v", err)
		}
		t.Cleanup(predecessor.Shutdown)
		waitForCondition(t, time.Second, func() bool {
			return predecessor.MaterializationState() == MaterializationReady
		}, "legacy handoff predecessor did not become ready")

		payload, err := predecessor.ShutdownForHandoff()
		if err != nil {
			t.Fatalf("ShutdownForHandoff(): %v", err)
		}
		t.Cleanup(func() { _ = payload.Abort() })
		adopted := lifecycleBoundarySnapshot("legacy-handoff-control")
		successor, err := NewOwnerFromHandoff(OwnerConfig{
			IPCPath:         testIPCPath(t),
			ServerID:        adopted.ServerID,
			TokenHandshake:  true,
			AdoptedSnapshot: &adopted,
			Logger:          testLogger(t),
		}, payload)
		if err != nil {
			t.Fatalf("NewOwnerFromHandoff(legacy): %v", err)
		}
		t.Cleanup(successor.Shutdown)
		if successor.protocolEra != era.EraLegacy {
			t.Fatalf("legacy handoff owner era = %v, want %v", successor.protocolEra, era.EraLegacy)
		}
		if history := successor.SessionMgr().ExportBoundHistory(); len(history) != 1 || history[0].Token != "lifecycle-bound-token" {
			t.Fatalf("legacy handoff constructor bound-token history = %+v, want lifecycle-bound-token", history)
		}
		if err := payload.Commit(); err != nil {
			t.Fatalf("legacy handoff payload.Commit(): %v", err)
		}
	})
}

func lifecycleBoundarySnapshot(serverID string) OwnerSnapshot {
	now := time.Now()
	return OwnerSnapshot{
		ServerID:    serverID,
		CachedInit:  base64Encode([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"legacy-lifecycle","version":"1"}}}`)),
		CachedTools: base64Encode([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`)),
		BoundTokens: []mcpsnapshot.BoundTokenSnapshot{{
			Token:    "lifecycle-bound-token",
			OwnerKey: serverID,
			Cwd:      "/legacy/lifecycle",
			BoundAt:  now,
			LastUsed: now,
		}},
	}
}

func ptrLifecycleBoundarySnapshot(serverID string) *OwnerSnapshot {
	snap := lifecycleBoundarySnapshot(serverID)
	return &snap
}

func TestSnapshotBackgroundSpawnBlocksRequestRespawn(t *testing.T) {
	var starts atomic.Int32
	allowDiscovery := make(chan struct{})
	release := make(chan struct{})
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"background-gate","version":"1"}}}`+"\n", req.ID); err != nil {
					return err
				}
			case "tools/list":
				<-allowDiscovery
				if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`+"\n", req.ID); err != nil {
					return err
				}
				<-release
				return nil
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, OwnerSnapshot{})
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot() error: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		o.Shutdown()
	})

	o.SpawnUpstreamBackground()
	waitForCondition(t, time.Second, func() bool { return starts.Load() == 1 }, "background upstream did not start")
	ready := make(chan error, 1)
	go func() { ready <- o.ensureUpstreamReadyForRequest() }()

	time.Sleep(100 * time.Millisecond)
	if got := starts.Load(); got != 1 {
		t.Fatalf("concurrent request started %d upstream generations, want 1", got)
	}
	close(allowDiscovery)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("ensureUpstreamReadyForRequest() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not join background materialization")
	}
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
