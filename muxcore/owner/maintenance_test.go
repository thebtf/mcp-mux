package owner

import (
	"bufio"
	"bytes"
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

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
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
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		t.Run(fmt.Sprintf("era_%d", protocol), func(t *testing.T) {
			paths := [2]string{newTestIPCPath(t), newTestIPCPath(t)}
			var servers [2]*echoServer
			var frames [2]chan string
			var connections [2]chan net.Conn
			for index, path := range paths {
				listener, err := ipc.Listen(path)
				if err != nil {
					t.Fatal(err)
				}
				server := &echoServer{ln: listener}
				servers[index] = server
				frames[index] = make(chan string, 16)
				connections[index] = make(chan net.Conn, 1)
				t.Cleanup(server.closeAll)
				go func() {
					defer close(frames[index])
					conn, err := server.accept()
					if err != nil {
						return
					}
					defer conn.Close()
					connections[index] <- conn
					scanner := bufio.NewScanner(conn)
					for scanner.Scan() {
						frames[index] <- scanner.Text()
					}
				}()
			}
			stdin, hostInput := io.Pipe()
			hostOutput, stdout := io.Pipe()
			output := &safeBuf{}
			outputDone := make(chan struct{})
			go func() {
				defer close(outputDone)
				scanner := bufio.NewScanner(hostOutput)
				for scanner.Scan() {
					_, _ = fmt.Fprintln(output, scanner.Text())
				}
			}()
			var resumed atomic.Bool
			var refusals, generations atomic.Int32
			clientDone := make(chan error, 1)
			go func() {
				clientDone <- RunResilientClient(ResilientClientConfig{
					Stdin: stdin, Stdout: stdout, InitialIPCPath: paths[0], Token: "old-token",
					ProtocolEra: protocol, ProbeGracePeriod: time.Nanosecond, Logger: resilientTestLogger(t),
					Reconnect: func() (string, string, error) {
						if !resumed.Load() {
							refusals.Add(1)
							return "", "", control.ErrMaintenanceHeld
						}
						generations.Add(1)
						return paths[1], "successor-token", nil
					},
				})
			}()
			clientStopped := false
			t.Cleanup(func() {
				for _, server := range servers {
					server.closeAll()
				}
				_ = stdin.Close()
				_ = hostInput.Close()
				_ = hostOutput.Close()
				_ = stdout.Close()
				if !clientStopped {
					select {
					case <-clientDone:
					case <-time.After(3 * time.Second):
						t.Error("resilient client did not settle after transport cleanup")
					}
				}
				<-outputDone
			})
			readFrame := func(ch <-chan string, want string) {
				t.Helper()
				select {
				case got, open := <-ch:
					if !open || got != want {
						t.Fatalf("IPC frame = %q (open=%t), want %q", got, open, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("IPC did not receive %q", want)
				}
			}
			send := func(raw string) {
				t.Helper()
				if _, err := fmt.Fprintln(hostInput, raw); err != nil {
					t.Fatal(err)
				}
			}
			var old net.Conn
			select {
			case old = <-connections[0]:
			case <-time.After(3 * time.Second):
				t.Fatal("initial IPC connection was not active")
			}
			readFrame(frames[0], "old-token")
			meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
			openingMethod, listenMethod := "initialize", "tools/call"
			if protocol == era.EraModern20260728 {
				openingMethod, listenMethod = "server/discover", "subscriptions/listen"
			}
			opening := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{%s}}`, openingMethod, meta)
			send(opening)
			readFrame(frames[0], opening)
			if _, err := fmt.Fprintln(old, `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`); err != nil {
				t.Fatal(err)
			}
			for _, raw := range []string{
				fmt.Sprintf(`{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{%s}}`, meta),
				fmt.Sprintf(`{"jsonrpc":"2.0","id":"old-listen","method":%q,"params":{%s}}`, listenMethod, meta),
				fmt.Sprintf(`{"jsonrpc":"2.0","id":42,"method":"maintenance/write","params":{%s}}`, meta),
			} {
				send(raw)
				readFrame(frames[0], raw)
			}
			// The real peer reports the fence but deliberately keeps the old IPC
			// socket open. Only the shim may initiate the successor switch.
			if _, err := fmt.Fprintln(old, `{"jsonrpc":"2.0","id":42,"error":{"code":-32005,"data":{"error_code":"maintenance_held"}}}`); err != nil {
				t.Fatal(err)
			}
			waitForCondition(t, 3*time.Second, func() bool { return strings.Contains(output.String(), `"id":42`) }, "wire fence was not forwarded")
			for _, raw := range []string{
				`{"jsonrpc":"2.0","id":2,"method":"maintenance/write"}`,
				`{"jsonrpc":"2.0","id":"held-string","method":"maintenance/write"}`,
				`{"jsonrpc":"2.0","method":"notifications/test"}`,
			} {
				send(raw)
			}
			waitForCondition(t, 3*time.Second, func() bool { return refusals.Load() == 3 }, "held input was not locally disposed")
			resumed.Store(true)
			fresh := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":%q,"params":{%s}}`, listenMethod, meta)
			send(fresh)
			var successor net.Conn
			oldFrames := (<-chan string)(frames[0])
			deadline := time.After(3 * time.Second)
			for successor == nil {
				select {
				case line, open := <-oldFrames:
					if open {
						t.Fatalf("post-resume demand reached the retired connection: %s", line)
					}
					oldFrames = nil
				case successor = <-connections[1]:
				case <-deadline:
					t.Fatal("admitted successor did not become active while old IPC stayed open")
				}
			}
			readFrame(frames[1], "successor-token")
			readFrame(frames[1], fresh)
			if _, err := fmt.Fprintln(successor, `{"jsonrpc":"2.0","id":3,"result":{"successor":true}}`); err != nil {
				t.Fatal(err)
			}
			waitForCondition(t, 3*time.Second, func() bool { return strings.Contains(output.String(), `"successor":true`) }, "fresh response was not delivered")
			_ = hostInput.Close()
			select {
			case err := <-clientDone:
				clientStopped = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("client did not exit after fresh response and host EOF")
			}
			for index, ch := range frames {
				select {
				case line, open := <-ch:
					if open {
						t.Fatalf("IPC %d received extra/replayed frame: %s", index, line)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("IPC %d did not settle", index)
				}
			}
			responses := parseJSONRPCResponsesWithID(t, output.String())
			ids := map[string]int{}
			for _, response := range responses {
				id := string(response.ID)
				ids[id]++
				switch id {
				case "2", `"held-string"`, "42":
					if response.Error == nil || response.Error.Code != -32005 {
						t.Fatalf("held original-ID disposition changed: %+v", response)
					}
				case "41", `"old-listen"`:
					if response.Error == nil || response.Error.Code != -32603 {
						t.Fatalf("old in-flight work was not failed without replay: %+v", response)
					}
				case "1", "3":
					if response.Error != nil || response.Result == nil {
						t.Fatalf("active connection response failed: %+v", response)
					}
				default:
					t.Fatalf("invented response obligation: %+v", response)
				}
			}
			if len(ids) != 7 || len(responses) != 7 || generations.Load() != 1 || strings.Contains(output.String(), "list_changed") {
				t.Fatalf("lost/duplicate IDs, duplicate successor, or replay bootstrap: ids=%v generations=%d stdout=%s", ids, generations.Load(), output.String())
			}
		})
	}
}

