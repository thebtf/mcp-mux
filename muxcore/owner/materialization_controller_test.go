package owner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/remap"
	muxsession "github.com/thebtf/mcp-mux/muxcore/session"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

type controllerTestSession struct {
	session *Session
	read    *io.PipeReader
	write   *io.PipeWriter
}

func newControllerTestSession(o *Owner) controllerTestSession {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	s := NewSession(serverR, serverW)
	o.AddSession(s)
	return controllerTestSession{session: s, read: clientR, write: clientW}
}

func (s controllerTestSession) close() {
	_ = s.write.Close()
	_ = s.read.Close()
	s.session.Close()
}

func controllerSnapshot() OwnerSnapshot {
	initResp := `{"jsonrpc":"2.0","id":"old-init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"cached","version":"1"}}}`
	toolsResp := `{"jsonrpc":"2.0","id":"old-tools","result":{"tools":[{"name":"old-tool"}]}}`
	return OwnerSnapshot{
		Classification: "shared",
		CachedInit:     base64Encode([]byte(initResp)),
		CachedTools:    base64Encode([]byte(toolsResp)),
	}
}

func writeControllerResponse(w io.Writer, id json.RawMessage, result string) error {
	_, err := fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
	return err
}

type controllerResponseResult struct {
	response []byte
	err      error
}

func scanControllerResponseWithID(r io.Reader, id int) controllerResponseResult {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		response := append([]byte(nil), scanner.Bytes()...)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(response, &obj); err != nil {
			return controllerResponseResult{err: fmt.Errorf("unmarshal response: %w (raw: %s)", err, response)}
		}
		if string(obj["id"]) == strconv.Itoa(id) {
			return controllerResponseResult{response: response}
		}
		if obj["id"] == nil && obj["method"] != nil {
			continue
		}
		return controllerResponseResult{err: fmt.Errorf("unexpected response while waiting for id %d: %s", id, response)}
	}
	if err := scanner.Err(); err != nil {
		return controllerResponseResult{err: err}
	}
	return controllerResponseResult{err: io.EOF}
}

type controllerBarrierResult struct {
	response      []byte
	notifications map[string]int
	err           error
}

func scanControllerResponseThroughBarrier(r io.Reader, id int, barrier string) controllerBarrierResult {
	result := controllerBarrierResult{notifications: make(map[string]int)}
	scanner := bufio.NewScanner(r)
	barrierSeen := false
	for scanner.Scan() {
		message := append([]byte(nil), scanner.Bytes()...)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(message, &obj); err != nil {
			result.err = fmt.Errorf("unmarshal response: %w (raw: %s)", err, message)
			return result
		}
		if string(obj["id"]) == strconv.Itoa(id) {
			result.response = message
		} else if obj["id"] == nil && obj["method"] != nil {
			var method string
			if err := json.Unmarshal(obj["method"], &method); err != nil {
				result.err = fmt.Errorf("unmarshal notification method: %w", err)
				return result
			}
			result.notifications[method]++
			barrierSeen = barrierSeen || method == barrier
		} else {
			result.err = fmt.Errorf("unexpected response while waiting for id %d: %s", id, message)
			return result
		}
		if result.response != nil && barrierSeen {
			return result
		}
	}
	if err := scanner.Err(); err != nil {
		result.err = err
	} else {
		result.err = io.EOF
	}
	return result
}

func TestConcurrentUncachedDemandsShareOneMaterialization(t *testing.T) {
	var starts atomic.Int32
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/one", "custom/two":
				if err := writeControllerResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s1 := newControllerTestSession(o)
	s2 := newControllerTestSession(o)
	defer s1.close()
	defer s2.close()

	result1 := make(chan controllerResponseResult, 1)
	result2 := make(chan controllerResponseResult, 1)
	go func() { result1 <- scanControllerResponseWithID(s1.read, 1) }()
	go func() { result2 <- scanControllerResponseWithID(s2.read, 2) }()
	sendReq(t, s1.write, 1, "custom/one", `{}`)
	sendReq(t, s2.write, 2, "custom/two", `{}`)
	for id, resultCh := range map[int]<-chan controllerResponseResult{1: result1, 2: result2} {
		select {
		case result := <-resultCh:
			if result.err != nil {
				t.Fatalf("response %d: %v", id, result.err)
			}
			assertResponseID(t, result.response, id)
		case <-time.After(3 * time.Second):
			t.Fatalf("response %d timed out", id)
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("upstream starts = %d, want exactly 1", got)
	}
	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationReady
	}, "shared materialization did not reach ready state")
}

func TestCancellationRacingForwardingWritesRequestBeforeUpstreamCancel(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	cancelSeen := make(chan struct{})
	var cancelOnce sync.Once
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"order","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/order":
				orderMu.Lock()
				order = append(order, req.Method)
				orderMu.Unlock()
				if err := writeControllerResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			case "notifications/cancelled":
				orderMu.Lock()
				order = append(order, req.Method)
				orderMu.Unlock()
				cancelOnce.Do(func() { close(cancelSeen) })
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()
	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	cancelAttempted := make(chan struct{})
	var writeOnce sync.Once
	var attemptedOnce sync.Once
	o.beforeQueuedDemandWrite = func() {
		writeOnce.Do(func() { close(writeEntered) })
		<-releaseWrite
	}
	o.beforeLocalDemandCancel = func() { attemptedOnce.Do(func() { close(cancelAttempted) }) }

	sendReq(t, s.write, 41, "custom/order", `{}`)
	select {
	case <-writeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("queued demand did not reach forwarding boundary")
	}
	if _, err := fmt.Fprintln(s.write, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":41}}`); err != nil {
		t.Fatalf("write cancellation: %v", err)
	}
	select {
	case <-cancelAttempted:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not race the forwarding boundary")
	}
	close(releaseWrite)
	result := scanControllerResponseWithID(s.read, 41)
	if result.err != nil || !strings.Contains(string(result.response), `"ok":true`) {
		t.Fatalf("forwarded request result: response=%s err=%v", result.response, result.err)
	}
	select {
	case <-cancelSeen:
	case <-time.After(time.Second):
		t.Fatal("remapped cancellation did not reach upstream")
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if len(gotOrder) != 2 || gotOrder[0] != "custom/order" || gotOrder[1] != "notifications/cancelled" {
		t.Fatalf("upstream order=%v, want request then cancellation", gotOrder)
	}
	waitForCondition(t, time.Second, func() bool { return o.Status()["pending_requests"] == int64(0) }, "forwarded request left pending residue")
}

func TestCancellationAfterForwardingDoesNotWaitForGeneration(t *testing.T) {
	o := newMinimalOwner()
	var upstream bytes.Buffer
	o.upstreamWriter = &upstream
	s := NewSession(strings.NewReader(""), io.Discard)
	o.admissionMu.Lock()
	o.addSessionLocked(s)
	o.admissionMu.Unlock()
	defer s.Close()

	msg, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":41,"method":"custom/forwarded","params":{}}`))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	key := string(remap.Remap(s.ID, msg.ID))
	launch := o.launchContextForSession(s)
	a := newMaterializationAttempt(1, MaterializationTriggerDemand)
	a.launch = launch
	o.materializationMu.Lock()
	o.materializationAttempt = a
	o.materializationState = MaterializationMaterializing
	o.pendingDemands[key] = &localDemand{key: key, session: s, message: msg, state: localDemandWaiting, generation: a.generation}
	o.materializationMu.Unlock()

	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	var writeOnce sync.Once
	o.beforeQueuedDemandWrite = func() {
		writeOnce.Do(func() { close(writeEntered) })
		<-releaseWrite
	}
	forwardDone := make(chan struct{})
	go func() {
		o.forwardQueuedDemand(key, a.generation, launch)
		close(forwardDone)
	}()
	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("queued demand did not reach forwarding boundary")
	}

	cancelResult := make(chan bool, 1)
	go func() { cancelResult <- o.cancelLocalDemand(s, msg.ID) }()
	close(releaseWrite)
	select {
	case local := <-cancelResult:
		if local {
			t.Fatal("forwarded request cancellation was consumed locally")
		}
	case <-time.After(time.Second):
		t.Fatal("forwarded request cancellation waited for generation completion")
	}
	select {
	case <-a.done:
		t.Fatal("test generation completed before cancellation result")
	default:
	}
	a.finish(nil)
	select {
	case <-forwardDone:
	case <-time.After(time.Second):
		t.Fatal("queued demand forwarding did not complete")
	}
}

func TestUnauthorizedWaiterRejectedAfterMaterialization(t *testing.T) {
	var forwarded atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"auth","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/unauthorized":
				forwarded.Add(1)
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		AuthorizeSession: func(context.Context, muxcore.ConnInfo, muxcore.ProjectContext) muxcore.SessionAuth {
			return muxcore.SessionAuth{Decision: muxcore.AuthAllow}
		},
		Logger: testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()

	sendReq(t, s.write, 51, "custom/unauthorized", `{}`)
	result := scanControllerResponseWithID(s.read, 51)
	if result.err != nil || !strings.Contains(string(result.response), "request no longer admitted") {
		t.Fatalf("unauthorized waiter result: response=%s err=%v", result.response, result.err)
	}
	if got := forwarded.Load(); got != 0 {
		t.Fatalf("unauthorized waiter forwarded %d times", got)
	}
	if status := o.Status(); status["pending_demand_count"] != 0 || status["pending_requests"] != int64(0) {
		t.Fatalf("unauthorized waiter left residue: %#v", status)
	}
}

