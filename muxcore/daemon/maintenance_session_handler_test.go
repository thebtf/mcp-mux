package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

type maintenanceCallbackBarrier struct {
	release   chan struct{}
	started   chan string
	cancelled chan string
	work      atomic.Int32
	returned  atomic.Int32
	openings  atomic.Int32
}

func (h *maintenanceCallbackBarrier) HandleRequest(ctx context.Context, _ muxcore.ProjectContext, raw []byte) ([]byte, error) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	var result any
	switch request.Method {
	case "initialize", "server/discover":
		h.openings.Add(1)
		result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "native-maintenance", "version": "1"}}
		if request.Method == "server/discover" {
			result.(map[string]any)["protocolVersion"] = "2026-07-28"
		}
	case "maintenance/native-wait", "maintenance/native-write":
		h.work.Add(1)
		if request.Method == "maintenance/native-wait" {
			h.started <- string(request.ID)
			select {
			case <-ctx.Done():
				h.cancelled <- string(request.ID)
				// Cancellation is deliberately not callback retirement.
				<-h.release
			case <-h.release:
			}
		}
		h.returned.Add(1)
		result = map[string]any{"completed": true}
	default:
		return nil, fmt.Errorf("unexpected native request %q", request.Method)
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", request.ID, result})
}

func TestMaintenanceSessionHandlerRetirementWaitsForActualCallbackReturn(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		for _, scenario := range []struct {
			name      string
			drainMS   int
			ttlMS     int64
			completes bool
		}{
			{name: "zero_drain", ttlMS: 60000},
			{name: "short_drain", drainMS: 150, ttlMS: 60000},
			{name: "expires_while_blocked", ttlMS: 100},
			{name: "finishes_before_deadline", drainMS: 3000, ttlMS: 60000, completes: true},
		} {
			t.Run(fmt.Sprintf("%v/%s", protocol, scenario.name), func(t *testing.T) {
				d := maintenanceDaemon(t)
				handler := &maintenanceCallbackBarrier{release: make(chan struct{}), started: make(chan string, 2), cancelled: make(chan string, 2)}
				d.sessionHandler = handler
				wire, _ := protocol.Wire()
				req := control.Request{Cmd: "spawn", Command: "native-maintenance-callback", Mode: "isolated", Cwd: t.TempDir(), ProtocolEra: wire}
				endpoint := d.ctlSrv.SocketPath()
				var admissions atomic.Int32
				spawn := func() (*control.Response, error) {
					response, err := control.SendWithTimeout(endpoint, req, 5*time.Second)
					if err != nil {
						return nil, err
					}
					if err := response.Err(); err != nil {
						return nil, err
					}
					if response.ProtocolEra != wire {
						return nil, era.NewAdmissionError(era.AdmissionControlEraMismatch)
					}
					admissions.Add(1)
					return response, nil
				}
				initial, err := spawn()
				if err != nil {
					t.Fatal(err)
				}
				entry := d.Entry(initial.ServerID)
				if entry == nil || entry.Owner == nil || entry.ProtocolEra != protocol {
					t.Fatal("control admission did not retain the exact native era")
				}
				identity := captureOwnerEntryIdentity(entry)
				hostIn, writer := io.Pipe()
				reader, hostOut := io.Pipe()
				frames := make(chan []byte, 32)
				go func() {
					defer close(frames)
					scanner := bufio.NewScanner(reader)
					for scanner.Scan() {
						frames <- append([]byte(nil), scanner.Bytes()...)
					}
				}()
				done := make(chan error, 1)
				var releaseOnce sync.Once
				t.Cleanup(func() {
					releaseOnce.Do(func() { close(handler.release) })
					_ = writer.Close()
					_ = hostIn.Close()
					_ = reader.Close()
					_ = hostOut.Close()
				})
				go func() {
					path, token := initial.IPCPath, initial.Token
					done <- owner.RunResilientClient(owner.ResilientClientConfig{
						Stdin: hostIn, Stdout: hostOut, InitialIPCPath: path, Token: token, ProtocolEra: protocol,
						ProbeGracePeriod: time.Nanosecond, ReconnectTimeout: time.Minute, Logger: testLogger(t),
						RefreshToken: func() (string, string, error) {
							response, err := control.SendWithTimeout(endpoint, control.Request{Cmd: "refresh-token", PrevToken: token, ProtocolEra: wire}, 5*time.Second)
							if err != nil {
								return "", "", err
							}
							if err := response.Err(); err != nil {
								return "", "", err
							}
							if response.ProtocolEra != wire {
								return "", "", era.NewAdmissionError(era.AdmissionControlEraMismatch)
							}
							token = response.Token
							return path, token, nil
						},
						Reconnect: func() (string, string, error) {
							response, err := spawn()
							if err != nil {
								return "", "", err
							}
							path, token = response.IPCPath, response.Token
							return path, token, nil
						},
					})
				}()
				seen := make(map[string][]byte)
				record := func(frame []byte) {
					t.Helper()
					var response struct {
						ID     json.RawMessage `json:"id"`
						Result json.RawMessage `json:"result"`
						Error  json.RawMessage `json:"error"`
					}
					if err := json.Unmarshal(frame, &response); err != nil || response.ID == nil || (response.Result == nil) == (response.Error == nil) {
						t.Fatalf("invalid terminal host frame: %s (%v)", frame, err)
					}
					id := string(response.ID)
					if seen[id] != nil {
						t.Fatalf("duplicate terminal response for original ID %s: %s then %s", id, seen[id], frame)
					}
					seen[id] = frame
				}
				read := func(ids ...string) {
					t.Helper()
					timer := time.NewTimer(5 * time.Second)
					defer timer.Stop()
					for _, id := range ids {
						for seen[id] == nil {
							select {
							case frame, ok := <-frames:
								if !ok {
									t.Fatal("original host transport closed")
								}
								record(frame)
							case <-timer.C:
								t.Fatalf("missing original-ID host response %s", id)
							}
						}
					}
				}
				send := func(id, method string) {
					t.Helper()
					params := map[string]any{}
					if protocol == era.EraModern20260728 {
						params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": wire, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
					} else if method == "initialize" {
						params = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "native-maintenance", "version": "1"}}
					}
					frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := fmt.Fprintf(writer, "%s\n", frame); err != nil {
						t.Fatal(err)
					}
				}
				opening := "initialize"
				if protocol == era.EraModern20260728 {
					opening = "server/discover"
				}
				send("1", opening)
				read("1")
				send("17", "maintenance/native-wait")
				send(`"unfinished-string"`, "maintenance/native-wait")
				for range 2 {
					select {
					case <-handler.started:
					case <-time.After(5 * time.Second):
						t.Fatal("actual native callback did not enter its barrier")
					}
				}
				if entry.Owner.PendingRequests() != 2 {
					t.Fatal("actual callback reservations were not counted")
				}
				type holdResponse struct {
					result *control.MaintenanceResult
					err    error
				}
				held := make(chan holdResponse, 1)
				go func() {
					result, err := control.SendMaintenance(endpoint, control.Request{Cmd: "hold", ServerID: initial.ServerID, DrainTimeoutMs: scenario.drainMS, HoldTTLMS: maintenanceTTL(scenario.ttlMS)}, 10*time.Second)
					held <- holdResponse{result, err}
				}()
				if scenario.completes {
					waitMaintenanceState(t, d, control.MaintenanceHolding)
					send("18", "maintenance/native-write")
					send(`"fenced-string"`, "maintenance/native-write")
					read("18", `"fenced-string"`)
					releaseOnce.Do(func() { close(handler.release) })
				}
				var result *control.MaintenanceResult
				select {
				case response := <-held:
					result = response.result
					if scenario.completes {
						if response.err != nil || result == nil || result.State != control.MaintenanceHeld || !result.TreesRetired {
							t.Fatalf("settled callback hold = %+v %v", result, response.err)
						}
					} else if !errors.Is(response.err, control.ErrMaintenanceRetirementBlocked) || result == nil || result.State != control.MaintenanceRetirementBlocked || result.TreesRetired {
						t.Fatalf("active native callbacks falsely retired: result=%+v err=%v pending=%d actual_work=%d returned=%d", result, response.err, entry.Owner.PendingRequests(), handler.work.Load(), handler.returned.Load())
					}
				case <-time.After(10 * time.Second):
					t.Fatal("native hold did not return a bounded retirement verdict")
				}
				if !scenario.completes {
					for range 2 {
						select {
						case <-handler.cancelled:
						case <-time.After(5 * time.Second):
							t.Fatal("owner teardown did not cancel the active callback context")
						}
					}
					send("18", "maintenance/native-write")
					send(`"fenced-string"`, "maintenance/native-write")
				}
				read("17", `"unfinished-string"`, "18", `"fenced-string"`)
				maintenanceResponseCode(t, seen["18"], "18", -32005)
				maintenanceResponseCode(t, seen[`"fenced-string"`], `"fenced-string"`, -32005)
				for _, id := range []string{"17", `"unfinished-string"`} {
					if scenario.completes {
						var response struct {
							Result struct{ Completed bool } `json:"result"`
						}
						if json.Unmarshal(seen[id], &response) != nil || !response.Result.Completed {
							t.Fatalf("pre-deadline callback result was lost: %s", seen[id])
						}
					} else {
						maintenanceResponseCode(t, seen[id], id, -32603)
					}
				}
				if !scenario.completes {
					delay := 250 * time.Millisecond
					if scenario.ttlMS == 100 {
						delay += max(time.Until(result.ExpiresAt), 0)
					}
					select {
					case err := <-done:
						t.Fatalf("original shim exited while callback authority remained: %v", err)
					case <-time.After(delay):
					}
					current := d.maintenanceResults()
					if len(current) != 1 || current[0].State != control.MaintenanceRetirementBlocked || current[0].TreesRetired || !current[0].DrainDeadline.Equal(result.DrainDeadline) || !current[0].ExpiresAt.Equal(result.ExpiresAt) {
						t.Fatalf("retry/TTL released or reclocked unfinished callback authority: %+v", current)
					}
					d.mu.RLock()
					retrying := entry.removalRetrying
					d.mu.RUnlock()
					if d.Entry(initial.ServerID) != entry || !identity.matches(entry) || !retrying || entry.Owner.MaintenanceRetired() || entry.Owner.PendingRequests() != 2 || handler.returned.Load() != 0 || entry.Owner.Status()["materialization_state"] != string(owner.MaterializationFinalizeBlocked) {
						t.Fatal("existing exact-entry retry lost active callback authority or its counter")
					}
					select {
					case <-entry.Owner.Done():
						t.Fatal("owner Done closed before the actual callback returned")
					default:
					}
					resumed, err := control.SendMaintenance(endpoint, control.Request{Cmd: "resume", HoldID: result.HoldID}, 5*time.Second)
					if !errors.Is(err, control.ErrMaintenanceRetirementBlocked) || resumed == nil || !resumed.DrainDeadline.Equal(result.DrainDeadline) || !resumed.ExpiresAt.Equal(result.ExpiresAt) {
						t.Fatalf("resume released or reclocked unfinished callback authority: %+v %v", resumed, err)
					}
					releaseOnce.Do(func() { close(handler.release) })
				}
				waitForDaemonCondition(t, 5*time.Second, func() bool {
					select {
					case <-entry.Owner.Done():
						return entry.Owner.PendingRequests() == 0 && entry.Owner.MaintenanceRetired() && d.Entry(initial.ServerID) == nil
					default:
						return false
					}
				}, "actual callback return did not let the existing exact retry retire its owner")
				if handler.work.Load() != 2 || handler.returned.Load() != 2 {
					t.Fatal("fenced work or orphan replay entered an actual callback")
				}
				if scenario.ttlMS == 100 {
					waitForDaemonCondition(t, 5*time.Second, func() bool { return len(d.maintenanceResults()) == 0 }, "expired blocked lease did not release after actual callback retirement")
				} else {
					waitMaintenanceState(t, d, control.MaintenanceHeld)
					current := d.maintenanceResults()[0]
					if !current.TreesRetired || !current.DrainDeadline.Equal(result.DrainDeadline) || !current.ExpiresAt.Equal(result.ExpiresAt) {
						t.Fatal("existing retirement retry resampled the accepted maintenance clock")
					}
					if _, err := control.SendMaintenance(endpoint, control.Request{Cmd: "resume", HoldID: result.HoldID}, 5*time.Second); err != nil {
						t.Fatal(err)
					}
				}
				send(`"fresh"`, "maintenance/native-write")
				read(`"fresh"`)
				var fresh struct {
					Result struct{ Completed bool } `json:"result"`
				}
				if json.Unmarshal(seen[`"fresh"`], &fresh) != nil || !fresh.Result.Completed {
					t.Fatalf("fresh same-pipe work failed: %s", seen[`"fresh"`])
				}
				waitForDaemonCondition(t, 5*time.Second, func() bool {
					d.mu.RLock()
					defer d.mu.RUnlock()
					for _, next := range d.owners {
						return len(d.owners) == 1 && next.Owner != entry.Owner && next.ProtocolEra == protocol && next.Owner.PendingRequests() == 0
					}
					return false
				}, "fresh work did not settle on exactly one same-era successor")
				if handler.work.Load() != 3 || handler.returned.Load() != 3 || handler.openings.Load() != 1 || admissions.Load() != 2 {
					t.Fatalf("unexpected actual replay/admission: work=%d returned=%d openings=%d admissions=%d", handler.work.Load(), handler.returned.Load(), handler.openings.Load(), admissions.Load())
				}
				_ = writer.Close()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("original shim did not settle on host EOF")
				}
				_ = hostOut.Close()
				for frame := range frames {
					record(frame)
				}
				if len(seen) != 6 {
					t.Fatalf("host terminal responses = %d, want exactly six original IDs", len(seen))
				}
			})
		}
	}
}

