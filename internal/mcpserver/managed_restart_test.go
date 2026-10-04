package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// This subprocess records real upstream input, not control-token echoes.
func TestManagedRestartHelperProcess(t *testing.T) {
	if os.Getenv("MCPMUX_RESTART_HELPER") != "1" {
		return
	}
	file, err := os.OpenFile(filepath.Join(os.Getenv("MCPMUX_RESTART_CAPTURE"), strconv.Itoa(os.Getpid())+".ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	defer file.Close()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		raw := scanner.Bytes()
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(raw, &req) != nil {
			os.Exit(4)
		}
		captured := raw
		if os.Getenv("MCPMUX_RESTART_PROTOCOL") == "" {
			// Legacy injection may contain private muxEnv; retain only method order.
			captured, _ = json.Marshal(struct {
				Method string `json:"method"`
			}{req.Method})
		}
		if _, err := fmt.Fprintln(file, string(captured)); err != nil {
			os.Exit(3)
		}
		if req.ID == nil {
			continue
		}
		cwd, _ := os.Getwd()
		var result any = map[string]any{"pid": os.Getpid(), "cwd": cwd, "context": os.Getenv("MCPMUX_RESTART_CONTEXT")}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}, "x-mux": map[string]any{"sharing": "shared"}}, "serverInfo": map[string]any{"name": "restart-resource", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "prompts/list":
			result = map[string]any{"prompts": []any{}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []any{}}
		case "restart/wait":
			for {
				if _, err := os.Stat(filepath.Join(cwd, "release-restart-work")); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		encoded, err := json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  any             `json:"result"`
		}{"2.0", req.ID, result})
		if err != nil {
			os.Exit(5)
		}
		fmt.Fprintln(os.Stdout, string(encoded))
	}
	os.Exit(0)
}

type managedRestartFixture struct {
	d       *daemon.Daemon
	req     control.Request
	initial *daemon.OwnerEntry
	base    string
	capture string
	token   string
}

func newManagedRestartFixture(t *testing.T, protocolEra string, persistent bool) *managedRestartFixture {
	t.Helper()
	profile := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, profile)
	}
	base := shortBaseDir(t, "mr-")
	capture := t.TempDir()
	f := &managedRestartFixture{base: base, capture: capture}
	var err error
	f.d, err = daemon.New(daemon.Config{
		ControlPath: filepath.Join(base, "actual.sock"), Namespace: "mr-" + filepath.Base(base),
		SkipSnapshot: true, Persistent: persistent, ZeroSessionCleanupDelay: 100 * time.Millisecond,
		OwnerIdleTimeout: time.Hour, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(f.req.Cwd, "release-restart-work"), []byte("release"), 0o600)
		f.d.Shutdown()
		select {
		case <-f.d.Done():
		case <-time.After(10 * time.Second):
			t.Error("restart fixture did not settle its managed resources")
		}
	})
	f.req = control.Request{Cmd: "spawn", Command: os.Args[0], Args: []string{"-test.run=^TestManagedRestartHelperProcess$"}, Cwd: t.TempDir(), Mode: "global", ProtocolEra: protocolEra, Env: map[string]string{
		"MCPMUX_RESTART_HELPER": "1", "MCPMUX_RESTART_CAPTURE": capture, "MCPMUX_RESTART_CONTEXT": "retained-native-context",
		"MCPMUX_RESTART_PROTOCOL": protocolEra,
	}}
	path, sid, token, err := f.d.Spawn(f.req)
	if err != nil {
		t.Fatal(err)
	}
	f.initial, f.token = f.d.Entry(sid), token
	conn, scanner := managedRestartConnect(t, path, token)
	managedRestartHostCheck(t, conn, scanner, protocolEra, f.req.Cwd)
	return f
}

func managedRestartWait(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}

func managedRestartConnect(t *testing.T, path, token string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := ipc.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(conn, token); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewScanner(conn)
}