func TestCancellationDuringMaterializationRemovesRemappedQueuedDemand(t *testing.T) {
	initSeen := make(chan struct{})
	releaseInit := make(chan struct{})
	var forwarded atomic.Int32
	var once sync.Once
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				once.Do(func() { close(initSeen) })
				<-releaseInit
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/slow":
				forwarded.Add(1)
				if err := writeControllerResponse(stdout, req.ID, `{"unexpected":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()

	sendReq(t, s.write, 7, "custom/slow", `{}`)
	select {
	case <-initSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach initialize")
	}
	expectedKey := string(remap.Remap(s.session.ID, json.RawMessage(`7`)))
	o.materializationMu.Lock()
	_, queuedByRemappedID := o.pendingDemands[expectedKey]
	o.materializationMu.Unlock()
	if !queuedByRemappedID {
		t.Fatalf("queued demand missing remapped key %q", expectedKey)
	}
	if _, err := fmt.Fprintln(s.write, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`); err != nil {
		t.Fatalf("write cancellation: %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return o.Status()["pending_demand_count"] == 0 }, "cancelled remapped demand remained queued")
	responseCh := make(chan controllerResponseResult, 1)
	go func() { responseCh <- scanControllerResponseWithID(s.read, 7) }()
	select {
	case result := <-responseCh:
		t.Fatalf("cancelled local demand produced terminal read: response=%s err=%v", result.response, result.err)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseInit)
	waitForCondition(t, 2*time.Second, func() bool { return o.MaterializationState() == MaterializationReady }, "materialization did not finish")
	select {
	case result := <-responseCh:
		t.Fatalf("cancelled demand produced late terminal read after readiness: response=%s err=%v", result.response, result.err)
	case <-time.After(150 * time.Millisecond):
	}
	if got := forwarded.Load(); got != 0 {
		t.Fatalf("cancelled demand forwarded %d times, want 0", got)
	}
}

func TestTimedOutDemandNeverForwardsWhileLaterWaiterContinues(t *testing.T) {
	originalTimeout := materializationDemandTimeout
	materializationDemandTimeout = 120 * time.Millisecond
	t.Cleanup(func() { materializationDemandTimeout = originalTimeout })

	initSeen := make(chan struct{})
	releaseInit := make(chan struct{})
	var starts atomic.Int32
	var forwardedOne atomic.Int32
	var forwardedTwo atomic.Int32
	var once sync.Once
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
				once.Do(func() { close(initSeen) })
				<-releaseInit
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/one":
				forwardedOne.Add(1)
				if err := writeControllerResponse(stdout, req.ID, `{"one":true}`); err != nil {
					return err
				}
			case "custom/two":
				forwardedTwo.Add(1)
				if err := writeControllerResponse(stdout, req.ID, `{"two":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()

	sendReq(t, s.write, 1, "custom/one", `{}`)
	select {
	case <-initSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach initialize")
	}
	time.Sleep(70 * time.Millisecond)
	sendReq(t, s.write, 2, "custom/two", `{}`)

	first := scanControllerResponseWithID(s.read, 1)
	if first.err != nil || !strings.Contains(string(first.response), "materialization timed out") {
		t.Fatalf("first waiter result: response=%s err=%v", first.response, first.err)
	}
	close(releaseInit)
	second := scanControllerResponseWithID(s.read, 2)
	if second.err != nil || !strings.Contains(string(second.response), `"two":true`) {
		t.Fatalf("second waiter result: response=%s err=%v", second.response, second.err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("upstream starts=%d, want 1 shared controller", got)
	}
	if got := forwardedOne.Load(); got != 0 {
		t.Fatalf("timed-out demand forwarded %d times", got)
	}
	if got := forwardedTwo.Load(); got != 1 {
		t.Fatalf("later demand forwarded %d times, want 1", got)
	}
	waitForCondition(t, time.Second, func() bool {
		return o.Status()["pending_demand_count"] == 0
	}, "forwarded demand remained pending after response delivery")
}

func TestMaterializationDiagnosticsDoNotExposeRawEnvValues(t *testing.T) {
	originalTimeout := materializationDemandTimeout
	materializationDemandTimeout = 120 * time.Millisecond
	t.Cleanup(func() { materializationDemandTimeout = originalTimeout })

	const secret = "diagnostic-secret-value"
	var logs bytes.Buffer
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		Command:               "",
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                log.New(&logs, "", 0),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()
	s.session.Cwd = "/project/redaction"
	s.session.Env = map[string]string{"GITHUB_TOKEN": secret}

	sendReq(t, s.write, 1, "custom/redact", `{}`)
	result := scanControllerResponseWithID(s.read, 1)
	if result.err != nil || !strings.Contains(string(result.response), "materialization timed out") {
		t.Fatalf("materialization result: response=%s err=%v", result.response, result.err)
	}
	diagnostics := logs.String() + fmt.Sprintf("%#v", o.Status())
	if strings.Contains(diagnostics, secret) {
		t.Fatalf("raw env value leaked in diagnostics: %s", diagnostics)
	}
}

func TestLaunchContextPinnedAndIncompatibleWaiterRejected(t *testing.T) {
	initSeen := make(chan struct{})
	releaseInit := make(chan struct{})
	var once sync.Once
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				once.Do(func() { close(initSeen) })
				<-releaseInit
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/run":
				if err := writeControllerResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	first := newControllerTestSession(o)
	defer first.close()
	first.session.Cwd = "/project/first"
	first.session.Env = map[string]string{"GITHUB_TOKEN": "first-secret"}
	second := newControllerTestSession(o)
	defer second.close()
	second.session.Cwd = "/project/second"
	second.session.Env = map[string]string{"GITHUB_TOKEN": "second-secret"}

	sendReq(t, first.write, 1, "custom/run", `{}`)
	select {
	case <-initSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach initialize")
	}
	first.session.Env["GITHUB_TOKEN"] = "mutated-after-start"
	sendReq(t, second.write, 2, "custom/run", `{}`)
	rejected := scanControllerResponseWithID(second.read, 2)
	if rejected.err != nil || !strings.Contains(string(rejected.response), "context incompatible") {
		t.Fatalf("incompatible waiter result: response=%s err=%v", rejected.response, rejected.err)
	}

	o.materializationMu.Lock()
	launch := o.materializationAttempt.launch
	o.materializationMu.Unlock()
	if launch.SessionID != first.session.ID || launch.Env["GITHUB_TOKEN"] != "first-secret" {
		t.Fatalf("pinned launch context mutated: %+v", launch)
	}
	first.session.Env["GITHUB_TOKEN"] = "first-secret"
	close(releaseInit)
	accepted := scanControllerResponseWithID(first.read, 1)
	if accepted.err != nil || !strings.Contains(string(accepted.response), `"ok":true`) {
		t.Fatalf("initiating waiter result: response=%s err=%v", accepted.response, accepted.err)
	}
}

func TestZeroSessionsCancelDisposableMaterialization(t *testing.T) {
	initSeen := make(chan struct{})
	handlerExited := make(chan struct{})
	var initOnce sync.Once
	var exitOnce sync.Once
	var starts atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, _ io.Writer) error {
		starts.Add(1)
		defer exitOnce.Do(func() { close(handlerExited) })
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			if req.Method == "initialize" {
				initOnce.Do(func() { close(initSeen) })
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	sendReq(t, s.write, 31, "custom/disconnect", `{}`)
	select {
	case <-initSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach initialize")
	}
	s.close()
	select {
	case <-handlerExited:
	case <-time.After(2 * time.Second):
		t.Fatal("zero-session materialization process did not finalize")
	}
	waitForCondition(t, time.Second, func() bool {
		o.materializationMu.Lock()
		defer o.materializationMu.Unlock()
		return o.materializationAttempt == nil && o.materializationState == MaterializationCacheOnly && len(o.pendingDemands) == 0
	}, "zero-session materialization did not return to clean cache-only state")
	if got := starts.Load(); got != 1 {
		t.Fatalf("zero-session cancellation started %d generations, want 1", got)
	}
}

func TestFailureOnlyTerminatesItsGenerationDemands(t *testing.T) {
	oldMessage, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"custom/old","params":{}}`))
	if err != nil {
		t.Fatalf("parse old message: %v", err)
	}
	newMessage, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":2,"method":"custom/new","params":{}}`))
	if err != nil {
		t.Fatalf("parse new message: %v", err)
	}
	var oldResponse bytes.Buffer
	var newResponse bytes.Buffer
	oldSession := NewSessionWithRawWriter(1, &oldResponse)
	newSession := NewSessionWithRawWriter(2, &newResponse)
	oldAttempt := newMaterializationAttempt(1, MaterializationTriggerDemand)
	newAttempt := newMaterializationAttempt(2, MaterializationTriggerDemand)
	o := &Owner{
		logger:                 testLogger(t),
		materializationStop:    make(chan struct{}),
		materializationState:   MaterializationMaterializing,
		materializationAttempt: newAttempt,
		pendingDemands: map[string]*localDemand{
			"old": {key: "old", session: oldSession, message: oldMessage, state: localDemandWaiting, generation: 1},
			"new": {key: "new", session: newSession, message: newMessage, state: localDemandWaiting, generation: 2},
		},
		pendingDemandOrder: []string{"old", "new"},
	}

	o.finishMaterializationFailure(oldAttempt, errors.New("old generation failed"))

	o.materializationMu.Lock()
	_, oldPending := o.pendingDemands["old"]
	newDemand := o.pendingDemands["new"]
	current := o.materializationAttempt
	order := append([]string(nil), o.pendingDemandOrder...)
	o.materializationMu.Unlock()
	if oldPending || newDemand == nil || current != newAttempt {
		t.Fatalf("generation cleanup crossed attempts: old=%v new=%+v current=%p want=%p", oldPending, newDemand, current, newAttempt)
	}
	if len(order) != 1 || order[0] != "new" {
		t.Fatalf("pending order = %v, want [new]", order)
	}
	if !strings.Contains(oldResponse.String(), "old generation failed") {
		t.Fatalf("old generation did not receive failure: %s", oldResponse.String())
	}
	if newResponse.Len() != 0 {
		t.Fatalf("new generation received stale failure: %s", newResponse.String())
	}
}

func TestAcquireRestartPinCancelsMaterializationAndFreezesSnapshot(t *testing.T) {
	initSeen := make(chan struct{})
	var starts atomic.Int32
	var once sync.Once
	handler := func(_ context.Context, stdin io.Reader, _ io.Writer) error {
		starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			if req.Method == "initialize" {
				once.Do(func() { close(initSeen) })
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()
	s.session.Cwd = "/project/restart"
	s.session.Env = map[string]string{"GITHUB_TOKEN": "restart-secret"}

	sendReq(t, s.write, 1, "custom/wait", `{}`)
	select {
	case <-initSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach initialize")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pin, err := o.AcquireRestartPin(ctx)
	if err != nil {
		t.Fatalf("AcquireRestartPin: %v", err)
	}
	if pin.Snapshot.Cwd != "/project/restart" || pin.Snapshot.Env["GITHUB_TOKEN"] != "restart-secret" {
		t.Fatalf("restart snapshot lost launch context: %+v", pin.Snapshot)
	}
	if pin.Snapshot.CachedInit == "" || pin.Snapshot.CachedTools == "" || len(pin.Sessions) != 1 {
		t.Fatalf("restart snapshot lost cache/session state: snapshot=%+v sessions=%+v", pin.Snapshot, pin.Sessions)
	}
	if state := o.MaterializationState(); state != MaterializationCacheOnly {
		t.Fatalf("materialization state=%s, want cache_only", state)
	}
	if !o.MaterializationBlocksEviction() || o.Status()["restart_pin_count"] != int64(1) {
		t.Fatalf("restart pin did not block eviction: %#v", o.Status())
	}

	sendReq(t, s.write, 2, "custom/during-restart", `{}`)
	first := scanControllerResponseWithID(s.read, 1)
	if first.err != nil || !strings.Contains(string(first.response), "cancelled for restart") {
		t.Fatalf("cancelled demand result: response=%s err=%v", first.response, first.err)
	}
	second := scanControllerResponseWithID(s.read, 2)
	if second.err != nil || !strings.Contains(string(second.response), "restart snapshot in progress") {
		t.Fatalf("pinned demand result: response=%s err=%v", second.response, second.err)
	}
	pin.Release()
	if o.Status()["restart_pin_count"] != int64(0) {
		t.Fatalf("restart pin leaked: %#v", o.Status())
	}
	waitForCondition(t, time.Second, func() bool { return starts.Load() == 2 }, "pin release did not resume the cancelled materialization obligation")
	time.Sleep(50 * time.Millisecond)
	if got := starts.Load(); got != 2 {
		t.Fatalf("pin release started %d generations, want exactly 2 total", got)
	}
}

func TestAcquireRestartPinPreservesSettledReelectedLaunchContext(t *testing.T) {
	firstInit := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	var releaseOnce sync.Once
	var starts atomic.Int32
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()

	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
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
				if generation == 1 {
					firstOnce.Do(func() { close(firstInit) })
					<-releaseFirst
				}
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"restart-pin","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		Cwd:                   "/project/pre-wait",
		Env:                   map[string]string{"CONFIG_PATH": "pre-wait"},
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	retained := newControllerTestSession(o)
	defer retained.close()
	retained.session.Cwd = "/project/reelected"
	retained.session.Env = map[string]string{"CONFIG_PATH": "reelected"}

	attempt := o.startMaterialization(MaterializationTriggerUpstreamExit)
	select {
	case <-firstInit:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach the pre-wait generation")
	}

	type pinResult struct {
		pin *RestartPin
		err error
	}
	resultCh := make(chan pinResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		pin, acquireErr := o.AcquireRestartPin(ctx)
		resultCh <- pinResult{pin: pin, err: acquireErr}
	}()
	waitForCondition(t, time.Second, func() bool {
		o.materializationMu.Lock()
		defer o.materializationMu.Unlock()
		return o.restartPins.Load() == 1 && attempt.launchFrozen && o.restartResumeTrigger == MaterializationTriggerUpstreamExit
	}, "restart pin did not freeze the pre-wait launch")
	release()

	var result pinResult
	select {
	case result = <-resultCh:
	case <-time.After(3 * time.Second):
		t.Fatal("AcquireRestartPin did not settle")
	}
	if result.err != nil {
		t.Fatalf("AcquireRestartPin: %v", result.err)
	}
	defer result.pin.Release()
	if attempt.err != nil {
		t.Fatalf("re-elected materialization failed: %v", attempt.err)
	}
	if result.pin.Snapshot.Cwd != retained.session.Cwd || result.pin.Snapshot.Env["CONFIG_PATH"] != "reelected" {
		t.Fatalf("restart snapshot context = %+v, want settled re-elected context", result.pin.Snapshot)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("upstream starts = %d, want pre-wait plus one re-elected generation", got)
	}
	result.pin.Release()
	time.Sleep(50 * time.Millisecond)
	if got := starts.Load(); got != 2 {
		t.Fatalf("pin release started duplicate generation: got %d starts, want 2", got)
	}
}

func TestRestartPinDefersLiveRecoveryUntilFinalRelease(t *testing.T) {
	var starts atomic.Int32
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"restart-resume","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstPin, err := o.AcquireRestartPin(ctx)
	if err != nil {
		t.Fatalf("first AcquireRestartPin: %v", err)
	}
	defer firstPin.Release()
	finalPin, err := o.AcquireRestartPin(ctx)
	if err != nil {
		t.Fatalf("second AcquireRestartPin: %v", err)
	}
	defer finalPin.Release()

	for _, trigger := range []MaterializationTrigger{MaterializationTriggerUpstreamExit, MaterializationTriggerWriteError} {
		blocked := o.startMaterialization(trigger)
		if blocked.err == nil || !strings.Contains(blocked.err.Error(), "restart snapshot in progress") {
			t.Fatalf("%s start while pinned = %v, want restart barrier", trigger, blocked.err)
		}
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("pinned recovery started %d generations, want 0", got)
	}
	firstPin.Release()
	time.Sleep(50 * time.Millisecond)
	if got := starts.Load(); got != 0 {
		t.Fatalf("non-final pin release started %d generations, want 0", got)
	}
	finalPin.Release()
	waitForCondition(t, 2*time.Second, func() bool {
		return starts.Load() == 1 && o.MaterializationState() == MaterializationReady
	}, "final pin release did not restore live-session recovery")
	time.Sleep(50 * time.Millisecond)
	if got := starts.Load(); got != 1 {
		t.Fatalf("deferred recovery started %d generations, want exactly 1", got)
	}
}

func TestRestartAndHandoffRejectActiveFinalization(t *testing.T) {
	newFinalizingOwner := func() *Owner {
		o := newMinimalOwner()
		proc := &upstream.Process{Done: make(chan struct{})}
		o.mu.Lock()
		o.upstream = proc
		o.mu.Unlock()
		o.materializationMu.Lock()
		o.materializationState = MaterializationFinalizing
		o.retiringProcess = proc
		o.materializationMu.Unlock()
		return o
	}

	t.Run("restart pin", func(t *testing.T) {
		o := newFinalizingOwner()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if pin, err := o.AcquireRestartPin(ctx); err == nil || pin != nil || !errors.Is(err, errFinalizationUnproven) {
			t.Fatalf("restart pin during finalization = pin=%v err=%v", pin, err)
		}
		if pins := o.restartPins.Load(); pins != 0 {
			t.Fatalf("failed restart pin leaked %d pins", pins)
		}
	})

	t.Run("handoff quiesce", func(t *testing.T) {
		o := newFinalizingOwner()
		if err := o.quiesceMaterializationForHandoff(); !errors.Is(err, errFinalizationUnproven) {
			t.Fatalf("handoff quiesce during finalization = %v", err)
		}
	})
}

func TestMaterializationAdmissionRejectsUnretiredGeneration(t *testing.T) {
	tests := []struct {
		name     string
		state    MaterializationState
		dead     bool
		retiring bool
	}{
		{name: "finalizing state", state: MaterializationFinalizing},
		{name: "retiring process", state: MaterializationCacheOnly, retiring: true},
		{name: "ready dead before retirement claim", state: MaterializationReady, dead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newMinimalOwner()
			proc := &upstream.Process{Done: make(chan struct{})}
			o.mu.Lock()
			o.upstream = proc
			o.mu.Unlock()
			o.upstreamDead.Store(tt.dead)
			o.materializationMu.Lock()
			o.materializationState = tt.state
			if tt.retiring {
				o.retiringProcess = proc
			}
			a, start := o.startMaterializationLocked(MaterializationTriggerDemand)
			state := o.materializationState
			generation := o.materializationGeneration
			o.materializationMu.Unlock()
			if start || a.err == nil {
				t.Fatalf("unretired generation admitted replacement: start=%v err=%v", start, a.err)
			}
			if state != tt.state || generation != 0 {
				t.Fatalf("rejected admission mutated controller: state=%s generation=%d", state, generation)
			}
		})
	}
}

func TestPostDiscoveryExitRetiresBeforeReadyPublication(t *testing.T) {
	done := make(chan struct{})
	close(done)
	proc := &upstream.Process{Done: done}
	o := newMinimalOwner()
	a := newMaterializationAttempt(1, MaterializationTriggerDemand)
	a.process = proc
	a.signals.signalDiscoveryReady()
	o.mu.Lock()
	o.upstream = proc
	o.mu.Unlock()
	o.materializationMu.Lock()
	o.materializationAttempt = a
	o.materializationState = MaterializationMaterializing
	o.materializationMu.Unlock()

	o.monitorMaterializedProcessExit(proc)
	if state := o.MaterializationState(); state != MaterializationCacheOnly {
		t.Fatalf("post-discovery exit state = %s, want cache_only after retirement", state)
	}
	if current := o.currentUpstream(); current != nil {
		t.Fatalf("post-discovery exit retained upstream %p", current)
	}

	o.finishMaterializationSuccess(a)
	select {
	case <-a.done:
	default:
		t.Fatal("failed post-discovery generation did not settle")
	}
	if a.err == nil || o.MaterializationState() == MaterializationReady {
		t.Fatalf("dead post-discovery generation published ready: err=%v state=%s", a.err, o.MaterializationState())
	}
}

func TestFinishMaterializationSuccessPreservesFinalizeBlocked(t *testing.T) {
	o := newMinimalOwner()
	proc := &upstream.Process{Done: make(chan struct{})}
	blockErr := errors.New("synthetic authority remains")
	a := newMaterializationAttempt(1, MaterializationTriggerDemand)
	a.process = proc
	o.mu.Lock()
	o.upstream = proc
	o.mu.Unlock()
	o.upstreamDead.Store(true)
	o.materializationMu.Lock()
	o.materializationAttempt = a
	o.materializationState = MaterializationFinalizeBlocked
	o.materializationBlockedErr = blockErr
	o.retiringProcess = proc
	o.materializationMu.Unlock()

	o.finishMaterializationSuccess(a)
	if state := o.MaterializationState(); state != MaterializationFinalizeBlocked {
		t.Fatalf("finish success overwrote blocked state with %s", state)
	}
	if !errors.Is(a.err, blockErr) || o.currentUpstream() != proc {
		t.Fatalf("blocked authority was not retained: err=%v current=%p", a.err, o.currentUpstream())
	}
}

func TestFailedStartAuthorityEntersFinalizeBlockedWithoutReplacement(t *testing.T) {
	o := newMinimalOwner()
	defer close(o.materializationStop)
	proc := upstream.NewProcessFromHandler(context.Background(), func(_ context.Context, stdin io.Reader, _ io.Writer) error {
		_, err := io.Copy(io.Discard, stdin)
		return err
	})
	a := newMaterializationAttempt(1, MaterializationTriggerUpstreamExit)
	o.materializationAttempt = a
	o.materializationState = MaterializationMaterializing

	var sawExactAuthority atomic.Bool
	o.materializationFinalizationProbe = func(got *upstream.Process) error {
		sawExactAuthority.Store(got == proc)
		return errors.New("synthetic failed-start authority remains")
	}
	retireErr := o.retireFailedMaterializationStart(a, proc)
	if retireErr == nil || !strings.Contains(retireErr.Error(), "authority remains") {
		t.Fatalf("failed-start retirement = %v, want unproven authority", retireErr)
	}
	startErr := errors.New("synthetic start failed after authority install")
	o.finishMaterializationFailure(a, errors.Join(startErr, retireErr))

	if !sawExactAuthority.Load() {
		t.Fatal("failed-start finalization did not receive the installed process authority")
	}
	if got := o.currentUpstream(); got != proc {
		t.Fatalf("authoritative failed-start process = %p, want %p", got, proc)
	}
	if a.process != proc || o.retiringProcess != proc || o.MaterializationState() != MaterializationFinalizeBlocked {
		t.Fatalf("failed-start authority was not retained: attempt=%p retiring=%p status=%#v", a.process, o.retiringProcess, o.Status())
	}
	blocked := o.startMaterialization(MaterializationTriggerUpstreamExit)
	if blocked.err == nil || !strings.Contains(blocked.err.Error(), "authority remains") {
		t.Fatalf("replacement start while failed-start authority remained = %v", blocked.err)
	}
}

func TestUnprovenFinalizationBlocksReplacementGeneration(t *testing.T) {
	var starts atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		if !scanner.Scan() {
			return scanner.Err()
		}
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return err
		}
		if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"crash","version":"1"}}`); err != nil {
			return err
		}
		return errors.New("synthetic exit before discovery")
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	o.materializationFinalizationProbe = func(*upstream.Process) error {
		return errors.New("synthetic process-tree finalization unproven")
	}
	s := newControllerTestSession(o)
	defer s.close()

	sendReq(t, s.write, 1, "custom/first", `{}`)
	first := scanControllerResponseWithID(s.read, 1)
	if first.err != nil || !strings.Contains(string(first.response), "finalization unproven") {
		t.Fatalf("first demand result: response=%s err=%v", first.response, first.err)
	}
	waitForCondition(t, time.Second, func() bool { return o.MaterializationState() == MaterializationFinalizeBlocked }, "owner did not enter finalize_blocked")
	sendReq(t, s.write, 2, "custom/second", `{}`)
	second := scanControllerResponseWithID(s.read, 2)
	if second.err != nil || !strings.Contains(string(second.response), "finalization unproven") {
		t.Fatalf("blocked demand result: response=%s err=%v", second.response, second.err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("replacement generation started %d upstreams, want 1", got)
	}
	status := o.Status()
	if status["finalization_error"] == "" || !o.MaterializationBlocksEviction() {
		t.Fatalf("finalization block not observable/protected: %#v", status)
	}
}

func TestLaterProcessExitClearsRecoverableFinalizationBlock(t *testing.T) {
	proc := upstream.NewProcessFromHandler(context.Background(), func(context.Context, io.Reader, io.Writer) error {
		return nil
	})
	select {
	case <-proc.Done:
	case <-time.After(time.Second):
		t.Fatal("fixture process did not exit")
	}
	o := &Owner{
		upstream:                  proc,
		logger:                    testLogger(t),
		initResp:                  []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`),
		sessionMgr:                NewSessionManager(),
		materializationStop:       make(chan struct{}),
		materializationState:      MaterializationFinalizeBlocked,
		materializationBlockedErr: fmt.Errorf("%w after timeout", errFinalizationUnproven),
		retiringProcess:           proc,
		pendingDemands:            make(map[string]*localDemand),
	}

	o.monitorMaterializedProcessExit(proc)

	if state := o.MaterializationState(); state != MaterializationCacheOnly {
		t.Fatalf("materialization state = %s, want cache_only after late retirement proof", state)
	}
	if o.currentUpstream() != nil || o.MaterializationBlocksEviction() {
		t.Fatalf("late retirement proof did not clear authority/block: status=%#v", o.Status())
	}
}