type maintenanceNativeBarrier struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	entered   atomic.Int32
	returned  atomic.Int32
	once      sync.Once
}

func newMaintenanceNativeBarrier(t *testing.T) *maintenanceNativeBarrier {
	t.Helper()
	b := &maintenanceNativeBarrier{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(b.open)
	return b
}

func (b *maintenanceNativeBarrier) open() { b.once.Do(func() { close(b.release) }) }

func (b *maintenanceNativeBarrier) wait(ctx context.Context) {
	b.entered.Add(1)
	close(b.started)
	if ctx == nil {
		<-b.release
	} else {
		select {
		case <-ctx.Done():
			close(b.cancelled)
			<-b.release // deliberately ignore cancellation until actual cleanup
		case <-b.release:
		}
	}
	b.returned.Add(1)
}

type maintenanceNotificationWork struct {
	maintenanceCallbackBarrier
	barrier    *maintenanceNativeBarrier
	ordinary   atomic.Int32
	metadata   atomic.Int32
	badMeta    atomic.Bool
	badContext atomic.Bool
}

func (h *maintenanceNotificationWork) HandleNotification(ctx context.Context, _ muxcore.ProjectContext, _ []byte) {
	h.ordinary.Add(1)
	if _, deadline := ctx.Deadline(); deadline {
		h.badContext.Store(true)
	}
	h.barrier.wait(ctx)
}

type maintenanceMetadataNotificationWork struct{ *maintenanceNotificationWork }

func (h *maintenanceMetadataNotificationWork) HandleNotificationWithSessionMeta(ctx context.Context, project muxcore.ProjectContext, meta muxcore.SessionMeta, _ []byte) {
	h.metadata.Add(1)
	if _, deadline := ctx.Deadline(); deadline {
		h.badContext.Store(true)
	}
	if project.Cwd == "" || project.ID != muxcore.ProjectContextID(project.Cwd) || meta.Conn.Platform == "" {
		h.badMeta.Store(true)
	}
	h.barrier.wait(ctx)
}

type maintenanceLifecycleWork struct {
	maintenanceCallbackBarrier
	barrier      *maintenanceNativeBarrier
	blockConnect bool
	connects     atomic.Int32
	disconnects  atomic.Int32
}

func (h *maintenanceLifecycleWork) OnProjectConnect(muxcore.ProjectContext) {
	h.connects.Add(1)
	if h.blockConnect {
		h.barrier.wait(nil)
	}
}

func (h *maintenanceLifecycleWork) OnProjectDisconnect(string) {
	h.disconnects.Add(1)
	if !h.blockConnect {
		h.barrier.wait(nil)
	}
}

func maintenanceNativeIPC(t *testing.T, d *Daemon, protocol era.ProtocolEra) (*control.Response, *OwnerEntry, net.Conn, <-chan []byte) {
	t.Helper()
	wire, _ := protocol.Wire()
	response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "spawn", Command: "native-nonrequest-maintenance", Mode: "isolated", Cwd: t.TempDir(), ProtocolEra: wire}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Err(); err != nil {
		t.Fatal(err)
	}
	entry := d.Entry(response.ServerID)
	if entry == nil || entry.Owner == nil || entry.ProtocolEra != protocol || response.ProtocolEra != wire {
		t.Fatal("native fixture did not retain exact era and entry")
	}
	conn, err := ipc.Dial(response.IPCPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	frames := make(chan []byte, 16)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			frames <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	if _, err := fmt.Fprintf(conn, "%s\n", response.Token); err != nil {
		t.Fatal(err)
	}
	return response, entry, conn, frames
}

func maintenanceNativeSend(t *testing.T, conn net.Conn, protocol era.ProtocolEra, id, method string) {
	t.Helper()
	params := map[string]any{}
	if protocol == era.EraModern20260728 {
		wire, _ := protocol.Wire()
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": wire, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	} else if method == "initialize" {
		params = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "native-notification", "version": "1"}}
	}
	frame := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != "" {
		frame["id"] = json.RawMessage(id)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", raw); err != nil {
		t.Fatal(err)
	}
}

