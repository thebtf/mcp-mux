package supervisor

import (
	"fmt"
	"testing"
)

func mustParseCorrelationFrame(t *testing.T, raw string) *parsedFrame {
	t.Helper()
	frame, err := parseFrame([]byte(raw), 4096)
	if err != nil || frame.utilityErr != nil {
		t.Fatalf("parse correlation frame: parse=%v utility=%v raw=%s", err, frame.utilityErr, raw)
	}
	return frame
}

func TestCorrelationTombstonesPreventImmediateReuseAndStayBounded(t *testing.T) {
	registry := newDirectionRegistry()
	request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"same","method":"tools/list"}`)
	record := registry.register(request, 1)
	registry.cancel(record)
	if err := registry.canRegister(request); err == nil {
		t.Fatal("immediate request ID reuse bypassed tombstone")
	}

	progressRole := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"other","method":"tools/call","params":{"_meta":{"progressToken":"same"}}}`)
	if err := registry.canRegister(progressRole); err != nil {
		t.Fatalf("request-ID tombstone crossed into progress-token role: %v", err)
	}

	tokenRequest := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"source","method":"tools/call","params":{"_meta":{"progressToken":"role"}}}`)
	tokenRecord := registry.register(tokenRequest, 1)
	registry.cancel(tokenRecord)
	requestRole := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"role","method":"tools/list"}`)
	if err := registry.canRegister(requestRole); err != nil {
		t.Fatalf("progress-token tombstone crossed into request-ID role: %v", err)
	}

	for index := 0; index < tombstoneLimit+32; index++ {
		frame := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"bounded-%d","method":"tools/list"}`, index))
		registry.cancel(registry.register(frame, 1))
	}
	if got := len(registry.tombstones.values); got != tombstoneLimit {
		t.Fatalf("tombstones = %d, want bounded %d", got, tombstoneLimit)
	}
	latest := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"bounded-%d","method":"tools/list"}`, tombstoneLimit+31))
	if err := registry.canRegister(latest); err == nil {
		t.Fatal("latest bounded tombstone was lost")
	}
	registry.clearTransitionTombstones()
	if err := registry.canRegister(latest); err != nil {
		t.Fatalf("transition clear did not release tombstone: %v", err)
	}
}

func TestCorrelationActiveRequestsStayBounded(t *testing.T) {
	registry := newDirectionRegistry()
	for index := 0; index < activeCorrelationLimit; index++ {
		frame := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"active-%d","method":"tools/list"}`, index))
		if err := registry.canRegister(frame); err != nil {
			t.Fatalf("register active request %d: %v", index, err)
		}
		registry.register(frame, 1)
	}
	overflow := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"overflow","method":"tools/list"}`)
	if err := registry.canRegister(overflow); err == nil {
		t.Fatal("active correlation limit accepted another request")
	}
}