func TestReadyGenerationRemainsAuthoritativeUntilRetirementProof(t *testing.T) {
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()

	proc := upstream.NewProcessFromHandler(context.Background(), func(context.Context, io.Reader, io.Writer) error {
		return errors.New("synthetic ready-generation exit")
	})
	select {
	case <-proc.Done:
	case <-time.After(time.Second):
		t.Fatal("fixture process did not exit")
	}
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.Lock()
	o.upstream = proc
	o.upstreamDead.Store(false)
	o.mu.Unlock()
	o.materializationState = MaterializationReady
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()

	var allowProof atomic.Bool
	o.materializationFinalizationProbe = func(*upstream.Process) error {
		if allowProof.Load() {
			return nil
		}
		return errors.New("synthetic process-tree authority remains")
	}
	o.monitorMaterializedProcessExit(proc)
	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationFinalizeBlocked
	}, "ready generation did not enter finalize_blocked")
	if got := o.currentUpstream(); got != proc {
		t.Fatalf("authoritative process changed before retirement proof: got=%p want=%p", got, proc)
	}
	if attempt := o.startMaterialization(MaterializationTriggerUpstreamExit); attempt.err == nil {
		t.Fatal("replacement materialization was accepted before retirement proof")
	}

	allowProof.Store(true)
	waitForCondition(t, 2*time.Second, func() bool {
		return o.MaterializationState() == MaterializationCacheOnly && o.currentUpstream() == nil && o.retirementRetryProcess.Load() == nil
	}, "retirement retry did not release generation after proof")
}