func maintenanceNativeRead(t *testing.T, frames <-chan []byte, id string) []byte {
	t.Helper()
	select {
	case raw, ok := <-frames:
		var response struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if !ok || json.Unmarshal(raw, &response) != nil || string(response.ID) != id || response.Result == nil || response.Error != nil {
			t.Fatalf("original-ID native result changed: wanted=%s raw=%s", id, raw)
		}
		return raw
	case <-time.After(5 * time.Second):
		t.Fatalf("missing original-ID response %s", id)
		return nil
	}
}

func maintenanceNativeNoReply(t *testing.T, frames <-chan []byte) {
	t.Helper()
	select {
	case raw, ok := <-frames:
		if ok {
			t.Fatalf("native notification acquired a reply or consumed work replayed: %s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retired native IPC reader did not close")
	}
}

func maintenanceNativeEntered(t *testing.T, b *maintenanceNativeBarrier) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("actual native callback did not enter")
	}
}

func maintenanceNativeRetained(t *testing.T, d *Daemon, entry *OwnerEntry) {
	t.Helper()
	d.mu.RLock()
	retrying := entry.removalRetrying
	d.mu.RUnlock()
	if d.Entry(entry.ServerID) != entry || !retrying || entry.Owner.MaintenanceRetired() || entry.Owner.PendingRequests() != 0 || entry.Owner.MaterializationState() != owner.MaterializationFinalizeBlocked {
		t.Fatal("unsettled non-request work lost exact-entry retry or polluted public request metrics")
	}
	select {
	case <-entry.Owner.Done():
		t.Fatal("native Done closed while user code was running")
	default:
	}
}

