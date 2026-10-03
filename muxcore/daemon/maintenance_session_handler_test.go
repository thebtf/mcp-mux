package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
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
