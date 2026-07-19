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

func seedRetiredTaskOperation(t *testing.T, method string) *directionRegistry {
	t.Helper()
	registry := newDirectionRegistry()
	request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","method":"tools/call","params":{"_meta":{"progressToken":"task-token"},"task":{}}}`)
	record := registry.register(request, 1)
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","result":{"task":{"taskId":"task-1","status":"working"}}}`)
	if err := registry.canComplete(record, response); err != nil {
		t.Fatal(err)
	}
	registry.complete(record, response)
	operation := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"operation","method":%q,"params":{"taskId":"task-1"}}`, method))
	if err := registry.canRegister(operation); err != nil {
		t.Fatal(err)
	}
	registry.register(operation, 1)
	registry.retireGeneration(1)
	return &registry
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

func TestCorrelationActiveRequestsAndTasksShareBudget(t *testing.T) {
	registry := newDirectionRegistry()
	for index := range activeCorrelationLimit - 2 {
		request := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"create-%d","method":"tools/call","params":{"task":{}}}`, index))
		if err := registry.canRegister(request); err != nil {
			t.Fatalf("register task request %d: %v", index, err)
		}
		record := registry.register(request, 1)
		response := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"create-%d","result":{"task":{"taskId":"task-%d","status":"working"}}}`, index, index))
		if err := registry.canComplete(record, response); err != nil {
			t.Fatalf("complete task request %d: %v", index, err)
		}
		registry.complete(record, response)
	}

	last := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"last","method":"tools/call","params":{"task":{}}}`)
	if err := registry.canRegister(last); err != nil {
		t.Fatalf("register final executing correlation: %v", err)
	}
	lastRecord := registry.register(last, 1)
	overflow := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"overflow","method":"tools/list"}`)
	if err := registry.canRegister(overflow); err == nil {
		t.Fatal("executing tasks consumed the reserved control slot")
	}
	lastResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"last","result":{"task":{"taskId":"task-last","status":"working"}}}`)
	if err := registry.canComplete(lastRecord, lastResponse); err != nil {
		t.Fatalf("request-to-task conversion consumed a second active slot: %v", err)
	}
	registry.complete(lastRecord, lastResponse)
	control := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"control","method":"tasks/get","params":{"taskId":"task-last"}}`)
	if err := registry.canRegister(control); err != nil {
		t.Fatalf("reserved control slot unavailable: %v", err)
	}
	registry.register(control, 1)
	if got := len(registry.requests) + len(registry.tasks); got != activeCorrelationLimit {
		t.Fatalf("combined active correlations = %d, want %d", got, activeCorrelationLimit)
	}
	secondControl := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"second-control","method":"tasks/get","params":{"taskId":"task-last"}}`)
	if err := registry.canRegister(secondControl); err == nil {
		t.Fatal("shared active ceiling accepted a 257th correlation")
	}
}