func TestSuccessfulReplayPreservesOnlyRetiredChildRequestFence(t *testing.T) {
	childInput := new(bufferWriteCloser)
	runner := &runner{
		state:             StateReplaying,
		current:           &generation{id: 2, stdin: childInput},
		child:             newDirectionRegistry(),
		host:              newDirectionRegistry(),
		initializeRequest: []byte(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`),
		initialized:       []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
	}

	completed := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"completed","method":"tools/list"}`)
	completedRecord := runner.child.register(completed, 1)
	completedResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"completed","result":{"tools":[]}}`)
	runner.child.complete(completedRecord, completedResponse)

	retired := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"retired","method":"tools/list","params":{"_meta":{"progressToken":"retired-token"}}}`)
	runner.child.register(retired, 1)
	runner.child.retireGeneration(1)
	tokenReuse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"token-reuse","method":"tools/list","params":{"_meta":{"progressToken":"retired-token"}}}`)

	hostRetired := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"host-retired","method":"tools/list"}`)
	runner.host.register(hostRetired, 1)
	runner.host.retireGeneration(1)

	replay := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"replacement","version":"1"}}}`)
	runner.completeReplay(replay)

	if err := runner.child.canRegister(completed); err != nil {
		t.Fatalf("completed child request ID remained fenced after replay: %v", err)
	}
	if err := runner.child.canRegister(retired); err == nil {
		t.Fatal("successful replay released a retired child request ID")
	}
	if err := runner.child.canRegister(tokenReuse); err == nil {
		t.Fatal("successful replay released a retired child progress token")
	}
	if err := runner.host.canRegister(hostRetired); err != nil {
		t.Fatalf("successful replay retained a retired host request ID: %v", err)
	}

	before := childInput.String()
	lateHostResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"retired","result":{"tools":[]}}`)
	runner.state = StateStarting
	runner.current = nil
	runner.handleHostEvent(hostFrameEvent{frame: lateHostResponse})
	if got := childInput.String(); got != before {
		t.Fatalf("late host response reached replacement child: %s", got)
	}
	if err := runner.child.canRegister(retired); err == nil {
		t.Fatal("late host response released the retired ID before the next replay")
	}
	if err := runner.child.canRegister(tokenReuse); err == nil {
		t.Fatal("late host response released the retired token before the next replay")
	}

	runner.state = StateReplaying
	runner.current = &generation{id: 3, stdin: childInput}
	runner.completeReplay(replay)
	if err := runner.child.canRegister(retired); err != nil {
		t.Fatalf("consumed retired response did not release the ID at the next replay: %v", err)
	}
	if err := runner.child.canRegister(tokenReuse); err != nil {
		t.Fatalf("consumed retired response did not release the token at the next replay: %v", err)
	}
}

func TestTaskCompletionRejectsDuplicateLiveTaskID(t *testing.T) {
	registry := newDirectionRegistry()
	first := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"task":{}}}`)
	firstRecord := registry.register(first, 1)
	firstResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":1,"result":{"task":{"taskId":"task-1","status":"working"}}}`)
	if err := registry.canComplete(firstRecord, firstResponse); err != nil {
		t.Fatal(err)
	}
	registry.complete(firstRecord, firstResponse)

	second := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"task":{}}}`)
	secondRecord := registry.register(second, 1)
	duplicate := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":2,"result":{"task":{"taskId":"task-1","status":"input_required"}}}`)
	if err := registry.canComplete(secondRecord, duplicate); err == nil {
		t.Fatal("duplicate live task ID was accepted")
	}
	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":2,"result":{"task":{"taskId":"task-1","status":"completed"}}}`)
	if err := registry.canComplete(secondRecord, terminal); err != nil {
		t.Fatalf("terminal task result should not bind a duplicate live task: %v", err)
	}
	missing := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`)
	if err := registry.canComplete(secondRecord, missing); err == nil {
		t.Fatal("task-augmented request accepted a result without task metadata")
	}
	malformed, parseErr := parseFrame([]byte(`{"jsonrpc":"2.0","id":2,"result":{"task":{"taskId":"task-2","status":"unknown"}}}`), 4096)
	if parseErr != nil || malformed.utilityErr == nil {
		t.Fatalf("malformed task result parse=%v utility=%v", parseErr, malformed.utilityErr)
	}
	if err := registry.canComplete(secondRecord, malformed); err == nil {
		t.Fatal("task-augmented request accepted malformed task metadata")
	}
	errorResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"rejected"}}`)
	if err := registry.canComplete(secondRecord, errorResponse); err != nil {
		t.Fatalf("task-augmented error response rejected: %v", err)
	}
}

func TestTaskRequestSupportedUsesReceiverCapabilities(t *testing.T) {
	serverInitialize := []byte(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"tasks":{"requests":{"tools":{"call":{}}}}},"serverInfo":{"name":"server","version":"1"}}}`)
	if !taskRequestSupported(serverInitialize, true, "tools/call") {
		t.Fatal("server tools/call task capability was not recognized")
	}
	for _, method := range []string{"tools/list", "sampling/createMessage", "unknown/call"} {
		if taskRequestSupported(serverInitialize, true, method) {
			t.Fatalf("server capability unexpectedly authorized %s", method)
		}
	}

	clientInitialize := []byte(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"tasks":{"requests":{"sampling":{"createMessage":{}},"elicitation":{"create":{}}}}},"clientInfo":{"name":"client","version":"1"}}}`)
	for _, method := range []string{"sampling/createMessage", "elicitation/create"} {
		if !taskRequestSupported(clientInitialize, false, method) {
			t.Fatalf("client capability did not authorize %s", method)
		}
	}
	if taskRequestSupported(clientInitialize, false, "tools/call") || taskRequestSupported(nil, false, "sampling/createMessage") {
		t.Fatal("absent client request capability authorized task augmentation")
	}
}

