package owner

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

func TestModernSharingEdges_LogLevelReturnsErrorWithoutUpstreamWrite(t *testing.T) {
	var upstream safeBuf
	o := newModernWriterOwner(t, &upstream, nil)
	o.modernShared = true
	cwd := t.TempDir()
	a, outA := addModernOwnerSession(t, o, cwd)
	_, outB := addModernOwnerSession(t, o, cwd)
	forwardModernEdge(t, o, a, `{"jsonrpc":"2.0","id":"logging","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/logLevel":"info"}}}`)
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(outA.String()), &response); err != nil {
		t.Fatalf("decode shared logging refusal: %v", err)
	}
	if string(response.ID) != `"logging"` || response.Error == nil || response.Error.Code == 0 || response.Error.Message == "" {
		t.Fatalf("shared logging did not return an explicit request-correlated error: %s", outA.String())
	}
	if upstream.String() != "" || outB.String() != "" || o.PendingRequests() != 0 || o.ActiveProgressTokens() != 0 {
		t.Fatalf("shared logging refusal caused upstream work, foreign delivery, or pending state: upstream=%q foreign=%q pending=%d progress=%d", upstream.String(), outB.String(), o.PendingRequests(), o.ActiveProgressTokens())
	}
}

func TestModernSharingEdges_SubscriptionAcknowledgementSubsetAndGracefulClosure(t *testing.T) {
	var upstream safeBuf
	o := newModernWriterOwner(t, &upstream, nil)
	o.modernShared = true
	cwd := t.TempDir()
	a, outA := addModernOwnerSession(t, o, cwd)
	_, outB := addModernOwnerSession(t, o, cwd)
	forwardModernEdge(t, o, a, `{"jsonrpc":"2.0","id":"listen","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true,"promptsListChanged":true,"resourceSubscriptions":["file:///a"]},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	requests := modernSharingFrames(t, upstream.String())
	if len(requests) != 1 {
		t.Fatalf("listen forwarded %d times, want once", len(requests))
	}
	wireID := requests[0].ID
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s}}}`, wireID))
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"notifications":{"resourcesListChanged":true}}}`, wireID))
	if outA.String() != "" || outB.String() != "" || o.PendingRequests() != 1 {
		t.Fatalf("pre-ACK event or non-subset ACK was delivered or ended the listen: a=%q b=%q pending=%d", outA.String(), outB.String(), o.PendingRequests())
	}
	accepted := `{"toolsListChanged":true,"resourceSubscriptions":["file:///a"]}`
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"notifications":%s}}`, wireID, accepted))
	for _, event := range []string{
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/prompts/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s}}}`, wireID),
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"uri":"file:///foreign"}}`, wireID),
	} {
		modernSharingDeliver(t, o, event)
	}
	if frames := modernSharingFrames(t, outA.String()); len(frames) != 1 {
		t.Fatalf("unsupported acknowledged-filter events were delivered: %s", outA.String())
	}
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"uri":"file:///a"}}`, wireID))
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s}}}`, wireID))
	frames := modernSharingFrames(t, outA.String())
	if len(frames) != 3 || frames[0].Method != "notifications/subscriptions/acknowledged" || frames[1].Params.URI != "file:///a" || frames[2].Method != "notifications/tools/list_changed" {
		t.Fatalf("accepted subscription lost its ACK/event order: %s", outA.String())
	}
	var gotFilter, wantFilter any
	if err := json.Unmarshal(frames[0].Params.Notifications, &gotFilter); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(accepted), &wantFilter); err != nil || !reflect.DeepEqual(gotFilter, wantFilter) {
		t.Fatalf("accepted ACK subset changed: %s", frames[0].Params.Notifications)
	}
	for _, frame := range frames {
		if string(frame.Params.Meta[modernSharingSubscriptionID]) != `"listen"` {
			t.Fatalf("subscription notification retained its upstream ID: %s", outA.String())
		}
	}
	completion := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","_meta":{"io.modelcontextprotocol/subscriptionId":%s,"fixture":"keep"}}}`, wireID, wireID)
	modernSharingDeliver(t, o, completion)
	lines := strings.Split(strings.TrimSpace(outA.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("graceful listen completion was not delivered once: %s", outA.String())
	}
	var ended struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Type string                     `json:"resultType"`
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[3]), &ended); err != nil {
		t.Fatal(err)
	}
	if string(ended.ID) != `"listen"` || ended.Result.Type != "complete" || string(ended.Result.Meta[modernSharingSubscriptionID]) != `"listen"` || string(ended.Result.Meta["fixture"]) != `"keep"` {
		t.Fatalf("graceful response ID/result metadata was not restored: %s", lines[3])
	}
	before := outA.String()
	modernSharingDeliver(t, o, completion)
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s}}}`, wireID))
	if outA.String() != before || outB.String() != "" || o.PendingRequests() != 0 {
		t.Fatalf("graceful completion retained a route or leaked traffic: a=%q b=%q pending=%d", outA.String(), outB.String(), o.PendingRequests())
	}
}

func TestModernSharingEdges_GenerationLossEndsEachHostRequestOnceAndDropsLateAsync(t *testing.T) {
	o := newModernWriterOwner(t, io.Discard, nil)
	o.modernShared = true
	cwd := t.TempDir()
	a, outA := addModernOwnerSession(t, o, cwd)
	b, outB := addModernOwnerSession(t, o, cwd)
	oldFrames := make(chan string, 2)
	oldProc := newModernLossRecordingProcess(t, oldFrames)
	setModernLossProcess(o, oldProc)
	forwardModernEdge(t, o, a, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":"same"}}}`)
	forwardModernEdge(t, o, b, `{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":"same"}}}`)
	requestA := modernSharingFrames(t, readModernLossFrame(t, oldFrames))[0]
	requestB := modernSharingFrames(t, readModernLossFrame(t, oldFrames))[0]
	ack := fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"notifications":{"toolsListChanged":true}}}`, requestB.ID)
	if err := o.handleUpstreamMessageFrom(oldProc, parseMessage([]byte(ack))); err != nil {
		t.Fatal(err)
	}
	if frames := modernSharingFrames(t, outB.String()); len(frames) != 1 || frames[0].Method != "notifications/subscriptions/acknowledged" || string(frames[0].Params.Meta[modernSharingSubscriptionID]) != "7" {
		t.Fatalf("subscription was not active before actual generation loss: %s", outB.String())
	}
	if o.PendingRequests() != 2 || o.ActiveProgressTokens() != 2 {
		t.Fatalf("shared requests were not both active before loss: pending=%d progress=%d", o.PendingRequests(), o.ActiveProgressTokens())
	}
	beforeA, beforeB := outA.String(), outB.String()
	retireModernLossProcess(t, oldProc)
	if !o.markCurrentUpstreamDead(oldProc) {
		t.Fatal("exact current shared process loss was not claimed")
	}
	assertModernProcessLossErrors(t, strings.TrimPrefix(outA.String(), beforeA), `7`)
	assertModernProcessLossErrors(t, strings.TrimPrefix(outB.String(), beforeB), `7`)
	if o.PendingRequests() != 0 || o.ActiveProgressTokens() != 0 {
		t.Fatalf("shared process loss retained routes: pending=%d progress=%d", o.PendingRequests(), o.ActiveProgressTokens())
	}
	currentProc := newModernLossProcess(t)
	setModernLossProcess(o, currentProc)
	afterA, afterB := outA.String(), outB.String()
	late := []string{
		ack,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":%s,"progress":1}}`, requestA.Params.Meta["progressToken"]),
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":%s,"progress":1}}`, requestB.Params.Meta["progressToken"]),
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s}}}`, requestB.ID),
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s}}`, requestB.ID),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete"}}`, requestA.ID),
	}
	for _, raw := range late {
		if err := o.handleUpstreamMessageFrom(oldProc, parseMessage([]byte(raw))); err != nil {
			t.Fatalf("late old-generation frame: %v", err)
		}
	}
	if o.markCurrentUpstreamDead(oldProc) {
		t.Fatal("late old loss claimed the replacement generation")
	}
	if outA.String() != afterA || outB.String() != afterB || o.PendingRequests() != 0 || o.ActiveProgressTokens() != 0 {
		t.Fatal("late old-generation traffic duplicated terminal delivery or recreated a route")
	}
}

