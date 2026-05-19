package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	envCtlPath   = "CURRENT_TOPOLOGY_POC_CTL"
	envRuntime   = "CURRENT_TOPOLOGY_POC_HOME"
	envSuccessor = "CURRENT_TOPOLOGY_POC_SUCCESSOR"

	daemonFlag = "--muxcore-daemon"
	serverName = "current-topology-poc"
)

type controlRequest struct {
	Cmd             string            `json:"cmd"`
	ServerID        string            `json:"server_id,omitempty"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	Mode            string            `json:"mode,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	PrevToken       string            `json:"prev_token,omitempty"`
	ReconnectReason string            `json:"reconnect_reason,omitempty"`
}

type ticket struct {
	Token            string
	ServerID         string
	DaemonGeneration string
	OwnerGeneration  string
	ExpiresAt        time.Time
}

type tokenHistoryEntry struct {
	Token     string    `json:"token"`
	ServerID  string    `json:"server_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ownerEntry struct {
	ServerID    string
	Command     string
	Args        []string
	Cwd         string
	Mode        string
	Generation  string
	Socket      string
	Listener    net.Listener
	LastSession time.Time
	NextSession int

	activeMu           sync.Mutex
	ActiveCalls        int
	MaxConcurrentCalls int
}

type ownerSnapshot struct {
	ServerID string   `json:"server_id"`
	Command  string   `json:"command"`
	Args     []string `json:"args,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Mode     string   `json:"mode"`
}

type daemonSnapshot struct {
	Owners       []ownerSnapshot     `json:"owners"`
	TokenHistory []tokenHistoryEntry `json:"token_history,omitempty"`
}

type daemonState struct {
	mu sync.RWMutex

	pid              int
	daemonGeneration string
	ready            bool
	startedAt        time.Time

	owners       map[string]*ownerEntry
	tickets      map[string]ticket
	tokenHistory map[string]tokenHistoryEntry

	zombieDetectedSpawn  int
	fallbackSpawns       int
	refreshRequests      int
	refreshSuccesses     int
	lastReconnectReason  string
	lastRefreshPrevToken string
	lastRefreshNewToken  string
}

