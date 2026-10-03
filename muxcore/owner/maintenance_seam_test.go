package owner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

func TestMaintenanceNonreadingHelper(t *testing.T) {
	mode := os.Getenv("MCPMUX_MAINTENANCE_NONREADING_HELPER")
	if mode == "" {
		return
	}
	// The lifetime is only a failed-regression cleanup bound, not a race trigger.
	time.AfterFunc(15*time.Second, func() { os.Exit(0) })
	if mode == "legacy" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				os.Exit(1)
			}
			switch frame.Method {
			case "initialize":
				fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"nonreading","version":"1"}}}`+"\n", frame.ID)
			case "tools/list":
				fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`+"\n", frame.ID)
				for {
					time.Sleep(time.Hour)
				}
			}
		}
		os.Exit(1)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestMaintenanceFenceRetiresReservedWrite(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		for _, queued := range []bool{false, true} {
			t.Run(fmt.Sprintf("era_%d_queued_%t", protocol, queued), func(t *testing.T) {
				var gate sync.RWMutex
				mode := "legacy"
				if protocol == era.EraModern20260728 {
					mode = "modern"
				}
				env := map[string]string{"MCPMUX_MAINTENANCE_NONREADING_HELPER": mode}
				o, err := NewOwner(OwnerConfig{
					Command: os.Args[0], Args: []string{"-test.run=^TestMaintenanceNonreadingHelper$"}, Env: env,
					IPCPath: testIPCPath(t), ProtocolEra: protocol, MaintenanceGate: &gate,
					DeferInitialMaterialization: queued, Logger: testLogger(t),
				})
				if err != nil {
					t.Fatal(err)
				}
				release := make(chan struct{})
				var releaseOnce sync.Once
				t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); o.Shutdown() })
				if !queued {
					waitForCondition(t, 5*time.Second, func() bool { return o.MaterializationState() == MaterializationReady }, "helper did not initialize")
				}
				s, output := addModernOwnerSession(t, o, t.TempDir())
				s.Env = env
				selected := make(chan *upstream.Process, 1)
				o.beforeCurrentUpstreamWrite = func(proc *upstream.Process) { selected <- proc; <-release }
				request := parseMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":301,"method":"maintenance/block","params":{"payload":"%s","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, strings.Repeat("x", 512*1024))))
				forwardDone := make(chan error, 1)
				go func() { forwardDone <- o.handleDownstreamMessage(s, request) }()
				var proc *upstream.Process
				select {
				case proc = <-selected:
				case <-time.After(5 * time.Second):
					t.Fatal("request did not reserve its process generation")
				}
				if o.PendingRequests() != 1 || proc != o.currentUpstream() {
					t.Fatal("admitted writer was not existing work on the exact generation")
				}
				if !gate.TryLock() {
					t.Fatal("admitted writer retained the fence admission lease")
				}
				o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: time.Now()})
				gate.Unlock()
				// Both cached discovery and an uncached request must remain closed;
				// notifications never acquire a response obligation.
				for _, raw := range []string{
					`{"jsonrpc":"2.0","id":"held-cache","method":"tools/list"}`,
					`{"jsonrpc":"2.0","id":"held-write","method":"maintenance/block"}`,
					`{"jsonrpc":"2.0","method":"notifications/test"}`,
				} {
					if err := o.handleDownstreamMessage(s, parseMessage([]byte(raw))); err != nil {
						t.Fatal(err)
					}
				}
				releaseOnce.Do(func() { close(release) })
				_, finalized, err := o.FinalizeForRemoval(false, time.Second)
				if !finalized || !o.MaintenanceRetired() || !proc.TreesDead() {
					t.Fatalf("force retirement could not interrupt the reserved generation: finalized=%t err=%v", finalized, err)
				}
				select {
				case err := <-forwardDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("reserved request did not finish after tree retirement")
				}
				responses := parseJSONRPCResponsesWithID(t, output.String())
				ids := map[string]int{}
				for _, response := range responses {
					if response.Error == nil || response.Error.Code != -32005 {
						t.Fatalf("retirement changed a maintenance disposition: %s", output.String())
					}
					ids[string(response.ID)]++
				}
				if len(responses) != 3 || ids["301"] != 1 || ids[`"held-cache"`] != 1 || ids[`"held-write"`] != 1 || o.PendingRequests() != 0 {
					t.Fatalf("lost or duplicate original-ID disposition: %s pending=%d", output.String(), o.PendingRequests())
				}
			})
		}
	}
}