func TestStaleWriteFailureDoesNotRetireReplacementGeneration(t *testing.T) {
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()

	stale := upstream.NewProcessFromHandler(context.Background(), func(context.Context, io.Reader, io.Writer) error {
		return nil
	})
	select {
	case <-stale.Done:
	case <-time.After(time.Second):
		t.Fatal("stale process did not exit")
	}
	replacement := upstream.NewProcessFromHandler(context.Background(), func(_ context.Context, stdin io.Reader, _ io.Writer) error {
		_, err := io.Copy(io.Discard, stdin)
		return err
	})

	sessionOutput := &bytes.Buffer{}
	s := NewSessionWithRawWriter(7001, sessionOutput)
	defer s.Close()
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	o.sessionMgr.RegisterSession(s, "")
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.Lock()
	o.upstream = stale
	o.upstreamDead.Store(false)
	o.mu.Unlock()
	o.materializationState = MaterializationReady
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()

	writeCaptured := make(chan *upstream.Process, 1)
	releaseWrite := make(chan struct{})
	o.beforeCurrentUpstreamWrite = func(proc *upstream.Process) {
		writeCaptured <- proc
		<-releaseWrite
	}
	msg, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":73,"method":"custom/stale-write","params":{}}`))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	forwardDone := make(chan error, 1)
	go func() { forwardDone <- o.forwardRequestNow(s, msg) }()
	select {
	case got := <-writeCaptured:
		if got != stale {
			t.Fatalf("captured write process=%p, want stale=%p", got, stale)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not reach generation capture seam")
	}
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.Lock()
	o.upstream = replacement
	o.upstreamDead.Store(false)
	o.mu.Unlock()
	o.materializationState = MaterializationReady
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	close(releaseWrite)
	select {
	case err := <-forwardDone:
		if err != nil {
			t.Fatalf("forwardRequestNow: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale write did not return after release")
	}

	if got := o.currentUpstream(); got != replacement {
		t.Fatalf("stale write failure replaced current process: got=%p want=%p", got, replacement)
	}
	if o.upstreamDead.Load() || o.MaterializationState() != MaterializationReady {
		t.Fatalf("replacement generation was marked failed: %#v", o.Status())
	}
	if o.PendingRequests() != 0 {
		t.Fatalf("failed stale write left pending requests: %d", o.PendingRequests())
	}
	if !strings.Contains(sessionOutput.String(), `"id":73`) || !strings.Contains(sessionOutput.String(), "upstream write failed") {
		t.Fatalf("failed request did not receive its own error: %s", sessionOutput.String())
	}
}

func TestClosedAttachedSessionCannotSeedMaterialization(t *testing.T) {
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()

	s := NewSession(strings.NewReader(""), io.Discard)
	s.Cwd = "/closed/waiter"
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	s.Close()

	o.materializationMu.Lock()
	o.pendingDemands["closed"] = &localDemand{session: s, state: localDemandWaiting}
	o.pendingDemandOrder = []string{"closed"}
	launch := o.electLaunchContextLocked()
	o.materializationMu.Unlock()
	o.materializationMu.Lock()
	delete(o.pendingDemands, "closed")
	o.pendingDemandOrder = nil
	o.materializationMu.Unlock()
	if launch.SessionID == s.ID || launch.Cwd == s.Cwd {
		t.Fatalf("closed attached session seeded launch context: %+v", launch)
	}
}

func TestProactiveIsolatedRecoveryRematerializesInOldestAttachedContext(t *testing.T) {
	firstInit := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	var starts atomic.Int32
	defer firstOnce.Do(func() { close(releaseFirst) })
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
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
				if generation == 1 {
					close(firstInit)
					<-releaseFirst
				}
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"recovery","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		Cwd:                   "/project/departed",
		Env:                   map[string]string{"CONFIG_PATH": "departed"},
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		ServerID:              "proactive-isolated-context",
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()

	oldest := newControllerTestSession(o)
	defer oldest.close()
	oldest.session.Cwd = "/project/retained"
	oldest.session.Env = map[string]string{"CONFIG_PATH": "retained"}
	newer := newControllerTestSession(o)
	defer newer.close()
	newer.session.Cwd = "/project/newer"
	newer.session.Env = map[string]string{"CONFIG_PATH": "newer"}

	attempt := o.startMaterialization(MaterializationTriggerUpstreamExit)
	select {
	case <-firstInit:
	case <-time.After(time.Second):
		t.Fatal("proactive recovery did not reach its first initialize")
	}
	o.materializationMu.Lock()
	firstLaunch := attempt.launch
	o.materializationMu.Unlock()
	if firstLaunch.SessionID != 0 || firstLaunch.Cwd != "/project/departed" {
		t.Fatalf("initial proactive launch = %+v, want stored owner context", firstLaunch)
	}
	firstOnce.Do(func() { close(releaseFirst) })
	waitForCondition(t, 3*time.Second, func() bool { return starts.Load() >= 2 }, "isolated verdict did not rematerialize")
	o.materializationMu.Lock()
	secondLaunch := attempt.launch
	o.materializationMu.Unlock()
	if secondLaunch.SessionID != oldest.session.ID || secondLaunch.Cwd != oldest.session.Cwd || secondLaunch.Env["CONFIG_PATH"] != "retained" {
		t.Fatalf("second launch context = %+v, want oldest attached session", secondLaunch)
	}
	select {
	case <-attempt.done:
	case <-time.After(5 * time.Second):
		t.Fatal("rematerialized isolated recovery did not finish")
	}
	if attempt.err != nil {
		t.Fatalf("rematerialized isolated recovery failed: %v", attempt.err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("upstream starts = %d, want owner-context probe plus retained-context generation", got)
	}
	snapshot := o.ExportSnapshot()
	if snapshot.Cwd != oldest.session.Cwd || snapshot.Env["CONFIG_PATH"] != "retained" || snapshot.Classification != "isolated" {
		t.Fatalf("committed recovery context = %+v", snapshot)
	}
	select {
	case <-oldest.session.Done():
		t.Fatal("retained oldest session was evicted")
	default:
	}
	waitForCondition(t, time.Second, func() bool {
		select {
		case <-newer.session.Done():
			return true
		default:
			return false
		}
	}, "newer session was not evicted after isolated rematerialization")
}

func TestProactiveIsolatedRecoveryReelectsWhenOldestLeavesBeforeRestart(t *testing.T) {
	firstInit := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	var starts atomic.Int32
	defer firstOnce.Do(func() { close(releaseFirst) })
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
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
				if generation == 1 {
					close(firstInit)
					<-releaseFirst
				}
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"recovery","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		Cwd:                   "/project/departed",
		Env:                   map[string]string{"CONFIG_PATH": "departed"},
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		ServerID:              "proactive-isolated-reelection",
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()

	oldest := newControllerTestSession(o)
	oldest.session.Cwd = "/project/oldest"
	oldest.session.Env = map[string]string{"CONFIG_PATH": "oldest"}
	newer := newControllerTestSession(o)
	defer newer.close()
	newer.session.Cwd = "/project/newer"
	newer.session.Env = map[string]string{"CONFIG_PATH": "newer"}

	attempt := o.startMaterialization(MaterializationTriggerUpstreamExit)
	select {
	case <-firstInit:
	case <-time.After(time.Second):
		t.Fatal("proactive recovery did not reach its first initialize")
	}
	o.materializationMu.Lock()
	firstSignals := attempt.signals
	o.materializationMu.Unlock()
	firstOnce.Do(func() { close(releaseFirst) })
	select {
	case <-firstSignals.failed:
	case <-time.After(time.Second):
		t.Fatal("first isolated verdict did not request retained-context rematerialization")
	}
	oldest.close()
	waitForCondition(t, time.Second, func() bool {
		return !o.sessionEligibleForDemand(oldest.session)
	}, "oldest session remained eligible before replacement start")

	waitForCondition(t, 3*time.Second, func() bool { return starts.Load() >= 2 }, "replacement generation did not start")
	o.materializationMu.Lock()
	secondLaunch := attempt.launch
	o.materializationMu.Unlock()
	if secondLaunch.SessionID != newer.session.ID || secondLaunch.Cwd != newer.session.Cwd || secondLaunch.Env["CONFIG_PATH"] != "newer" {
		t.Fatalf("replacement launch context = %+v, want next-oldest attached session", secondLaunch)
	}
	select {
	case <-attempt.done:
	case <-time.After(5 * time.Second):
		t.Fatal("re-elected isolated recovery did not finish")
	}
	if attempt.err != nil {
		t.Fatalf("re-elected isolated recovery failed: %v", attempt.err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("upstream starts = %d, want owner-context probe plus one re-elected generation", got)
	}
	snapshot := o.ExportSnapshot()
	if snapshot.Cwd != newer.session.Cwd || snapshot.Env["CONFIG_PATH"] != "newer" || snapshot.Classification != "isolated" {
		t.Fatalf("committed re-elected recovery context = %+v", snapshot)
	}
	select {
	case <-newer.session.Done():
		t.Fatal("re-elected session was evicted")
	default:
	}
}

func TestFreshIsolatedClassificationRetainsInitiatingSession(t *testing.T) {
	initSeen := make(chan struct{})
	releaseInit := make(chan struct{})
	var once sync.Once
	var highForwarded atomic.Int32
	var lowForwarded atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				once.Do(func() { close(initSeen) })
				<-releaseInit
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/high":
				highForwarded.Add(1)
				if err := writeControllerResponse(stdout, req.ID, `{"high":true}`); err != nil {
					return err
				}
			case "custom/low":
				lowForwarded.Add(1)
				if err := writeControllerResponse(stdout, req.ID, `{"low":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
		ServerID:              "fresh-isolated-reconnect",
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	low := newControllerTestSession(o)
	defer low.close()
	low.session.Cwd = "/project/low"
	low.session.Env = map[string]string{"GITHUB_TOKEN": "same"}
	high := newControllerTestSession(o)
	defer high.close()
	high.session.Cwd = "/project/high"
	high.session.Env = map[string]string{"GITHUB_TOKEN": "same"}
	for token, session := range map[string]*Session{"low-bound": low.session, "high-bound": high.session} {
		o.SessionMgr().PreRegisterForOwner(token, o.ServerID(), session.Cwd, session.Env)
		if !o.SessionMgr().Bind(token, o.ServerID(), session) {
			t.Fatalf("Bind(%q) returned false", token)
		}
	}

	sendReq(t, high.write, 9, "custom/high", `{}`)
	select {
	case <-initSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach initialize")
	}
	sendReq(t, low.write, 8, "custom/low", `{}`)
	close(releaseInit)
	highResult := scanControllerResponseWithID(high.read, 9)
	if highResult.err != nil || !strings.Contains(string(highResult.response), `"high":true`) {
		t.Fatalf("initiating session result: response=%s err=%v", highResult.response, highResult.err)
	}
	waitForCondition(t, time.Second, func() bool {
		select {
		case <-low.session.Done():
			return true
		default:
			return false
		}
	}, "non-initiating session was not evicted")
	select {
	case <-high.session.Done():
		t.Fatal("initiating higher-ID session was evicted")
	default:
	}
	if got := highForwarded.Load(); got != 1 {
		t.Fatalf("initiating demand forwarded %d times, want 1", got)
	}
	if got := lowForwarded.Load(); got != 0 {
		t.Fatalf("evicted demand forwarded %d times, want 0", got)
	}
	snapshot := o.ExportSnapshot()
	if snapshot.Cwd != "/project/high" || snapshot.Classification != "isolated" {
		t.Fatalf("committed initiating context/classification = %+v", snapshot)
	}
	if _, err := o.SessionMgr().RegisterReconnect("low-bound", func(string) bool { return true }); !errors.Is(err, muxsession.ErrUnknownToken) {
		t.Fatalf("evicted reconnect error = %v, want ErrUnknownToken", err)
	}
	if _, err := o.SessionMgr().RegisterReconnect("high-bound", func(string) bool { return true }); err != nil {
		t.Fatalf("retained reconnect authority was revoked: %v", err)
	}
}

func TestMaterializationIsolatedCommitRevokesTokenDuringAuthorization(t *testing.T) {
	toolsSeen := make(chan struct{})
	releaseTools := make(chan struct{})
	authSeen := make(chan struct{})
	releaseAuth := make(chan struct{})
	var toolsOnce sync.Once
	var authOnce sync.Once
	var releaseToolsOnce sync.Once
	var releaseAuthOnce sync.Once
	defer releaseToolsOnce.Do(func() { close(releaseTools) })
	defer releaseAuthOnce.Do(func() { close(releaseAuth) })

	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				toolsOnce.Do(func() { close(toolsSeen) })
				<-releaseTools
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeControllerResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		ServerID:              "isolated-auth-race",
		TokenHandshake:        true,
		MaterializationPolicy: MaterializationOnDemand,
		AuthorizeSession: func(_ context.Context, _ muxcore.ConnInfo, project muxcore.ProjectContext) muxcore.SessionAuth {
			if project.Cwd != "/project/extra" {
				t.Errorf("authorization cwd = %q, want pending context", project.Cwd)
			}
			authOnce.Do(func() { close(authSeen) })
			<-releaseAuth
			return muxcore.SessionAuth{Decision: muxcore.AuthAllow}
		},
		Logger: testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	initiator := newControllerTestSession(o)
	defer initiator.close()
	initiator.session.Cwd = "/project/initiator"
	initiator.session.SetMeta(muxcore.SessionMeta{AuthorizedAt: time.Now()})

	const token = "deadbeef"
	if !o.PreRegister(token, "/project/extra", map[string]string{"PROJECT": "extra"}) {
		t.Fatal("extra token preregistration rejected before isolation")
	}
	response := make(chan controllerResponseResult, 1)
	go func() { response <- scanControllerResponseWithID(initiator.read, 77) }()
	sendReq(t, initiator.write, 77, "custom/wake", `{}`)
	select {
	case <-toolsSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach tools/list")
	}

	conn := connectWithToken(t, o.IPCPath(), token)
	defer conn.Close()
	select {
	case <-authSeen:
	case <-time.After(time.Second):
		t.Fatal("authorization callback did not start")
	}
	releaseToolsOnce.Do(func() { close(releaseTools) })
	if result := <-response; result.err != nil {
		t.Fatalf("initiating demand: %v", result.err)
	}
	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationReady && o.IsClassifiedIsolated()
	}, "isolated generation did not commit")
	if got := o.SessionMgr().PendingCount(); got != 0 {
		t.Fatalf("pending tokens after isolated commit = %d, want 0", got)
	}
	if got := o.SessionCount(); got != 1 {
		t.Fatalf("sessions at isolated commit = %d, want initiator only", got)
	}
	select {
	case <-initiator.session.Done():
		t.Fatal("isolated commit evicted initiating session")
	default:
	}

	releaseAuthOnce.Do(func() { close(releaseAuth) })
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set rejection deadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("revoked authorization connection remained open")
	}
	waitForCondition(t, time.Second, func() bool { return o.SessionCount() == 1 }, "revoked authorization added a session")
	probe := NewSession(strings.NewReader(""), io.Discard)
	defer probe.Close()
	if o.SessionMgr().Bind(token, o.ServerID(), probe) {
		t.Fatal("revoked token remained bindable after authorization returned")
	}
}

func TestFreshIsolatedClassificationFinalizesWhenInitiatorDisconnects(t *testing.T) {
	initSeen := make(chan struct{})
	releaseFirstInit := make(chan struct{})
	var initOnce sync.Once
	var starts atomic.Int32
	var lowForwarded atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
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
				if generation == 1 {
					initOnce.Do(func() { close(initSeen) })
					<-releaseFirstInit
				}
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/low":
				lowForwarded.Add(1)
				if err := writeControllerResponse(stdout, req.ID, `{"low":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	low := newControllerTestSession(o)
	defer low.close()
	low.session.Cwd = "/project/low"
	low.session.Env = map[string]string{"GITHUB_TOKEN": "same"}
	high := newControllerTestSession(o)
	defer high.close()
	high.session.Cwd = "/project/high"
	high.session.Env = map[string]string{"GITHUB_TOKEN": "same"}

	sendReq(t, high.write, 9, "custom/high", `{}`)
	select {
	case <-initSeen:
	case <-time.After(time.Second):
		t.Fatal("materialization did not reach initialize")
	}
	sendReq(t, low.write, 8, "custom/low", `{}`)
	high.close()
	waitForCondition(t, time.Second, func() bool { return o.SessionCount() == 1 }, "initiating session did not disconnect")
	close(releaseFirstInit)

	firstLow := scanControllerResponseWithID(low.read, 8)
	if firstLow.err != nil || !strings.Contains(string(firstLow.response), "initiator disconnected") {
		t.Fatalf("non-initiating waiter result: response=%s err=%v", firstLow.response, firstLow.err)
	}
	if got := lowForwarded.Load(); got != 0 {
		t.Fatalf("first isolated generation forwarded %d non-initiating demands", got)
	}
	select {
	case <-low.session.Done():
		t.Fatal("non-initiating session was rebound to the departed context and evicted")
	default:
	}
	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationCacheOnly
	}, "departed initiator generation did not finalize to cache-only")

	sendReq(t, low.write, 10, "custom/low", `{}`)
	secondLow := scanControllerResponseWithID(low.read, 10)
	if secondLow.err != nil || !strings.Contains(string(secondLow.response), `"low":true`) {
		t.Fatalf("fresh low-context generation result: response=%s err=%v", secondLow.response, secondLow.err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("upstream starts = %d, want one finalized generation plus one fresh generation", got)
	}
	if got := lowForwarded.Load(); got != 1 {
		t.Fatalf("fresh low-context demand forwarded %d times, want 1", got)
	}
	snapshot := o.ExportSnapshot()
	if snapshot.Cwd != "/project/low" || snapshot.Classification != "isolated" {
		t.Fatalf("fresh generation committed wrong context: %+v", snapshot)
	}
}

func TestUpstreamExitDuringQueuedDemandRetriesSameController(t *testing.T) {
	var starts atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			if generation == 1 && req.Method == "initialize" {
				return errors.New("first generation exits during initialize")
			}
			switch req.Method {
			case "initialize":
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"retry","version":"2"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/retry":
				if err := writeControllerResponse(stdout, req.ID, `{"generation":2}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()

	sendReq(t, s.write, 9, "custom/retry", `{}`)
	resp := readRespWithID(t, s.read, 9)
	assertResponseID(t, resp, 9)
	if !strings.Contains(string(resp), `"generation":2`) {
		t.Fatalf("retry response = %s", resp)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("upstream generations = %d, want 2", got)
	}
}

func TestCacheRefreshCommitsAtomically(t *testing.T) {
	toolsRequested := make(chan struct{})
	releaseTools := make(chan struct{})
	var toolsOnce sync.Once
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"new","version":"2"}}`); err != nil {
					return err
				}
			case "tools/list":
				toolsOnce.Do(func() { close(toolsRequested) })
				<-releaseTools
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[{"name":"new-tool"}]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeControllerResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	wake := newControllerTestSession(o)
	lists := newControllerTestSession(o)
	defer wake.close()
	defer lists.close()

	sendReq(t, wake.write, 11, "custom/wake", `{}`)
	select {
	case <-toolsRequested:
	case <-time.After(time.Second):
		t.Fatal("refresh did not reach tools/list")
	}
	sendReq(t, lists.write, 12, "tools/list", `{}`)
	oldResp := readResp(t, lists.read)
	if !strings.Contains(string(oldResp), "old-tool") || strings.Contains(string(oldResp), "new-tool") {
		t.Fatalf("cache changed before discovery commit: %s", oldResp)
	}

	close(releaseTools)
	waitForCondition(t, 2*time.Second, func() bool { return o.MaterializationState() == MaterializationReady }, "refresh did not commit")
	const barrierMethod = "test/cache-commit-barrier"
	wake.session.SendNotification([]byte(`{"jsonrpc":"2.0","method":"` + barrierMethod + `"}`))
	resultCh := make(chan controllerBarrierResult, 1)
	go func() { resultCh <- scanControllerResponseThroughBarrier(wake.read, 11, barrierMethod) }()
	var committed controllerBarrierResult
	select {
	case committed = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cache commit response and notification barrier")
	}
	if committed.err != nil {
		t.Fatalf("cache commit response: %v", committed.err)
	}
	assertResponseID(t, committed.response, 11)
	for _, method := range []string{"notifications/tools/list_changed", "notifications/prompts/list_changed", "notifications/resources/list_changed"} {
		if got := committed.notifications[method]; got != 1 {
			t.Fatalf("%s notifications = %d, want exactly 1 (all=%v)", method, got, committed.notifications)
		}
	}
	if got := committed.notifications[barrierMethod]; got != 1 {
		t.Fatalf("barrier notifications = %d, want 1", got)
	}

	sendReq(t, lists.write, 13, "tools/list", `{}`)
	newResp := readRespWithID(t, lists.read, 13)
	if !strings.Contains(string(newResp), "new-tool") || strings.Contains(string(newResp), "old-tool") {
		t.Fatalf("cache did not swap after discovery commit: %s", newResp)
	}
}

func TestQueuedDemandWaitsForGenerationInstallAcknowledgement(t *testing.T) {
	publishStarted := make(chan struct{})
	releasePublish := make(chan struct{})
	demandForwarded := make(chan struct{})
	var publishOnce sync.Once
	var demandOnce sync.Once
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"install-ack","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[{"name":"installed-tool"}]}`); err != nil {
					return err
				}
			case "custom/wake":
				demandOnce.Do(func() { close(demandForwarded) })
				if err := writeControllerResponse(stdout, req.ID, `{"installed":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()
	o.onCacheReady = func(*Owner, OwnerSnapshot) bool {
		publishOnce.Do(func() { close(publishStarted) })
		<-releasePublish
		return true
	}

	sendReq(t, s.write, 41, "custom/wake", `{}`)
	select {
	case <-publishStarted:
	case <-time.After(time.Second):
		t.Fatal("generation never reached install acknowledgement")
	}
	select {
	case <-demandForwarded:
		t.Fatal("queued demand reached upstream before generation install acknowledgement")
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePublish)
	select {
	case <-demandForwarded:
	case <-time.After(time.Second):
		t.Fatal("queued demand was not forwarded after generation install acknowledgement")
	}
	response := readRespWithID(t, s.read, 41)
	if !strings.Contains(string(response), `"installed":true`) {
		t.Fatalf("queued demand response=%s", response)
	}
}

func TestFailedCacheRefreshPreservesLastKnownGoodSnapshot(t *testing.T) {
	oldDemandTimeout := materializationDemandTimeout
	materializationDemandTimeout = 250 * time.Millisecond
	defer func() { materializationDemandTimeout = oldDemandTimeout }()
	var starts atomic.Int32
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
			if req.Method == "initialize" {
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"failed-new","version":"2"}}`); err != nil {
					return err
				}
				continue
			}
			if req.Method == "tools/list" {
				return errors.New("synthetic discovery failure")
			}
		}
		return scanner.Err()
	}
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	defer s.close()

	sendReq(t, s.write, 31, "custom/fail-refresh", `{}`)
	failed := scanControllerResponseWithID(s.read, 31)
	if failed.err != nil || !strings.Contains(string(failed.response), "materialization timed out") {
		t.Fatalf("failed materialization response: response=%s err=%v", failed.response, failed.err)
	}
	waitForCondition(t, time.Second, func() bool { return o.MaterializationState() == MaterializationCacheOnly }, "failed refresh did not return to cache_only")
	sendReq(t, s.write, 32, "tools/list", `{}`)
	cached := readRespWithID(t, s.read, 32)
	if !strings.Contains(string(cached), "old-tool") || strings.Contains(string(cached), "failed-new") {
		t.Fatalf("failed staging mutated last-known-good cache: %s", cached)
	}
	if got := starts.Load(); got < 1 {
		t.Fatalf("failed refresh started %d upstream generations, want at least 1", got)
	}
}

func TestPendingPersistenceRetriesAfterDiscoveryFailureWithoutSessions(t *testing.T) {
	firstToolsSeen := make(chan struct{})
	releaseFirstTools := make(chan struct{})
	var starts atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
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
				result := `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"persistent-retry","version":"1"}}`
				if generation == 1 {
					result = `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"persistent":true}},"serverInfo":{"name":"persistent-retry","version":"1"}}`
				}
				if err := writeControllerResponse(stdout, req.ID, result); err != nil {
					return err
				}
			case "tools/list":
				if generation == 1 {
					close(firstToolsSeen)
					<-releaseFirstTools
					return errors.New("synthetic first discovery failure")
				}
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	s := newControllerTestSession(o)
	sendReq(t, s.write, 41, "custom/persistent-retry", `{}`)
	select {
	case <-firstToolsSeen:
	case <-time.After(time.Second):
		t.Fatal("first generation did not latch persistence before tools failure")
	}
	if !o.PersistentPending() {
		t.Fatal("initialize persistent declaration was not latched")
	}
	s.close()
	waitForCondition(t, time.Second, func() bool { return o.SessionCount() == 0 }, "initiating session did not disconnect")
	close(releaseFirstTools)
	waitForCondition(t, 3*time.Second, func() bool {
		return starts.Load() >= 2 && o.MaterializationState() == MaterializationReady
	}, "pending persistence did not retain one retry obligation")
	if o.PersistentPending() {
		t.Fatal("successful commit did not promote the pending obligation")
	}
	o.materializationMu.Lock()
	policy := o.materializationPolicy
	o.materializationMu.Unlock()
	if policy != MaterializationPersistent {
		t.Fatalf("second generation downgraded latched persistence: policy=%s", policy)
	}
}

func TestResolvePersistentCannotDowngradeCommittedPersistence(t *testing.T) {
	o := &Owner{materializationPolicy: MaterializationPersistent}

	o.ResolvePersistent(false)

	o.materializationMu.Lock()
	policy := o.materializationPolicy
	o.materializationMu.Unlock()
	if policy != MaterializationPersistent {
		t.Fatalf("later persistent=false resolution downgraded committed persistence: policy=%s", policy)
	}
}

func TestExportSnapshotPreservesCommittedPersistentPolicy(t *testing.T) {
	o := newMinimalOwner()
	o.materializationPolicy = MaterializationPersistent

	snapshot := o.ExportSnapshot()
	if !snapshot.Persistent {
		t.Fatal("committed persistent policy was not preserved in snapshot")
	}
}

func TestMaterializationStatusReportsCacheOnlyAndLiveGeneration(t *testing.T) {
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"status","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			default:
				if err := writeControllerResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()

	status := o.Status()
	if status["materialization_state"] != string(MaterializationCacheOnly) || status["cache_ready"] != true || status["upstream_live"] != false {
		t.Fatalf("cache-only status = %#v", status)
	}
	if status["materialization_policy"] != "on_demand" {
		t.Fatalf("materialization policy = %#v, want on_demand", status["materialization_policy"])
	}
	s := newControllerTestSession(o)
	defer s.close()
	sendReq(t, s.write, 21, "custom/status", `{}`)
	assertResponseID(t, readRespWithID(t, s.read, 21), 21)
	waitForCondition(t, time.Second, func() bool {
		status = o.Status()
		return status["materialization_state"] == string(MaterializationReady) && status["upstream_live"] == true
	}, "ready status was not published")
	if generation, ok := status["materialization_generation"].(uint64); !ok || generation != 1 {
		t.Fatalf("materialization generation = %#v, want uint64(1)", status["materialization_generation"])
	}
	if status["pending_demand_count"] != 0 || status["persistent_pending"] != false || status["finalization_error"] != "" {
		t.Fatalf("materialization status residue = %#v", status)
	}
}

func TestCacheCommitPublishesBeforeVisibilityAndIsolatedEviction(t *testing.T) {
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"new","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[{"name":"new-tool"}]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeControllerResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	o, err := NewOwnerFromSnapshot(OwnerConfig{HandlerFunc: handler, IPCPath: testIPCPath(t), MaterializationPolicy: MaterializationOnDemand, Logger: testLogger(t)}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	initiator := newControllerTestSession(o)
	evictee := newControllerTestSession(o)
	defer initiator.close()
	defer evictee.close()
	published := make(chan error, 1)
	o.onCacheReady = func(got *Owner, snap OwnerSnapshot) bool {
		if got != o {
			published <- errors.New("cache callback received wrong owner")
			return false
		}
		if current := string(o.getCachedResponse("tools/list")); !strings.Contains(current, "old-tool") {
			published <- fmt.Errorf("old cache changed before publish: %s", current)
			return false
		}
		select {
		case <-evictee.session.Done():
			published <- errors.New("isolated session evicted before publish")
			return false
		default:
		}
		if o.PreRegister("during-commit", "/other", nil) {
			published <- errors.New("fresh admission remained open during cache publish")
			return false
		}
		tools, decodeErr := Base64Decode(snap.CachedTools)
		if decodeErr != nil || !strings.Contains(string(tools), "new-tool") {
			published <- fmt.Errorf("published snapshot missing new cache: %s err=%v", tools, decodeErr)
			return false
		}
		published <- nil
		return true
	}
	response := make(chan controllerResponseResult, 1)
	go func() { response <- scanControllerResponseWithID(initiator.read, 91) }()
	sendReq(t, initiator.write, 91, "custom/wake", `{}`)
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if result := <-response; result.err != nil {
		t.Fatalf("demand response: %v", result.err)
	}
	select {
	case <-evictee.session.Done():
	case <-time.After(time.Second):
		t.Fatal("isolated evictee remained after successful publish")
	}
	if current := string(o.getCachedResponse("tools/list")); !strings.Contains(current, "new-tool") {
		t.Fatalf("new cache not visible after publish: %s", current)
	}
}

func TestMaterializationListChangedFailurePreservesLastKnownGoodCache(t *testing.T) {
	o := newMinimalOwner()
	oldInit := []byte(`{"jsonrpc":"2.0","id":"old-init","result":{"capabilities":{}}}`)
	oldTools := []byte(`{"jsonrpc":"2.0","id":"old-tools","result":{"tools":[{"name":"old"}]}}`)
	proc := &upstream.Process{Done: make(chan struct{})}
	a := newMaterializationAttempt(1, MaterializationTriggerDemand)
	a.process = proc
	a.signals = newMaterializationSignals()

	o.mu.Lock()
	o.upstream = proc
	o.initResp = append([]byte(nil), oldInit...)
	o.toolList = append([]byte(nil), oldTools...)
	o.initDone = true
	o.mu.Unlock()
	o.materializationMu.Lock()
	o.materializationState = MaterializationMaterializing
	o.materializationAttempt = a
	o.cacheStage = &cacheStage{generation: a.generation, process: proc, signals: a.signals}
	o.materializationMu.Unlock()
	var invalidations atomic.Int32
	var publishes atomic.Int32
	o.onCacheInvalidated = func(*Owner) { invalidations.Add(1) }
	o.onCacheReady = func(*Owner, OwnerSnapshot) bool {
		publishes.Add(1)
		return true
	}

	o.cacheResponseFrom(proc, "initialize", []byte(`{"jsonrpc":"2.0","id":"fresh-init","result":{"capabilities":{}}}`))
	notification, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	if err != nil {
		t.Fatalf("parse notification: %v", err)
	}
	if err := o.handleUpstreamMessageFrom(proc, notification); err != nil {
		t.Fatalf("handle list_changed: %v", err)
	}
	o.finishMaterializationFailure(a, errors.New("tools discovery failed"))

	if got := string(o.getCachedResponse("initialize")); got != string(oldInit) {
		t.Fatalf("failed generation changed initialize cache: %s", got)
	}
	if got := string(o.getCachedResponse("tools/list")); got != string(oldTools) {
		t.Fatalf("failed generation changed tools cache: %s", got)
	}
	if got := invalidations.Load(); got != 0 {
		t.Fatalf("failed staged notification invalidated live cache %d times", got)
	}
	if got := publishes.Load(); got != 0 {
		t.Fatalf("failed generation published template %d times", got)
	}
}

func TestStaleProcessEventsQueuedBehindSuccessorInstallAreInert(t *testing.T) {
	o := newMinimalOwner()
	old := &upstream.Process{Done: make(chan struct{})}
	successor := &upstream.Process{Done: make(chan struct{})}
	o.mu.Lock()
	o.upstream = old
	o.mu.Unlock()
	o.materializationMu.Lock()
	o.materializationState = MaterializationReady
	o.materializationMu.Unlock()

	msg, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":"s1:n:7","result":{"tools":[{"name":"stale"}]}}`))
	if err != nil {
		t.Fatalf("parse stale response: %v", err)
	}
	requestID := string(msg.ID)

	// Queue all three old-generation paths behind the writer lease. Installing
	// the successor while owning that lease deterministically reproduces the
	// former check/use windows without timing sleeps or source hooks.
	o.upstreamEventMu.Lock()
	started := make(chan struct{}, 3)
	done := make(chan struct{}, 3)
	handlerErr := make(chan error, 1)
	markResult := make(chan bool, 1)
	go func() {
		started <- struct{}{}
		handlerErr <- o.handleUpstreamMessageFrom(old, msg)
		done <- struct{}{}
	}()
	go func() {
		started <- struct{}{}
		o.monitorMaterializedProcessExit(old)
		done <- struct{}{}
	}()
	go func() {
		started <- struct{}{}
		markResult <- o.markCurrentUpstreamDead(old)
		done <- struct{}{}
	}()
	for range 3 {
		<-started
	}

	o.materializationMu.Lock()
	o.mu.Lock()
	o.upstream = successor
	o.toolList = []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"successor"}]}}`)
	o.upstreamDead.Store(false)
	o.mu.Unlock()
	o.materializationState = MaterializationReady

	o.materializationMu.Unlock()
	o.methodTags.Store(requestID, "tools/list")
	o.inflightTracker.Store(requestID, &InflightRequest{Method: "tools/list", SessionID: 1})
	o.pendingRequests.Store(1)
	o.upstreamEventMu.Unlock()

	for range 3 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("stale process event did not settle")
		}
	}
	if err := <-handlerErr; err != nil {
		t.Fatalf("stale response handler: %v", err)
	}
	if marked := <-markResult; marked {
		t.Fatal("stale deferred reader marked successor dead")
	}
	if got := string(o.getCachedResponse("tools/list")); !strings.Contains(got, "successor") || strings.Contains(got, "stale") {
		t.Fatalf("stale response changed successor cache: %s", got)
	}
	if _, ok := o.methodTags.Load(requestID); !ok {
		t.Fatal("stale response claimed successor method tag")
	}
	if _, ok := o.inflightTracker.Load(requestID); !ok {
		t.Fatal("stale response claimed successor inflight tracker")
	}
	if o.PendingRequests() != 1 {
		t.Fatalf("pending requests = %d, want successor value 1", o.PendingRequests())
	}
	if o.currentUpstream() != successor || o.MaterializationState() != MaterializationReady || o.UpstreamDead() {
		t.Fatalf("stale exit changed successor generation: status=%#v", o.Status())
	}
}

func TestStaleCacheResponseQueuedBehindSuccessorInstallIsInert(t *testing.T) {
	o := newMinimalOwner()
	old := &upstream.Process{Done: make(chan struct{})}
	successor := &upstream.Process{Done: make(chan struct{})}
	o.mu.Lock()
	o.upstream = old
	o.mu.Unlock()
	o.materializationMu.Lock()
	o.materializationState = MaterializationReady
	o.materializationMu.Unlock()

	o.upstreamEventMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		o.cacheResponseFrom(old, "tools/list", []byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"stale"}]}}`))
		close(done)
	}()
	<-started
	o.materializationMu.Lock()
	o.mu.Lock()
	o.upstream = successor
	o.toolList = []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"successor"}]}}`)
	o.upstreamDead.Store(false)
	o.mu.Unlock()
	o.materializationState = MaterializationReady
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale cache response did not settle")
	}
	if got := string(o.getCachedResponse("tools/list")); !strings.Contains(got, "successor") || strings.Contains(got, "stale") {
		t.Fatalf("stale fallback changed successor cache: %s", got)
	}
}

func TestMaterializationListChangedSuccessBroadcastsFromColdCommit(t *testing.T) {
	o := newMinimalOwner()
	var notifications safeBuf
	s := NewSession(strings.NewReader(""), &notifications)
	defer s.Close()
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()
	proc := &upstream.Process{Done: make(chan struct{})}
	a := newMaterializationAttempt(1, MaterializationTriggerDemand)
	a.process = proc
	a.signals = newMaterializationSignals()
	o.mu.Lock()
	o.upstream = proc
	o.mu.Unlock()
	o.materializationMu.Lock()
	o.materializationState = MaterializationMaterializing
	o.materializationAttempt = a
	o.cacheStage = &cacheStage{generation: a.generation, process: proc, signals: a.signals}
	o.materializationMu.Unlock()

	o.cacheResponseFrom(proc, "initialize", []byte(`{"jsonrpc":"2.0","id":"fresh-init","result":{"capabilities":{}}}`))
	notification, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	if err != nil {
		t.Fatalf("parse notification: %v", err)
	}
	if err := o.handleUpstreamMessageFrom(proc, notification); err != nil {
		t.Fatalf("handle list_changed: %v", err)
	}
	o.cacheResponseFrom(proc, "tools/list", []byte(`{"jsonrpc":"2.0","id":"fresh-tools","result":{"tools":[{"name":"fresh"}]}}`))

	waitForCondition(t, time.Second, func() bool {
		return strings.Contains(notifications.String(), "notifications/tools/list_changed")
	}, "cold staged list_changed was not broadcast after commit")
	if got := string(o.getCachedResponse("tools/list")); !strings.Contains(got, "fresh") {
		t.Fatalf("successful staged cache not committed: %s", got)
	}
}

func TestMaterializationDiscoveryErrorsFailWithoutStaging(t *testing.T) {
	for _, method := range []string{"initialize", "tools/list"} {
		t.Run(method, func(t *testing.T) {
			o := newMinimalOwner()
			proc := &upstream.Process{Done: make(chan struct{})}
			a := newMaterializationAttempt(1, MaterializationTriggerDemand)
			a.process = proc
			a.signals = newMaterializationSignals()
			o.materializationAttempt = a
			o.cacheStage = &cacheStage{generation: a.generation, process: proc, signals: a.signals}

			raw := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"discovery failed"}}`)
			if !o.stageMaterializationCacheResponse(proc, method, raw) {
				t.Fatal("materialization response was not claimed")
			}
			select {
			case <-a.signals.failed:
			default:
				t.Fatal("JSON-RPC discovery error did not fail materialization")
			}
			if method == "initialize" {
				select {
				case <-a.signals.initReady:
				default:
					t.Fatal("initialize error did not settle the init usability gate")
				}
				if a.signals.initOK {
					t.Fatal("initialize error marked the response usable")
				}
			}
			if o.cacheStage.initResp != nil || o.cacheStage.toolList != nil {
				t.Fatalf("discovery error entered staged cache: %#v", o.cacheStage)
			}
		})
	}
}

