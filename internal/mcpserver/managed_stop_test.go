package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

type managedStopHandler struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (h *managedStopHandler) wait(ctx context.Context, project muxcore.ProjectContext) error {
	h.started <- struct{}{}
	select {
	case <-ctx.Done():
		h.cancelled <- struct{}{}
		<-h.release
	case <-h.release:
	}
	return os.WriteFile(filepath.Join(project.Cwd, "completed-callback"), []byte("completed"), 0o600)
}

func (h *managedStopHandler) HandleRequest(ctx context.Context, project muxcore.ProjectContext, raw []byte) ([]byte, error) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "stop-boundary", "version": "1"}}
	case "stop/wait":
		if err := h.wait(ctx, project); err != nil {
			return nil, err
		}
		result = map[string]any{"completed": true}
	default:
		return nil, fmt.Errorf("unexpected stop fixture method %q", request.Method)
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", request.ID, result})
}

func (h *managedStopHandler) HandleNotification(ctx context.Context, project muxcore.ProjectContext, raw []byte) {
	var notification struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(raw, &notification) == nil && notification.Method == "stop/notify-wait" {
		_ = h.wait(ctx, project)
	}
}

type managedStopDaemonControl struct {
	*daemon.Daemon
	stops   chan control.Request
	listed  chan control.ListOwnersResponse
	release <-chan struct{}
}

func (h *managedStopDaemonControl) HandleListOwners(req control.Request) (control.ListOwnersResponse, error) {
	list, err := h.Daemon.HandleListOwners(req)
	if h.listed != nil {
		h.listed <- list
		<-h.release
	}
	return list, err
}

func (h *managedStopDaemonControl) HandleStopOwner(req control.Request) (string, error) {
	h.stops <- req
	return h.Daemon.HandleStopOwner(req)
}

type managedStopOwnerControl struct {
	*owner.Owner
	shutdowns chan int
}

func (h *managedStopOwnerControl) HandleShutdown(drain int) string {
	h.shutdowns <- drain
	return h.Owner.HandleShutdown(drain)
}

func managedStopFixture(t *testing.T) (*daemon.Daemon, *owner.Owner, net.Conn, *managedStopHandler, string, func()) {
	t.Helper()
	profile := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, profile)
	}
	base := shortBaseDir(t, "ms-")
	handler := &managedStopHandler{started: make(chan struct{}, 2), cancelled: make(chan struct{}, 2), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(handler.release) }) }
	d, err := daemon.New(daemon.Config{
		ControlPath: filepath.Join(base, "actual-daemon.sock"), Namespace: "stop-" + filepath.Base(base),
		SkipSnapshot: true, Persistent: true, SessionHandler: handler, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		d.Shutdown()
		select {
		case <-d.Done():
		case <-time.After(10 * time.Second):
			t.Error("stop fixture daemon did not release its own resources")
		}
	})
	cwd := t.TempDir()
	path, sid, token, err := d.Spawn(control.Request{Cmd: "spawn", Command: "stop-boundary-native", Cwd: cwd, Mode: "cwd"})
	if err != nil {
		t.Fatal(err)
	}
	o := d.Entry(sid).Owner
	conn, err := ipc.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintln(conn, token); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"stop-boundary","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() || !bytes.Contains(scanner.Bytes(), []byte(`"result"`)) {
		t.Fatalf("actual native owner did not initialize: %s %v", scanner.Bytes(), scanner.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	return d, o, conn, handler, base, release
}

