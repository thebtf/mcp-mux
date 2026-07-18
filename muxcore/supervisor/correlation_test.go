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

func seedRetiredTaskRequest(t *testing.T, registry *directionRegistry, requestID, token string) *parsedFrame {
	t.Helper()
	request := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"sampling/createMessage","params":{"_meta":{"progressToken":%q},"task":{},"messages":[]}}`, requestID, token))
	if err := registry.canRegister(request); err != nil {
		t.Fatal(err)
	}
	registry.register(request, 1)
	registry.retireGeneration(1)
	return request
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

	for index := range tombstoneLimit + 32 {
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
	for index := range activeCorrelationLimit {
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
	duplicate := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":2,"result":{"task":{"taskId":"task-1","status":"working"}}}`)
	if err := registry.canComplete(secondRecord, duplicate); err == nil {
		t.Fatal("duplicate live task ID was accepted")
	}
	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":2,"result":{"task":{"taskId":"task-1","status":"completed"}}}`)
	if err := registry.canComplete(secondRecord, terminal); err == nil {
		t.Fatal("terminal initial task result was accepted")
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

func TestCreateTaskResultRequiresInitialWorking(t *testing.T) {
	for _, status := range []string{"input_required", "completed", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			registry := newDirectionRegistry()
			request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"request","method":"tools/call","params":{"task":{}}}`)
			record := registry.register(request, 1)
			response := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"request","result":{"task":{"taskId":"task-1","status":%q}}}`, status))
			if err := registry.canComplete(record, response); err == nil {
				t.Fatalf("initial CreateTaskResult status %q was accepted", status)
			}
		})
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

func TestRetiredLateWorkingResultPreservesTaskAndToken(t *testing.T) {
	registry := newDirectionRegistry()
	request := seedRetiredTaskRequest(t, &registry, "retired-request", "retired-token")
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"retired-request","result":{"task":{"taskId":"retired-task","status":"working"}}}`)
	handled, err := registry.consumeRetiredResponse(response)
	if err != nil || !handled {
		t.Fatalf("consume retired response: handled=%v err=%v", handled, err)
	}
	task := registry.retiredTasks["retired-task"]
	if task == nil || task.status != "working" || task.generation != 1 || task.requestID != request.id.key {
		t.Fatalf("retired task = %#v", task)
	}
	if task.progressToken == nil || request.progressToken == nil || *task.progressToken != request.progressToken.key {
		t.Fatalf("retired task token = %#v, request token = %#v", task.progressToken, request.progressToken)
	}
	if _, ok := registry.retiredTokens[request.progressToken.key]; !ok {
		t.Fatal("retired progress token association was lost")
	}
	tokenReuse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"token-reuse","method":"roots/list","params":{"_meta":{"progressToken":"retired-token"}}}`)
	if err := registry.canRegister(tokenReuse); err == nil {
		t.Fatal("retired task progress token was reusable")
	}
	replacement := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","method":"sampling/createMessage","params":{"task":{},"messages":[]}}`)
	replacementRecord := registry.register(replacement, 2)
	reusedTask := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","result":{"task":{"taskId":"retired-task","status":"working"}}}`)
	if err := registry.canComplete(replacementRecord, reusedTask); err == nil {
		t.Fatal("retired task ID was reusable")
	}
}