type ownerHello struct {
	Token            string `json:"token"`
	DaemonGeneration string `json:"daemon_generation"`
	OwnerGeneration  string `json:"owner_generation"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type probeRPCClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	nextID  int
}

type ownerConnectInfo struct {
	ServerID         string
	OwnerSocket      string
	Token            string
	DaemonGeneration string
	OwnerGeneration  string
	ReconnectReason  string
}

type shimOwnerConn struct {
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan ownerResponse
	closeOnce sync.Once
	info      ownerConnectInfo
}

type ownerResponse struct {
	data []byte
	err  error
}

type shimState struct {
	mu               sync.Mutex
	ownerConn        *shimOwnerConn
	lastOwner        ownerConnectInfo
	cachedInitialize []byte
	stdoutMu         sync.Mutex
}

func main() {
	runDaemonFlag := flag.Bool("muxcore-daemon", false, "run the dummy daemon authority")
	pocControl := flag.String("poc-control", "", "send a control command to the dummy daemon")
	pocProbeStaleToken := flag.Bool("poc-probe-stale-token", false, "verify owner rejects a stale generation token")
	pocProbeOwnerRegistry := flag.Bool("poc-probe-owner-registry", false, "verify cwd/global/isolated owner registry semantics")
	pocProbeZombieOwner := flag.Bool("poc-probe-zombie-owner", false, "verify spawn replaces a registered owner with a dead listener")
	pocProbeLiveReconnect := flag.Bool("poc-probe-live-reconnect", false, "verify one stdio shim survives daemon graceful restart")
	pocProbeInflightReconnect := flag.Bool("poc-probe-inflight-reconnect", false, "verify in-flight and buffered requests survive daemon graceful restart")
	pocProbeConcurrentDemux := flag.Bool("poc-probe-concurrent-demux", false, "verify concurrent out-of-order responses demux by ID across daemon graceful restart")
	pocProbeRefreshReconnect := flag.Bool("poc-probe-refresh-reconnect", false, "verify reconnect uses refresh-token instead of fallback spawn")
	flag.Parse()

	switch {
	case *runDaemonFlag:
		if err := runDaemon(context.Background()); err != nil {
			logf("daemon failed: %v", err)
			os.Exit(1)
		}
	case *pocControl != "":
		resp, err := sendControl(controlPath(), *pocControl, nil, 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "control %s failed: %v\n", *pocControl, err)
			os.Exit(1)
		}
		_ = writeJSONLine(os.Stdout, resp)
	case *pocProbeStaleToken:
		if err := probeStaleToken(); err != nil {
			fmt.Fprintf(os.Stderr, "stale-token probe failed: %v\n", err)
			os.Exit(1)
		}
	case *pocProbeOwnerRegistry:
		if err := probeOwnerRegistry(); err != nil {
			fmt.Fprintf(os.Stderr, "owner-registry probe failed: %v\n", err)
			os.Exit(1)
		}
	case *pocProbeZombieOwner:
		if err := probeZombieOwner(); err != nil {
			fmt.Fprintf(os.Stderr, "zombie-owner probe failed: %v\n", err)
			os.Exit(1)
		}
	case *pocProbeLiveReconnect:
		if err := probeLiveReconnect(); err != nil {
			fmt.Fprintf(os.Stderr, "live-reconnect probe failed: %v\n", err)
			os.Exit(1)
		}
	case *pocProbeInflightReconnect:
		if err := probeInflightReconnect(); err != nil {
			fmt.Fprintf(os.Stderr, "inflight-reconnect probe failed: %v\n", err)
			os.Exit(1)
		}
	case *pocProbeConcurrentDemux:
		if err := probeConcurrentDemux(); err != nil {
			fmt.Fprintf(os.Stderr, "concurrent-demux probe failed: %v\n", err)
			os.Exit(1)
		}
	case *pocProbeRefreshReconnect:
		if err := probeRefreshReconnect(); err != nil {
			fmt.Fprintf(os.Stderr, "refresh-reconnect probe failed: %v\n", err)
			os.Exit(1)
		}
	default:
		if err := runShim(context.Background()); err != nil {
			logf("shim failed: %v", err)
			os.Exit(1)
		}
	}
}

func runDaemon(ctx context.Context) error {
	ctl := controlPath()
	if err := os.MkdirAll(filepath.Dir(ctl), 0o700); err != nil {
		return err
	}

	wait := 0 * time.Second
	if os.Getenv(envSuccessor) == "1" {
		wait = 10 * time.Second
	}
	controlLn, err := listenUnixExclusive(ctl, wait)
	if err != nil {
		return fmt.Errorf("listen control: %w", err)
	}
	defer cleanupListener(controlLn, ctl)

	state := &daemonState{
		pid:              os.Getpid(),
		daemonGeneration: newGeneration("daemon"),
		ready:            false,
		startedAt:        time.Now(),
		owners:           make(map[string]*ownerEntry),
		tickets:          make(map[string]ticket),
		tokenHistory:     make(map[string]tokenHistoryEntry),
	}
	defer state.closeOwners()

	shutdown := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { close(shutdown) })
	}

	go acceptControl(controlLn, state, stop)
	if os.Getenv(envSuccessor) == "1" {
		if err := state.restoreSnapshot(); err != nil {
			return fmt.Errorf("restore snapshot: %w", err)
		}
	}
	state.setReady()

	logf("daemon ready pid=%d daemon_generation=%s ctl=%s", state.pid, state.daemonGeneration, ctl)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-shutdown:
		logf("daemon shutdown pid=%d daemon_generation=%s", state.pid, state.daemonGeneration)
		return nil
	}
}

func acceptControl(ln net.Listener, state *daemonState, stop func()) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleControl(conn, state, stop)
	}
}

func handleControl(conn net.Conn, state *daemonState, stop func()) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	if !scanner.Scan() {
		return
	}

	var req controlRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		_ = writeJSONLine(conn, map[string]any{"ok": false, "error": "bad_control_json", "message": err.Error()})
		return
	}

	var after func()
	var resp map[string]any
	switch req.Cmd {
	case "ping":
		resp = map[string]any{"ok": true, "message": "pong"}
	case "status":
		resp = state.status()
	case "spawn":
		resp = state.spawn(req)
	case "refresh-token":
		resp = state.refreshToken(req)
	case "poison-owner":
		resp = state.poisonOwner(req)
	case "shutdown":
		resp = map[string]any{"ok": true, "message": "shutdown scheduled"}
		after = func() {
			time.Sleep(100 * time.Millisecond)
			stop()
		}
	case "graceful-restart":
		if err := state.writeSnapshot(); err != nil {
			resp = map[string]any{"ok": false, "message": "snapshot write failed", "error": err.Error()}
			break
		}
		if err := startSuccessor(); err != nil {
			resp = map[string]any{"ok": false, "message": "successor spawn failed", "error": err.Error()}
			break
		}
		resp = map[string]any{
			"ok":      true,
			"message": "successor spawned; current daemon will release control socket",
			"handoff": "process_restart_without_upstream_preservation",
		}
		after = func() {
			time.Sleep(100 * time.Millisecond)
			stop()
		}
	default:
		resp = map[string]any{"ok": false, "error": "unknown_control_command", "cmd": req.Cmd}
	}

	_ = writeJSONLine(conn, resp)
	if after != nil {
		go after()
	}
}

func (s *daemonState) status() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owners := make([]map[string]any, 0, len(s.owners))
	for _, entry := range s.owners {
		owners = append(owners, ownerStatus(entry))
	}
	var ownerGeneration, ownerSocket string
	if len(owners) > 0 {
		ownerGeneration, _ = owners[0]["owner_generation"].(string)
		ownerSocket, _ = owners[0]["owner_socket"].(string)
	}
	return map[string]any{
		"ok":                      true,
		"pid":                     s.pid,
		"daemon_pid":              s.pid,
		"ready":                   s.ready,
		"daemon_generation":       s.daemonGeneration,
		"owner_generation":        ownerGeneration,
		"owner_socket":            ownerSocket,
		"owner_count":             len(s.owners),
		"owners":                  owners,
		"pending_tickets":         len(s.tickets),
		"token_history_size":      len(s.tokenHistory),
		"zombie_detected":         s.zombieDetectedSpawn,
		"fallback_spawns":         s.fallbackSpawns,
		"refresh_requests":        s.refreshRequests,
		"refresh_successes":       s.refreshSuccesses,
		"last_reconnect_reason":   s.lastReconnectReason,
		"last_refresh_prev_token": s.lastRefreshPrevToken,
		"last_refresh_new_token":  s.lastRefreshNewToken,
		"uptime_ms":               time.Since(s.startedAt).Milliseconds(),
		"handoff":                 "none",
	}
}

func (s *daemonState) setReady() {
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
}

func ownerStatus(entry *ownerEntry) map[string]any {
	entry.activeMu.Lock()
	activeCalls := entry.ActiveCalls
	maxConcurrentCalls := entry.MaxConcurrentCalls
	entry.activeMu.Unlock()
	return map[string]any{
		"server_id":            entry.ServerID,
		"command":              entry.Command,
		"args":                 append([]string(nil), entry.Args...),
		"cwd":                  entry.Cwd,
		"mode":                 entry.Mode,
		"owner_generation":     entry.Generation,
		"owner_socket":         entry.Socket,
		"last_session_ms":      time.Since(entry.LastSession).Milliseconds(),
		"active_calls":         activeCalls,
		"max_concurrent_calls": maxConcurrentCalls,
	}
}

func (s *daemonState) spawn(req controlRequest) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return map[string]any{"ok": false, "error": "daemon_not_ready"}
	}

	normalized := normalizeSpawnRequest(req)
	if req.ReconnectReason == "fallback_spawn" {
		s.fallbackSpawns++
		s.lastReconnectReason = req.ReconnectReason
	}
	sid := serverID(normalized)
	entry, existed := s.owners[sid]
	if existed && !activeUnix(entry.Socket, 200*time.Millisecond) {
		s.zombieDetectedSpawn++
		cleanupListener(entry.Listener, entry.Socket)
		delete(s.owners, sid)
		entry = nil
		existed = false
	}
	if !existed {
		var err error
		entry, err = s.startOwnerLocked(sid, normalized)
		if err != nil {
			return map[string]any{"ok": false, "error": "owner_start_failed", "message": err.Error()}
		}
	}
	entry.LastSession = time.Now()

	token := randomToken()
	t := ticket{
		Token:            token,
		ServerID:         entry.ServerID,
		DaemonGeneration: s.daemonGeneration,
		OwnerGeneration:  entry.Generation,
		ExpiresAt:        time.Now().Add(30 * time.Second),
	}
	s.tickets[token] = t

	return map[string]any{
		"ok":                true,
		"new_owner":         !existed,
		"server_id":         entry.ServerID,
		"daemon_generation": s.daemonGeneration,
		"owner_generation":  entry.Generation,
		"owner_socket":      entry.Socket,
		"token":             token,
		"expires_unix_ms":   t.ExpiresAt.UnixMilli(),
	}
}

func (s *daemonState) refreshToken(req controlRequest) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refreshRequests++
	prevToken := strings.TrimSpace(req.PrevToken)
	if prevToken == "" {
		return map[string]any{"ok": false, "error": "missing_prev_token"}
	}
	history, ok := s.tokenHistory[prevToken]
	if !ok {
		return map[string]any{"ok": false, "error": "unknown_token"}
	}
	now := time.Now()
	if now.After(history.ExpiresAt) {
		delete(s.tokenHistory, prevToken)
		return map[string]any{"ok": false, "error": "expired_token"}
	}
	entry, ok := s.owners[history.ServerID]
	if !ok {
		return map[string]any{"ok": false, "error": "owner_gone", "server_id": history.ServerID}
	}
	if !activeUnix(entry.Socket, 200*time.Millisecond) {
		return map[string]any{"ok": false, "error": "owner_gone", "server_id": history.ServerID}
	}

	token := randomToken()
	t := ticket{
		Token:            token,
		ServerID:         entry.ServerID,
		DaemonGeneration: s.daemonGeneration,
		OwnerGeneration:  entry.Generation,
		ExpiresAt:        now.Add(30 * time.Second),
	}
	s.tickets[token] = t
	s.refreshSuccesses++
	s.lastReconnectReason = "refresh_token"
	s.lastRefreshPrevToken = prevToken
	s.lastRefreshNewToken = token

	return map[string]any{
		"ok":                true,
		"server_id":         entry.ServerID,
		"daemon_generation": s.daemonGeneration,
		"owner_generation":  entry.Generation,
		"owner_socket":      entry.Socket,
		"token":             token,
		"prev_token":        prevToken,
		"expires_unix_ms":   t.ExpiresAt.UnixMilli(),
		"refresh_used":      true,
	}
}

func (s *daemonState) poisonOwner(req controlRequest) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	sid := req.ServerID
	if sid == "" {
		sid = serverID(normalizeSpawnRequest(req))
	}
	entry, ok := s.owners[sid]
	if !ok {
		return map[string]any{"ok": false, "error": "owner_not_found", "server_id": sid}
	}
	_ = entry.Listener.Close()
	return map[string]any{
		"ok":               true,
		"server_id":        entry.ServerID,
		"owner_generation": entry.Generation,
		"poisoned":         true,
	}
}

func (s *daemonState) startOwnerLocked(sid string, req controlRequest) (*ownerEntry, error) {
	ownerGen := newGeneration("owner")
	ownerSock := ownerPath(sid, ownerGen)
	ownerLn, err := listenUnixExclusive(ownerSock, 0)
	if err != nil {
		return nil, err
	}
	entry := &ownerEntry{
		ServerID:    sid,
		Command:     req.Command,
		Args:        append([]string(nil), req.Args...),
		Cwd:         req.Cwd,
		Mode:        normalizeMode(req.Mode),
		Generation:  ownerGen,
		Socket:      ownerSock,
		Listener:    ownerLn,
		LastSession: time.Now(),
	}
	s.owners[sid] = entry
	go acceptOwner(ownerLn, s, entry)
	return entry, nil
}

func (s *daemonState) consumeTicket(h ownerHello, entry *ownerEntry) (int, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tickets[h.Token]
	if !ok {
		return 0, "unknown_token", false
	}
	delete(s.tickets, h.Token)

	now := time.Now()
	switch {
	case now.After(t.ExpiresAt):
		return 0, "expired_token", false
	case t.ServerID != entry.ServerID:
		return 0, "server_id_mismatch", false
	case h.DaemonGeneration != t.DaemonGeneration:
		return 0, "daemon_generation_mismatch", false
	case h.OwnerGeneration != t.OwnerGeneration:
		return 0, "owner_generation_mismatch", false
	case h.DaemonGeneration != s.daemonGeneration || h.OwnerGeneration != entry.Generation:
		return 0, "current_generation_mismatch", false
	}

	s.tokenHistory[t.Token] = tokenHistoryEntry{
		Token:     t.Token,
		ServerID:  t.ServerID,
		ExpiresAt: now.Add(30 * time.Minute),
	}
	entry.NextSession++
	return entry.NextSession, "", true
}

func acceptOwner(ln net.Listener, state *daemonState, entry *ownerEntry) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleOwnerSession(conn, state, entry)
	}
}

func handleOwnerSession(conn net.Conn, state *daemonState, entry *ownerEntry) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var hello ownerHello
	if err := json.Unmarshal(line, &hello); err != nil {
		_ = writeJSONLine(conn, map[string]any{"ok": false, "error": "bad_owner_hello", "message": err.Error()})
		return
	}

	sessionID, rejectReason, ok := state.consumeTicket(hello, entry)
	if !ok {
		_ = writeJSONLine(conn, map[string]any{"ok": false, "error": rejectReason})
		logf("owner rejected session token reason=%s daemon_generation=%s owner_generation=%s",
			rejectReason, hello.DaemonGeneration, hello.OwnerGeneration)
		return
	}
	if err := writeJSONLine(conn, map[string]any{"ok": true, "session_id": sessionID}); err != nil {
		return
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			raw := append([]byte{}, line...)
			go handleRPCLine(conn, state, entry, sessionID, raw)
		}
		if err != nil {
			return
		}
	}
}

func handleRPCLine(w io.Writer, state *daemonState, entry *ownerEntry, sessionID int, line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if len(msg.ID) == 0 {
		return
	}

	switch msg.Method {
	case "initialize":
		_ = writeRPCResult(w, msg.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]any{
				"name":    serverName,
				"version": state.versionString(entry),
			},
			"capabilities": map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{},
			},
		})
	case "tools/list":
		_ = writeRPCResult(w, msg.ID, map[string]any{
			"tools": []map[string]any{
				{
					"name":        "topology_state",
					"description": "Return daemon and owner generation state for the current dummy session.",
					"inputSchema": map[string]any{
						"type":                 "object",
						"additionalProperties": true,
					},
				},
			},
		})
	case "tools/call":
		handleToolCall(w, state, entry, sessionID, msg)
	case "resources/read":
		_ = writeRPCResult(w, msg.ID, map[string]any{
			"contents": []map[string]any{
				{
					"uri":      "current-topology-poc://state",
					"mimeType": "application/json",
					"text":     mustJSON(statePayload(state, entry, sessionID)),
				},
			},
		})
	default:
		_ = writeRPCError(w, msg.ID, -32601, "method not found")
	}
}

func handleToolCall(w io.Writer, state *daemonState, entry *ownerEntry, sessionID int, msg rpcMessage) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	if params.Name != "topology_state" {
		_ = writeRPCResult(w, msg.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "unknown tool"}},
			"isError": true,
		})
		return
	}

	sleepMS := intArg(params.Arguments, "sleep_ms")
	if sleepMS < 0 {
		sleepMS = 0
	}
	if sleepMS > 5000 {
		sleepMS = 5000
	}
	entry.beginToolCall()
	defer entry.endToolCall()
	if sleepMS > 0 {
		time.Sleep(time.Duration(sleepMS) * time.Millisecond)
	}

	payload := statePayload(state, entry, sessionID)
	payload["delay_ms"] = sleepMS
	if tag, ok := params.Arguments["tag"].(string); ok {
		payload["tag"] = tag
	}
	_ = writeRPCResult(w, msg.ID, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": mustJSON(payload)},
		},
		"isError": false,
	})
}

func (entry *ownerEntry) beginToolCall() {
	entry.activeMu.Lock()
	defer entry.activeMu.Unlock()
	entry.ActiveCalls++
	if entry.ActiveCalls > entry.MaxConcurrentCalls {
		entry.MaxConcurrentCalls = entry.ActiveCalls
	}
}

func (entry *ownerEntry) endToolCall() {
	entry.activeMu.Lock()
	defer entry.activeMu.Unlock()
	if entry.ActiveCalls > 0 {
		entry.ActiveCalls--
	}
}

func statePayload(state *daemonState, entry *ownerEntry, sessionID int) map[string]any {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return map[string]any{
		"pid":               state.pid,
		"daemon_generation": state.daemonGeneration,
		"server_id":         entry.ServerID,
		"owner_generation":  entry.Generation,
		"owner_socket":      entry.Socket,
		"mode":              entry.Mode,
		"cwd":               entry.Cwd,
		"ready":             state.ready,
		"session_id":        sessionID,
		"runtime":           runtime.GOOS,
	}
}

func (s *daemonState) versionString(entry *ownerEntry) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.daemonGeneration + "/" + entry.Generation
}

func runShim(ctx context.Context) error {
	_ = ctx
	state := &shimState{}
	if _, err := state.ensureOwner(); err != nil {
		return err
	}
	defer state.close()

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 1<<20), 1<<20)
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for stdin.Scan() {
		raw := append([]byte{}, stdin.Bytes()...)
		msg, isRequest := parseRPCRequest(raw)
		if isRequest && msg.Method == "initialize" {
			state.cacheInitialize(raw)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := state.handleShimMessage(raw, msg, isRequest); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()

		select {
		case err := <-errCh:
			return err
		default:
		}
	}
	if err := stdin.Err(); err != nil {
		return err
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
	}
	return nil
}

func (s *shimState) cacheInitialize(raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedInitialize == nil {
		s.cachedInitialize = append([]byte{}, raw...)
	}
}

func (s *shimState) handleShimMessage(raw []byte, msg rpcMessage, isRequest bool) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ownerConn, err := s.ensureOwner()
		if err != nil {
			lastErr = err
			continue
		}

		if !isRequest {
			if err := ownerConn.writeNotification(raw); err != nil {
				s.invalidateOwner(ownerConn)
				lastErr = err
				continue
			}
			return nil
		}

		resp, err := ownerConn.roundTrip(raw, msg.ID)
		if err != nil {
			s.invalidateOwner(ownerConn)
			lastErr = err
			continue
		}
		s.stdoutMu.Lock()
		err = writeRawLine(os.Stdout, resp)
		s.stdoutMu.Unlock()
		if err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("shim request %s failed after reconnect retry: %w", msg.Method, lastErr)
}

func (s *shimState) ensureOwner() (*shimOwnerConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownerConn != nil {
		return s.ownerConn, nil
	}
	var refreshErr error
	if s.lastOwner.Token != "" {
		ownerConn, err := connectShimOwnerWithRefresh(s.lastOwner.Token)
		if err == nil {
			if s.cachedInitialize != nil {
				if err := ownerConn.warmInitialize(s.cachedInitialize); err != nil {
					ownerConn.close()
					refreshErr = err
				} else {
					s.ownerConn = ownerConn
					s.lastOwner = ownerConn.info
					return ownerConn, nil
				}
			} else {
				s.ownerConn = ownerConn
				s.lastOwner = ownerConn.info
				return ownerConn, nil
			}
		} else {
			refreshErr = err
		}
	}
	reconnectReason := "initial_spawn"
	if s.lastOwner.Token != "" {
		reconnectReason = "fallback_spawn"
	}
	ownerConn, err := connectShimOwner(reconnectReason)
	if err != nil {
		if refreshErr != nil {
			return nil, fmt.Errorf("%w; refresh-token failed: %v", err, refreshErr)
		}
		return nil, err
	}
	if s.cachedInitialize != nil {
		if err := ownerConn.warmInitialize(s.cachedInitialize); err != nil {
			ownerConn.close()
			return nil, err
		}
	}
	s.ownerConn = ownerConn
	s.lastOwner = ownerConn.info
	return ownerConn, nil
}

func (s *shimState) invalidateOwner(ownerConn *shimOwnerConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownerConn == ownerConn {
		s.ownerConn.close()
		s.ownerConn = nil
	}
}

func (s *shimState) close() {
	s.mu.Lock()
	ownerConn := s.ownerConn
	s.ownerConn = nil
	s.mu.Unlock()
	if ownerConn != nil {
		ownerConn.close()
	}
}

func connectShimOwner(reconnectReason string) (*shimOwnerConn, error) {
	if err := ensureDaemonReady(); err != nil {
		return nil, err
	}

	extra := shimSpawnExtra()
	if reconnectReason != "" {
		extra["reconnect_reason"] = reconnectReason
	}
	spawnResp, err := sendControl(controlPath(), "spawn", extra, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	if ok, _ := spawnResp["ok"].(bool); !ok {
		return nil, fmt.Errorf("spawn rejected: %v", spawnResp)
	}

	return connectOwnerFromControlResponse(spawnResp, reconnectReason)
}

func connectShimOwnerWithRefresh(prevToken string) (*shimOwnerConn, error) {
	if err := ensureDaemonReady(); err != nil {
		return nil, err
	}
	resp, err := sendControl(controlPath(), "refresh-token", map[string]any{"prev_token": prevToken}, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("refresh-token: %w", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		return nil, fmt.Errorf("refresh-token rejected: %v", resp)
	}
	return connectOwnerFromControlResponse(resp, "refresh_token")
}

func connectOwnerFromControlResponse(resp map[string]any, reconnectReason string) (*shimOwnerConn, error) {
	ownerSocket, _ := resp["owner_socket"].(string)
	token, _ := resp["token"].(string)
	daemonGen, _ := resp["daemon_generation"].(string)
	ownerGen, _ := resp["owner_generation"].(string)
	serverID, _ := resp["server_id"].(string)
	if ownerSocket == "" || token == "" || daemonGen == "" || ownerGen == "" || serverID == "" {
		return nil, fmt.Errorf("incomplete owner connection response: %v", resp)
	}

	conn, err := net.DialTimeout("unix", ownerSocket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial owner: %w", err)
	}

	reader := bufio.NewReader(conn)
	hello := ownerHello{Token: token, DaemonGeneration: daemonGen, OwnerGeneration: ownerGen}
	if err := writeJSONLine(conn, hello); err != nil {
		_ = conn.Close()
		return nil, err
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("owner auth read: %w", err)
	}
	var auth map[string]any
	if err := json.Unmarshal(line, &auth); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("owner auth decode: %w", err)
	}
	if ok, _ := auth["ok"].(bool); !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("owner auth rejected: %v", auth)
	}

	ownerConn := &shimOwnerConn{
		conn:    conn,
		reader:  reader,
		pending: make(map[string]chan ownerResponse),
		info: ownerConnectInfo{
			ServerID:         serverID,
			OwnerSocket:      ownerSocket,
			Token:            token,
			DaemonGeneration: daemonGen,
			OwnerGeneration:  ownerGen,
			ReconnectReason:  reconnectReason,
		},
	}
	go ownerConn.readLoop()
	return ownerConn, nil
}

func (c *shimOwnerConn) close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
		c.failPending(io.ErrClosedPipe)
	})
}

func (c *shimOwnerConn) warmInitialize(raw []byte) error {
	msg, ok := parseRPCRequest(raw)
	if !ok {
		return nil
	}
	_, err := c.roundTrip(raw, msg.ID)
	return err
}

func (c *shimOwnerConn) roundTrip(raw []byte, wantID json.RawMessage) ([]byte, error) {
	key := string(wantID)
	ch := make(chan ownerResponse, 1)
	c.pendingMu.Lock()
	c.pending[key] = ch
	c.pendingMu.Unlock()

	c.writeMu.Lock()
	err := writeRawLine(c.conn, raw)
	c.writeMu.Unlock()
	if err != nil {
		c.deletePending(key)
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp.data, resp.err
	case <-time.After(10 * time.Second):
		c.deletePending(key)
		return nil, fmt.Errorf("timeout waiting for owner response id %s", key)
	}
}

func (c *shimOwnerConn) writeNotification(raw []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeRawLine(c.conn, raw)
}

func (c *shimOwnerConn) readLoop() {
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			c.failPending(err)
			return
		}
		msg, ok := parseRPCRequest(bytesTrimSpace(line))
		if !ok || len(msg.ID) == 0 {
			continue
		}
		key := string(msg.ID)
		c.pendingMu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.pendingMu.Unlock()
		if ch != nil {
			ch <- ownerResponse{data: bytesTrimNewline(line)}
			close(ch)
		}
	}
}

func (c *shimOwnerConn) deletePending(key string) {
	c.pendingMu.Lock()
	delete(c.pending, key)
	c.pendingMu.Unlock()
}

func (c *shimOwnerConn) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan ownerResponse)
	c.pendingMu.Unlock()
	for _, ch := range pending {
		ch <- ownerResponse{err: err}
		close(ch)
	}
}

func parseRPCRequest(raw []byte) (rpcMessage, bool) {
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return msg, false
	}
	return msg, len(msg.ID) > 0
}

func writeRawLine(w io.Writer, raw []byte) error {
	data := append([]byte{}, raw...)
	data = append(bytesTrimNewline(data), '\n')
	_, err := w.Write(data)
	return err
}

func bytesTrimNewline(data []byte) []byte {
	for len(data) > 0 && (data[len(data)-1] == '\n' || data[len(data)-1] == '\r') {
		data = data[:len(data)-1]
	}
	return data
}

func bytesTrimSpace(data []byte) []byte {
	return []byte(strings.TrimSpace(string(data)))
}

func ensureDaemonReady() error {
	deadline := time.Now().Add(10 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		status, err := sendControl(controlPath(), "status", nil, 500*time.Millisecond)
		if err == nil {
			if ready, _ := status["ready"].(bool); ready {
				return nil
			}
		}

		if !started {
			if err := startDaemonProcess(false); err != nil {
				return err
			}
			started = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready before timeout")
}

func startSuccessor() error {
	return startDaemonProcess(true)
}

func startDaemonProcess(successor bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, daemonFlag)
	cmd.Env = os.Environ()
	if successor {
		cmd.Env = append(cmd.Env, envSuccessor+"=1")
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	logf("started daemon process pid=%d successor=%v", cmd.Process.Pid, successor)
	return nil
}

func sendControl(path, cmd string, extra map[string]any, timeout time.Duration) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	req := map[string]any{"cmd": cmd}
	for k, v := range extra {
		req[k] = v
	}
	if err := writeJSONLine(conn, req); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	if !scanner.Scan() {
		if scanner.Err() != nil {
			return nil, scanner.Err()
		}
		return nil, io.ErrUnexpectedEOF
	}
	var resp map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func shimSpawnExtra() map[string]any {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return map[string]any{
		"command": os.Args[0],
		"args":    append([]string(nil), flag.Args()...),
		"cwd":     cwd,
		"mode":    "cwd",
	}
}

func probeStaleToken() error {
	if err := ensureDaemonReady(); err != nil {
		return err
	}
	spawnResp, err := sendControl(controlPath(), "spawn", map[string]any{
		"command": "probe-stale-token",
		"cwd":     "probe-workspace",
		"mode":    "cwd",
	}, 5*time.Second)
	if err != nil {
		return err
	}

	ownerSocket, _ := spawnResp["owner_socket"].(string)
	token, _ := spawnResp["token"].(string)
	daemonGen, _ := spawnResp["daemon_generation"].(string)
	ownerGen, _ := spawnResp["owner_generation"].(string)

	conn, err := net.DialTimeout("unix", ownerSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	badHello := ownerHello{Token: token, DaemonGeneration: daemonGen, OwnerGeneration: "stale-" + ownerGen}
	if err := writeJSONLine(conn, badHello); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return fmt.Errorf("no owner rejection response")
	}
	var resp map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return err
	}
	if ok, _ := resp["ok"].(bool); ok {
		return fmt.Errorf("stale token unexpectedly accepted: %v", resp)
	}
	if resp["error"] != "owner_generation_mismatch" {
		return fmt.Errorf("unexpected stale token error: %v", resp)
	}
	return writeJSONLine(os.Stdout, map[string]any{"ok": true, "probe": "stale_token_rejected", "owner_response": resp})
}

func probeOwnerRegistry() error {
	if err := ensureDaemonReady(); err != nil {
		return err
	}

	spawn := func(command, cwd, mode string) (map[string]any, error) {
		return sendControl(controlPath(), "spawn", map[string]any{
			"command": command,
			"cwd":     cwd,
			"mode":    mode,
		}, 5*time.Second)
	}

	a1, err := spawn("registry-probe", "workspace-a", "cwd")
	if err != nil {
		return err
	}
	a2, err := spawn("registry-probe", "workspace-a", "cwd")
	if err != nil {
		return err
	}
	b1, err := spawn("registry-probe", "workspace-b", "cwd")
	if err != nil {
		return err
	}
	g1, err := spawn("registry-probe", "workspace-a", "global")
	if err != nil {
		return err
	}
	g2, err := spawn("registry-probe", "workspace-b", "global")
	if err != nil {
		return err
	}
	i1, err := spawn("registry-probe", "workspace-a", "isolated")
	if err != nil {
		return err
	}
	i2, err := spawn("registry-probe", "workspace-a", "isolated")
	if err != nil {
		return err
	}
	status, err := sendControl(controlPath(), "status", nil, 5*time.Second)
	if err != nil {
		return err
	}

	assertSame := func(name string, left, right map[string]any) error {
		if left["server_id"] != right["server_id"] || left["owner_generation"] != right["owner_generation"] {
			return fmt.Errorf("%s did not reuse owner: left=%v right=%v", name, left, right)
		}
		return nil
	}
	assertDifferent := func(name string, left, right map[string]any) error {
		if left["server_id"] == right["server_id"] || left["owner_generation"] == right["owner_generation"] {
			return fmt.Errorf("%s unexpectedly reused owner: left=%v right=%v", name, left, right)
		}
		return nil
	}

	if err := assertSame("same cwd", a1, a2); err != nil {
		return err
	}
	if err := assertDifferent("different cwd", a1, b1); err != nil {
		return err
	}
	if err := assertSame("global mode", g1, g2); err != nil {
		return err
	}
	if err := assertDifferent("isolated mode", i1, i2); err != nil {
		return err
	}

	return writeJSONLine(os.Stdout, map[string]any{
		"ok":          true,
		"probe":       "owner_registry",
		"owner_count": status["owner_count"],
		"cwd_reuse":   a1["server_id"],
		"cwd_split":   []any{a1["server_id"], b1["server_id"]},
		"global":      g1["server_id"],
		"isolated":    []any{i1["server_id"], i2["server_id"]},
	})
}

func probeZombieOwner() error {
	if err := ensureDaemonReady(); err != nil {
		return err
	}
	spawnReq := map[string]any{
		"command": "zombie-probe",
		"cwd":     "workspace-zombie",
		"mode":    "cwd",
	}
	first, err := sendControl(controlPath(), "spawn", spawnReq, 5*time.Second)
	if err != nil {
		return err
	}
	serverID, _ := first["server_id"].(string)
	firstGeneration, _ := first["owner_generation"].(string)
	if serverID == "" || firstGeneration == "" {
		return fmt.Errorf("incomplete first spawn response: %v", first)
	}
	poison, err := sendControl(controlPath(), "poison-owner", map[string]any{"server_id": serverID}, 5*time.Second)
	if err != nil {
		return err
	}
	if ok, _ := poison["ok"].(bool); !ok {
		return fmt.Errorf("poison-owner rejected: %v", poison)
	}
	second, err := sendControl(controlPath(), "spawn", spawnReq, 5*time.Second)
	if err != nil {
		return err
	}
	secondGeneration, _ := second["owner_generation"].(string)
	if second["server_id"] != serverID {
		return fmt.Errorf("server id changed during zombie replacement: first=%v second=%v", first, second)
	}
	if secondGeneration == "" || secondGeneration == firstGeneration {
		return fmt.Errorf("zombie owner was not replaced: first=%v second=%v", first, second)
	}
	if newOwner, _ := second["new_owner"].(bool); !newOwner {
		return fmt.Errorf("replacement spawn did not report new_owner: %v", second)
	}
	status, err := sendControl(controlPath(), "status", nil, 5*time.Second)
	if err != nil {
		return err
	}
	if detected, _ := status["zombie_detected"].(float64); detected < 1 {
		return fmt.Errorf("zombie_detected counter not incremented: %v", status)
	}
	return writeJSONLine(os.Stdout, map[string]any{
		"ok":                          true,
		"probe":                       "zombie_owner_replaced",
		"server_id":                   serverID,
		"first_owner_generation":      firstGeneration,
		"replaced_owner_generation":   secondGeneration,
		"zombie_detected_spawn_count": status["zombie_detected"],
	})
}

func probeLiveReconnect() error {
	if err := ensureDaemonReady(); err != nil {
		return err
	}
	beforeStatus, err := sendControl(controlPath(), "status", nil, 5*time.Second)
	if err != nil {
		return err
	}
	beforePID, _ := jsonNumberToInt(beforeStatus["pid"])
	beforeGeneration, _ := beforeStatus["daemon_generation"].(string)

	client, err := newProbeRPCClient()
	if err != nil {
		return err
	}
	defer client.close()

	if _, err := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "current-topology-poc-probe", "version": "1.0.0"},
		"capabilities":    map[string]any{},
	}, 10*time.Second); err != nil {
		return fmt.Errorf("initialize before restart: %w", err)
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	if _, err := client.call("tools/list", map[string]any{}, 10*time.Second); err != nil {
		return fmt.Errorf("tools/list before restart: %w", err)
	}

	restartResp, err := sendControl(controlPath(), "graceful-restart", nil, 10*time.Second)
	if err != nil {
		return fmt.Errorf("graceful-restart: %w", err)
	}
	if ok, _ := restartResp["ok"].(bool); !ok {
		return fmt.Errorf("graceful-restart rejected: %v", restartResp)
	}
	afterStatus, err := waitSuccessorReady(beforePID, beforeGeneration, 10*time.Second)
	if err != nil {
		return err
	}

	_, err = client.call("tools/call", map[string]any{
		"name":      "topology_state",
		"arguments": map[string]any{},
	}, 10*time.Second)
	if err != nil {
		_ = writeJSONLine(os.Stdout, map[string]any{
			"ok":             false,
			"probe":          "live_reconnect",
			"phase":          "phase4",
			"break_observed": true,
			"before_pid":     beforePID,
			"before_gen":     beforeGeneration,
			"after_status":   afterStatus,
			"error":          err.Error(),
		})
		return fmt.Errorf("same stdio session did not survive daemon restart: %w", err)
	}

	return writeJSONLine(os.Stdout, map[string]any{
		"ok":             true,
		"probe":          "live_reconnect",
		"phase":          "phase4",
		"break_observed": false,
		"before_pid":     beforePID,
		"before_gen":     beforeGeneration,
		"after_status":   afterStatus,
	})
}

func probeInflightReconnect() error {
	if err := ensureDaemonReady(); err != nil {
		return err
	}
	beforeStatus, err := sendControl(controlPath(), "status", nil, 5*time.Second)
	if err != nil {
		return err
	}
	beforePID, _ := jsonNumberToInt(beforeStatus["pid"])
	beforeGeneration, _ := beforeStatus["daemon_generation"].(string)

	client, err := newProbeRPCClient()
	if err != nil {
		return err
	}
	defer client.close()

	if _, err := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "current-topology-poc-probe", "version": "1.0.0"},
		"capabilities":    map[string]any{},
	}, 10*time.Second); err != nil {
		return fmt.Errorf("initialize before in-flight restart: %w", err)
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	if _, err := client.call("tools/list", map[string]any{}, 10*time.Second); err != nil {
		return fmt.Errorf("tools/list before in-flight restart: %w", err)
	}

	slowID, err := client.sendRequest("tools/call", map[string]any{
		"name": "topology_state",
		"arguments": map[string]any{
			"sleep_ms": 900,
		},
	})
	if err != nil {
		return fmt.Errorf("send slow in-flight request: %w", err)
	}
	time.Sleep(150 * time.Millisecond)

	bufferedID, err := client.sendRequest("tools/call", map[string]any{
		"name": "topology_state",
		"arguments": map[string]any{
			"sleep_ms": 900,
		},
	})
	if err != nil {
		return fmt.Errorf("send buffered request: %w", err)
	}

	restartResp, err := sendControl(controlPath(), "graceful-restart", nil, 10*time.Second)
	if err != nil {
		return fmt.Errorf("graceful-restart during in-flight request: %w", err)
	}
	if ok, _ := restartResp["ok"].(bool); !ok {
		return fmt.Errorf("graceful-restart rejected during in-flight request: %v", restartResp)
	}
	afterStatus, err := waitSuccessorReady(beforePID, beforeGeneration, 10*time.Second)
	if err != nil {
		return err
	}

	responses, responseOrder, err := client.readResponses([]int{slowID, bufferedID}, 10*time.Second)
	if err != nil {
		_ = writeJSONLine(os.Stdout, map[string]any{
			"ok":             false,
			"probe":          "inflight_reconnect",
			"phase":          "phase5",
			"break_observed": true,
			"before_pid":     beforePID,
			"before_gen":     beforeGeneration,
			"after_status":   afterStatus,
			"slow_id":        slowID,
			"buffered_id":    bufferedID,
			"response_order": responseOrder,
			"error":          err.Error(),
		})
		return fmt.Errorf("in-flight requests did not survive daemon restart: %w", err)
	}
	slowResp := responses[slowID]
	bufferedResp := responses[bufferedID]

	slowPayload, err := topologyToolPayload(slowResp)
	if err != nil {
		return fmt.Errorf("decode slow response payload: %w", err)
	}
	bufferedPayload, err := topologyToolPayload(bufferedResp)
	if err != nil {
		return fmt.Errorf("decode buffered response payload: %w", err)
	}
	afterGeneration, _ := afterStatus["daemon_generation"].(string)
	slowGeneration, _ := slowPayload["daemon_generation"].(string)
	bufferedGeneration, _ := bufferedPayload["daemon_generation"].(string)
	slowDelay, _ := jsonNumberToInt(slowPayload["delay_ms"])
	if slowDelay < 900 {
		return fmt.Errorf("slow request false-positive guard failed, delay_ms=%d payload=%v", slowDelay, slowPayload)
	}
	if slowGeneration != afterGeneration {
		return fmt.Errorf("slow in-flight request completed on wrong daemon generation: slow=%s after=%s payload=%v", slowGeneration, afterGeneration, slowPayload)
	}
	if bufferedGeneration != afterGeneration {
		return fmt.Errorf("buffered request completed on wrong daemon generation: buffered=%s after=%s payload=%v", bufferedGeneration, afterGeneration, bufferedPayload)
	}

	return writeJSONLine(os.Stdout, map[string]any{
		"ok":               true,
		"probe":            "inflight_reconnect",
		"phase":            "phase5",
		"break_observed":   false,
		"before_pid":       beforePID,
		"before_gen":       beforeGeneration,
		"after_status":     afterStatus,
		"slow_id":          slowID,
		"buffered_id":      bufferedID,
		"response_order":   responseOrder,
		"slow_payload":     slowPayload,
		"buffered_payload": bufferedPayload,
	})
}

func probeConcurrentDemux() error {
	if err := ensureDaemonReady(); err != nil {
		return err
	}
	beforeStatus, err := sendControl(controlPath(), "status", nil, 5*time.Second)
	if err != nil {
		return err
	}
	beforePID, _ := jsonNumberToInt(beforeStatus["pid"])
	beforeGeneration, _ := beforeStatus["daemon_generation"].(string)

	client, err := newProbeRPCClient()
	if err != nil {
		return err
	}
	defer client.close()

	if _, err := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "current-topology-poc-probe", "version": "1.0.0"},
		"capabilities":    map[string]any{},
	}, 10*time.Second); err != nil {
		return fmt.Errorf("initialize before concurrent restart: %w", err)
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	if _, err := client.call("tools/list", map[string]any{}, 10*time.Second); err != nil {
		return fmt.Errorf("tools/list before concurrent restart: %w", err)
	}

	slowID, err := client.sendRequest("tools/call", map[string]any{
		"name": "topology_state",
		"arguments": map[string]any{
			"sleep_ms": 900,
			"tag":      "slow",
		},
	})
	if err != nil {
		return fmt.Errorf("send slow concurrent request: %w", err)
	}
	fastID, err := client.sendRequest("tools/call", map[string]any{
		"name": "topology_state",
		"arguments": map[string]any{
			"sleep_ms": 700,
			"tag":      "fast",
		},
	})
	if err != nil {
		return fmt.Errorf("send fast concurrent request: %w", err)
	}

	concurrentStatus, err := waitOwnerConcurrency(2, 500*time.Millisecond)
	if err != nil {
		_ = writeJSONLine(os.Stdout, map[string]any{
			"ok":             false,
			"probe":          "concurrent_demux",
			"phase":          "phase6",
			"break_observed": true,
			"before_pid":     beforePID,
			"before_gen":     beforeGeneration,
			"slow_id":        slowID,
			"fast_id":        fastID,
			"error":          err.Error(),
		})
		return fmt.Errorf("concurrent false-positive guard failed: %w", err)
	}

	restartResp, err := sendControl(controlPath(), "graceful-restart", nil, 10*time.Second)
	if err != nil {
		return fmt.Errorf("graceful-restart during concurrent requests: %w", err)
	}
	if ok, _ := restartResp["ok"].(bool); !ok {
		return fmt.Errorf("graceful-restart rejected during concurrent requests: %v", restartResp)
	}
	afterStatus, err := waitSuccessorReady(beforePID, beforeGeneration, 10*time.Second)
	if err != nil {
		return err
	}

	responses, order, err := client.readResponses([]int{slowID, fastID}, 10*time.Second)
	if err != nil {
		_ = writeJSONLine(os.Stdout, map[string]any{
			"ok":                false,
			"probe":             "concurrent_demux",
			"phase":             "phase6",
			"break_observed":    true,
			"before_pid":        beforePID,
			"before_gen":        beforeGeneration,
			"after_status":      afterStatus,
			"concurrent_status": concurrentStatus,
			"slow_id":           slowID,
			"fast_id":           fastID,
			"response_order":    order,
			"error":             err.Error(),
		})
		return fmt.Errorf("concurrent responses did not survive daemon restart: %w", err)
	}

	slowPayload, err := topologyToolPayload(responses[slowID])
	if err != nil {
		return fmt.Errorf("decode slow concurrent response payload: %w", err)
	}
	fastPayload, err := topologyToolPayload(responses[fastID])
	if err != nil {
		return fmt.Errorf("decode fast concurrent response payload: %w", err)
	}
	afterGeneration, _ := afterStatus["daemon_generation"].(string)
	slowGeneration, _ := slowPayload["daemon_generation"].(string)
	fastGeneration, _ := fastPayload["daemon_generation"].(string)
	if slowGeneration != afterGeneration || fastGeneration != afterGeneration {
		return fmt.Errorf("concurrent responses completed on wrong daemon generation: slow=%s fast=%s after=%s", slowGeneration, fastGeneration, afterGeneration)
	}
	if len(order) < 2 || order[0] != fastID || order[1] != slowID {
		return fmt.Errorf("response order false-positive guard failed: order=%v fast_id=%d slow_id=%d", order, fastID, slowID)
	}

	return writeJSONLine(os.Stdout, map[string]any{
		"ok":                true,
		"probe":             "concurrent_demux",
		"phase":             "phase6",
		"break_observed":    false,
		"before_pid":        beforePID,
		"before_gen":        beforeGeneration,
		"after_status":      afterStatus,
		"concurrent_status": concurrentStatus,
		"slow_id":           slowID,
		"fast_id":           fastID,
		"response_order":    order,
		"slow_payload":      slowPayload,
		"fast_payload":      fastPayload,
	})
}

func probeRefreshReconnect() error {
	if err := ensureDaemonReady(); err != nil {
		return err
	}
	beforeStatus, err := sendControl(controlPath(), "status", nil, 5*time.Second)
	if err != nil {
		return err
	}
	beforePID, _ := jsonNumberToInt(beforeStatus["pid"])
	beforeGeneration, _ := beforeStatus["daemon_generation"].(string)

	client, err := newProbeRPCClient()
	if err != nil {
		return err
	}
	defer client.close()

	if _, err := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "current-topology-poc-probe", "version": "1.0.0"},
		"capabilities":    map[string]any{},
	}, 10*time.Second); err != nil {
		return fmt.Errorf("initialize before refresh restart: %w", err)
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	if _, err := client.call("tools/list", map[string]any{}, 10*time.Second); err != nil {
		return fmt.Errorf("tools/list before refresh restart: %w", err)
	}

	restartResp, err := sendControl(controlPath(), "graceful-restart", nil, 10*time.Second)
	if err != nil {
		return fmt.Errorf("graceful-restart before refresh reconnect: %w", err)
	}
	if ok, _ := restartResp["ok"].(bool); !ok {
		return fmt.Errorf("graceful-restart rejected before refresh reconnect: %v", restartResp)
	}
	afterStatus, err := waitSuccessorReady(beforePID, beforeGeneration, 10*time.Second)
	if err != nil {
		return err
	}

	resp, err := client.call("tools/call", map[string]any{
		"name":      "topology_state",
		"arguments": map[string]any{},
	}, 10*time.Second)
	if err != nil {
		_ = writeJSONLine(os.Stdout, map[string]any{
			"ok":             false,
			"probe":          "refresh_reconnect",
			"phase":          "phase7",
			"break_observed": true,
			"before_pid":     beforePID,
			"before_gen":     beforeGeneration,
			"after_status":   afterStatus,
			"error":          err.Error(),
		})
		return fmt.Errorf("same stdio session did not reconnect through refresh-token path: %w", err)
	}
	payload, err := topologyToolPayload(resp)
	if err != nil {
		return fmt.Errorf("decode refresh reconnect payload: %w", err)
	}
	finalStatus, err := sendControl(controlPath(), "status", nil, 5*time.Second)
	if err != nil {
		return err
	}
	refreshSuccesses, _ := jsonNumberToInt(finalStatus["refresh_successes"])
	fallbackSpawns, _ := jsonNumberToInt(finalStatus["fallback_spawns"])
	prevToken, _ := finalStatus["last_refresh_prev_token"].(string)
	newToken, _ := finalStatus["last_refresh_new_token"].(string)
	refreshUsed := refreshSuccesses > 0
	fallbackUsed := fallbackSpawns > 0
	tokenChanged := prevToken != "" && newToken != "" && prevToken != newToken
	if !refreshUsed || fallbackUsed || !tokenChanged {
		_ = writeJSONLine(os.Stdout, map[string]any{
			"ok":                  false,
			"probe":               "refresh_reconnect",
			"phase":               "phase7",
			"break_observed":      true,
			"before_pid":          beforePID,
			"before_gen":          beforeGeneration,
			"after_status":        afterStatus,
			"final_status":        finalStatus,
			"payload":             payload,
			"refresh_used":        refreshUsed,
			"fallback_spawn_used": fallbackUsed,
			"token_changed":       tokenChanged,
		})
		return fmt.Errorf("refresh-token reconnect guard failed: refresh_used=%v fallback_spawn_used=%v token_changed=%v", refreshUsed, fallbackUsed, tokenChanged)
	}

	return writeJSONLine(os.Stdout, map[string]any{
		"ok":                  true,
		"probe":               "refresh_reconnect",
		"phase":               "phase7",
		"break_observed":      false,
		"before_pid":          beforePID,
		"before_gen":          beforeGeneration,
		"after_status":        afterStatus,
		"final_status":        finalStatus,
		"payload":             payload,
		"refresh_used":        refreshUsed,
		"fallback_spawn_used": fallbackUsed,
		"original_token":      prevToken,
		"refreshed_token":     newToken,
	})
}

func newProbeRPCClient() (*probeRPCClient, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	return &probeRPCClient{
		cmd:     cmd,
		stdin:   stdin,
		scanner: scanner,
		nextID:  1,
	}, nil
}

func (c *probeRPCClient) call(method string, params any, timeout time.Duration) (map[string]any, error) {
	id, err := c.sendRequest(method, params)
	if err != nil {
		return nil, err
	}
	return c.readResponse(id, timeout)
}

func (c *probeRPCClient) sendRequest(method string, params any) (int, error) {
	id := c.nextID
	c.nextID++
	if err := writeJSONLine(c.stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		return 0, err
	}
	return id, nil
}

func (c *probeRPCClient) readResponse(id int, timeout time.Duration) (map[string]any, error) {
	type result struct {
		resp map[string]any
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		for c.scanner.Scan() {
			var msg map[string]any
			if err := json.Unmarshal(c.scanner.Bytes(), &msg); err != nil {
				continue
			}
			gotID, ok := jsonNumberToInt(msg["id"])
			if !ok || gotID != id {
				continue
			}
			ch <- result{resp: msg}
			return
		}
		if err := c.scanner.Err(); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{err: io.ErrUnexpectedEOF}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		if res.resp["error"] != nil {
			return res.resp, fmt.Errorf("json-rpc error: %v", res.resp["error"])
		}
		return res.resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response id %d", id)
	}
}

func (c *probeRPCClient) readResponses(ids []int, timeout time.Duration) (map[int]map[string]any, []int, error) {
	wanted := make(map[int]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	responses := make(map[int]map[string]any, len(ids))
	order := make([]int, 0, len(ids))
	deadline := time.Now().Add(timeout)
	for len(responses) < len(wanted) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return responses, order, fmt.Errorf("timeout waiting for responses ids=%v got_order=%v", ids, order)
		}
		lineCh := make(chan []byte, 1)
		errCh := make(chan error, 1)
		go func() {
			if c.scanner.Scan() {
				lineCh <- append([]byte{}, c.scanner.Bytes()...)
				return
			}
			if err := c.scanner.Err(); err != nil {
				errCh <- err
				return
			}
			errCh <- io.ErrUnexpectedEOF
		}()
		select {
		case line := <-lineCh:
			var msg map[string]any
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			gotID, ok := jsonNumberToInt(msg["id"])
			if !ok || !wanted[gotID] {
				continue
			}
			if msg["error"] != nil {
				return responses, order, fmt.Errorf("json-rpc error for id %d: %v", gotID, msg["error"])
			}
			if _, exists := responses[gotID]; !exists {
				responses[gotID] = msg
				order = append(order, gotID)
			}
		case err := <-errCh:
			return responses, order, err
		case <-time.After(remaining):
			return responses, order, fmt.Errorf("timeout waiting for responses ids=%v got_order=%v", ids, order)
		}
	}
	return responses, order, nil
}

func (c *probeRPCClient) notify(method string, params any) error {
	return writeJSONLine(c.stdin, map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *probeRPCClient) close() {
	_ = c.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
}

func waitControlReady(timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := sendControl(controlPath(), "status", nil, 500*time.Millisecond)
		if err == nil {
			if ready, _ := status["ready"].(bool); ready {
				return status, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not become ready after restart")
}

func waitSuccessorReady(oldPID int, oldGeneration string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		status, err := sendControl(controlPath(), "status", nil, 500*time.Millisecond)
		if err == nil {
			last = status
			ready, _ := status["ready"].(bool)
			pid, _ := jsonNumberToInt(status["pid"])
			generation, _ := status["daemon_generation"].(string)
			if ready && (pid != oldPID || generation != oldGeneration) {
				return status, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("successor daemon did not become ready; old_pid=%d old_generation=%s last_status=%v", oldPID, oldGeneration, last)
}

func waitOwnerConcurrency(want int, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		status, err := sendControl(controlPath(), "status", nil, 500*time.Millisecond)
		if err == nil {
			last = status
			if maxOwnerConcurrency(status) >= want {
				return status, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last, fmt.Errorf("owner concurrency did not reach %d; last_status=%v", want, last)
}

func maxOwnerConcurrency(status map[string]any) int {
	owners, _ := status["owners"].([]any)
	maxSeen := 0
	for _, owner := range owners {
		obj, _ := owner.(map[string]any)
		if n, ok := jsonNumberToInt(obj["max_concurrent_calls"]); ok && n > maxSeen {
			maxSeen = n
		}
	}
	return maxSeen
}

func jsonNumberToInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func intArg(args map[string]any, key string) int {
	if len(args) == 0 {
		return 0
	}
	n, _ := jsonNumberToInt(args[key])
	return n
}

func topologyToolPayload(resp map[string]any) (map[string]any, error) {
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing result object: %v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return nil, fmt.Errorf("missing content array: %v", resp)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("first content item is not an object: %v", content[0])
	}
	text, ok := first["text"].(string)
	if !ok {
		return nil, fmt.Errorf("first content item has no text: %v", first)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func listenUnixExclusive(path string, wait time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(wait)
	for {
		if activeUnix(path, 200*time.Millisecond) {
			if wait > 0 && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("listener already active at %s", path)
		}

		_ = os.Remove(path)
		ln, err := net.Listen("unix", path)
		if err == nil {
			return ln, nil
		}
		if wait > 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return nil, err
	}
}

func activeUnix(path string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func cleanupListener(ln net.Listener, path string) {
	_ = ln.Close()
	_ = os.Remove(path)
}

func (s *daemonState) closeOwners() {
	s.mu.Lock()
	owners := make([]*ownerEntry, 0, len(s.owners))
	for _, entry := range s.owners {
		owners = append(owners, entry)
	}
	s.owners = make(map[string]*ownerEntry)
	s.mu.Unlock()

	for _, entry := range owners {
		cleanupListener(entry.Listener, entry.Socket)
	}
}

func (s *daemonState) writeSnapshot() error {
	s.mu.RLock()
	snaps := make([]ownerSnapshot, 0, len(s.owners))
	for _, entry := range s.owners {
		snaps = append(snaps, ownerSnapshot{
			ServerID: entry.ServerID,
			Command:  entry.Command,
			Args:     append([]string(nil), entry.Args...),
			Cwd:      entry.Cwd,
			Mode:     entry.Mode,
		})
	}
	history := make([]tokenHistoryEntry, 0, len(s.tokenHistory))
	now := time.Now()
	for token, entry := range s.tokenHistory {
		if now.After(entry.ExpiresAt) {
			continue
		}
		entry.Token = token
		history = append(history, entry)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(daemonSnapshot{Owners: snaps, TokenHistory: history}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(snapshotPath(), data, 0o600)
}

func (s *daemonState) restoreSnapshot() error {
	data, err := os.ReadFile(snapshotPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer os.Remove(snapshotPath())

	var snap daemonSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		var legacyOwners []ownerSnapshot
		if legacyErr := json.Unmarshal(data, &legacyOwners); legacyErr != nil {
			return err
		}
		snap.Owners = legacyOwners
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, owner := range snap.Owners {
		req := controlRequest{
			Command: owner.Command,
			Args:    append([]string(nil), owner.Args...),
			Cwd:     owner.Cwd,
			Mode:    owner.Mode,
		}
		sid := owner.ServerID
		if sid == "" {
			sid = serverID(normalizeSpawnRequest(req))
		}
		if _, exists := s.owners[sid]; exists {
			continue
		}
		if _, err := s.startOwnerLocked(sid, req); err != nil {
			return err
		}
	}
	now := time.Now()
	for _, entry := range snap.TokenHistory {
		if entry.Token == "" || entry.ServerID == "" || now.After(entry.ExpiresAt) {
			continue
		}
		if _, ok := s.owners[entry.ServerID]; !ok {
			continue
		}
		s.tokenHistory[entry.Token] = entry
	}
	return nil
}

func controlPath() string {
	if v := os.Getenv(envCtlPath); strings.TrimSpace(v) != "" {
		return v
	}
	return filepath.Join(runtimeDir(), "control.sock")
}

func snapshotPath() string {
	return filepath.Join(runtimeDir(), "owners.snapshot.json")
}

func ownerPath(serverID, generation string) string {
	return filepath.Join(runtimeDir(), serverID+"-"+generation+".owner.sock")
}

func runtimeDir() string {
	if v := os.Getenv(envRuntime); strings.TrimSpace(v) != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "current-topology-poc")
}

func normalizeSpawnRequest(req controlRequest) controlRequest {
	mode := normalizeMode(req.Mode)
	command := req.Command
	if strings.TrimSpace(command) == "" {
		command = serverName
	}
	return controlRequest{
		Command: command,
		Args:    append([]string(nil), req.Args...),
		Cwd:     req.Cwd,
		Mode:    mode,
		Env:     cloneEnv(req.Env),
	}
}

func normalizeMode(mode string) string {
	switch mode {
	case "global", "isolated", "cwd":
		return mode
	case "":
		return "cwd"
	default:
		return "cwd"
	}
}

func cloneEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func serverID(req controlRequest) string {
	mode := normalizeMode(req.Mode)
	if mode == "isolated" {
		return "isolated-" + randomToken()[:16]
	}

	identityCwd := req.Cwd
	if mode == "global" {
		identityCwd = ""
	}
	sum := sha1.Sum([]byte(strings.Join([]string{
		mode,
		req.Command,
		strings.Join(req.Args, "\x00"),
		identityCwd,
	}, "\x1f")))
	return mode + "-" + hex.EncodeToString(sum[:])[:16]
}

func newGeneration(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, os.Getpid(), randomToken()[:12])
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func writeRPCResult(w io.Writer, id json.RawMessage, result any) error {
	return writeJSONLine(w, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func writeRPCError(w io.Writer, id json.RawMessage, code int, message string) error {
	return writeJSONLine(w, map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
}

func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func copyScannerLines(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := append([]byte{}, scanner.Bytes()...)
		line = append(line, '\n')
		if _, err := w.Write(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func copyReaderLines(r *bufio.Reader, w io.Writer) error {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := w.Write(line); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			return err
		}
	}
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(data)
}

func logf(format string, args ...any) {
	if os.Getenv("CURRENT_TOPOLOGY_POC_QUIET") == "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "[current-topology-poc] "+format+"\n", args...)
}