func managedStopEndpoints(t *testing.T, base string, d *daemon.Daemon, o *owner.Owner, h *managedStopDaemonControl) (string, *managedStopOwnerControl) {
	t.Helper()
	endpoint := serverid.DaemonControlPath(base, "mcp-mux")
	srv, err := control.NewServer(endpoint, h, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return endpoint, managedStopLegacyEndpoint(t, base, o)
}

func managedStopLegacyEndpoint(t *testing.T, base string, o *owner.Owner) *managedStopOwnerControl {
	t.Helper()
	legacy := &managedStopOwnerControl{Owner: o, shutdowns: make(chan int, 4)}
	srv, err := control.NewServer(serverid.ControlPath(base, "mcp-mux", o.ServerID()), legacy, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return legacy
}

type managedStopOutcome struct {
	output []byte
	err    error
}

func managedStopStart(base, endpoint, sid string, force bool) <-chan managedStopOutcome {
	done := make(chan managedStopOutcome, 1)
	go func() {
		input := fmt.Sprintf(`{"jsonrpc":"2.0","id":"original-stop-id","method":"tools/call","params":{"name":"mux_stop","arguments":{"server_id":%q,"force":%t}}}`+"\n", sid, force)
		var output bytes.Buffer
		srv := NewServer(strings.NewReader(input), &output, log.New(io.Discard, "", 0))
		srv.BaseDir, srv.DaemonCtlPath = base, endpoint
		err := srv.Run()
		done <- managedStopOutcome{output.Bytes(), err}
	}()
	return done
}

func managedStopRead(t *testing.T, done <-chan managedStopOutcome) (string, bool) {
	t.Helper()
	var outcome managedStopOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("actual tools/call mux_stop did not return")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	decoder := json.NewDecoder(bytes.NewReader(outcome.output))
	var frame struct {
		ID     string `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := decoder.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.ID != "original-stop-id" || len(frame.Result.Content) != 1 {
		t.Fatalf("mux_stop changed original tool framing: %+v", frame)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("mux_stop emitted duplicate original-ID responses: %v %v", extra, err)
	}
	return frame.Result.Content[0].Text, frame.Result.IsError
}

func TestManagedMuxStopHoldingAndStaleListAdmission(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, stale := range []bool{false, true} {
			for _, force := range []bool{false, true} {
				t.Run(fmt.Sprintf("pending_%t/stale_%t/force_%t", pending, stale, force), func(t *testing.T) {
					d, o, conn, handler, base, releaseWork := managedStopFixture(t)
					if pending {
						fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"original-work","method":"stop/wait","params":{}}`)
					} else {
						fmt.Fprintln(conn, `{"jsonrpc":"2.0","method":"stop/notify-wait","params":{}}`)
					}
					select {
					case <-handler.started:
					case <-time.After(5 * time.Second):
						t.Fatal("actual owner callback did not reserve work")
					}
					entry := d.Entry(o.ServerID())
					h := &managedStopDaemonControl{Daemon: d, stops: make(chan control.Request, 4)}
					listRelease := make(chan struct{})
					var listOnce sync.Once
					if stale {
						h.listed, h.release = make(chan control.ListOwnersResponse, 1), listRelease
						t.Cleanup(func() { listOnce.Do(func() { close(listRelease) }) })
					}
					endpoint, legacy := managedStopEndpoints(t, base, d, o, h)
					var stopped <-chan managedStopOutcome
					if stale {
						stopped = managedStopStart(base, endpoint, o.ServerID(), force)
						select {
						case snapshot := <-h.listed:
							if len(snapshot.Owners) != 1 || snapshot.Owners[0].Maintenance != nil || snapshot.Owners[0].Sessions != 1 {
								t.Fatalf("barrier did not capture real pre-hold list snapshot: %+v", snapshot)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("mux_stop did not reach list snapshot barrier")
						}
					}
					type heldOutcome struct {
						result control.MaintenanceResult
						err    error
					}
					held := make(chan heldOutcome, 1)
					var before control.MaintenanceResult
					holdReleased := false
					t.Cleanup(func() {
						if holdReleased {
							return
						}
						releaseWork()
						select {
						case result, ok := <-held:
							if ok {
								before = result.result
							}
						case <-time.After(10 * time.Second):
							t.Error("stop fixture hold did not settle after callback release")
						}
						if before.HoldID != "" {
							_, _ = d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: before.HoldID})
						}
					})
					go func() {
						defer close(held)
						ttl := int64(600000)
						result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: o.ServerID(), DrainTimeoutMs: 30000, HoldTTLMS: &ttl})
						held <- heldOutcome{result, err}
					}()
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						list, err := d.HandleListOwners(control.Request{Cmd: "list_owners"})
						if err != nil {
							t.Fatal(err)
						}
						if len(list.Owners) == 1 && list.Owners[0].Maintenance != nil {
							before = *list.Owners[0].Maintenance
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
					if before.State != control.MaintenanceHolding || before.TreesRetired || !o.IsAccepting() {
						t.Fatalf("actual hold did not commit HOLDING before callback retirement: %+v", before)
					}
					if stale {
						listOnce.Do(func() { close(listRelease) })
					} else {
						stopped = managedStopStart(base, endpoint, o.ServerID(), force)
					}
					text, isError := managedStopRead(t, stopped)
					if len(legacy.shutdowns) != 0 {
						t.Fatal("mux_stop invoked actual Owner.HandleShutdown after committed HOLDING")
					}
					var refusal control.Response
					if err := json.Unmarshal([]byte(text), &refusal); err != nil || !isError || !errors.Is(refusal.Err(), control.ErrMaintenanceHeld) || refusal.Maintenance == nil || *refusal.Maintenance != before {
						t.Fatalf("actual mux_stop lost typed refusal/original hold clocks: %q %+v %v", text, refusal, err)
					}
					if len(h.stops) != 1 || len(legacy.shutdowns) != 0 || len(handler.cancelled) != 0 || d.Entry(o.ServerID()) != entry || !o.IsAccepting() {
						t.Fatalf("refused stop bypassed registry or tore down callback: daemon=%d owner=%d cancelled=%d", len(h.stops), len(legacy.shutdowns), len(handler.cancelled))
					}
					request := <-h.stops
					wantDrain := 30000
					if force {
						wantDrain = 0
					}
					if request.ServerID != o.ServerID() || request.DrainTimeoutMs != wantDrain {
						t.Fatalf("mux_stop changed original soft/hard drain: %+v", request)
					}
					releaseWork()
					select {
					case result := <-held:
						if result.err != nil || result.result.State != control.MaintenanceHeld || !result.result.DrainDeadline.Equal(before.DrainDeadline) || !result.result.ExpiresAt.Equal(before.ExpiresAt) {
							t.Fatalf("original maintenance retirement lost authority or clocks: %+v %v", result.result, result.err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("maintenance finalization deadlocked after operator refusal")
					}
					cwd, _ := o.Status()["cwd"].(string)
					if _, err := os.Stat(filepath.Join(cwd, "completed-callback")); err != nil {
						t.Fatalf("admitted callback's actual filesystem work did not finish: %v", err)
					}
					if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: before.HoldID}); err != nil {
						t.Fatal(err)
					}
					holdReleased = true
				})
			}
		}
	}
}