func TestSuccessorAcceptsWorkWithRetiredRequestTolerance(t *testing.T) {
	registry := newDirectionRegistry()
	for index := range activeCorrelationLimit {
		request := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"retired-%d","method":"tools/list"}`, index))
		if err := registry.canRegister(request); err != nil {
			t.Fatal(err)
		}
		registry.register(request, 1)
	}
	registry.retireGeneration(1)
	successor := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"successor","method":"tools/list"}`)
	if err := registry.canRegister(successor); err != nil {
		t.Fatalf("successor request rejected with %d retired requests: %v", activeCorrelationLimit, err)
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

func TestTerminalTaskFencesSurviveCrashReplay(t *testing.T) {
	childInput := new(bufferWriteCloser)
	runner := &runner{
		state:             StateReplaying,
		current:           &generation{id: 2, stdin: childInput},
		child:             newDirectionRegistry(),
		host:              newDirectionRegistry(),
		initializeRequest: []byte(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`),
		initialized:       []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
	}
	request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","method":"tools/call","params":{"_meta":{"progressToken":"task-token"},"task":{}}}`)
	record := runner.child.register(request, 1)
	response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","result":{"task":{"taskId":"task-1","status":"working"}}}`)
	if err := runner.child.canComplete(record, response); err != nil {
		t.Fatal(err)
	}
	runner.child.complete(record, response)
	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"task-1","status":"completed"}}`)
	handled, err := runner.child.updateTask(terminal.taskStatus, 1)
	if err != nil || !handled {
		t.Fatalf("terminal task status: handled=%v err=%v", handled, err)
	}
	runner.child.retireGeneration(1)
	replay := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"replacement","version":"1"}}}`)
	runner.completeReplay(replay)

	if err := runner.child.canRegister(request); err == nil {
		t.Fatal("terminal task request ID fence was cleared by replay")
	}
	tokenReuse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"token-reuse","method":"tools/list","params":{"_meta":{"progressToken":"task-token"}}}`)
	if err := runner.child.canRegister(tokenReuse); err == nil {
		t.Fatal("terminal task progress token fence was cleared by replay")
	}
	replacement := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","method":"tools/call","params":{"task":{}}}`)
	replacementRecord := runner.child.register(replacement, 2)
	reused := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"replacement","result":{"task":{"taskId":"task-1","status":"working"}}}`)
	if err := runner.child.canComplete(replacementRecord, reused); err == nil {
		t.Fatal("terminal task ID fence was cleared by replay")
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

func TestTaskOperationResponsesRetainOrFinalizeWithoutStatusNotification(t *testing.T) {
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
		name           string
		method         string
		response       string
		terminalStatus string
	}{
		{name: "terminal get retained", method: "tasks/get", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`, terminalStatus: "completed"},
		{name: "cancel retained", method: "tasks/cancel", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"cancelled","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`, terminalStatus: "cancelled"},
		{name: "result finalized", method: "tasks/result", response: `{"jsonrpc":"2.0","id":"operation","result":{"content":[]}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := seed(t)
			operation := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"operation","method":%q,"params":{"taskId":"task-1"}}`, testCase.method))
			record := registry.register(operation, 1)
			response := mustParseCorrelationFrame(t, testCase.response)
			if err := registry.canComplete(record, response); err != nil {
				t.Fatal(err)
			}
			if err := registry.complete(record, response); err != nil {
				t.Fatal(err)
			}
			if len(registry.tasks) != 0 || len(registry.tokens) != 0 {
				t.Fatalf("terminal operation retained active state: tasks=%d tokens=%d", len(registry.tasks), len(registry.tokens))
			}
			terminal := registry.terminalTasks["task-1"]
			if testCase.terminalStatus != "" {
				if terminal == nil || terminal.status != testCase.terminalStatus {
					t.Fatalf("terminal operation did not retain terminal task: %#v", terminal)
				}
				if registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("task-1")) {
					t.Fatal("routable terminal task was prematurely fenced")
				}
			} else if terminal != nil || !registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("task-1")) {
				t.Fatalf("tasks/result did not finalize task: terminal=%#v", terminal)
			}
		})
	}
}

func TestTaskLifecycleReservesControlSlot(t *testing.T) {
	t.Run("executing tasks", func(t *testing.T) {
		registry := newDirectionRegistry()
		for index := range activeCorrelationLimit - 1 {
			id := fmt.Sprintf("task-%d", index)
			registry.tasks[id] = &taskRecord{id: id, generation: 1, status: "working"}
		}
		ordinary := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"ordinary","method":"tools/list"}`)
		if err := registry.canRegister(ordinary); err == nil {
			t.Fatal("executing tasks consumed the reserved control slot")
		}
		control := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"control","method":"tasks/get","params":{"taskId":"task-0"}}`)
		if err := registry.canRegister(control); err != nil {
			t.Fatalf("reserved task control slot unavailable: %v", err)
		}
	})

	t.Run("pending create", func(t *testing.T) {
		registry := newDirectionRegistry()
		for index := range activeCorrelationLimit - 2 {
			request := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"ordinary-%d","method":"tools/list"}`, index))
			registry.register(request, 1)
		}
		create := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","method":"tools/call","params":{"task":{}}}`)
		if err := registry.canRegister(create); err != nil {
			t.Fatalf("pending create at reserved ceiling: %v", err)
		}
		registry.register(create, 1)
		ordinary := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"ordinary-overflow","method":"tools/list"}`)
		if err := registry.canRegister(ordinary); err == nil {
			t.Fatal("pending task create did not preserve control slot")
		}
	})

	t.Run("terminal task", func(t *testing.T) {
		registry := newDirectionRegistry()
		registry.terminalTasks["task-1"] = &taskRecord{id: "task-1", generation: 1, status: "completed"}
		for index := range activeCorrelationLimit - 1 {
			request := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"ordinary-%d","method":"tools/list"}`, index))
			registry.register(request, 1)
		}
		ordinary := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"ordinary-overflow","method":"tools/list"}`)
		if err := registry.canRegister(ordinary); err == nil {
			t.Fatal("terminal task did not preserve control slot")
		}
		control := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"control","method":"tasks/result","params":{"taskId":"task-1"}}`)
		if err := registry.canRegister(control); err != nil {
			t.Fatalf("terminal task control slot unavailable: %v", err)
		}
	})
}