func TestRetiredTaskErrorTransitionsRequestAndTokenToTombstones(t *testing.T) {
	registry := newDirectionRegistry()
	request := seedRetiredTaskRequest(t, &registry, "retired-error", "retired-error-token")
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"retired-error","error":{"code":-32000,"message":"rejected"}}`)
	handled, err := registry.consumeRetiredResponse(response)
	if err != nil || !handled {
		t.Fatalf("consume retired error: handled=%v err=%v", handled, err)
	}
	if len(registry.retiredRequests) != 0 || len(registry.retiredTokens) != 0 || len(registry.retiredTasks) != 0 {
		t.Fatalf("retired error retained state: requests=%d tokens=%d tasks=%d", len(registry.retiredRequests), len(registry.retiredTokens), len(registry.retiredTasks))
	}
	if !registry.retiredTombstones.contains(tombstoneRequest, request.id.key) || !registry.retiredTombstones.contains(tombstoneToken, request.progressToken.key) {
		t.Fatal("retired error did not transition request and token to tombstones")
	}
}

func TestInvalidRetiredCreateTaskResultKeepsFence(t *testing.T) {
	testCases := []struct {
		name string
		raw  string
	}{
		{name: "missing task", raw: `{"jsonrpc":"2.0","id":"retired-invalid","result":{"content":[]}}`},
		{name: "input required", raw: `{"jsonrpc":"2.0","id":"retired-invalid","result":{"task":{"taskId":"task-1","status":"input_required"}}}`},
		{name: "completed", raw: `{"jsonrpc":"2.0","id":"retired-invalid","result":{"task":{"taskId":"task-1","status":"completed"}}}`},
		{name: "failed", raw: `{"jsonrpc":"2.0","id":"retired-invalid","result":{"task":{"taskId":"task-1","status":"failed"}}}`},
		{name: "cancelled", raw: `{"jsonrpc":"2.0","id":"retired-invalid","result":{"task":{"taskId":"task-1","status":"cancelled"}}}`},
		{name: "malformed", raw: `{"jsonrpc":"2.0","id":"retired-invalid","result":{"task":{"taskId":"task-1","status":"unknown"}}}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			registry := newDirectionRegistry()
			request := seedRetiredTaskRequest(t, &registry, "retired-invalid", "retired-invalid-token")
			response, parseErr := parseFrame([]byte(testCase.raw), 4096)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			handled, err := registry.consumeRetiredResponse(response)
			if err == nil || !handled {
				t.Fatalf("invalid retired result: handled=%v err=%v", handled, err)
			}
			if registry.retiredRequests[request.id.key] == nil {
				t.Fatal("invalid retired result released request fence")
			}
			if _, ok := registry.retiredTokens[request.progressToken.key]; !ok {
				t.Fatal("invalid retired result released token fence")
			}
			if len(registry.retiredTasks) != 0 {
				t.Fatal("invalid retired result created task state")
			}
		})
	}
}