type maintenanceEOFConn struct {
	net.Conn
	reading, closing    chan struct{}
	readOnce, closeOnce sync.Once
}

func (c *maintenanceEOFConn) Read(data []byte) (int, error) {
	c.readOnce.Do(func() { close(c.reading) })
	return c.Conn.Read(data)
}

func (c *maintenanceEOFConn) Close() error {
	c.closeOnce.Do(func() { close(c.closing) })
	return c.Conn.Close()
}

func TestMaintenanceEOFDequeuedFrameHasOneDisposition(t *testing.T) {
	for _, scenario := range []string{"ordinary", "held", "released_wire"} {
		held := scenario == "held"
		for _, frame := range []string{
			`{"jsonrpc":"2.0","id":47,"method":"tools/call"}`,
			`{"jsonrpc":"2.0","id":"eof-string","method":"tools/call"}`,
			`{"jsonrpc":"2.0","method":"notifications/test"}`,
		} {
			t.Run(fmt.Sprintf("%s_id_%s", scenario, extractRequestID([]byte(frame))), func(t *testing.T) {
				output := &safeBuf{}
				rc := maintenanceTestClient(t, output)
				rc.cfg.Reconnect = func() (string, string, error) {
					if held {
						return "", "", control.ErrMaintenanceHeld
					}
					return "", "", ErrReconnectExit
				}
				local, peer := net.Pipe()
				conn := &maintenanceEOFConn{Conn: local, reading: make(chan struct{}), closing: make(chan struct{})}
				t.Cleanup(func() { _ = peer.Close(); _ = conn.Close() })
				stdinDone := make(chan error, 1)
				proxyDone := make(chan error, 1)
				go func() { proxyDone <- rc.runProxy(conn, rc.outputMu, stdinDone) }()
				select {
				case <-conn.reading:
				case <-time.After(time.Second):
					t.Fatal("IPC reader did not start")
				}
				// The writer has received the frame but cannot yet inspect EOF.
				// Closing the peer before unlocking chooses the failing seam exactly.
				func() {
					rc.suspendMu.Lock()
					defer rc.suspendMu.Unlock()
					rc.localWork.Add(1)
					rc.msgFromCC <- []byte(frame)
					waitForCondition(t, time.Second, func() bool { return len(rc.msgFromCC) == 0 }, "writer did not dequeue the host frame")
					if scenario == "released_wire" {
						if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":22,"error":{"code":-32005,"data":{"error_code":"maintenance_held"}}}`+"\n"); err != nil {
							t.Fatal(err)
						}
						waitForCondition(t, time.Second, rc.maintenanceObserved, "wire refusal did not fence ingress")
						rc.leaveMaintenance()
					}
					_ = peer.Close()
					select {
					case <-conn.closing:
					case <-time.After(time.Second):
						t.Fatal("proxy did not observe IPC EOF")
					}
				}()
				waitForCondition(t, time.Second, func() bool { return rc.localWork.Load() == 0 }, "deferred EOF work was not disposed")
				if held {
					waitForCondition(t, time.Second, rc.maintenanceObserved, "reconnect did not observe maintenance")
					stdinDone <- io.EOF
				}
				select {
				case err := <-proxyDone:
					if held && err != nil || !held && !errors.Is(err, ErrReconnectExit) {
						t.Fatalf("proxy transition: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("proxy did not finish its EOF transition")
				}
				rc.leaveMaintenance()
				responses := parseJSONRPCResponsesWithID(t, output.String())
				id := extractRequestID([]byte(frame))
				if id == "" {
					if output.String() != "" {
						t.Fatalf("notification acquired a response: %s", output.String())
					}
				} else {
					code := -32603
					if scenario != "ordinary" {
						code = -32005
					}
					if len(responses) != 1 || string(responses[0].ID) != id || responses[0].Error == nil || responses[0].Error.Code != code {
						t.Fatalf("lost or duplicate EOF disposition: %s", output.String())
					}
				}
				if len(rc.msgFromCC) != 0 || rc.countInflight() != 0 || rc.localWork.Load() != 0 {
					t.Fatal("old EOF demand survived for replay")
				}
			})
		}
	}
}