func TestTerminalTaskLifecycleAndResultFinalization(t *testing.T) {
	registry := newDirectionRegistry()
	tokenKey := scalarKey{kind: scalarString, value: "task-token"}
	registry.tokens[tokenKey] = &tokenRecord{generation: 1}
	registry.tasks["task-1"] = &taskRecord{
		id:            "task-1",
		requestID:     taskTombstoneKey("create"),
		generation:    1,
		progressToken: &tokenKey,
		status:        "working",
	}
	handled, err := registry.updateTask(&taskMeta{id: "task-1", status: "completed"}, 1)
	if err != nil || !handled {
		t.Fatalf("terminal update: handled=%v err=%v", handled, err)
	}
	if registry.tasks["task-1"] != nil || registry.terminalTasks["task-1"] == nil {
		t.Fatalf("terminal transition active=%#v terminal=%#v", registry.tasks["task-1"], registry.terminalTasks["task-1"])
	}
	if registry.tokens[tokenKey] != nil || !registry.persistentFences.contains(tombstoneToken, tokenKey) {
		t.Fatal("terminal transition did not remove and fence progress token")
	}
	if registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("task-1")) {
		t.Fatal("routable terminal task was prematurely fenced")
	}
	if registryIdle(&registry) {
		t.Fatal("routable terminal task allowed registry idle")
	}

	get := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","method":"tasks/get","params":{"taskId":"task-1"}}`)
	getRecord := registry.register(get, 1)
	getResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","result":{"taskId":"task-1","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
	if err := registry.canComplete(getRecord, getResponse); err != nil {
		t.Fatalf("terminal tasks/get response: %v", err)
	}
	if err := registry.complete(getRecord, getResponse); err != nil {
		t.Fatal(err)
	}
	if task := registry.terminalTasks["task-1"]; task == nil || task.status != "completed" {
		t.Fatalf("terminal tasks/get lost metadata: %#v", task)
	}

	resultError := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"result-error","method":"tasks/result","params":{"taskId":"task-1"}}`)
	resultErrorRecord := registry.register(resultError, 1)
	errorResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"result-error","error":{"code":-32000,"message":"retry"}}`)
	if err := registry.canComplete(resultErrorRecord, errorResponse); err != nil {
		t.Fatal(err)
	}
	if err := registry.complete(resultErrorRecord, errorResponse); err != nil {
		t.Fatal(err)
	}
	if registry.terminalTasks["task-1"] == nil {
		t.Fatal("tasks/result error made terminal task non-retriable")
	}

	result := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"result","method":"tasks/result","params":{"taskId":"task-1"}}`)
	resultRecord := registry.register(result, 1)
	resultResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"result","result":{"content":[]}}`)
	if err := registry.canComplete(resultRecord, resultResponse); err != nil {
		t.Fatal(err)
	}
	if err := registry.complete(resultRecord, resultResponse); err != nil {
		t.Fatal(err)
	}
	if registry.terminalTasks["task-1"] != nil || !registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("task-1")) {
		t.Fatal("successful tasks/result did not finalize terminal task")
	}
	if !registryIdle(&registry) {
		t.Fatal("finalized task kept registry non-idle")
	}
}