func TestModernSharingEdges_MRTRPreservesOpaqueStateAndUsesFreshRequestID(t *testing.T) {
	var upstream safeBuf
	o := newModernWriterOwner(t, &upstream, nil)
	o.modernShared = true
	a, downstream := addModernOwnerSession(t, o, t.TempDir())
	forwardModernEdge(t, o, a, `{"jsonrpc":"2.0","id":"round-one","method":"tools/call","params":{"name":"confirm","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	first := modernSharingFrames(t, upstream.String())
	if len(first) != 1 {
		t.Fatalf("initial MRTR request forwarded %d times, want once", len(first))
	}
	state := `{"sealed":"A+/=\\u0000","number":9007199254740993,"progressToken":"opaque-not-a-route","nested":[false,null,{"subscriptionId":7}]}`
	inputs := `{"confirmation":{"method":"elicitation/create","params":{"requestedSchema":{"type":"object"}}}}`
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"input_required","inputRequests":%s,"requestState":%s}}`, first[0].ID, inputs, state))
	var inputRequired struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Type     string          `json:"resultType"`
			State    json.RawMessage `json:"requestState"`
			Requests json.RawMessage `json:"inputRequests"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(downstream.String()), &inputRequired); err != nil {
		t.Fatal(err)
	}
	if string(inputRequired.ID) != `"round-one"` || inputRequired.Result.Type != "input_required" || string(inputRequired.Result.State) != state || string(inputRequired.Result.Requests) != inputs || o.PendingRequests() != 0 {
		t.Fatalf("input_required altered opaque fields or retained the completed first round: %s", downstream.String())
	}
	answers := `{"confirmation":{"action":"accept","content":{"number":9007199254740993,"text":"unchanged"}}}`
	forwardModernEdge(t, o, a, fmt.Sprintf(`{"jsonrpc":"2.0","id":"round-two","method":"tools/call","params":{"name":"confirm","requestState":%s,"inputResponses":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, state, answers))
	lines := strings.Split(strings.TrimSpace(upstream.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("MRTR resumed with replay or duplicate upstream work: %s", upstream.String())
	}
	var resumed struct {
		ID     json.RawMessage `json:"id"`
		Params struct {
			State     json.RawMessage `json:"requestState"`
			Responses json.RawMessage `json:"inputResponses"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resumed); err != nil {
		t.Fatal(err)
	}
	if len(resumed.ID) == 0 || string(resumed.ID) == string(first[0].ID) || string(resumed.Params.State) != state || string(resumed.Params.Responses) != answers {
		t.Fatalf("MRTR retry reused its wire ID or changed opaque input: %s", lines[1])
	}
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","source":"round-two"}}`, resumed.ID))
	responses := modernSharingFrames(t, downstream.String())
	if len(responses) != 2 || string(responses[1].ID) != `"round-two"` || responses[1].Result.Source != "round-two" || o.PendingRequests() != 0 {
		t.Fatalf("resumed MRTR did not complete under its fresh host ID: %s", downstream.String())
	}
}