func maintenanceNativeSettled(t *testing.T, d *Daemon, entry *OwnerEntry, result *control.MaintenanceResult, b *maintenanceNativeBarrier) {
	t.Helper()
	waitForDaemonCondition(t, 5*time.Second, func() bool {
		return d.Entry(entry.ServerID) == nil && entry.Owner.MaintenanceRetired() && entry.Owner.PendingRequests() == 0
	}, "actual native return did not settle through the existing exact-entry retry")
	waitMaintenanceState(t, d, control.MaintenanceHeld)
	current := d.maintenanceResults()[0]
	if !current.TreesRetired || current.HoldID != result.HoldID || !current.DrainDeadline.Equal(result.DrainDeadline) || !current.ExpiresAt.Equal(result.ExpiresAt) || b.entered.Load() != 1 || b.returned.Load() != 1 {
		t.Fatalf("settlement replayed work or reclocked the original lease: %+v", current)
	}
	select {
	case <-entry.Owner.Done():
	default:
		t.Fatal("settled owner Done remained open")
	}
}

func TestMaintenanceNativeNonRequestWorkRetainsAuthority(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		for _, kind := range []string{"notification", "metadata_notification", "connect", "disconnect", "authorize", "frame_hook"} {
			if protocol == era.EraModern20260728 && (kind == "notification" || kind == "metadata_notification") {
				continue
			}
			for _, completes := range []bool{false, true} {
				if completes && kind != "notification" && kind != "metadata_notification" {
					continue
				}
				t.Run(fmt.Sprintf("era_%d/%s/completes_%t", protocol, kind, completes), func(t *testing.T) {
					d := maintenanceDaemon(t)
					b := newMaintenanceNativeBarrier(t)
					var notification *maintenanceNotificationWork
					var lifecycle *maintenanceLifecycleWork
					d.sessionHandler = &maintenanceCallbackBarrier{}
					switch kind {
					case "notification", "metadata_notification":
						notification = &maintenanceNotificationWork{barrier: b}
						d.sessionHandler = notification
						if kind == "metadata_notification" {
							d.sessionHandler = &maintenanceMetadataNotificationWork{notification}
						}
					case "connect", "disconnect":
						lifecycle = &maintenanceLifecycleWork{barrier: b, blockConnect: kind == "connect"}
						d.sessionHandler = lifecycle
					case "authorize":
						d.authorizeSession = func(ctx context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
							b.wait(ctx)
							return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: "native-tenant"}
						}
					case "frame_hook":
						d.onFrameReceived = func(_ string, _ int, method string) muxcore.FrameAction {
							if method == "maintenance/native-write" {
								b.wait(nil)
								return muxcore.FrameError // late verdict must remain discarded
							}
							return muxcore.FramePass
						}
					}
					initial, entry, conn, frames := maintenanceNativeIPC(t, d, protocol)
					identity := captureOwnerEntryIdentity(entry)
					if kind != "authorize" {
						opening := "initialize"
						if protocol == era.EraModern20260728 {
							opening = "server/discover"
						}
						maintenanceNativeSend(t, conn, protocol, "1", opening)
						maintenanceNativeRead(t, frames, "1")
					}
					switch kind {
					case "notification", "metadata_notification":
						maintenanceNativeSend(t, conn, protocol, "", "notifications/cancelled")
						maintenanceNativeSend(t, conn, protocol, "", "maintenance/native-notification")
					case "disconnect":
						_ = conn.Close()
					case "frame_hook":
						maintenanceNativeSend(t, conn, protocol, `"hook-string"`, "maintenance/native-write")
						maintenanceNativeRead(t, frames, `"hook-string"`)
					}
					maintenanceNativeEntered(t, b)
					waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.PendingRequests() == 0 }, "non-request callback polluted PendingRequests")
					type holdResponse struct {
						result *control.MaintenanceResult
						err    error
					}
					response := make(chan holdResponse, 1)
					drainMS := 0
					if completes {
						drainMS = 3000
					}
					go func() {
						result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: initial.ServerID, DrainTimeoutMs: drainMS, HoldTTLMS: maintenanceTTL(60000)}, 10*time.Second)
						response <- holdResponse{result, err}
					}()
					if completes {
						waitMaintenanceState(t, d, control.MaintenanceHolding)
						select {
						case early := <-response:
							t.Fatalf("positive-grace hold finished before actual native return: %+v %v", early.result, early.err)
						default:
						}
						// The fence cannot dispatch a second notification or invent a reply.
						maintenanceNativeSend(t, conn, protocol, "", "maintenance/fenced-notification")
						b.open()
					}
					var held holdResponse
					select {
					case held = <-response:
					case <-time.After(10 * time.Second):
						t.Fatal("native hold did not return its bounded verdict")
					}
					if completes {
						if held.err != nil || held.result == nil || held.result.State != control.MaintenanceHeld || !held.result.TreesRetired {
							t.Fatalf("positive-grace native completion failed: %+v %v", held.result, held.err)
						}
					} else {
						if !errors.Is(held.err, control.ErrMaintenanceRetirementBlocked) || held.result == nil || held.result.State != control.MaintenanceRetirementBlocked || held.result.TreesRetired {
							t.Fatalf("active non-request work falsely retired: %+v %v", held.result, held.err)
						}
						maintenanceNativeRetained(t, d, entry)
						if !identity.matches(entry) || b.returned.Load() != 0 {
							t.Fatal("active native generation identity changed")
						}
						if kind == "notification" || kind == "metadata_notification" || kind == "authorize" {
							select {
							case <-b.cancelled:
							case <-time.After(5 * time.Second):
								t.Fatal("native context did not observe teardown while Done was withheld")
							}
						}
						// No resume/renew/second hold: only actual callback return may settle it.
						b.open()
					}
					maintenanceNativeSettled(t, d, entry, held.result, b)
					maintenanceNativeNoReply(t, frames)
					if notification != nil {
						ordinary, metadata := int32(1), int32(0)
						if kind == "metadata_notification" {
							ordinary, metadata = 0, 1
						}
						if notification.ordinary.Load() != ordinary || notification.metadata.Load() != metadata || notification.badMeta.Load() || notification.badContext.Load() {
							t.Fatal("notification interface precedence or session metadata changed")
						}
					}
					if lifecycle != nil && (lifecycle.connects.Load() != 1 || lifecycle.disconnects.Load() != 1) {
						t.Fatal("native lifecycle dispatch was dropped or replayed")
					}
				})
			}
		}
	}
}

