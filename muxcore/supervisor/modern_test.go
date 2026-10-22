package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/era"
)

func TestModernSupervisor_NativeWireStaysWithItsHostWithoutInitialization(t *testing.T) {
	children := []*testChild{newTestChild(), newTestChild()}
	inputs := []lineStream{streamLines(children[0].inputReader), streamLines(children[1].inputReader)}
	harnesses := make([]runHarness, len(children))
	for i, child := range children {
		selected := child
		harnesses[i] = startHarness(t, Config{
			ProtocolEra: era.EraModern20260728,
			Resolve:     func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
			Start: func(context.Context, EngineRef) (StartResult, error) {
				return StartResult{Child: selected, Actual: EngineRef{ID: "engine"}}, nil
			},
		})
		t.Cleanup(func() { _ = harnesses[i].hostInput.Close() })
	}
	for i, harness := range harnesses {
		request := `{"params":{"name":"echo","arguments":{"host":` + string(rune('0'+i)) + `},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":"same"}},"id":7,"method":"tools/call","jsonrpc":"2.0"}`
		harness.send(t, request)
		if got := nextLine(t, inputs[i]); got != request {
			t.Fatalf("host %d native opening was changed or preceded by bootstrap: %s", i, got)
		}
		progress := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"same","progress":1,"message":"host-` + string(rune('0'+i)) + `"}}`
		writeModernChild(t, children[i], progress)
		if got := nextLine(t, harness.hostOutput); got != progress {
			t.Fatalf("host %d progress changed or reached another host: %s", i, got)
		}
		inputRequired := `{"jsonrpc":"2.0","id":7,"result":{"resultType":"input_required","inputRequests":{"confirm":{"method":"elicitation/create","params":{"requestedSchema":{"type":"object"}}}},"requestState":{"opaque":9007199254740993,"progressToken":"do-not-rewrite"}}}`
		writeModernChild(t, children[i], inputRequired)
		if got := nextLine(t, harness.hostOutput); got != inputRequired {
			t.Fatalf("host %d MRTR response changed: %s", i, got)
		}
		resume := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"echo","requestState":{"opaque":9007199254740993,"progressToken":"do-not-rewrite"},"inputResponses":{"confirm":{"action":"accept"}},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		harness.send(t, resume)
		if got := nextLine(t, inputs[i]); got != resume {
			t.Fatalf("host %d MRTR retry was replayed or changed: %s", i, got)
		}
		complete := `{"jsonrpc":"2.0","id":8,"result":{"resultType":"complete","host":` + string(rune('0'+i)) + `}}`
		writeModernChild(t, children[i], complete)
		if got := nextLine(t, harness.hostOutput); got != complete {
			t.Fatalf("host %d completion changed: %s", i, got)
		}
		listen := `{"jsonrpc":"2.0","id":"sub","method":"subscriptions/listen","params":{"notifications":{"resourceSubscriptions":["file:///project"]},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		harness.send(t, listen)
		if got := nextLine(t, inputs[i]); got != listen {
			t.Fatalf("host %d listen changed: %s", i, got)
		}
		for _, raw := range []string{
			`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"sub"},"notifications":{"resourceSubscriptions":["file:///project"]}}}`,
			`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"sub"},"uri":"file:///project"}}`,
			`{"jsonrpc":"2.0","id":"sub","result":{"resultType":"complete","_meta":{"io.modelcontextprotocol/subscriptionId":"sub"}}}`,
		} {
			writeModernChild(t, children[i], raw)
			if got := nextLine(t, harness.hostOutput); got != raw {
				t.Fatalf("host %d subscription frame was changed or misrouted: %s", i, got)
			}
		}
		listen = `{"jsonrpc":"2.0","id":"server-ended","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		harness.send(t, listen)
		if got := nextLine(t, inputs[i]); got != listen {
			t.Fatalf("host %d second listen changed: %s", i, got)
		}
		for _, raw := range []string{
			`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"server-ended"},"notifications":{"toolsListChanged":true}}}`,
			`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"server-ended","reason":"server ended subscription"}}`,
		} {
			writeModernChild(t, children[i], raw)
			if got := nextLine(t, harness.hostOutput); got != raw {
				t.Fatalf("host %d server subscription termination changed: %s", i, got)
			}
		}
		cancelledRequest := `{"jsonrpc":"2.0","id":"cancel","method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		harness.send(t, cancelledRequest)
		if got := nextLine(t, inputs[i]); got != cancelledRequest {
			t.Fatalf("host %d cancellable request changed: %s", i, got)
		}
		cancel := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel","reason":"host cancelled"}}`
		harness.send(t, cancel)
		if got := nextLine(t, inputs[i]); got != cancel {
			t.Fatalf("host %d cancellation changed: %s", i, got)
		}
		barrier := `{"jsonrpc":"2.0","id":"barrier","method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		harness.send(t, barrier)
		if got := nextLine(t, inputs[i]); got != barrier {
			t.Fatalf("host %d generated unexpected child traffic: %s", i, got)
		}
		writeModernChild(t, children[i], `{"jsonrpc":"2.0","id":"child-request","method":"sampling/createMessage","params":{"messages":[],"maxTokens":1}}`)
		response := `{"jsonrpc":"2.0","id":"barrier","result":{"resultType":"complete"}}`
		writeModernChild(t, children[i], response)
		if got := nextLine(t, harness.hostOutput); got != response {
			t.Fatalf("host %d received a child JSON-RPC request, synthetic frame, or foreign traffic: %s", i, got)
		}
	}
	for _, harness := range harnesses {
		harness.closeAndWait(t)
	}
}

func TestModernSupervisor_ReplacementFailsWrittenRequestsOnceWithoutReplay(t *testing.T) {
	first, second := newTestChild(), newTestChild()
	firstInput, secondInput := streamLines(first.inputReader), streamLines(second.inputReader)
	firstAdmission := newTestAdmission(true)
	starts := 0
	var startMu sync.Mutex
	harness := startHarness(t, Config{
		ProtocolEra: era.EraModern20260728,
		Resolve:     func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			startMu.Lock()
			defer startMu.Unlock()
			starts++
			if starts == 1 {
				return StartResult{Child: first, Actual: EngineRef{ID: "engine"}, Admission: firstAdmission}, nil
			}
			if starts != 2 {
				return StartResult{}, errors.New("unexpected extra modern generation")
			}
			select {
			case <-first.stopped:
			default:
				return StartResult{}, errors.New("replacement preceded child settlement")
			}
			select {
			case <-firstAdmission.closed:
			default:
				return StartResult{}, errors.New("replacement preceded admission closure")
			}
			return StartResult{Child: second, Actual: EngineRef{ID: "engine"}}, nil
		},
		ReplacementNotifications: []ListChangedKind{ListChangedTools, ListChangedPrompts, ListChangedResources},
		RetryDelay:               time.Millisecond,
	})
	t.Cleanup(func() { _ = harness.hostInput.Close() })
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1e0,"method":"tools/call","params":{"name":"written","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","id":"sub","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
	} {
		harness.send(t, raw)
		if got := nextLine(t, firstInput); got != raw {
			t.Fatalf("first generation received bootstrap or changed request: %s", got)
		}
	}
	ack := `{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"sub"},"notifications":{"toolsListChanged":true}}}`
	writeModernChild(t, first, ack)
	if got := nextLine(t, harness.hostOutput); got != ack {
		t.Fatalf("subscription ACK = %s", got)
	}
	first.crash(errors.New("modern child lost"))
	lost := map[string]int{}
	for range 2 {
		raw := nextLine(t, harness.hostOutput)
		var response struct {
			ID    json.RawMessage `json:"id"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &response); err != nil || response.Error == nil || response.Error.Code == 0 {
			t.Fatalf("written modern work did not fail explicitly on generation loss: %s", raw)
		}
		lost[string(response.ID)]++
	}
	if len(lost) != 2 || lost["1e0"] != 1 || lost[`"sub"`] != 1 {
		t.Fatalf("lost modern requests did not terminate once under original IDs: %v", lost)
	}
	fresh := `{"jsonrpc":"2.0","id":"fresh","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	harness.send(t, fresh)
	if got := nextLine(t, secondInput); got != fresh {
		t.Fatalf("replacement replayed written work/subscription or injected initialization: %s", got)
	}
	response := `{"jsonrpc":"2.0","id":"fresh","result":{"resultType":"complete","tools":[]}}`
	writeModernChild(t, second, response)
	if got := nextLine(t, harness.hostOutput); got != response {
		t.Fatalf("replacement fabricated bootstrap/list-change/duplicate terminal traffic: %s", got)
	}
	harness.closeAndWait(t)
}

func TestModernSupervisor_WriteOutcomePreservesOnlyDefinitelyUnwrittenFIFO(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "definitely-unwritten"
		if partial {
			name = "uncertain-partial"
		}
		t.Run(name, func(t *testing.T) {
			first, second := newFaultChild(), newTestChild()
			secondInput := streamLines(second.inputReader)
			written := 0
			if partial {
				written = 1
			}
			first.writer.arm(written, io.ErrUnexpectedEOF)
			replacementStarting, releaseReplacement := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseReplacement) }) }
			t.Cleanup(release)
			starts := 0
			harness := startHarness(t, Config{
				ProtocolEra: era.EraModern20260728,
				Resolve:     func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
				Start: func(ctx context.Context, _ EngineRef) (StartResult, error) {
					starts++
					if starts == 1 {
						return StartResult{Child: first, Actual: EngineRef{ID: "engine"}}, nil
					}
					if starts != 2 {
						return StartResult{}, errors.New("unexpected extra start")
					}
					close(replacementStarting)
					select {
					case <-releaseReplacement:
						return StartResult{Child: second, Actual: EngineRef{ID: "engine"}}, nil
					case <-ctx.Done():
						return StartResult{}, ctx.Err()
					}
				},
				RetryDelay: time.Millisecond,
			})
			t.Cleanup(func() { _ = harness.hostInput.Close() })
			opening := `{"jsonrpc":"2.0","id":"opening","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
			harness.send(t, opening)
			if partial {
				var lost struct {
					ID    string          `json:"id"`
					Error json.RawMessage `json:"error"`
				}
				if raw := nextLine(t, harness.hostOutput); json.Unmarshal([]byte(raw), &lost) != nil || lost.ID != "opening" || len(lost.Error) == 0 || string(lost.Error) == "null" {
					t.Fatalf("uncertain first request did not receive a loss error: %s", raw)
				}
			}
			select {
			case <-replacementStarting:
			case <-time.After(3 * time.Second):
				t.Fatal("write failure did not start replacement")
			}
			queued := `{"jsonrpc":"2.0","id":"queued","method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
			harness.send(t, queued)
			release()
			if !partial {
				if got := nextLine(t, secondInput); got != opening {
					t.Fatalf("unwritten first request lost FIFO or gained bootstrap: %s", got)
				}
				response := `{"jsonrpc":"2.0","id":"opening","result":{"resultType":"complete","tools":[]}}`
				writeModernChild(t, second, response)
				if got := nextLine(t, harness.hostOutput); got != response {
					t.Fatalf("unwritten request failed or changed: %s", got)
				}
			}
			if got := nextLine(t, secondInput); got != queued {
				t.Fatalf("queued FIFO was changed or uncertain work was replayed: %s", got)
			}
			response := `{"jsonrpc":"2.0","id":"queued","result":{"resultType":"complete"}}`
			writeModernChild(t, second, response)
			if got := nextLine(t, harness.hostOutput); got != response {
				t.Fatalf("replacement emitted duplicate/error/bootstrap traffic: %s", got)
			}
			harness.closeAndWait(t)
		})
	}
}

func TestModernSupervisor_LateOldGenerationOutputIsDropped(t *testing.T) {
	var host bytes.Buffer
	r := &runner{
		cfg:     Config{ProtocolEra: era.EraModern20260728, HostOut: &host},
		state:   StateActive,
		current: &generation{id: 2},
		host:    newDirectionRegistry(), child: newDirectionRegistry(),
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete"}}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"same","progress":1}}`,
		`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"sub"},"notifications":{"toolsListChanged":true}}}`,
		`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"sub"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"sub"}}`,
	} {
		frame, err := parseFrame([]byte(raw), 4096)
		if err != nil {
			t.Fatal(err)
		}
		r.handleEvent(childFrameEvent{generation: 1, frame: frame})
	}
	if host.Len() != 0 || r.current == nil || r.current.id != 2 || r.state != StateActive || r.terminal {
		t.Fatalf("late old-generation output affected the modern host/current generation: host=%q state=%s", host.String(), r.state)
	}
}

