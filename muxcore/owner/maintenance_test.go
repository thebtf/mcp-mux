package owner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

func maintenanceTestClient(t *testing.T, stdout io.Writer) *resilientClient {
	rc := newReconnectClient(t, stdout, nil)
	rc.outputMu = &sync.Mutex{}
	rc.queueSpace = make(chan struct{}, 1)
	rc.transportDone = make(chan struct{})
	return rc
}

func TestMaintenanceIngressRejectsCapacityWaiterAcrossRelease(t *testing.T) {
	var output bytes.Buffer
	rc := maintenanceTestClient(t, &output)
	rc.msgFromCC = make(chan []byte, 1)
	first := []byte(`{"jsonrpc":"2.0","id":17,"method":"tools/list"}`)
	second := []byte(`{"jsonrpc":"2.0","id":"held-string","method":"tools/call"}`)
	rc.msgFromCC <- first
	rc.localWork.Add(1)
	msg, err := jsonrpc.Parse(second)
	if err != nil {
		t.Fatal(err)
	}
	// Force the capacity waiter to capture the pre-fence sequence, then block
	// at suspend accounting while it owns ingress. No sleep chooses this race.
	rc.suspendMu.Lock()
	done := make(chan error, 1)
	go func() { done <- rc.enqueueHostFrame(second, msg) }()
	waitForCondition(t, time.Second, func() bool {
		if rc.ingressMu.TryLock() {
			rc.ingressMu.Unlock()
			return false
		}
		return true
	}, "capacity waiter never reached ingress")
	rc.suspendMu.Unlock()
	rc.enterMaintenance(control.ErrMaintenanceHeld)
	rc.leaveMaintenance()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("capacity waiter remained blocked")
	}
	responses := parseJSONRPCResponsesWithID(t, output.String())
	if len(responses) != 2 {
		t.Fatalf("held request dispositions: %s", output.String())
	}
	ids := map[string]bool{}
	for _, response := range responses {
		if response.Error == nil || response.Error.Code != -32005 {
			t.Fatalf("not a maintenance refusal: %+v", response)
		}
		ids[string(response.ID)] = true
	}
	if !ids["17"] || !ids[`"held-string"`] || len(rc.msgFromCC) != 0 || rc.localWork.Load() != 0 {
		t.Fatalf("held input leaked across release: ids=%v queued=%d work=%d", ids, len(rc.msgFromCC), rc.localWork.Load())
	}
}