func TestMaintenanceModernNativeNotificationRemainsNonDispatch(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata_%t", metadata), func(t *testing.T) {
			d := maintenanceDaemon(t)
			b := newMaintenanceNativeBarrier(t)
			h := &maintenanceNotificationWork{barrier: b}
			d.sessionHandler = h
			if metadata {
				d.sessionHandler = &maintenanceMetadataNotificationWork{h}
			}
			initial, entry, conn, frames := maintenanceNativeIPC(t, d, era.EraModern20260728)
			maintenanceNativeSend(t, conn, era.EraModern20260728, "", "maintenance/native-notification")
			maintenanceNativeSend(t, conn, era.EraModern20260728, "", "notifications/cancelled")
			maintenanceNativeSend(t, conn, era.EraModern20260728, "7", "server/discover")
			maintenanceNativeRead(t, frames, "7") // same reader has passed both notifications
			result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: initial.ServerID, HoldTTLMS: maintenanceTTL(60000)}, 5*time.Second)
			if err != nil || result == nil || result.State != control.MaintenanceHeld || !entry.Owner.MaintenanceRetired() || h.ordinary.Load() != 0 || h.metadata.Load() != 0 || b.entered.Load() != 0 {
				t.Fatalf("modern native notification gained dispatch: %+v %v", result, err)
			}
			maintenanceNativeNoReply(t, frames)
		})
	}
}