func TestManagedMuxStopLiveOwnerUsesDaemonOnce(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force_%t", force), func(t *testing.T) {
			d, o, _, _, base, _ := managedStopFixture(t)
			h := &managedStopDaemonControl{Daemon: d, stops: make(chan control.Request, 2)}
			endpoint, legacy := managedStopEndpoints(t, base, d, o, h)
			text, isError := managedStopRead(t, managedStopStart(base, endpoint, o.ServerID(), force))
			if isError || len(h.stops) != 1 || len(legacy.shutdowns) != 0 || d.Entry(o.ServerID()) != nil || o.IsAccepting() {
				t.Fatalf("live stop did not retire once through daemon: %q daemon=%d owner=%d", text, len(h.stops), len(legacy.shutdowns))
			}
			request := <-h.stops
			wantDrain := 30000
			if force {
				wantDrain = 0
			}
			if request.ServerID != o.ServerID() || request.DrainTimeoutMs != wantDrain {
				t.Fatalf("normal live stop changed drain semantics: %+v", request)
			}
		})
	}
}

func TestManagedMuxStopContactedDaemonFallbackBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		fallback   bool
	}{
		{name: "uncertain_disconnect"},
		{name: "malformed_json", wire: "{invalid}\n"},
		{name: "malformed_null", wire: "null\n"},
		{name: "untyped_stop_failure", wire: `{"ok":false,"message":"stop_owner failed: tree proof unavailable"}` + "\n"},
		{name: "misleading_unsupported", wire: `{"ok":false,"message":"stop_owner failed: unknown command: stop_owner"}` + "\n"},
		{name: "typed_unsupported_text", wire: `{"ok":false,"message":"unknown command: stop_owner","error_code":"maintenance_held"}` + "\n"},
		{name: "old_unknown_command", wire: `{"ok":false,"message":"unknown command: stop_owner"}` + "\n", fallback: true},
		{name: "old_unsupported_command", wire: `{"ok":false,"message":"stop_owner not supported (not a daemon)"}` + "\n", fallback: true},
	} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force_%t", tc.name, force), func(t *testing.T) {
				d, o, _, _, base, _ := managedStopFixture(t)
				legacy := managedStopLegacyEndpoint(t, base, o)
				endpoint := serverid.DaemonControlPath(base, "mcp-mux")
				listener, err := ipc.Listen(endpoint)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { listener.Close() })
				requests := make(chan control.Request, 2)
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
						var request control.Request
						if json.NewDecoder(conn).Decode(&request) == nil {
							if request.Cmd == "list_owners" {
								list, err := d.HandleListOwners(request)
								if err == nil {
									data, _ := json.Marshal(list)
									_ = json.NewEncoder(conn).Encode(control.Response{OK: true, Data: data})
								}
							} else {
								requests <- request
								_, _ = io.WriteString(conn, tc.wire)
							}
						}
						_ = conn.Close()
					}
				}()
				text, isError := managedStopRead(t, managedStopStart(base, endpoint, o.ServerID(), force))
				if !tc.fallback && len(legacy.shutdowns) != 0 {
					t.Fatal("contacted daemon uncertainty invoked actual Owner.HandleShutdown")
				}
				if len(requests) != 1 || isError == tc.fallback {
					t.Fatalf("contacted daemon outcome was lost or bypassed: %q isError=%t requests=%d", text, isError, len(requests))
				}
				request := <-requests
				if request.Cmd != "stop_owner" || request.ServerID != o.ServerID() {
					t.Fatalf("incorrect original stop request: %+v", request)
				}
				if !tc.fallback {
					if len(legacy.shutdowns) != 0 || !o.IsAccepting() || d.Entry(o.ServerID()) == nil {
						t.Fatal("uncertain/typed/malformed daemon result reached actual owner shutdown")
					}
				} else {
					if len(legacy.shutdowns) != 1 {
						t.Fatal("known old-daemon protocol did not use its single compatibility stop")
					}
					wantDrain := 30000
					if force {
						wantDrain = 0
					}
					if drain := <-legacy.shutdowns; drain != wantDrain {
						t.Fatalf("legacy fallback changed requested drain: %d want %d", drain, wantDrain)
					}
				}
			})
		}
	}
}