func TestModernSupervisor_VerifiedDormancyRejectsLiveWorkAndWakesNatively(t *testing.T) {
	first, second := newTestChild(), newTestChild()
	firstInput, secondInput := streamLines(first.inputReader), streamLines(second.inputReader)
	admission := newTestAdmission(true)
	events := make(chan Event, 32)
	starts := 0
	harness := startHarness(t, Config{
		ProtocolEra: era.EraModern20260728,
		Resolve:     func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			starts++
			if starts == 1 {
				return StartResult{Child: first, Actual: EngineRef{ID: "engine"}, Admission: admission}, nil
			}
			if starts != 2 {
				return StartResult{}, errors.New("unexpected extra wake")
			}
			select {
			case <-admission.closed:
			default:
				return StartResult{}, errors.New("wake preceded admission settlement")
			}
			return StartResult{Child: second, Actual: EngineRef{ID: "engine"}, Admission: newTestAdmission(true)}, nil
		},
		Observe: func(event Event) { events <- event },
	})
	t.Cleanup(func() { _ = harness.hostInput.Close() })
	listen := `{"jsonrpc":"2.0","id":"sub","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	harness.send(t, listen)
	if got := nextLine(t, firstInput); got != listen {
		t.Fatalf("native listen = %s", got)
	}
	ack := `{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"sub"},"notifications":{"toolsListChanged":true}}}`
	writeModernChild(t, first, ack)
	if got := nextLine(t, harness.hostOutput); got != ack {
		t.Fatalf("subscription ACK = %s", got)
	}
	writeModernChild(t, first, string(ProtocolV2().Frame(ControlDormantReady)))
	awaitModernSupervisorEvent(t, events, StateActive, ReasonDormantRejected)
	cancel := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"sub"}}`
	harness.send(t, cancel)
	if got := nextLine(t, firstInput); got != cancel {
		t.Fatalf("subscription did not block dormancy or cancellation changed: %s", got)
	}
	work := `{"jsonrpc":"2.0","id":"work","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	harness.send(t, work)
	if got := nextLine(t, firstInput); got != work {
		t.Fatalf("native work = %s", got)
	}
	writeModernChild(t, first, string(ProtocolV2().Frame(ControlDormantReady)))
	awaitModernSupervisorEvent(t, events, StateActive, ReasonDormantRejected)
	response := `{"jsonrpc":"2.0","id":"work","result":{"resultType":"complete","tools":[]}}`
	writeModernChild(t, first, response)
	if got := nextLine(t, harness.hostOutput); got != response {
		t.Fatalf("live work did not survive rejected dormancy: %s", got)
	}
	writeModernChild(t, first, string(ProtocolV2().Frame(ControlDormantReady)))
	if got := nextLine(t, firstInput); got != string(ProtocolV2().Frame(ControlCommitDormant)) {
		t.Fatalf("idle modern commit = %s", got)
	}
	writeModernChild(t, first, string(ProtocolV2().Frame(ControlDormantAck)))
	first.dormantExit()
	awaitModernSupervisorEvent(t, events, StateDormant, ReasonDormantCommit)
	wake := `{"jsonrpc":"2.0","id":"wake","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	harness.send(t, wake)
	if got := nextLine(t, secondInput); got != wake {
		t.Fatalf("modern wake injected initialize/replay instead of native demand: %s", got)
	}
	response = `{"jsonrpc":"2.0","id":"wake","result":{"resultType":"complete","tools":[]}}`
	writeModernChild(t, second, response)
	if got := nextLine(t, harness.hostOutput); got != response {
		t.Fatalf("modern wake response = %s", got)
	}
	harness.closeAndWait(t)
}

func writeModernChild(t *testing.T, child *testChild, raw string) {
	t.Helper()
	if err := child.write(raw); err != nil {
		t.Fatalf("write modern child frame: %v", err)
	}
}

func awaitModernSupervisorEvent(t *testing.T, events <-chan Event, state State, reason Reason) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if event.State == state && event.Reason == reason {
				return
			}
		case <-deadline:
			t.Fatalf("supervisor did not reach %s/%s", state, reason)
		}
	}
}