func TestMaintenancePinsOrdinaryNativeRetirementBeforeRegistryDeletion(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		t.Run(fmt.Sprintf("era_%d", protocol), func(t *testing.T) {
			d := maintenanceDaemon(t)
			b := newMaintenanceNativeBarrier(t)
			if protocol == era.EraLegacy {
				d.sessionHandler = &maintenanceNotificationWork{barrier: b}
			} else {
				d.sessionHandler = &maintenanceLifecycleWork{barrier: b, blockConnect: true}
			}
			initial, entry, conn, frames := maintenanceNativeIPC(t, d, protocol)
			if protocol == era.EraLegacy {
				maintenanceNativeSend(t, conn, protocol, "1", "initialize")
				maintenanceNativeRead(t, frames, "1")
				maintenanceNativeSend(t, conn, protocol, "", "maintenance/native-notification")
			}
			maintenanceNativeEntered(t, b)
			original := finalizeOwnerForRemoval
			paused, proceed := make(chan struct{}), make(chan struct{})
			var pausedOnce atomic.Bool
			var proceedOnce sync.Once
			unpause := func() { proceedOnce.Do(func() { close(proceed) }) }
			finalizeOwnerForRemoval = func(o *owner.Owner, soft bool) (int, bool, error) {
				code, finalized, err := original(o, soft) // REAL finalization, never an invented result
				if o == entry.Owner && pausedOnce.CompareAndSwap(false, true) {
					close(paused)
					<-proceed // transaction has not reacquired d.mu for deletion
				}
				return code, finalized, err
			}
			t.Cleanup(func() {
				unpause()
				b.open()
				waitForDaemonCondition(t, 5*time.Second, func() bool { return d.Entry(initial.ServerID) == nil }, "ordinary remover did not settle during cleanup")
				finalizeOwnerForRemoval = original
			})
			removed := make(chan error, 1)
			go func() { removed <- d.Remove(initial.ServerID) }()
			select {
			case <-paused:
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary remover did not reach post-real-finalizer seam")
			}
			type holdResponse struct {
				result *control.MaintenanceResult
				err    error
			}
			held := make(chan holdResponse, 1)
			go func() {
				result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: initial.ServerID, HoldTTLMS: maintenanceTTL(60000)}, 10*time.Second)
				held <- holdResponse{result, err}
			}()
			waitMaintenanceState(t, d, control.MaintenanceHolding)
			d.maintenanceGate.RLock()
			pinned := false
			for _, lease := range d.maintenanceLeases {
				for _, pin := range lease.pins {
					pinned = pinned || pin.entry == entry
				}
			}
			d.maintenanceGate.RUnlock()
			if !pinned {
				t.Fatal("hold did not pin the exact ordinary-removal entry")
			}
			unpause()
			select {
			case err := <-removed:
				if err == nil {
					t.Error("ordinary removal reported completion while native user code remained")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary removal did not return a bounded blocked verdict")
			}
			var result holdResponse
			select {
			case result = <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("pinned hold did not settle its removal attempt")
			}
			if !errors.Is(result.err, control.ErrMaintenanceRetirementBlocked) || result.result == nil || result.result.State != control.MaintenanceRetirementBlocked || result.result.TreesRetired {
				t.Fatalf("ordinary-removal gap falsely granted HELD: %+v %v", result.result, result.err)
			}
			maintenanceNativeRetained(t, d, entry)
			b.open()
			maintenanceNativeSettled(t, d, entry, result.result, b)
			maintenanceNativeNoReply(t, frames)
		})
	}
}