func TestMaintenanceFreshDemandRechecksAdmissionAndReusesOneSuccessor(t *testing.T) {
	var output bytes.Buffer
	rc := maintenanceTestClient(t, &output)
	held, generations := true, 0
	rc.cfg.Reconnect = func() (string, string, error) {
		if held {
			return "", "", control.ErrMaintenanceHeld
		}
		generations++
		return "successor", "successor-token", nil
	}
	rc.enterMaintenance(control.ErrMaintenanceHeld)
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"maintenance/write"}`,
		`{"jsonrpc":"2.0","id":"held-string","method":"maintenance/write"}`,
	} {
		msg, err := jsonrpc.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := rc.enqueueHostFrame([]byte(raw), msg); err != nil {
			t.Fatal(err)
		}
	}
	held = false
	fresh := []byte(`{"jsonrpc":"2.0","id":3,"method":"maintenance/write"}`)
	msg, err := jsonrpc.Parse(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.enqueueHostFrame(fresh, msg); err != nil {
		t.Fatal(err)
	}
	res := rc.attemptReconnect(rc.cfg.Reconnect)
	if res.err != nil || res.path != "successor" || res.token != "successor-token" || generations != 1 {
		t.Fatalf("fresh demand created duplicate successors: result=%+v generations=%d", res, generations)
	}
	rc.failBufferedRequestsDuringReconnect(rc.outputMu)
	responses := parseJSONRPCResponsesWithID(t, output.String())
	if len(responses) != 2 || string(responses[0].ID) != "2" || string(responses[1].ID) != `"held-string"` {
		t.Fatalf("original held IDs were not rejected exactly once: %s", output.String())
	}
	for _, response := range responses {
		if response.Error == nil || response.Error.Code != -32005 {
			t.Fatalf("held demand disposition: %+v", response)
		}
	}
	if len(rc.msgFromCC) != 1 || rc.localWork.Load() != 1 || !bytes.Equal(<-rc.msgFromCC, fresh) {
		t.Fatal("fresh demand was rejected or held demand replayed")
	}
	rc.noteDequeued()
}

func TestMaintenanceReconnectDisposesEscapedWakeBeforeRecovery(t *testing.T) {
	var output bytes.Buffer
	rc := maintenanceTestClient(t, &output)
	wake := []byte(`{"jsonrpc":"2.0","id":"wake","method":"tools/call"}`)
	rc.localWork.Add(1)
	rc.initCache.request = []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	rc.initCache.requestID = "1"
	deadline := time.Now().Add(time.Minute)
	degraded := false
	_, err := rc.awaitReconnectAttempt(&deadline, make(chan error), rc.outputMu, &degraded, func() (string, string, error) {
		return "", "", control.ErrMaintenanceRetirementBlocked
	}, &wake)
	if !errors.Is(err, control.ErrMaintenanceRetirementBlocked) || wake != nil || rc.localWork.Load() != 0 {
		t.Fatalf("wake survived typed reconnect refusal: frame=%s err=%v work=%d", wake, err, rc.localWork.Load())
	}
	rc.leaveMaintenance()
	if rc.initCache.request != nil {
		t.Fatal("maintenance recovery retained initialize replay")
	}
	responses := parseJSONRPCResponsesWithID(t, output.String())
	if len(responses) != 1 || string(responses[0].ID) != `"wake"` || responses[0].Error == nil || responses[0].Error.Code != -32005 {
		t.Fatalf("wake disposition: %s", output.String())
	}
}

func TestMaintenanceReaderClosesIngressBeforeQueueReplay(t *testing.T) {
	var output bytes.Buffer
	rc := maintenanceTestClient(t, &output)
	rc.msgFromCC <- []byte(`{"jsonrpc":"2.0","id":23,"method":"tools/call"}`)
	rc.localWork.Add(1)
	eof := make(chan struct{})
	rc.runIPCReader(strings.NewReader(`{"jsonrpc":"2.0","id":22,"error":{"code":-32005,"data":{"error_code":"maintenance_held"}}}`+"\n"), eof)
	select {
	case <-eof:
	default:
		t.Fatal("reader did not finish")
	}
	if !rc.held || !rc.maintenanceObserved() || len(rc.msgFromCC) != 0 {
		t.Fatal("wire refusal did not fence queued ingress")
	}
	responses := parseJSONRPCResponsesWithID(t, output.String())
	if len(responses) != 1 || string(responses[0].ID) != "23" || responses[0].Error.Code != -32005 {
		t.Fatalf("queued request disposition: %s", output.String())
	}
	if extractResponseID(<-rc.msgFromIPC) != "22" {
		t.Fatal("original upstream refusal was lost")
	}
}

func TestMaintenanceOwnerHelperProcess(t *testing.T) {
	if os.Getenv("MCPMUX_MAINTENANCE_OWNER_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestMaintenanceStartGateCoversPhysicalStartThroughAuthorityInstall(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial_failure_%t", partial), func(t *testing.T) {
			var gate sync.RWMutex
			started := make(chan struct{})
			release := make(chan struct{})
			injected := errors.New("failure after physical start")
			original := materializationStartProcess
			materializationStartProcess = func(command string, args []string, env map[string]string, cwd string, logger *log.Logger) (*upstream.Process, error) {
				proc, err := original(command, args, env, cwd, logger)
				if err != nil {
					return proc, err
				}
				if line, err := proc.ReadLine(); err != nil || string(line) != "ready" {
					_ = proc.Close()
					return nil, fmt.Errorf("helper readiness: %s %v", line, err)
				}
				close(started)
				<-release
				if partial {
					return proc, injected
				}
				return proc, nil
			}
			t.Cleanup(func() { materializationStartProcess = original })
			o, err := NewOwner(OwnerConfig{Command: os.Args[0], Args: []string{"-test.run=^TestMaintenanceOwnerHelperProcess$"}, Env: map[string]string{"MCPMUX_MAINTENANCE_OWNER_HELPER": "1"}, IPCPath: testIPCPath(t), DeferInitialMaterialization: true, MaintenanceGate: &gate, Logger: testLogger(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(o.Shutdown)
			a := newMaterializationAttempt(1, MaterializationTriggerEager)
			a.launchFrozen = true
			a.launch = LaunchContext{Env: map[string]string{"MCPMUX_MAINTENANCE_OWNER_HELPER": "1"}}
			result := make(chan error, 1)
			go func() { _, _, err := o.startAdmittedMaterialization(a); result <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("physical start did not reach barrier")
			}
			if gate.TryLock() {
				gate.Unlock()
				t.Fatal("fence acquired before process authority installation")
			}
			close(release)
			select {
			case err := <-result:
				if partial && !errors.Is(err, injected) {
					t.Fatalf("partial start: %v", err)
				}
				if !partial && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("start did not install authority")
			}
			gate.Lock()
			installed := o.currentUpstream()
			if installed == nil || installed != a.process || installed.PID() == 0 {
				gate.Unlock()
				t.Fatal("physical tree escaped installation")
			}
			o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: time.Now()})
			gate.Unlock()
			if _, _, err := o.startAdmittedMaterialization(newMaterializationAttempt(2, MaterializationTriggerBackground)); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("retry crossed fence: %v", err)
			}
			if _, finalized, _ := o.FinalizeForRemoval(false, time.Second); !finalized || !installed.TreesDead() || !o.MaintenanceRetired() {
				t.Fatal("installed authority was not retired")
			}
		})
	}
}
