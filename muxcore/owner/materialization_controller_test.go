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
	if state := o.MaterializationState(); state != MaterializationReady {
		t.Fatalf("materialization state = %s, want ready", state)
	}
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
	if status := o.Status(); status["pending_demand_count"] != 0 {
		t.Fatalf("pending demand residue: %#v", status)
	}
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
				if err := writeControllerResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"persistent":true}},"serverInfo":{"name":"persistent-retry","version":"1"}}`); err != nil {
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
		t.Fatal("successful persistent commit did not promote the pending obligation")
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
	s := newControllerTestSession(o)
	defer s.close()
	sendReq(t, s.write, 21, "custom/status", `{}`)
	assertResponseID(t, readRespWithID(t, s.read, 21), 21)
	status = o.Status()
	if status["materialization_state"] != string(MaterializationReady) || status["upstream_live"] != true {
		t.Fatalf("ready status = %#v", status)
	}
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