func TestCacheResponseStateRejectsJSONRPCError(t *testing.T) {
	o := newMinimalOwner()
	raw := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"not cacheable"}}`)
	o.cacheResponseState("initialize", raw)
	o.cacheResponseState("tools/list", raw)
	if o.getCachedResponse("initialize") != nil || o.getCachedResponse("tools/list") != nil {
		t.Fatal("JSON-RPC error response entered the reusable cache")
	}
}

func TestLiveCachePublicationRejectionDoesNotCommitLocalTools(t *testing.T) {
	o := newMinimalOwner()
	o.initResp = []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`)
	o.initDone = true
	var staged OwnerSnapshot
	o.onCacheReady = func(got *Owner, snap OwnerSnapshot) bool {
		if got != o {
			t.Fatalf("cache callback owner = %p, want %p", got, o)
		}
		staged = snap
		return false
	}

	raw := []byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"fresh"}]}}`)
	o.cacheResponseState("tools/list", raw)
	if o.getCachedResponse("tools/list") != nil {
		t.Fatal("rejected live cache publication committed tools locally")
	}
	if staged.CachedTools == "" || staged.ClassificationSource != "tools" {
		t.Fatalf("cache callback did not receive staged tools/classification: %+v", staged)
	}
	o.mu.RLock()
	classificationSource := o.classificationSource
	o.mu.RUnlock()
	if classificationSource != "" {
		t.Fatalf("rejected live cache publication committed classification source %q", classificationSource)
	}
}

func TestBlockedMaterializationFinalizationReprovesAutomatically(t *testing.T) {
	done := make(chan struct{})
	close(done)
	proc := &upstream.Process{Done: done}
	o := newMinimalOwner()
	o.initResp = []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	a := newMaterializationAttempt(1, MaterializationTriggerDemand)
	o.materializationAttempt = a
	o.materializationState = MaterializationFinalizing
	o.retiringProcess = proc
	o.upstream = proc

	var allowProof atomic.Bool
	o.materializationFinalizationProbe = func(*upstream.Process) error {
		if allowProof.Load() {
			return nil
		}
		return errors.New("synthetic authority still live")
	}
	blockErr := o.blockMaterializationFinalization(a, proc, errors.New("synthetic authority still live"))
	o.finishMaterializationFailure(a, blockErr)
	allowProof.Store(true)

	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationCacheOnly && o.currentUpstream() == nil
	}, "blocked materialization finalization was not automatically re-proven")
}

func TestRetirementReproofBeforeAttemptFailureStillRestartsLiveOwner(t *testing.T) {
	var starts atomic.Int32
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"reproof","version":"2"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	session := newControllerTestSession(o)
	defer session.close()

	retired := upstream.NewProcessFromHandler(context.Background(), func(context.Context, io.Reader, io.Writer) error { return nil })
	select {
	case <-retired.Done:
	case <-time.After(time.Second):
		t.Fatal("retired fixture did not exit")
	}
	a := newMaterializationAttempt(1, MaterializationTriggerUpstreamExit)
	a.process = retired
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.Lock()
	o.upstream = retired
	o.upstreamDead.Store(true)
	o.mu.Unlock()
	o.materializationGeneration = 1
	o.materializationAttempt = a
	o.materializationState = MaterializationFinalizeBlocked
	o.materializationBlockedErr = errors.New("synthetic authority remained")
	o.retiringProcess = retired
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	o.materializationFinalizationProbe = func(got *upstream.Process) error {
		if got != retired {
			return fmt.Errorf("reproof process = %p, want %p", got, retired)
		}
		return nil
	}

	if !o.completeRetiringProcess(retired, MaterializationTriggerUpstreamExit) {
		t.Fatal("retirement reproof did not succeed")
	}
	o.finishMaterializationFailure(a, errors.New("replacement readiness failed"))

	waitForCondition(t, 2*time.Second, func() bool {
		return starts.Load() == 1 && o.MaterializationState() == MaterializationReady
	}, "live owner did not restart after retirement reproof won the attempt-settlement race")
}

func TestRetirementReproofKeepsFreshDemandForPostAttemptRecovery(t *testing.T) {
	var starts atomic.Int32
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"reproof-demand","version":"2"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeControllerResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/recover":
				if err := writeControllerResponse(stdout, req.ID, `{"replacement":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	o, err := NewOwnerFromSnapshot(OwnerConfig{
		HandlerFunc:           handler,
		IPCPath:               testIPCPath(t),
		MaterializationPolicy: MaterializationOnDemand,
		Logger:                testLogger(t),
	}, controllerSnapshot())
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	session := newControllerTestSession(o)
	defer session.close()

	retired := upstream.NewProcessFromHandler(context.Background(), func(context.Context, io.Reader, io.Writer) error { return nil })
	select {
	case <-retired.Done:
	case <-time.After(time.Second):
		t.Fatal("retired fixture did not exit")
	}
	a := newMaterializationAttempt(1, MaterializationTriggerUpstreamExit)
	a.process = retired
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.Lock()
	o.upstream = retired
	o.upstreamDead.Store(true)
	o.mu.Unlock()
	o.materializationGeneration = 1
	o.materializationAttempt = a
	o.materializationState = MaterializationFinalizeBlocked
	o.materializationBlockedErr = errors.New("synthetic authority remained")
	o.retiringProcess = retired
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	o.materializationFinalizationProbe = func(got *upstream.Process) error {
		if got != retired {
			return fmt.Errorf("reproof process = %p, want %p", got, retired)
		}
		return nil
	}

	if !o.completeRetiringProcess(retired, MaterializationTriggerUpstreamExit) {
		t.Fatal("retirement reproof did not succeed")
	}
	msg, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":72,"method":"custom/recover","params":{}}`))
	if err != nil {
		t.Fatalf("parse recovery demand: %v", err)
	}
	handled, err := o.enqueueLocalDemand(session.session, msg)
	if err != nil {
		t.Fatalf("enqueue recovery demand: %v", err)
	}
	if !handled {
		t.Fatal("recovery demand bypassed materialization controller")
	}
	key := string(remap.Remap(session.session.ID, msg.ID))
	o.materializationMu.Lock()
	demand := o.pendingDemands[key]
	trigger := o.restartResumeTrigger
	o.materializationMu.Unlock()
	if trigger != MaterializationTriggerUpstreamExit {
		t.Fatalf("post-attempt trigger = %q, want %q", trigger, MaterializationTriggerUpstreamExit)
	}
	if demand == nil {
		t.Fatal("recovery demand was not retained")
	}
	if demand.generation != 0 {
		t.Fatalf("recovery demand generation = %d, want 0 until failed attempt settles", demand.generation)
	}

	o.finishMaterializationFailure(a, errors.New("replacement readiness failed"))
	result := make(chan controllerResponseResult, 1)
	go func() { result <- scanControllerResponseWithID(session.read, 72) }()
	select {
	case got := <-result:
		if got.err != nil || !strings.Contains(string(got.response), `"replacement":true`) {
			t.Fatalf("recovery response: response=%s err=%v", got.response, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovery demand was not served by the successor generation")
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("successor starts = %d, want 1", got)
	}
}

func TestBlockedQueuedWriteDoesNotHoldMaterializationMutex(t *testing.T) {
	o := newMinimalOwner()
	var upstream bytes.Buffer
	o.upstreamWriter = &upstream
	s := NewSession(strings.NewReader(""), io.Discard)
	o.admissionMu.Lock()
	o.addSessionLocked(s)
	o.admissionMu.Unlock()
	defer s.Close()

	msg, err := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":41,"method":"custom/blocked","params":{}}`))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	key := string(remap.Remap(s.ID, msg.ID))
	o.pendingDemands[key] = &localDemand{key: key, session: s, message: msg, state: localDemandWaiting, generation: 1}
	o.materializationState = MaterializationReady

	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	o.beforeQueuedDemandWrite = func() {
		close(writeEntered)
		<-releaseWrite
	}
	forwardDone := make(chan struct{})
	go func() {
		o.forwardQueuedDemand(key, 1, o.launchContextForSession(s))
		close(forwardDone)
	}()
	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("queued write did not reach the blocking boundary")
	}

	lockAcquired := make(chan struct{})
	go func() {
		o.materializationMu.Lock()
		o.materializationMu.Unlock()
		close(lockAcquired)
	}()
	select {
	case <-lockAcquired:
	case <-time.After(150 * time.Millisecond):
		close(releaseWrite)
		<-forwardDone
		t.Fatal("blocked upstream write held materializationMu")
	}
	close(releaseWrite)
	select {
	case <-forwardDone:
	case <-time.After(time.Second):
		t.Fatal("queued write did not complete")
	}
}