func TestRetiredLateResultRejectsEveryTaskIDCollision(t *testing.T) {
	testCases := []struct {
		name  string
		setup func(*directionRegistry)
	}{
		{name: "active", setup: func(registry *directionRegistry) {
			registry.tasks["collision"] = &taskRecord{id: "collision", generation: 2}
		}},
		{name: "retired", setup: func(registry *directionRegistry) {
			registry.retiredTasks["collision"] = &taskRecord{id: "collision", generation: 1}
		}},
		{name: "tombstoned", setup: func(registry *directionRegistry) {
			registry.tombstones.add(tombstoneTask, taskTombstoneKey("collision"))
		}},
		{name: "retired tombstoned", setup: func(registry *directionRegistry) {
			registry.retiredTombstones.add(tombstoneTask, taskTombstoneKey("collision"))
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			registry := newDirectionRegistry()
			request := seedRetiredTaskRequest(t, &registry, "late-collision", "late-collision-token")
			testCase.setup(&registry)
			response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"late-collision","result":{"task":{"taskId":"collision","status":"working"}}}`)
			handled, err := registry.consumeRetiredResponse(response)
			if err == nil || !handled {
				t.Fatalf("task ID collision: handled=%v err=%v", handled, err)
			}
			if registry.retiredRequests[request.id.key] == nil {
				t.Fatal("task ID collision released the retired request fence")
			}
			if _, ok := registry.retiredTokens[request.progressToken.key]; !ok {
				t.Fatal("task ID collision released the retired token fence")
			}
		})
	}
}

func TestRetiredTaskIDReuseRejected(t *testing.T) {
	registry := newDirectionRegistry()
	request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","method":"tools/call","params":{"task":{}}}`)
	record := registry.register(request, 1)
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","result":{"task":{"taskId":"task-1","status":"working"}}}`)
	if err := registry.canComplete(record, response); err != nil {
		t.Fatal(err)
	}
	registry.complete(record, response)
	registry.retireGeneration(1)

	replacement := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","method":"tools/call","params":{"task":{}}}`)
	replacementRecord := registry.register(replacement, 2)
	reused := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","result":{"task":{"taskId":"task-1","status":"working"}}}`)
	if err := registry.canComplete(replacementRecord, reused); err == nil {
		t.Fatal("replacement CreateTaskResult reused a retired task ID")
	}
}

func TestTerminalRetiredTaskStatusFencesImmediateReuse(t *testing.T) {
	registry := newDirectionRegistry()
	request := seedRetiredTaskRequest(t, &registry, "late-working", "late-working-token")
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"late-working","result":{"task":{"taskId":"retired-task","status":"working"}}}`)
	if handled, err := registry.consumeRetiredResponse(response); err != nil || !handled {
		t.Fatalf("consume retired working response: handled=%v err=%v", handled, err)
	}
	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"retired-task","status":"completed"}}`)
	if handled, err := registry.consumeRetiredTaskStatus(terminal.taskStatus); err != nil || !handled {
		t.Fatalf("consume retired terminal status: handled=%v err=%v", handled, err)
	}
	if len(registry.retiredTasks) != 0 || len(registry.retiredTokens) != 0 {
		t.Fatalf("terminal retired status retained state: tasks=%d tokens=%d", len(registry.retiredTasks), len(registry.retiredTokens))
	}
	if !registry.retiredTombstones.contains(tombstoneTask, taskTombstoneKey("retired-task")) || !registry.retiredTombstones.contains(tombstoneToken, request.progressToken.key) {
		t.Fatal("terminal retired status did not create task and token tombstones")
	}
	replacement := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","method":"tools/call","params":{"task":{}}}`)
	replacementRecord := registry.register(replacement, 2)
	reused := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","result":{"task":{"taskId":"retired-task","status":"working"}}}`)
	if err := registry.canComplete(replacementRecord, reused); err == nil {
		t.Fatal("terminal retired task ID was immediately reusable")
	}
	tokenReuse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"token-reuse","method":"roots/list","params":{"_meta":{"progressToken":"late-working-token"}}}`)
	if err := registry.canRegister(tokenReuse); err == nil {
		t.Fatal("terminal retired task token was immediately reusable")
	}
}

func TestSuccessfulReplayPreservesRetiredTaskTransitionFences(t *testing.T) {
	childInput := new(bufferWriteCloser)
	runner := &runner{
		state:             StateReplaying,
		current:           &generation{id: 2, stdin: childInput},
		child:             newDirectionRegistry(),
		host:              newDirectionRegistry(),
		initializeRequest: []byte(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`),
		initialized:       []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
	}
	request := seedRetiredTaskRequest(t, &runner.child, "retired-request", "retired-token")
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"retired-request","result":{"task":{"taskId":"retired-task","status":"working"}}}`)
	if handled, err := runner.child.consumeRetiredResponse(response); err != nil || !handled {
		t.Fatalf("consume retired response: handled=%v err=%v", handled, err)
	}
	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"retired-task","status":"completed"}}`)
	if handled, err := runner.child.consumeRetiredTaskStatus(terminal.taskStatus); err != nil || !handled {
		t.Fatalf("consume retired terminal status: handled=%v err=%v", handled, err)
	}
	replay := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"replacement","version":"1"}}}`)
	runner.completeReplay(replay)

	if err := runner.child.canRegister(request); err == nil {
		t.Fatal("successful replay released retired task request ID fence")
	}
	tokenReuse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"token-reuse","method":"roots/list","params":{"_meta":{"progressToken":"retired-token"}}}`)
	if err := runner.child.canRegister(tokenReuse); err == nil {
		t.Fatal("successful replay released retired task token fence")
	}
	replacement := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","method":"tools/call","params":{"task":{}}}`)
	replacementRecord := runner.child.register(replacement, 2)
	reused := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","result":{"task":{"taskId":"retired-task","status":"working"}}}`)
	if err := runner.child.canComplete(replacementRecord, reused); err == nil {
		t.Fatal("successful replay released retired task ID fence")
	}
}