func TestModernSharingEdges_CancelDuringBlockedWriteFollowsItsPublishedRequest(t *testing.T) {
	o := newModernWriterOwner(t, io.Discard, nil)
	o.modernShared = true
	a, outA := addModernOwnerSession(t, o, t.TempDir())
	frames := make(chan string, 3)
	proc := newModernLossRecordingProcess(t, frames)
	setModernLossProcess(o, proc)
	writeEntered, releaseWrite := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWrite) }) }
	t.Cleanup(release)
	o.beforeCurrentUpstreamWrite = func(selected *upstream.Process) {
		if selected != proc {
			return
		}
		close(writeEntered)
		<-releaseWrite
	}
	cancelEntered := make(chan struct{})
	o.beforeLocalDemandCancel = func() { close(cancelEntered) }
	requestDone := make(chan error, 1)
	go func() {
		request := parseMessage([]byte(`{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":"blocked"}}}`))
		requestDone <- o.handleDownstreamMessage(a, request)
	}()
	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach its published pre-write boundary")
	}
	cancelDone := make(chan error, 1)
	go func() {
		cancel := parseMessage([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":41}}`))
		cancelDone <- o.handleDownstreamMessage(a, cancel)
	}()
	select {
	case <-cancelEntered:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not enter while its request write was blocked")
	}
	release()
	for _, done := range []<-chan error{requestDone, cancelDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("blocked-write request/cancel did not settle after release")
		}
	}
	request := modernSharingFrames(t, readModernLossFrame(t, frames))[0]
	cancel := modernSharingFrames(t, readModernLossFrame(t, frames))[0]
	if request.Method != "tools/call" || cancel.Method != "notifications/cancelled" || string(cancel.Params.RequestID) != string(request.ID) {
		t.Fatalf("shared cancellation did not follow its own published wire request: request=%+v cancel=%+v", request, cancel)
	}
	if o.PendingRequests() != 0 || o.ActiveProgressTokens() != 0 || outA.String() != "" {
		t.Fatalf("blocked-write cancellation retained routes or emitted a synthetic terminal: pending=%d progress=%d downstream=%q", o.PendingRequests(), o.ActiveProgressTokens(), outA.String())
	}
	late := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete"}}`, request.ID)
	if err := o.handleUpstreamMessageFrom(proc, parseMessage([]byte(late))); err != nil {
		t.Fatal(err)
	}
	if outA.String() != "" {
		t.Fatalf("cancelled blocked-write request accepted a late result: %s", outA.String())
	}
}

func forwardModernEdge(t *testing.T, o *Owner, session *Session, raw string) {
	t.Helper()
	if err := o.handleDownstreamMessage(session, parseMessage([]byte(raw))); err != nil {
		t.Fatalf("forward modern edge request: %v", err)
	}
}
