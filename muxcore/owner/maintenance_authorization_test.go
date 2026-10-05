package owner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

func maintenanceAuthorizationStream(_ context.Context, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return err
		}
		if len(request.ID) == 0 {
			continue
		}
		result := `{}`
		switch request.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"authorization-fixture","version":"1"}}`
		case "tools/list":
			result = `{"tools":[]}`
		}
		if err := writeControllerResponse(stdout, request.ID, result); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func TestMaintenanceAuthorizationHelperProcess(t *testing.T) {
	if os.Getenv("MCPMUX_MAINTENANCE_AUTHORIZATION_HELPER") != "1" {
		return
	}
	// This is a failed-regression cleanup bound, never the race trigger.
	time.AfterFunc(15*time.Second, func() { os.Exit(1) })
	if err := maintenanceAuthorizationStream(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func maintenanceAuthorizationOwner(t *testing.T, mode string, protocol era.ProtocolEra, tokenHandshake bool, callback func(context.Context, muxcore.ConnInfo, muxcore.ProjectContext) muxcore.SessionAuth) (*Owner, *sync.RWMutex, *safeBuffer) {
	t.Helper()
	gate := &sync.RWMutex{}
	logs := &safeBuffer{}
	cfg := OwnerConfig{
		ServerID: "authorization-authority", IPCPath: shortSocketPath(t), ProtocolEra: protocol,
		TokenHandshake: tokenHandshake, AuthorizeSession: callback, MaintenanceGate: gate, Logger: log.New(logs, "", 0),
	}
	if mode == "subprocess" {
		cfg.Command = os.Args[0]
		cfg.Args = []string{"-test.run=^TestMaintenanceAuthorizationHelperProcess$"}
		cfg.Env = map[string]string{"MCPMUX_MAINTENANCE_AUTHORIZATION_HELPER": "1"}
	} else {
		cfg.HandlerFunc = maintenanceAuthorizationStream
	}
	o, err := NewOwner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Shutdown)
	waitForCondition(t, 5*time.Second, func() bool {
		return o.MaterializationState() == MaterializationReady && o.PendingRequests() == 0
	}, "real authorization upstream did not become ready")
	return o, gate, logs
}

func maintenanceAuthorizationConnect(t *testing.T, o *Owner, tokenHandshake bool) net.Conn {
	t.Helper()
	var conn net.Conn
	if tokenHandshake {
		o.SessionMgr().PreRegister("cafebabe", t.TempDir(), nil)
		conn = connectWithToken(t, o.IPCPath(), "cafebabe")
	} else {
		var err error
		conn, err = ipc.Dial(o.IPCPath())
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestMaintenanceAuthorizationRetainsAuthorityAllOwnerModes(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
			for _, held := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/era_%d/held_%t", mode, protocol, held), func(t *testing.T) {
					entered := make(chan context.Context, 1)
					release := make(chan struct{})
					var releaseOnce sync.Once
					o, gate, _ := maintenanceAuthorizationOwner(t, mode, protocol, true, func(ctx context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
						entered <- ctx
						<-release
						return muxcore.SessionAuth{Decision: muxcore.AuthAllow, TenantID: "authorized"}
					})
					t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
					conn := maintenanceAuthorizationConnect(t, o, true)
					var ctx context.Context
					select {
					case ctx = <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("real accepted session did not enter authorization")
					}
					if !gate.TryLock() {
						t.Fatal("authorization retained maintenance admission across user code")
					}
					if held {
						o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: time.Now()})
					}
					gate.Unlock()
					maintenanceNativeBlocked(t, o)
					if o.nativeWork.Load() != 1 || o.SessionCount() != 0 {
						t.Fatal("authorization producer was not privately reserved before session publication")
					}
					reader, writer := io.Pipe()
					defer writer.Close()
					late := NewSession(reader, &safeBuf{})
					o.AddSession(late)
					if !late.IsClosed() || o.SessionCount() != 0 || o.nativeWork.Load() != 1 {
						t.Fatal("late direct registration crossed the same authorization retirement fence")
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
						t.Fatal("authorization did not observe listener teardown while Done remained open")
					}
					releaseOnce.Do(func() { close(release) })
					_ = conn.SetReadDeadline(time.Now().Add(time.Second))
					var data [1]byte
					if n, err := conn.Read(data[:]); n != 0 || err == nil {
						t.Fatalf("late authorization produced a session or response: n=%d err=%v", n, err)
					} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
						t.Fatal("late authorization did not reject its connection")
					}
					waitForCondition(t, time.Second, func() bool { return o.nativeWork.Load() == 0 }, "authorization producer did not settle")
					_, finalized, err := o.FinalizeForRemoval(false, time.Second)
					if !finalized || err != nil || !o.MaintenanceRetired() || o.SessionCount() != 0 || o.PendingRequests() != 0 {
						t.Fatalf("actual authorization return did not permit existing finalization: finalized=%t err=%v", finalized, err)
					}
				})
			}
		}
	}
}

func TestMaintenanceAuthorizationRegistrationProducerAndFence(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
			t.Run(fmt.Sprintf("%s/era_%d", mode, protocol), func(t *testing.T) {
				entered := make(chan struct{})
				releaseAuth := make(chan struct{})
				releaseProof := make(chan struct{})
				var authOnce, proofOnce sync.Once
				o, _, logs := maintenanceAuthorizationOwner(t, mode, protocol, false, func(context.Context, muxcore.ConnInfo, muxcore.ProjectContext) muxcore.SessionAuth {
					close(entered)
					<-releaseAuth
					return muxcore.SessionAuth{Decision: muxcore.AuthAllow}
				})
				t.Cleanup(func() { authOnce.Do(func() { close(releaseAuth) }); proofOnce.Do(func() { close(releaseProof) }) })
				conn := maintenanceAuthorizationConnect(t, o, false)
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("authorization did not reach callback barrier")
				}
				proofEntered := make(chan struct{})
				var proofEnteredOnce sync.Once
				o.materializationFinalizationProbe = func(*upstream.Process) error {
					proofEnteredOnce.Do(func() { close(proofEntered) })
					<-releaseProof
					return nil
				}
				finalized := make(chan bool, 1)
				go func() { _, proven, _ := o.FinalizeForRemoval(false, time.Second); finalized <- proven }()
				select {
				case <-proofEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("retirement did not complete its existing-session sweep")
				}
				// Hold the real publication lock after teardown's sweep. The
				// callback can return, but registration is still a live producer.
				o.admissionMu.Lock()
				var unlockOnce sync.Once
				unlock := func() { unlockOnce.Do(o.admissionMu.Unlock) }
				defer unlock()
				authOnce.Do(func() { close(releaseAuth) })
				waitForCondition(t, time.Second, func() bool { return strings.Contains(logs.String(), "auth_allow sid=") }, "authorization did not return into registration")
				if o.nativeWork.Load() != 1 || o.PendingRequests() != 0 {
					t.Errorf("post-callback registration escaped private accounting: work=%d pending=%d", o.nativeWork.Load(), o.PendingRequests())
				}
				unlock()
				closed := make(chan struct{})
				go func() { var data [1]byte; _, _ = conn.Read(data[:]); close(closed) }()
				waitForCondition(t, time.Second, func() bool {
					select {
					case <-closed:
						return true
					default:
						return o.SessionCount() != 0
					}
				}, "late authorization reached neither rejection nor publication")
				if o.SessionCount() != 0 {
					t.Error("AuthAllow installed a session after the existing-session retirement sweep")
				}
				proofOnce.Do(func() { close(releaseProof) })
				select {
				case <-finalized:
				case <-time.After(time.Second):
					t.Fatal("controlled retirement did not return")
				}
				o.materializationFinalizationProbe = nil
				waitForCondition(t, time.Second, func() bool { return o.nativeWork.Load() == 0 }, "registration producer did not settle")
				if _, proven, err := o.FinalizeForRemoval(false, time.Second); !proven || err != nil || !o.MaintenanceRetired() {
					t.Fatalf("settled producer could not finalize: proven=%t err=%v", proven, err)
				}
			})
		}
	}
}

func TestMaintenanceAuthorizationOrdinaryVerdictsAllOwnerModes(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, decision := range []muxcore.AuthDecision{muxcore.AuthAllow, muxcore.AuthDeny} {
			t.Run(fmt.Sprintf("%s/decision_%d", mode, decision), func(t *testing.T) {
				o, _, _ := maintenanceAuthorizationOwner(t, mode, era.EraLegacy, true, func(context.Context, muxcore.ConnInfo, muxcore.ProjectContext) muxcore.SessionAuth {
					return muxcore.SessionAuth{Decision: decision, TenantID: "ordinary-tenant", Reason: "ordinary-deny"}
				})
				conn := maintenanceAuthorizationConnect(t, o, true)
				if decision == muxcore.AuthDeny {
					if response := readDenyResponse(t, conn, time.Second); !strings.Contains(response, `"code":-32000`) || !strings.Contains(response, "ordinary-deny") {
						t.Fatalf("ordinary deny changed: %s", response)
					}
				} else {
					waitForCondition(t, time.Second, func() bool { return o.SessionCount() == 1 }, "ordinary AuthAllow did not register")
					o.mu.RLock()
					for _, s := range o.sessions {
						if meta := s.Meta(); meta.TenantID != "ordinary-tenant" || !meta.IsAuthorized() {
							t.Errorf("ordinary AuthAllow metadata changed: %+v", meta)
						}
					}
					o.mu.RUnlock()
					if response := sendRequestAndWait(t, conn, time.Second); !strings.Contains(response, `"id":"1"`) {
						t.Fatalf("ordinary allowed request changed: %s", response)
					}
				}
				waitForCondition(t, time.Second, func() bool { return o.nativeWork.Load() == 0 }, "ordinary authorization work leaked")
				if o.PendingRequests() != 0 || (decision == muxcore.AuthDeny && o.SessionCount() != 0) {
					t.Fatal("authorization changed public request metrics or registered a denied session")
				}
			})
		}
	}
}

func TestMaintenanceAuthorizationHandoffRetainsProducer(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			o, _, _ := maintenanceAuthorizationOwner(t, mode, era.EraLegacy, true, func(context.Context, muxcore.ConnInfo, muxcore.ProjectContext) muxcore.SessionAuth {
				close(entered)
				<-release
				return muxcore.SessionAuth{Decision: muxcore.AuthAllow}
			})
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			maintenanceAuthorizationConnect(t, o, true)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("authorization did not enter before handoff")
			}
			payload, err := o.ShutdownForHandoff()
			if !errors.Is(err, errFinalizationUnproven) || payload.PID != 0 {
				_ = payload.Abort()
				t.Fatalf("handoff released live authorization authority: payload=%+v err=%v", payload, err)
			}
			select {
			case <-o.Done():
				t.Fatal("handoff closed Done before authorization returned")
			default:
			}
			releaseOnce.Do(func() { close(release) })
			waitForCondition(t, time.Second, func() bool { return o.nativeWork.Load() == 0 }, "authorization did not settle after handoff refusal")
			if _, proven, err := o.FinalizeForRemoval(false, time.Second); !proven || err != nil || !o.MaintenanceRetired() {
				t.Fatalf("existing finalizer could not settle refused handoff: proven=%t err=%v", proven, err)
			}
		})
	}
}

func TestMaintenanceFrameHookRetainsAuthorityAllOwnerModes(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
			for _, held := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/era_%d/held_%t", mode, protocol, held), func(t *testing.T) {
					o, gate, logs := maintenanceAuthorizationOwner(t, mode, protocol, true, nil)
					entered, release := make(chan struct{}), make(chan struct{})
					var releaseOnce sync.Once
					var calls atomic.Int32
					o.onFrameReceived = func(_ string, _ int, _ string) muxcore.FrameAction {
						if calls.Add(1) == 1 {
							close(entered)
						}
						<-release
						return muxcore.FrameError
					}
					t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
					conn := maintenanceAuthorizationConnect(t, o, true)
					raw := `{"jsonrpc":"2.0","id":701,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
					if _, err := fmt.Fprintln(conn, raw); err != nil {
						t.Fatal(err)
					}
					select {
					case <-entered:
					case <-time.After(time.Second):
						t.Fatal("real session reader did not enter frame callback")
					}
					response := readDenyResponse(t, conn, 2*time.Second)
					if !strings.Contains(response, `"id":701`) || strings.Contains(response, `"error"`) || !strings.Contains(logs.String(), "frame_hook_timeout") {
						t.Fatalf("reader did not pass the actual 1ms hook timeout: %s", response)
					}
					waitForCondition(t, time.Second, func() bool { return o.PendingRequests() == 0 }, "frame callback polluted request accounting")
					var session *Session
					o.mu.RLock()
					for _, current := range o.sessions {
						session = current
					}
					o.mu.RUnlock()
					if session == nil {
						t.Fatal("frame callback lost its real reader session")
					}
					// Freeze the existing reader's removal producer, not the callback.
					// Retirement must join both after the hook verdict has timed out.
					o.launchContextMu.Lock()
					var unlockOnce sync.Once
					unlock := func() { unlockOnce.Do(o.launchContextMu.Unlock) }
					defer unlock()
					if held {
						gate.Lock()
						o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: time.Now()})
						gate.Unlock()
					}
					maintenanceNativeBlocked(t, o)
					if o.nativeWork.Load() != 1 || o.SessionCount() != 1 {
						t.Fatal("timed-out callback or retained reader escaped retirement accounting")
					}
					releaseOnce.Do(func() { close(release) })
					waitForCondition(t, time.Second, func() bool { return o.nativeWork.Load() == 0 }, "actual frame callback return did not settle work")
					maintenanceNativeBlocked(t, o)
					unlock()
					waitForCondition(t, time.Second, func() bool { return o.SessionCount() == 0 }, "reader did not settle after callback return")
					if _, proven, err := o.FinalizeForRemoval(false, time.Second); !proven || err != nil || !o.MaintenanceRetired() {
						t.Fatalf("settled frame/reader producers could not finalize: proven=%t err=%v", proven, err)
					}
					o.SetMaintenance(nil)
					if action := o.invokeFrameHook(session, parseMessage([]byte(raw))); action != muxcore.FramePass || calls.Load() != 1 || o.nativeWork.Load() != 0 {
						t.Fatal("closed producer admission scheduled a late frame hook after quiescence")
					}
				})
			}
		}
	}
}

func TestMaintenanceFrameHookNilPreservesOrdinaryRetirementAllOwnerModes(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
			t.Run(fmt.Sprintf("%s/era_%d", mode, protocol), func(t *testing.T) {
				o, _, _ := maintenanceAuthorizationOwner(t, mode, protocol, true, nil)
				conn := maintenanceAuthorizationConnect(t, o, true)
				if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":702,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`); err != nil {
					t.Fatal(err)
				}
				if response := readDenyResponse(t, conn, time.Second); !strings.Contains(response, `"id":702`) || strings.Contains(response, `"error"`) {
					t.Fatalf("nil-hook ordinary dispatch changed: %s", response)
				}
				waitForCondition(t, time.Second, func() bool { return o.PendingRequests() == 0 && o.nativeWork.Load() == 0 }, "nil-hook ordinary work did not settle")
				if _, proven, err := o.FinalizeForRemoval(false, time.Second); !proven || err != nil || !o.MaintenanceRetired() {
					t.Fatalf("nil-hook ordinary owner gained a retirement deadlock: proven=%t err=%v", proven, err)
				}
			})
		}
	}
}