func TestTaskOperationResponsesRetireWithoutStatusNotification(t *testing.T) {
	seed := func(t *testing.T) *directionRegistry {
		t.Helper()
		registry := newDirectionRegistry()
		request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","method":"tools/call","params":{"_meta":{"progressToken":"progress"},"task":{}}}`)
		record := registry.register(request, 1)
		response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","result":{"task":{"taskId":"task-1","status":"working"}}}`)
		if err := registry.canComplete(record, response); err != nil {
			t.Fatal(err)
		}
		registry.complete(record, response)
		if registry.tasks["task-1"] == nil || len(registry.tokens) != 1 {
			t.Fatalf("seed task state = tasks:%d tokens:%d", len(registry.tasks), len(registry.tokens))
		}
		return &registry
	}

	t.Run("non-terminal get remains live", func(t *testing.T) {
		registry := seed(t)
		operation := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","method":"tasks/get","params":{"taskId":"task-1"}}`)
		record := registry.register(operation, 1)
		response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","result":{"taskId":"task-1","status":"input_required","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
		if err := registry.canComplete(record, response); err != nil {
			t.Fatal(err)
		}
		registry.complete(record, response)
		if task := registry.tasks["task-1"]; task == nil || task.status != "input_required" || len(registry.tokens) != 1 {
			t.Fatalf("non-terminal state = task:%#v tokens:%d", task, len(registry.tokens))
		}
	})

	for _, method := range []string{"tasks/get", "tasks/cancel", "tasks/result"} {
		t.Run(method+" error remains live", func(t *testing.T) {
			registry := seed(t)
			operation := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"operation","method":%q,"params":{"taskId":"task-1"}}`, method))
			record := registry.register(operation, 1)
			response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"operation","error":{"code":-32000,"message":"not ready"}}`)
			if err := registry.canComplete(record, response); err != nil {
				t.Fatal(err)
			}
			registry.complete(record, response)
			if task := registry.tasks["task-1"]; task == nil || len(registry.tokens) != 1 {
				t.Fatalf("operation error retired live task: task=%#v tokens=%d", task, len(registry.tokens))
			}
		})
	}

	for _, testCase := range []struct {
		name     string
		method   string
		response string
	}{
		{name: "terminal get", method: "tasks/get", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`},
		{name: "cancel", method: "tasks/cancel", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"cancelled","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`},
		{name: "result", method: "tasks/result", response: `{"jsonrpc":"2.0","id":"operation","result":{"content":[]}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := seed(t)
			operation := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"operation","method":%q,"params":{"taskId":"task-1"}}`, testCase.method))
			record := registry.register(operation, 1)
			response := mustParseCorrelationFrame(t, testCase.response)
			if err := registry.canComplete(record, response); err != nil {
				t.Fatal(err)
			}
			registry.complete(record, response)
			if len(registry.tasks) != 0 || len(registry.tokens) != 0 {
				t.Fatalf("terminal operation retained state: tasks=%d tokens=%d", len(registry.tasks), len(registry.tokens))
			}
		})
	}
}

func TestTaskOperationResponseRejectsMismatchedTaskID(t *testing.T) {
	registry := newDirectionRegistry()
	registry.tasks["task-1"] = &taskRecord{id: "task-1", generation: 1}
	operation := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","method":"tasks/get","params":{"taskId":"task-1"}}`)
	record := registry.register(operation, 1)
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","result":{"taskId":"other","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
	if err := registry.canComplete(record, response); err == nil {
		t.Fatal("tasks/get accepted a mismatched taskId")
	}
}