func managedRestartHostCheck(t *testing.T, conn net.Conn, scanner *bufio.Scanner, protocolEra, cwd string) {
	t.Helper()
	frame := `{"jsonrpc":"2.0","id":"actual-host","method":"restart/check","params":{}}`
	if protocolEra != "" {
		frame = `{"jsonrpc":"2.0","id":"actual-host","method":"restart/check","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	}
	if _, err := fmt.Fprintln(conn, frame); err != nil {
		t.Fatal(err)
	}
	for scanner.Scan() {
		var response struct {
			ID     string `json:"id"`
			Result struct {
				PID     int    `json:"pid"`
				Cwd     string `json:"cwd"`
				Context string `json:"context"`
			} `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) == nil && response.ID == "actual-host" {
			if response.Result.PID <= 0 || response.Result.Context != "retained-native-context" {
				t.Fatalf("actual upstream lost selected launch context: %s", scanner.Bytes())
			}
			selectedDir, selectedErr := os.Stat(cwd)
			actualDir, actualErr := os.Stat(response.Result.Cwd)
			if selectedErr != nil || actualErr != nil {
				t.Fatalf("cannot verify actual upstream working directory identity: selected=%v actual=%v", selectedErr, actualErr)
			}
			if !selectedDir.IsDir() || !actualDir.IsDir() || !os.SameFile(selectedDir, actualDir) {
				t.Fatal("actual upstream lost selected working directory identity")
			}
			_ = conn.SetDeadline(time.Time{})
			return
		}
	}
	t.Fatalf("actual host got no native upstream response: %v", scanner.Err())
}

type restartReservationObservation struct {
	owner    *owner.Owner
	pending  int
	sessions int
	bound    bool
	verdict  control.SuspendCheckResponse
}

type managedRestartDaemonControl struct {
	*daemon.Daemon
	delay                time.Duration
	requests             chan control.Request
	replies              chan control.Response
	replacement          *daemon.OwnerEntry
	pendingBefore        int
	sessionsBefore       int
	historyBefore        bool
	confirmationSessions atomic.Int32
	observations         chan restartReservationObservation
	changeReply          func(*control.Response)
	afterRestart         func(control.Response)
	externalSpawns       atomic.Int32
	externalStops        atomic.Int32
}

func (h *managedRestartDaemonControl) HandleRestartOwner(req control.Request) (control.Response, error) {
	h.requests <- req
	time.Sleep(h.delay)
	resp, err := h.Daemon.HandleRestartOwner(req)
	if err == nil {
		h.replacement = h.Daemon.Entry(resp.ServerID)
		h.pendingBefore = h.replacement.Owner.SessionMgr().PendingCount()
		h.sessionsBefore = h.replacement.Owner.SessionCount()
		_, _, _, h.historyBefore = h.replacement.Owner.SessionMgr().LookupHistory(resp.Token)
		h.replies <- resp
		if h.afterRestart != nil {
			h.afterRestart(resp)
		}
		h.confirmationSessions.Store(int32(h.replacement.Owner.SessionCount() + 1))
		if h.changeReply != nil {
			h.changeReply(&resp)
		}
	}
	return resp, err
}

func (h *managedRestartDaemonControl) HandleCanSuspendForOwner(token, sid string) (control.SuspendCheckResponse, error) {
	verdict, err := h.Daemon.HandleCanSuspendForOwner(token, sid)
	if err == nil {
		entry := h.Daemon.Entry(sid)
		if entry != nil && entry.Owner != nil {
			// Bind publishes history just before AddSession. Include the tool's
			// own admission even when another host is already registered.
			deadline := time.Now().Add(time.Second)
			expected := int(h.confirmationSessions.Load())
			for (entry.Owner.SessionCount() < expected || entry.Owner.SessionMgr().SessionCount() < expected) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			key, _, _, bound := entry.Owner.SessionMgr().LookupHistory(token)
			h.observations <- restartReservationObservation{entry.Owner, entry.Owner.SessionMgr().PendingCount(), entry.Owner.SessionCount(), bound && key == sid, verdict}
		}
	}
	return verdict, err
}

func (h *managedRestartDaemonControl) HandleSpawn(req control.Request) (string, string, string, error) {
	h.externalSpawns.Add(1)
	return h.Daemon.HandleSpawn(req)
}

func (h *managedRestartDaemonControl) HandleStopOwner(req control.Request) (string, error) {
	h.externalStops.Add(1)
	return h.Daemon.HandleStopOwner(req)
}

