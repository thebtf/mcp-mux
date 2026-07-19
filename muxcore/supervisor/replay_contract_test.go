package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunRejectsReplayProtocolVersionChange(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	third := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	thirdInput := streamLines(third.inputReader)
	children := []*testChild{first, second, third}
	starts := make(chan int, len(children))
	var startMu sync.Mutex
	var observeMu sync.Mutex
	var observed []Event
	startCount := 0

	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) {
			return EngineRef{ID: "engine"}, nil
		},
		Start: func(context.Context, EngineRef) (StartResult, error) {
			startMu.Lock()
			defer startMu.Unlock()
			if startCount >= len(children) {
				return StartResult{}, errors.New("unexpected extra start")
			}
			child := children[startCount]
			startCount++
			starts <- startCount
			return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
		},
		RetryDelay: 10 * time.Millisecond,
		Observe: func(event Event) {
			observeMu.Lock()
			defer observeMu.Unlock()
			observed = append(observed, event)
		},
	})

	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if got := nextLine(t, firstInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("first initialize = %s", got)
	}
	if err := first.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"first","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, firstInput)
	first.crash(errors.New("boom"))

	awaitStart := func(want int) {
		t.Helper()
		for {
			select {
			case got := <-starts:
				if got == want {
					return
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("start %d did not occur", want)
			}
		}
	}
	awaitStart(2)
	if got := nextLine(t, secondInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("second replay initialize = %s", got)
	}
	if err := second.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"second","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	awaitStart(3)
	observeMu.Lock()
	protocolFailure := false
	for _, event := range observed {
		if event.State == StateFinalizing && event.Reason == ReasonProtocolFailure {
			protocolFailure = true
			break
		}
	}
	observeMu.Unlock()
	if !protocolFailure {
		t.Fatal("replay protocol mismatch did not finalize with ReasonProtocolFailure")
	}
	select {
	case <-second.stopped:
	case <-time.After(time.Second):
		t.Fatal("mismatched replay generation was not retired")
	}
	if got := nextLine(t, thirdInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("third replay initialize = %s", got)
	}
	if err := third.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"third","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, thirdInput); !strings.Contains(got, "notifications/initialized") {
		t.Fatalf("third initialized notification = %s", got)
	}

	harness.send(t, `{"jsonrpc":"2.0","id":"tools","method":"tools/list"}`)
	if got := nextLine(t, thirdInput); !strings.Contains(got, `"id":"tools"`) {
		t.Fatalf("post-replay request = %s", got)
	}
	if err := third.write(`{"jsonrpc":"2.0","id":"tools","result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"tools"`) {
		t.Fatalf("post-replay response = %s", got)
	}
	harness.closeAndWait(t)
}

func TestSameInitializeContractRequiresEquivalentProtocolVersionAndCapabilities(t *testing.T) {
	original := []byte(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"first","version":"1"}}}`)
	matching, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"next","version":"2"}}}`), 4096)
	if err != nil || !sameInitializeContract(original, matching) {
		t.Fatalf("matching replay = %v, %v", sameInitializeContract(original, matching), err)
	}
	mismatch, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"next","version":"2"}}}`), 4096)
	if err != nil || sameInitializeContract(original, mismatch) {
		t.Fatalf("mismatched replay = %v, %v", sameInitializeContract(original, mismatch), err)
	}

	nestedOriginal := []byte(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true,"nested":{"enabled":true,"limit":9007199254740993}}},"serverInfo":{"name":"first","version":"1"}}}`)
	for _, testCase := range []struct {
		name        string
		replacement string
		want        bool
	}{
		{
			name:        "server info drift and object key order are allowed",
			replacement: `{"jsonrpc":"2.0","id":"init","result":{"serverInfo":{"version":"2","name":"next"},"capabilities":{"tools":{"nested":{"limit":9.007199254740993e15,"enabled":true},"listChanged":true}},"protocolVersion":"2025-11-25"}}`,
			want:        true,
		},
		{
			name:        "nested capability shrink is rejected",
			replacement: `{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true,"nested":{"enabled":true}}},"serverInfo":{"name":"next","version":"2"}}}`,
		},
		{
			name:        "nested capability expansion is rejected",
			replacement: `{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true,"nested":{"enabled":true,"limit":9007199254740993,"extra":false}}},"serverInfo":{"name":"next","version":"2"}}}`,
		},
		{
			name:        "nested capability value drift is rejected",
			replacement: `{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true,"nested":{"enabled":true,"limit":9007199254740992}}},"serverInfo":{"name":"next","version":"2"}}}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			replacement, err := parseFrame([]byte(testCase.replacement), 4096)
			if err != nil {
				t.Fatal(err)
			}
			if got := sameInitializeContract(nestedOriginal, replacement); got != testCase.want {
				t.Fatalf("same replay initialize contract = %v, want %v", got, testCase.want)
			}
		})
	}
}