func TestMaintenanceParkedDemandActivatesSuccessor(t *testing.T) {
	paths := [2]string{newTestIPCPath(t), newTestIPCPath(t)}
	var servers [2]*echoServer
	var frames [2]chan string
	var connections [2]chan net.Conn
	for index, path := range paths {
		listener, err := ipc.Listen(path)
		if err != nil {
			t.Fatal(err)
		}
		server := &echoServer{ln: listener}
		servers[index] = server
		frames[index] = make(chan string, 8)
		connections[index] = make(chan net.Conn, 1)
		t.Cleanup(server.closeAll)
		go func() {
			defer close(frames[index])
			conn, err := server.accept()
			if err != nil {
				return
			}
			defer conn.Close()
			connections[index] <- conn
			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				frames[index] <- scanner.Text()
			}
		}()
	}
	stdin, hostInput := io.Pipe()
	output := &safeBuf{}
	logs := &capturingLogger{}
	gateEntered, allowPark := make(chan struct{}), make(chan struct{})
	var gateOnce, releaseOnce sync.Once
	releasePark := func() { releaseOnce.Do(func() { close(allowPark) }) }
	var resumed atomic.Bool
	var refusals, admissions atomic.Int32
	admitted := make(chan struct{}, 1)
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- RunResilientClient(ResilientClientConfig{
			Stdin: stdin, Stdout: output, InitialIPCPath: paths[0], Token: "old-token",
			ProtocolEra: era.EraLegacy, ProbeGracePeriod: time.Nanosecond,
			IdleSuspendDelay: 10 * time.Millisecond, Logger: log.New(logs, "", 0),
			IdleSuspendGate: func() (bool, string, error) {
				gateOnce.Do(func() { close(gateEntered) })
				<-allowPark
				return true, "", nil
			},
			Reconnect: func() (string, string, error) {
				if !resumed.Load() {
					refusals.Add(1)
					return "", "", control.ErrMaintenanceHeld
				}
				admissions.Add(1)
				admitted <- struct{}{}
				return paths[1], "successor-token", nil
			},
		})
	}()
	clientStopped := false
	t.Cleanup(func() {
		releasePark()
		for _, server := range servers {
			server.closeAll()
		}
		_ = stdin.Close()
		_ = hostInput.Close()
		if !clientStopped {
			select {
			case <-clientDone:
			case <-time.After(time.Second):
				t.Error("parked client did not settle after failed-regression cleanup")
			}
		}
	})
	readFrame := func(ch <-chan string, want string) {
		t.Helper()
		select {
		case got, open := <-ch:
			if !open || got != want {
				t.Fatalf("IPC frame = %q (open=%t), want %q", got, open, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("IPC did not receive %q", want)
		}
	}
	send := func(raw string) {
		t.Helper()
		if _, err := fmt.Fprintln(hostInput, raw); err != nil {
			t.Fatal(err)
		}
	}
	var old net.Conn
	select {
	case old = <-connections[0]:
	case <-time.After(time.Second):
		t.Fatal("initial IPC connection was not active")
	}
	readFrame(frames[0], "old-token")
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	send(initialize)
	readFrame(frames[0], initialize)
	if _, err := fmt.Fprintln(old, `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`); err != nil {
		t.Fatal(err)
	}
	// The public idle gate witnesses actual eligibility, then holds parking until
	// the wire fence and held dispositions complete. No private clock is changed.
	select {
	case <-gateEntered:
	case <-time.After(time.Second):
		t.Fatal("initialized client did not reach the public idle gate")
	}
	probe := `{"jsonrpc":"2.0","id":42,"method":"maintenance/write"}`
	send(probe)
	readFrame(frames[0], probe)
	if _, err := fmt.Fprintln(old, `{"jsonrpc":"2.0","id":42,"error":{"code":-32005,"data":{"error_code":"maintenance_held"}}}`); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, time.Second, func() bool { return strings.Contains(output.String(), `"id":42`) }, "wire fence was not forwarded")
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"maintenance/write"}`,
		`{"jsonrpc":"2.0","id":"held-string","method":"maintenance/write"}`,
		`{"jsonrpc":"2.0","method":"notifications/test"}`,
	} {
		send(raw)
	}
	waitForCondition(t, time.Second, func() bool { return refusals.Load() == 3 }, "held input was not disposed before parking")
	releasePark()
	select {
	case line, open := <-frames[0]:
		if open {
			t.Fatalf("held input reached the parking connection: %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("public idle lifecycle did not actually close old IPC")
	}
	if !strings.Contains(logs.String(), "shim.suspend.idle") {
		t.Fatal("old EOF had no actual idle-suspend witness")
	}
	resumed.Store(true)
	fresh := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fresh"}}`
	send(fresh)
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("parked fresh demand did not obtain control admission")
	}
	var successor net.Conn
	select {
	case successor = <-connections[1]:
	case <-time.After(time.Second):
		t.Fatal("parked resume admission did not activate successor; host admission and parked proxy are mutually waiting")
	}
	readFrame(frames[1], "successor-token")
	readFrame(frames[1], fresh)
	if _, err := fmt.Fprintln(successor, `{"jsonrpc":"2.0","id":3,"result":{"successor":true}}`); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, time.Second, func() bool { return strings.Contains(output.String(), `"successor":true`) }, "parked fresh response was not delivered")
	_ = hostInput.Close()
	select {
	case err := <-clientDone:
		clientStopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("activated parked client did not consume host EOF")
	}
	select {
	case line, open := <-frames[1]:
		if open {
			t.Fatalf("parked successor received replay/duplicate frame: %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("parked successor IPC did not settle")
	}
	responses := parseJSONRPCResponsesWithID(t, output.String())
	ids := map[string]int{}
	for _, response := range responses {
		id := string(response.ID)
		ids[id]++
		switch id {
		case "2", `"held-string"`, "42":
			if response.Error == nil || response.Error.Code != -32005 {
				t.Fatalf("parked original-ID refusal changed: %+v", response)
			}
		case "1", "3":
			if response.Error != nil || response.Result == nil {
				t.Fatalf("parked active connection response failed: %+v", response)
			}
		default:
			t.Fatalf("parked route invented a response: %+v", response)
		}
	}
	if len(ids) != 5 || len(responses) != 5 || admissions.Load() != 1 || strings.Contains(output.String(), "list_changed") {
		t.Fatalf("parked lost/duplicate ID, admission, or bootstrap: ids=%v admissions=%d stdout=%s", ids, admissions.Load(), output.String())
	}
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