func managedRestartEndpoint(t *testing.T, f *managedRestartFixture) (string, *managedRestartDaemonControl) {
	t.Helper()
	h := &managedRestartDaemonControl{Daemon: f.d, requests: make(chan control.Request, 4), replies: make(chan control.Response, 4), observations: make(chan restartReservationObservation, 4)}
	h.afterRestart = func(resp control.Response) {
		if resp.ProtocolEra == "" {
			return // A valid legacy replacement may remain template-backed/cache-only.
		}
		entry := f.d.Entry(resp.ServerID)
		managedRestartWait(t, func() bool {
			pid, _ := entry.Owner.Status()["upstream_pid"].(int)
			_, err := os.Stat(filepath.Join(f.capture, strconv.Itoa(pid)+".ndjson"))
			return pid > 0 && err == nil && entry.Owner.MaterializationState() == owner.MaterializationReady
		}, "replacement managed subprocess never became live")
	}
	endpoint := serverid.DaemonControlPath(f.base, "mcp-mux")
	srv, err := control.NewServer(endpoint, h, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return endpoint, h
}

func managedRestartTool(t *testing.T, base, endpoint, sid string) (string, bool) {
	t.Helper()
	input := fmt.Sprintf(`{"jsonrpc":"2.0","id":"restart-resource-id","method":"tools/call","params":{"name":"mux_restart","arguments":{"server_id":%q,"force":true}}}`+"\n", sid)
	var output bytes.Buffer
	s := NewServer(strings.NewReader(input), &output, log.New(io.Discard, "", 0))
	s.BaseDir, s.DaemonCtlPath = base, endpoint
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	var frame struct {
		ID     string `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	dec := json.NewDecoder(&output)
	if err := dec.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.ID != "restart-resource-id" || len(frame.Result.Content) != 1 {
		t.Fatal("restart tool changed original response framing")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatal("restart tool returned more than one response")
	}
	return frame.Result.Content[0].Text, frame.Result.IsError
}

func managedRestartConsumed(t *testing.T, h *managedRestartDaemonControl, resp control.Response) *owner.Owner {
	t.Helper()
	select {
	case observation := <-h.observations:
		if !observation.bound || observation.pending != 0 || observation.sessions == 0 || observation.owner != h.replacement.Owner || observation.owner.ServerID() != resp.ServerID {
			t.Fatalf("restart reservation was not actually consumed by its owner: pending=%d sessions=%d bound=%v", observation.pending, observation.sessions, observation.bound)
		}
		t.Logf("actual restart reservation after admission: era=%q pending=%d sessions=%d bound=%v", resp.ProtocolEra, observation.pending, observation.sessions, observation.bound)
		return observation.owner
	case <-time.After(time.Second):
		t.Fatalf("tool returned without native consumption: pending=%d sessions=%d preregistered=%v", h.replacement.Owner.SessionMgr().PendingCount(), h.replacement.Owner.SessionCount(), h.replacement.Owner.SessionMgr().IsPreRegistered(resp.Token))
		return nil
	}
}

func TestManagedRestartReservationConsumedAndZeroSessionCleanup(t *testing.T) {
	for _, wire := range []string{"", "2026-07-28"} {
		t.Run("era_"+wire, func(t *testing.T) {
			f := newManagedRestartFixture(t, wire, false)
			endpoint, h := managedRestartEndpoint(t, f)
			text, isError := managedRestartTool(t, f.base, endpoint, f.initial.Owner.ServerID())
			resp := <-h.replies
			var result control.Response
			if json.Unmarshal([]byte(text), &result) != nil || isError || !result.OK || result.ServerID != resp.ServerID || result.ProtocolEra != wire || strings.Contains(text, resp.Token) || strings.Contains(text, resp.IPCPath) {
				t.Fatalf("restart result was unsuccessful or exposed its reservation: isError=%v", isError)
			}
			next := h.replacement
			if next == nil || next == f.initial || next.OwnerGeneration == f.initial.OwnerGeneration || next.ProtocolEra != f.initial.ProtocolEra {
				t.Fatal("restart did not replace the exact original owner in the same era")
			}
			select {
			case <-f.initial.Owner.Done():
			default:
				t.Fatal("restart succeeded before original process-tree retirement settled")
			}
			if h.pendingBefore != 1 || h.sessionsBefore != 0 || h.historyBefore {
				t.Fatal("actual restart did not begin with one unused reservation and no bound history/session")
			}
			t.Logf("actual restart reservation before admission: era=%q pending=%d sessions=%d bound=%v", wire, h.pendingBefore, h.sessionsBefore, h.historyBefore)
			o := managedRestartConsumed(t, h, resp)
			pid, _ := o.Status()["upstream_pid"].(int)
			managedRestartWait(t, func() bool { return o.SessionCount() == 0 && o.SessionMgr().PendingCount() == 0 }, "tool IPC EOF did not remove the actual bound session")
			managedRestartWait(t, func() bool { return f.d.Entry(resp.ServerID) == nil }, "ordinary zero-session cleanup was still vetoed by the tool reservation")
			_, _, _, historyRemaining := o.SessionMgr().LookupHistory(resp.Token)
			if historyRemaining {
				t.Fatal("ordinary owner retirement retained the tool's consumed-token history")
			}
			t.Logf("actual restart reservation after EOF/ordinary cleanup: era=%q pending=%d sessions=%d bound=%v owner_removed=true", wire, o.SessionMgr().PendingCount(), o.SessionCount(), historyRemaining)
			select {
			case <-o.Done():
			default:
				t.Fatal("zero-session cleanup discarded unsettled process authority")
			}
			removals := f.d.HandleStatus()["owner_removal"].(map[string]any)
			byReason := removals["by_reason"].(map[string]uint64)
			if byReason["operator_hard"] != 1 || byReason["idle"] != 1 {
				t.Fatal("replacement was not retired through ordinary zero-session idle authority")
			}
			if wire != "" {
				data, err := os.ReadFile(filepath.Join(f.capture, strconv.Itoa(pid)+".ndjson"))
				if err != nil || len(data) != 0 {
					t.Fatalf("modern operator restart generated MCP traffic: bytes=%d err=%v", len(data), err)
				}
			}
			path, sid, token, err := f.d.Spawn(f.req)
			if err != nil {
				t.Fatal(err)
			}
			if wire != "" && (sid == resp.ServerID || sid == f.initial.Owner.ServerID()) {
				t.Fatal("future modern host attached to an operator phantom instead of fresh isolated admission")
			}
			conn, scanner := managedRestartConnect(t, path, token)
			managedRestartHostCheck(t, conn, scanner, wire, f.req.Cwd)
			if h.externalSpawns.Load() != 0 || h.externalStops.Load() != 0 || len(h.requests) != 1 {
				t.Fatal("tool used restart replay, a fallback spawn, or replacement stop")
			}
		})
	}
}

func TestManagedRestartFailedReservationPreservesAuthority(t *testing.T) {
	for _, failure := range []string{"stale_token", "cross_owner_token", "ipc_absent", "wrong_era", "token_newline"} {
		t.Run(failure, func(t *testing.T) {
			f := newManagedRestartFixture(t, "2026-07-28", false)
			endpoint, h := managedRestartEndpoint(t, f)
			otherToken := f.token
			var other *owner.Owner
			if failure == "cross_owner_token" {
				path, otherSID, token, err := f.d.Spawn(f.req)
				if err != nil {
					t.Fatal(err)
				}
				conn, scanner := managedRestartConnect(t, path, token)
				managedRestartHostCheck(t, conn, scanner, f.req.ProtocolEra, f.req.Cwd)
				otherToken = token
				other = f.d.Entry(otherSID).Owner
			}
			h.changeReply = func(resp *control.Response) {
				switch failure {
				case "stale_token", "cross_owner_token":
					resp.Token = otherToken
				case "ipc_absent":
					resp.IPCPath = filepath.Join(f.base, "absent.sock")
				case "wrong_era":
					resp.ProtocolEra = ""
				case "token_newline":
					resp.Token += "\n{\"jsonrpc\":\"2.0\",\"method\":\"initialize\"}"
				}
			}
			text, isError := managedRestartTool(t, f.base, endpoint, f.initial.Owner.ServerID())
			resp := <-h.replies
			if !isError || strings.Contains(text, resp.Token) || strings.Contains(text, otherToken) || strings.Contains(text, resp.IPCPath) || strings.Contains(text, `"ok":true`) {
				t.Fatal("uncertain reservation returned success or disclosed token/transport data")
			}
			next := f.d.Entry(resp.ServerID)
			if next == nil || next.Owner.SessionMgr().PendingCount() != 1 || !next.Owner.SessionMgr().IsPreRegistered(resp.Token) || next.Owner.SessionCount() != 0 {
				t.Fatal("failed token consumption lost the replacement's existing reservation authority")
			}
			select {
			case <-next.Owner.Done():
				t.Fatal("failed tool killed the replacement")
			default:
			}
			if other != nil {
				entry := f.d.Entry(other.ServerID())
				if entry == nil || entry.Owner != other || other.SessionCount() != 1 || other.SessionMgr().PendingCount() != 0 {
					t.Fatal("cross-owner token failure disturbed the other actual host's authority")
				}
			}
			if h.externalSpawns.Load() != 0 || h.externalStops.Load() != 0 {
				t.Fatal("failed tool used a destructive or fresh-token fallback")
			}
		})
	}
}

func TestManagedRestartPersistentReservationReleasedWithoutEviction(t *testing.T) {
	f := newManagedRestartFixture(t, "2026-07-28", true)
	endpoint, h := managedRestartEndpoint(t, f)
	text, isError := managedRestartTool(t, f.base, endpoint, f.initial.Owner.ServerID())
	resp := <-h.replies
	if isError || !strings.Contains(text, `"ok": true`) {
		t.Fatal("persistent restart was incorrectly refused")
	}
	o := managedRestartConsumed(t, h, resp)
	managedRestartWait(t, func() bool { return o.SessionCount() == 0 && o.SessionMgr().PendingCount() == 0 }, "persistent replacement retained its tool session/reservation")
	time.Sleep(250 * time.Millisecond)
	if entry := f.d.Entry(resp.ServerID); entry == nil || entry.Owner != o || !entry.Persistent {
		t.Fatal("tool bypassed the native persistent-owner zero-session veto")
	}
}

func TestManagedRestartPendingWorkDenialConfirmsConsumptionAndKeepsHost(t *testing.T) {
	f := newManagedRestartFixture(t, "", false)
	endpoint, h := managedRestartEndpoint(t, f)
	var host net.Conn
	var scanner *bufio.Scanner
	h.afterRestart = func(resp control.Response) {
		path, sid, token, err := f.d.Spawn(f.req)
		if err != nil || sid != resp.ServerID {
			t.Fatal("actual replacement host did not reuse its native legacy context")
		}
		host, scanner = managedRestartConnect(t, path, token)
		if _, err := fmt.Fprintln(host, `{"jsonrpc":"2.0","id":"kept-work","method":"restart/wait","params":{}}`); err != nil {
			t.Fatal(err)
		}
		managedRestartWait(t, func() bool {
			o := f.d.Entry(sid).Owner
			if o.MaterializationState() != owner.MaterializationReady || o.MaterializationBlocksEviction() || o.PendingRequests() != 1 {
				return false
			}
			pid, _ := o.Status()["upstream_pid"].(int)
			data, err := os.ReadFile(filepath.Join(f.capture, strconv.Itoa(pid)+".ndjson"))
			return err == nil && bytes.Contains(data, []byte("{\"method\":\"restart/wait\"}\n"))
		}, "replacement upstream did not actually receive pending work after materialization settled")
	}
	text, isError := managedRestartTool(t, f.base, endpoint, f.initial.Owner.ServerID())
	resp := <-h.replies
	if isError || !strings.Contains(text, `"ok": true`) {
		t.Fatal("legitimate owner-wide pending-work denial was treated as failed restart consumption")
	}
	var observation restartReservationObservation
	select {
	case observation = <-h.observations:
	case <-time.After(time.Second):
		t.Fatal("tool returned without the actual native consumed-token/pending-work verdict")
	}
	if !observation.bound || observation.pending != 0 || observation.sessions != 2 || observation.verdict.Allowed || observation.verdict.Reason != "pending_requests" {
		t.Fatalf("confirmation did not follow actual native exact-token lookup and pending-work denial: bound=%v pending=%d sessions=%d allowed=%v reason=%q", observation.bound, observation.pending, observation.sessions, observation.verdict.Allowed, observation.verdict.Reason)
	}
	o := observation.owner
	managedRestartWait(t, func() bool { return o.SessionCount() == 1 && o.SessionMgr().PendingCount() == 0 }, "tool EOF did not remove only its own session")
	time.Sleep(250 * time.Millisecond)
	if entry := f.d.Entry(resp.ServerID); entry == nil || entry.Owner != o || o.PendingRequests() != 1 {
		t.Fatal("releasing the tool reservation retired another host's actual pending work")
	}
	if err := os.WriteFile(filepath.Join(f.req.Cwd, "release-restart-work"), []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	completed := false
	for scanner.Scan() {
		var response struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) == nil && response.ID == "kept-work" && response.Result != nil {
			completed = true
			break
		}
	}
	if !completed {
		t.Fatal("native pending work did not complete for its original actual host")
	}
	managedRestartHostCheck(t, host, scanner, "", f.req.Cwd)
	_ = host.Close()
	managedRestartWait(t, func() bool { return f.d.Entry(resp.ServerID) == nil }, "ordinary cleanup did not resume after the actual host's final EOF")
}

// Forward every operation to the actual daemon before injecting a wire failure.
// Native binding/resource observations still come from the real session manager.
func managedRestartConfirmationProxy(t *testing.T, f *managedRestartFixture, endpoint string, change func(*control.Response)) string {
	t.Helper()
	path := filepath.Join(f.base, "confirmation.sock")
	listener, err := ipc.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				var req control.Request
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				resp, err := control.Send(endpoint, req)
				if err != nil {
					return
				}
				if req.Cmd == "can_suspend" && resp.Err() == nil {
					change(resp)
				}
				_ = json.NewEncoder(conn).Encode(resp)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("confirmation proxy did not settle")
		}
	})
	return path
}

func TestManagedRestartUntrustedConfirmationCannotReportSuccess(t *testing.T) {
	for _, failure := range []string{"missing_boolean", "unknown_denial", "contradictory_allow", "old_endpoint", "typed_refusal", "private_error"} {
		t.Run(failure, func(t *testing.T) {
			f := newManagedRestartFixture(t, "2026-07-28", true)
			endpoint, h := managedRestartEndpoint(t, f)
			proxy := managedRestartConfirmationProxy(t, f, endpoint, func(resp *control.Response) {
				switch failure {
				case "missing_boolean":
					resp.Data = json.RawMessage(`{"reason":"busy"}`)
				case "unknown_denial":
					resp.Data = json.RawMessage(`{"allowed":false,"reason":"unknown token"}`)
				case "contradictory_allow":
					resp.Data = json.RawMessage(`{"allowed":true,"reason":"owner gone"}`)
				case "old_endpoint":
					*resp = control.Response{Message: "unknown command: can_suspend"}
				case "typed_refusal":
					*resp = control.Response{ErrorCode: control.ErrMaintenanceHeld.Code, Message: "private token " + f.token}
				case "private_error":
					*resp = control.Response{Message: "private token " + f.token}
				}
			})
			text, isError := managedRestartTool(t, f.base, proxy, f.initial.Owner.ServerID())
			resp := <-h.replies
			if !isError || strings.Contains(text, f.token) || strings.Contains(text, resp.Token) || strings.Contains(text, "private token") {
				t.Fatal("untrusted confirmation reported success or exposed its private error/token")
			}
			if failure == "typed_refusal" {
				var result control.Response
				if json.Unmarshal([]byte(text), &result) != nil || !errors.Is(result.Err(), control.ErrMaintenanceHeld) {
					t.Fatal("confirmation lost its typed refusal")
				}
			}
			o := managedRestartConsumed(t, h, resp)
			managedRestartWait(t, func() bool { return o.SessionCount() == 0 && o.SessionMgr().PendingCount() == 0 }, "failed confirmation leaked the already-bound IPC resource")
			if entry := f.d.Entry(resp.ServerID); entry == nil || entry.Owner != o || !entry.Persistent || h.externalSpawns.Load() != 0 || h.externalStops.Load() != 0 {
				t.Fatal("confirmation failure killed or replaced retained native authority")
			}
		})
	}
}