func TestActiveAndRetiredCorrelationBudgetsStayBounded(t *testing.T) {
	t.Run("request IDs", func(t *testing.T) {
		registry := newDirectionRegistry()
		split := activeCorrelationLimit / 2
		for index := range split {
			frame := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"retired-%d","method":"roots/list"}`, index))
			if err := registry.canRegister(frame); err != nil {
				t.Fatal(err)
			}
			registry.register(frame, 1)
		}
		registry.retireGeneration(1)
		for index := split; index < activeCorrelationLimit; index++ {
			frame := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"active-%d","method":"roots/list"}`, index))
			if err := registry.canRegister(frame); err != nil {
				t.Fatal(err)
			}
			registry.register(frame, 2)
		}
		if got := len(registry.requests) + len(registry.retiredRequests); got != activeCorrelationLimit {
			t.Fatalf("request correlations = %d, want %d", got, activeCorrelationLimit)
		}
		overflow := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"overflow","method":"roots/list"}`)
		if err := registry.canRegister(overflow); err == nil {
			t.Fatal("active+retired request budget accepted overflow")
		}
	})

	t.Run("progress tokens and task IDs", func(t *testing.T) {
		registry := newDirectionRegistry()
		addTask := func(index int, generation uint64) {
			t.Helper()
			request := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"request-%d","method":"tools/call","params":{"_meta":{"progressToken":"token-%d"},"task":{}}}`, index, index))
			if err := registry.canRegister(request); err != nil {
				t.Fatal(err)
			}
			record := registry.register(request, generation)
			response := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"request-%d","result":{"task":{"taskId":"task-%d","status":"working"}}}`, index, index))
			if err := registry.canComplete(record, response); err != nil {
				t.Fatal(err)
			}
			registry.complete(record, response)
		}
		split := activeCorrelationLimit / 2
		for index := range split {
			addTask(index, 1)
		}
		registry.retireGeneration(1)
		for index := split; index < activeCorrelationLimit; index++ {
			addTask(index, 2)
		}
		if got := len(registry.tokens) + len(registry.retiredTokens); got != activeCorrelationLimit {
			t.Fatalf("progress token correlations = %d, want %d", got, activeCorrelationLimit)
		}
		if got := len(registry.tasks) + len(registry.retiredTasks); got != activeCorrelationLimit {
			t.Fatalf("task correlations = %d, want %d", got, activeCorrelationLimit)
		}
		tokenOverflow := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"token-overflow","method":"roots/list","params":{"_meta":{"progressToken":"token-overflow"}}}`)
		if err := registry.canRegister(tokenOverflow); err == nil {
			t.Fatal("active+retired progress token budget accepted overflow")
		}
		taskOverflow := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"task-overflow","method":"tools/call","params":{"task":{}}}`)
		if err := registry.canRegister(taskOverflow); err != nil {
			t.Fatal(err)
		}
		overflowRecord := registry.register(taskOverflow, 2)
		overflowResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"task-overflow","result":{"task":{"taskId":"task-overflow","status":"working"}}}`)
		if err := registry.canComplete(overflowRecord, overflowResponse); err == nil {
			t.Fatal("active+retired task budget accepted overflow")
		}
	})
}

func TestRetiredTaskTransitionTombstonesStayBounded(t *testing.T) {
	registry := newDirectionRegistry()
	for index := range tombstoneLimit + 32 {
		registry.retiredTombstones.add(tombstoneTask, taskTombstoneKey(fmt.Sprintf("task-%d", index)))
	}
	if got := len(registry.retiredTombstones.values); got != tombstoneLimit {
		t.Fatalf("retired task tombstones = %d, want bounded %d", got, tombstoneLimit)
	}
	if !registry.retiredTombstones.contains(tombstoneTask, taskTombstoneKey(fmt.Sprintf("task-%d", tombstoneLimit+31))) {
		t.Fatal("latest task tombstone was lost")
	}
}

func TestHostRetiredTaskStatusIsConsumedBeforeCurrentGeneration(t *testing.T) {
	childInput := new(bufferWriteCloser)
	runner := &runner{
		state:             StateActive,
		current:           &generation{id: 2, stdin: childInput},
		child:             newDirectionRegistry(),
		host:              newDirectionRegistry(),
		initializeRequest: []byte(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`),
		initialized:       []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
	}
	seedRetiredTaskRequest(t, &runner.child, "late-status", "late-status-token")
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"late-status","result":{"task":{"taskId":"retired-task","status":"working"}}}`)
	if handled, err := runner.child.consumeRetiredResponse(response); err != nil || !handled {
		t.Fatalf("consume retired response: handled=%v err=%v", handled, err)
	}

	ongoing := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"retired-task","status":"input_required"}}`)
	runner.handleHostEvent(hostFrameEvent{frame: ongoing})
	if got := childInput.String(); got != "" {
		t.Fatalf("retired ongoing status reached replacement child: %s", got)
	}
	if task := runner.child.retiredTasks["retired-task"]; task == nil || task.status != "input_required" {
		t.Fatalf("retired ongoing status = %#v", task)
	}

	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"retired-task","status":"completed"}}`)
	runner.handleHostEvent(hostFrameEvent{frame: terminal})
	if got := childInput.String(); got != "" {
		t.Fatalf("retired terminal status reached replacement child: %s", got)
	}
	if len(runner.child.retiredTasks) != 0 || len(runner.child.retiredTokens) != 0 {
		t.Fatalf("retired terminal status retained state: tasks=%d tokens=%d", len(runner.child.retiredTasks), len(runner.child.retiredTokens))
	}
	duplicate := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"retired-task","status":"failed"}}`)
	runner.handleHostEvent(hostFrameEvent{frame: duplicate})
	if got := childInput.String(); got != "" {
		t.Fatalf("tombstoned retired status reached replacement child: %s", got)
	}
}