func TestTerminalTaskRetentionAndGenerationInvalidation(t *testing.T) {
	t.Run("terminal cap", func(t *testing.T) {
		registry := newDirectionRegistry()
		for index := range retainedCorrelationLimit - activeCorrelationLimit {
			id := fmt.Sprintf("terminal-%d", index)
			registry.terminalTasks[id] = &taskRecord{id: id, generation: 1, status: "completed"}
		}
		registry.tasks["active"] = &taskRecord{id: "active", generation: 1, status: "working"}
		handled, err := registry.updateTask(&taskMeta{id: "active", status: "completed"}, 1)
		if err == nil || !handled {
			t.Fatalf("terminal cap: handled=%v err=%v", handled, err)
		}
		if registry.tasks["active"] == nil || registry.terminalTasks["active"] != nil {
			t.Fatal("terminal cap failure mutated task state")
		}
	})

	t.Run("generation fence preflight", func(t *testing.T) {
		registry := newDirectionRegistry()
		for index := range persistentFenceLimit {
			if err := registry.persistentFences.add(tombstoneTask, taskTombstoneKey(fmt.Sprintf("fenced-%d", index))); err != nil {
				t.Fatal(err)
			}
		}
		registry.terminalTasks["task-1"] = &taskRecord{id: "task-1", generation: 1, status: "completed"}
		if _, err := registry.retireGenerationChecked(1); err == nil {
			t.Fatal("generation invalidation hid terminal task fence exhaustion")
		}
		if registry.terminalTasks["task-1"] == nil {
			t.Fatal("failed generation preflight removed terminal task")
		}
	})

	t.Run("combined task retention cap", func(t *testing.T) {
		registry := newDirectionRegistry()
		for index := range terminalTaskLimit {
			id := fmt.Sprintf("terminal-%d", index)
			registry.terminalTasks[id] = &taskRecord{id: id, generation: 1, status: "completed"}
		}
		for index := range activeCorrelationLimit {
			id := fmt.Sprintf("retired-%d", index)
			registry.retiredTasks[id] = &taskRecord{id: id, generation: 1, status: "working"}
		}
		request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"create","method":"tools/call","params":{"task":{}}}`)
		if err := registry.canRegister(request); err == nil {
			t.Fatal("combined terminal and retired task cap accepted task-augmented request")
		}
		if err := registry.canBindTask("new-task"); err == nil {
			t.Fatal("combined terminal and retired task cap accepted task binding")
		}
		registry.tasks["overflow"] = &taskRecord{id: "overflow", generation: 2, status: "working"}
		if _, err := registry.retireGenerationChecked(2); err == nil {
			t.Fatal("generation retirement ignored combined task retention overflow")
		}
	})

	t.Run("generation invalidation", func(t *testing.T) {
		registry := newDirectionRegistry()
		registry.terminalTasks["task-1"] = &taskRecord{id: "task-1", generation: 1, status: "completed"}
		operation := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"operation","method":"tasks/get","params":{"taskId":"task-1"}}`)
		registry.register(operation, 1)
		if _, err := registry.retireGenerationChecked(1); err != nil {
			t.Fatal(err)
		}
		if registry.terminalTasks["task-1"] != nil || !registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("task-1")) {
			t.Fatal("generation invalidation did not fence and remove terminal task")
		}
		response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
		if handled, err := registry.consumeRetiredResponse(response); err != nil || !handled {
			t.Fatalf("late response after terminal invalidation: handled=%v err=%v", handled, err)
		}
	})
}

func TestActiveTaskOperationResponseAfterTerminalStatus(t *testing.T) {
	registry := newDirectionRegistry()
	registry.tasks["task-1"] = &taskRecord{id: "task-1", generation: 1, status: "working"}
	operation := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","method":"tasks/get","params":{"taskId":"task-1"}}`)
	record := registry.register(operation, 1)
	if handled, err := registry.updateTask(&taskMeta{id: "task-1", status: "completed"}, 1); err != nil || !handled {
		t.Fatalf("terminal update: handled=%v err=%v", handled, err)
	}
	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","result":{"taskId":"task-1","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
	if err := registry.canComplete(record, terminal); err != nil {
		t.Fatalf("terminal response after terminal status: %v", err)
	}
	working := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","result":{"taskId":"task-1","status":"working","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
	if err := registry.canComplete(record, working); err == nil {
		t.Fatal("nonterminal response after terminal status was accepted")
	}
}