type maintenanceNotifierConstructor struct {
	maintenanceCallbackBarrier
	barrier  *maintenanceNativeBarrier
	notifier muxcore.Notifier
}

func (h *maintenanceNotifierConstructor) SetNotifier(n muxcore.Notifier) {
	h.notifier = n
	h.barrier.wait(nil)
}

func TestMaintenanceNativeSetNotifierUsesExistingCreatingBarrier(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		t.Run(fmt.Sprintf("era_%d", protocol), func(t *testing.T) {
			d := maintenanceDaemon(t)
			b := newMaintenanceNativeBarrier(t)
			h := &maintenanceNotifierConstructor{barrier: b}
			d.sessionHandler = h
			wire, _ := protocol.Wire()
			spawned := make(chan error, 1)
			cwd := t.TempDir()
			go func() {
				_, _, _, err := d.Spawn(control.Request{Command: "native-constructor", Mode: "isolated", Cwd: cwd, ProtocolEra: wire})
				spawned <- err
			}()
			maintenanceNativeEntered(t, b)
			d.mu.RLock()
			var sid string
			for id := range d.owners {
				sid = id
			}
			d.mu.RUnlock()
			type holdResponse struct {
				result *control.MaintenanceResult
				err    error
			}
			held := make(chan holdResponse, 1)
			go func() {
				result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: sid, DrainTimeoutMs: 3000, HoldTTLMS: maintenanceTTL(60000)}, 5*time.Second)
				held <- holdResponse{result, err}
			}()
			waitMaintenanceState(t, d, control.MaintenanceHolding)
			select {
			case response := <-held:
				t.Fatalf("constructor escaped existing creating barrier: %+v %v", response.result, response.err)
			default:
			}
			b.open()
			select {
			case err := <-spawned:
				if !errors.Is(err, control.ErrMaintenanceHeld) {
					t.Fatalf("late native construction was published: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("native constructor did not return")
			}
			var response holdResponse
			select {
			case response = <-held:
			case <-time.After(5 * time.Second):
				t.Fatal("hold did not observe native constructor settlement")
			}
			result := response.result
			if response.err != nil || result == nil || result.State != control.MaintenanceHeld || !result.TreesRetired {
				t.Fatalf("settled native constructor hold = %+v %v", result, response.err)
			}
			waitMaintenanceState(t, d, control.MaintenanceHeld)
			current := d.maintenanceResults()[0]
			if !current.TreesRetired || !current.DrainDeadline.Equal(result.DrainDeadline) || !current.ExpiresAt.Equal(result.ExpiresAt) || h.notifier == nil || b.entered.Load() != 1 || b.returned.Load() != 1 {
				t.Fatal("existing constructor settlement replayed construction or reclocked lease")
			}
		})
	}
}