func TestTerminalTaskStatusIsImmutable(t *testing.T) {
	registry := newDirectionRegistry()
	registry.terminalTasks["task-1"] = &taskRecord{id: "task-1", generation: 1, status: "completed"}
	if handled, err := registry.updateTask(&taskMeta{id: "task-1", status: "cancelled"}, 1); err == nil || !handled {
		t.Fatalf("terminal status mutation: handled=%v err=%v", handled, err)
	}
	if status := registry.terminalTasks["task-1"].status; status != "completed" {
		t.Fatalf("terminal status mutated to %q", status)
	}

	get := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","method":"tasks/get","params":{"taskId":"task-1"}}`)
	getRecord := registry.register(get, 1)
	mismatch := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"get","result":{"taskId":"task-1","status":"cancelled","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
	if err := registry.canComplete(getRecord, mismatch); err == nil {
		t.Fatal("terminal operation response changed immutable status")
	}

	result := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"result","method":"tasks/result","params":{"taskId":"task-1"}}`)
	resultRecord := registry.register(result, 1)
	resultResponse := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"result","result":{"content":[]}}`)
	if err := registry.canComplete(resultRecord, resultResponse); err != nil {
		t.Fatalf("tasks/result synthetic terminal status was compared: %v", err)
	}

	full := newDirectionRegistry()
	for index := range terminalTaskLimit {
		id := fmt.Sprintf("terminal-%d", index)
		full.terminalTasks[id] = &taskRecord{id: id, generation: 1, status: "completed"}
	}
	full.tasks["active"] = &taskRecord{id: "active", generation: 1, status: "working"}
	operation := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"active-get","method":"tasks/get","params":{"taskId":"active"}}`)
	record := full.register(operation, 1)
	terminal := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"active-get","result":{"taskId":"active","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
	if err := full.canComplete(record, terminal); err == nil {
		t.Fatal("active terminal response ignored terminal retention preflight")
	}
	if full.tasks["active"] == nil || full.terminalTasks["active"] != nil {
		t.Fatal("terminal retention preflight mutated task state")
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

func TestRetiredTaskOperationLateResponses(t *testing.T) {
	t.Run("nonterminal get updates retired task", func(t *testing.T) {
		registry := seedRetiredTaskOperation(t, "tasks/get")
		response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"input_required","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
		handled, err := registry.consumeRetiredResponse(response)
		if err != nil || !handled {
			t.Fatalf("consume nonterminal get: handled=%v err=%v", handled, err)
		}
		if task := registry.retiredTasks["task-1"]; task == nil || task.status != "input_required" {
			t.Fatalf("retired task after get = %#v", task)
		}
		if registry.retiredRequests[response.id.key] != nil {
			t.Fatal("nonterminal get retained operation request")
		}
		if !registry.persistentFences.contains(tombstoneRequest, response.id.key) {
			t.Fatal("nonterminal get did not persistently fence operation request")
		}
	})

	for _, method := range []string{"tasks/get", "tasks/cancel", "tasks/result"} {
		t.Run(method+" error keeps retired task live", func(t *testing.T) {
			registry := seedRetiredTaskOperation(t, method)
			response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"operation","error":{"code":-32000,"message":"not ready"}}`)
			handled, err := registry.consumeRetiredResponse(response)
			if err != nil || !handled {
				t.Fatalf("consume operation error: handled=%v err=%v", handled, err)
			}
			if registry.retiredTasks["task-1"] == nil {
				t.Fatal("operation error released retired task")
			}
			if _, exists := registry.retiredTokens[scalarKey{kind: scalarString, value: "task-token"}]; !exists {
				t.Fatal("operation error released retired task token")
			}
			if registry.retiredRequests[response.id.key] != nil || !registry.persistentFences.contains(tombstoneRequest, response.id.key) {
				t.Fatal("operation error did not release and persistently fence request")
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
		t.Run(testCase.name+" retires without status", func(t *testing.T) {
			registry := seedRetiredTaskOperation(t, testCase.method)
			response := mustParseCorrelationFrame(t, testCase.response)
			handled, err := registry.consumeRetiredResponse(response)
			if err != nil || !handled {
				t.Fatalf("consume terminal operation: handled=%v err=%v", handled, err)
			}
			tokenKey := scalarKey{kind: scalarString, value: "task-token"}
			if len(registry.retiredTasks) != 0 || len(registry.retiredTokens) != 0 {
				t.Fatalf("terminal operation retained task state: tasks=%d tokens=%d", len(registry.retiredTasks), len(registry.retiredTokens))
			}
			if !registry.persistentFences.contains(tombstoneRequest, response.id.key) ||
				!registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("task-1")) ||
				!registry.persistentFences.contains(tombstoneToken, tokenKey) {
				t.Fatal("terminal operation did not persistently fence request/task/token")
			}
		})
	}

	for _, testCase := range []struct {
		name     string
		method   string
		response string
	}{
		{name: "get mismatch", method: "tasks/get", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"other","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`},
		{name: "cancel nonterminal", method: "tasks/cancel", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"working","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`},
		{name: "malformed get", method: "tasks/get", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"unknown"}}`},
	} {
		t.Run(testCase.name+" retains all state", func(t *testing.T) {
			registry := seedRetiredTaskOperation(t, testCase.method)
			response := mustParseCorrelationFrame(t, testCase.response)
			handled, err := registry.consumeRetiredResponse(response)
			if err == nil || !handled {
				t.Fatalf("invalid retired operation: handled=%v err=%v", handled, err)
			}
			if registry.retiredRequests[response.id.key] == nil || registry.retiredTasks["task-1"] == nil {
				t.Fatal("invalid retired operation released request or task")
			}
			if _, exists := registry.retiredTokens[scalarKey{kind: scalarString, value: "task-token"}]; !exists {
				t.Fatal("invalid retired operation released task token")
			}
		})
	}
}

func TestRetiredTaskOperationResponseAfterTerminalStatus(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		method         string
		terminalStatus string
		response       string
	}{
		{name: "terminal get", method: "tasks/get", terminalStatus: "completed", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`},
		{name: "cancel", method: "tasks/cancel", terminalStatus: "cancelled", response: `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"cancelled","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`},
		{name: "result", method: "tasks/result", terminalStatus: "completed", response: `{"jsonrpc":"2.0","id":"operation","result":{"content":[]}}`},
		{name: "error", method: "tasks/get", terminalStatus: "completed", response: `{"jsonrpc":"2.0","id":"operation","error":{"code":-32000,"message":"already complete"}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := seedRetiredTaskOperation(t, testCase.method)
			terminal := &taskMeta{id: "task-1", status: testCase.terminalStatus}
			if handled, err := registry.consumeRetiredTaskStatus(terminal); err != nil || !handled {
				t.Fatalf("consume terminal status: handled=%v err=%v", handled, err)
			}
			response := mustParseCorrelationFrame(t, testCase.response)
			handled, err := registry.consumeRetiredResponse(response)
			if err != nil || !handled {
				t.Fatalf("consume response after terminal status: handled=%v err=%v", handled, err)
			}
			tokenKey := scalarKey{kind: scalarString, value: "task-token"}
			if registry.retiredRequests[response.id.key] != nil {
				t.Fatal("response after terminal status retained operation request")
			}
			if !registry.persistentFences.contains(tombstoneRequest, response.id.key) ||
				!registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("task-1")) ||
				!registry.persistentFences.contains(tombstoneToken, tokenKey) {
				t.Fatal("response after terminal status lost request/task/token fences")
			}
		})
	}

	t.Run("nonterminal result remains fatal", func(t *testing.T) {
		registry := seedRetiredTaskOperation(t, "tasks/get")
		if handled, err := registry.consumeRetiredTaskStatus(&taskMeta{id: "task-1", status: "completed"}); err != nil || !handled {
			t.Fatalf("consume terminal status: handled=%v err=%v", handled, err)
		}
		response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"operation","result":{"taskId":"task-1","status":"working","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttl":60000}}`)
		handled, err := registry.consumeRetiredResponse(response)
		if err == nil || !handled {
			t.Fatalf("nonterminal response after terminal status: handled=%v err=%v", handled, err)
		}
		if registry.retiredRequests[response.id.key] == nil {
			t.Fatal("nonterminal response after terminal status released operation request")
		}
	})
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
	if !registry.persistentFences.contains(tombstoneRequest, request.id.key) || !registry.persistentFences.contains(tombstoneToken, request.progressToken.key) {
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
			_ = registry.persistentFences.add(tombstoneTask, taskTombstoneKey("collision"))
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
	if !registry.persistentFences.contains(tombstoneTask, taskTombstoneKey("retired-task")) || !registry.persistentFences.contains(tombstoneToken, request.progressToken.key) {
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

func TestCorrelationRetentionCeilingAllowsBulkRetirementWithoutOvershoot(t *testing.T) {
	registry := newDirectionRegistry()
	retentionLimit := tombstoneLimit + activeCorrelationLimit
	for wave := range retentionLimit / activeCorrelationLimit {
		generation := uint64(wave + 1)
		for index := range activeCorrelationLimit {
			id := wave*activeCorrelationLimit + index
			request := mustParseCorrelationFrame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":"request-%d","method":"roots/list","params":{"_meta":{"progressToken":"token-%d"}}}`, id, id))
			if err := registry.canRegister(request); err != nil {
				t.Fatalf("register retained correlation %d: %v", id, err)
			}
			registry.register(request, generation)
		}
		registry.retireGeneration(generation)
	}
	if got := len(registry.retiredRequests); got != retentionLimit {
		t.Fatalf("retired requests = %d, want %d", got, retentionLimit)
	}
	if got := len(registry.retiredTokens); got != retentionLimit {
		t.Fatalf("retired tokens = %d, want %d", got, retentionLimit)
	}
	overflow := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"overflow","method":"roots/list"}`)
	if err := registry.canRegister(overflow); err == nil {
		t.Fatal("active+retired retention ceiling accepted request overflow")
	}
	if len(registry.retiredRequests) != retentionLimit || len(registry.retiredTokens) != retentionLimit {
		t.Fatal("retention overflow mutated retired state")
	}

	taskRegistry := newDirectionRegistry()
	for index := range retentionLimit {
		taskRegistry.retiredTasks[fmt.Sprintf("task-%d", index)] = &taskRecord{}
	}
	if err := taskRegistry.canBindTask("task-overflow"); err == nil {
		t.Fatal("active+retired task retention ceiling accepted overflow")
	}
}

func TestPersistentTaskFencesArePerRoleNonEvicting(t *testing.T) {
	for _, role := range []tombstoneRole{tombstoneRequest, tombstoneToken, tombstoneTask} {
		t.Run(fmt.Sprintf("role-%d", role), func(t *testing.T) {
			registry := newDirectionRegistry()
			for index := range persistentFenceLimit {
				if err := registry.persistentFences.add(role, taskTombstoneKey(fmt.Sprintf("value-%d", index))); err != nil {
					t.Fatalf("add persistent fence %d: %v", index, err)
				}
			}
			overflow := taskTombstoneKey("overflow")
			if err := registry.persistentFences.add(role, overflow); err == nil {
				t.Fatal("persistent fence accepted the 1025th role entry")
			}
			if !registry.persistentFences.contains(role, taskTombstoneKey("value-0")) {
				t.Fatal("persistent fence evicted the oldest entry")
			}
			if registry.persistentFences.contains(role, overflow) {
				t.Fatal("rejected persistent fence entry was retained")
			}
			registry.clearTransitionTombstones()
			if !registry.persistentFences.contains(role, taskTombstoneKey("value-0")) {
				t.Fatal("transition clear removed persistent fence")
			}
			registry.clearRetiredCorrelations()
			if registry.persistentFences.contains(role, taskTombstoneKey("value-0")) {
				t.Fatal("retired correlation clear retained persistent fence")
			}
		})
	}
}

func TestPersistentFenceCapacityErrorsPropagate(t *testing.T) {
	fill := func(t *testing.T, registry *directionRegistry, role tombstoneRole) {
		t.Helper()
		for index := range persistentFenceLimit {
			if err := registry.persistentFences.add(role, taskTombstoneKey(fmt.Sprintf("full-%d", index))); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("complete", func(t *testing.T) {
		registry := newDirectionRegistry()
		fill(t, &registry, tombstoneRequest)
		request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"complete","method":"tools/call","params":{"task":{}}}`)
		record := registry.register(request, 1)
		response := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"complete","error":{"code":-32000,"message":"failed"}}`)
		if err := registry.complete(record, response); err == nil {
			t.Fatal("complete hid persistent request fence exhaustion")
		}
		if registry.requests[record.id.key] == nil {
			t.Fatal("failed complete released an unfenced request")
		}
	})

	t.Run("cancel", func(t *testing.T) {
		registry := newDirectionRegistry()
		fill(t, &registry, tombstoneRequest)
		request := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","id":"cancel","method":"tasks/get","params":{"taskId":"task-1"}}`)
		record := registry.register(request, 1)
		if err := registry.cancel(record); err == nil {
			t.Fatal("cancel hid persistent request fence exhaustion")
		}
		if registry.requests[record.id.key] == nil {
			t.Fatal("failed cancel released an unfenced request")
		}
	})

	t.Run("terminal task update", func(t *testing.T) {
		registry := newDirectionRegistry()
		fill(t, &registry, tombstoneToken)
		tokenKey := taskTombstoneKey("task-token")
		registry.tasks["task-1"] = &taskRecord{id: "task-1", generation: 1, progressToken: &tokenKey, status: "working"}
		registry.tokens[tokenKey] = &tokenRecord{generation: 1}
		handled, err := registry.updateTask(&taskMeta{id: "task-1", status: "completed"}, 1)
		if err == nil || !handled {
			t.Fatalf("terminal update: handled=%v err=%v", handled, err)
		}
		if registry.tasks["task-1"] == nil {
			t.Fatal("failed terminal update released an unfenced task")
		}
	})

	t.Run("generation retirement", func(t *testing.T) {
		registry := newDirectionRegistry()
		fill(t, &registry, tombstoneRequest)
		registry.tasks["task-1"] = &taskRecord{
			id:         "task-1",
			requestID:  taskTombstoneKey("task-request"),
			generation: 1,
			status:     "working",
		}
		if _, err := registry.retireGenerationChecked(1); err == nil {
			t.Fatal("generation retirement hid persistent request fence exhaustion")
		}
		if registry.tasks["task-1"] == nil || registry.retiredTasks["task-1"] != nil {
			t.Fatal("failed generation retirement partially moved task state")
		}
	})
}

func TestRetiredTaskStatusCollisionTerminatesRunner(t *testing.T) {
	childInput := new(bufferWriteCloser)
	runner := &runner{
		cancel:            func() {},
		state:             StateActive,
		current:           &generation{id: 2, stdin: childInput},
		child:             newDirectionRegistry(),
		host:              newDirectionRegistry(),
		initializeRequest: []byte(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`),
		initialized:       []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
	}
	runner.child.retiredTasks["collision"] = &taskRecord{id: "collision", generation: 1, status: "working"}
	runner.child.tasks["collision"] = &taskRecord{id: "collision", generation: 2, status: "working"}
	status := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"collision","status":"completed"}}`)
	runner.handleHostEvent(hostFrameEvent{frame: status})
	if !runner.terminal || runner.terminalReason != ReasonProtocolFailure {
		t.Fatalf("status collision terminal=%v reason=%s", runner.terminal, runner.terminalReason)
	}
	later := mustParseCorrelationFrame(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"collision","status":"failed"}}`)
	runner.handleHostEvent(hostFrameEvent{frame: later})
	if got := childInput.String(); got != "" {
		t.Fatalf("traffic reached child after terminal status collision: %s", got)
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
