package owner

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const modernSharingSubscriptionID = "io.modelcontextprotocol/subscriptionId"

// These tests attach two admitted sessions directly; daemon admission is tested
// separately so forced isolation cannot hide unsafe shared-owner wire routing.
func TestModernSharingOwner_CollidingIDsAndProgressStaySessionLocal(t *testing.T) {
	for _, original := range []string{`"same"`, `7`} {
		t.Run(original, func(t *testing.T) {
			var upstream safeBuf
			o := newModernWriterOwner(t, &upstream, nil)
			o.modernShared = true
			cwd := t.TempDir()
			a, outA := addModernOwnerSession(t, o, cwd)
			b, outB := addModernOwnerSession(t, o, cwd)
			for i, session := range []*Session{a, b} {
				request := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"echo","arguments":{"source":%q},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":%s}}}`, original, []string{"a", "b"}[i], original)
				if err := o.handleDownstreamMessage(session, parseMessage([]byte(request))); err != nil {
					t.Fatal(err)
				}
			}
			forwarded := modernSharingFrames(t, upstream.String())
			if len(forwarded) != 2 {
				t.Fatalf("forwarded requests = %d, want 2: %s", len(forwarded), upstream.String())
			}
			if string(forwarded[0].ID) == string(forwarded[1].ID) {
				t.Fatalf("two hosts with id %s received the same upstream id: %s", original, forwarded[0].ID)
			}
			tokenA, tokenB := forwarded[0].Params.Meta["progressToken"], forwarded[1].Params.Meta["progressToken"]
			if len(tokenA) == 0 || len(tokenB) == 0 || string(tokenA) == string(tokenB) {
				t.Fatalf("two hosts with progressToken %s were not separated: %s / %s", original, tokenA, tokenB)
			}
			for _, i := range []int{1, 0} {
				source := []string{"a", "b"}[i]
				token := forwarded[i].Params.Meta["progressToken"]
				modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":%s,"progress":1,"message":%q}}`, token, source))
			}
			waitForCondition(t, time.Second, func() bool {
				return strings.Count(outA.String(), "\n") == 1 && strings.Count(outB.String(), "\n") == 1
			}, "each host must receive its own progress before completion")
			for _, i := range []int{1, 0} {
				modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","source":%q}}`, forwarded[i].ID, []string{"a", "b"}[i]))
			}
			for i, output := range []*safeBuf{outA, outB} {
				frames := modernSharingFrames(t, output.String())
				if len(frames) != 2 {
					t.Fatalf("host %d got %d frames, want progress and result: %s", i, len(frames), output.String())
				}
				if frames[0].Method != "notifications/progress" || string(frames[0].Params.ProgressToken) != original || frames[0].Params.Message != []string{"a", "b"}[i] {
					t.Fatalf("host %d got foreign progress or unrestored token: %s", i, output.String())
				}
				if string(frames[1].ID) != original || frames[1].Result.Source != []string{"a", "b"}[i] {
					t.Fatalf("host %d got foreign result or unrestored id: %s", i, output.String())
				}
			}
			beforeA, beforeB := outA.String(), outB.String()
			for _, token := range []json.RawMessage{tokenA, tokenB, json.RawMessage(`"unowned"`)} {
				modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":%s,"progress":2}}`, token))
			}
			if waitCondition(t, 100*time.Millisecond, func() bool { return outA.String() != beforeA || outB.String() != beforeB }) {
				t.Fatal("completed or unknown progress route delivered a stale notification")
			}
		})
	}
}

func TestModernSharingOwner_SubscriptionsAndCancellationStaySessionLocal(t *testing.T) {
	var upstream safeBuf
	o := newModernWriterOwner(t, &upstream, nil)
	o.modernShared = true
	cwd := t.TempDir()
	a, outA := addModernOwnerSession(t, o, cwd)
	b, outB := addModernOwnerSession(t, o, cwd)
	filters := []string{`{"toolsListChanged":true,"resourceSubscriptions":["file:///a"]}`, `{"resourcesListChanged":true,"resourceSubscriptions":["file:///b"]}`}
	for i, session := range []*Session{a, b} {
		request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"notifications":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, filters[i])
		if err := o.handleDownstreamMessage(session, parseMessage([]byte(request))); err != nil {
			t.Fatal(err)
		}
	}
	forwarded := modernSharingFrames(t, upstream.String())
	if len(forwarded) != 2 || string(forwarded[0].ID) == string(forwarded[1].ID) {
		t.Fatalf("colliding listen ids were not separated: %s", upstream.String())
	}
	for i, output := range []*safeBuf{outA, outB} {
		var wantFilter any
		if err := json.Unmarshal([]byte(filters[i]), &wantFilter); err != nil {
			t.Fatal(err)
		}
		var gotFilter any
		if err := json.Unmarshal(forwarded[i].Params.Notifications, &gotFilter); err != nil || !reflect.DeepEqual(gotFilter, wantFilter) {
			t.Fatalf("host %d subscription filter changed upstream: %s", i, upstream.String())
		}
		modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s,"fixture":"keep"},"notifications":%s}}`, forwarded[i].ID, filters[i]))
		modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s,"fixture":"keep"},"uri":"file:///%s"}}`, forwarded[i].ID, []string{"a", "b"}[i]))
		waitForCondition(t, time.Second, func() bool { return strings.Count(output.String(), "\n") == 2 }, "subscription acknowledgement and update were not delivered to their host")
		frames := modernSharingFrames(t, output.String())
		if frames[0].Method != "notifications/subscriptions/acknowledged" || frames[1].Method != "notifications/resources/updated" || frames[1].Params.URI != "file:///"+[]string{"a", "b"}[i] {
			t.Fatalf("host %d subscription order or recipient is wrong: %s", i, output.String())
		}
		for _, frame := range frames {
			if string(frame.Params.Meta[modernSharingSubscriptionID]) != "1" || string(frame.Params.Meta["fixture"]) != `"keep"` {
				t.Fatalf("host %d subscription metadata was not restored: %s", i, output.String())
			}
		}
		if err := json.Unmarshal(frames[0].Params.Notifications, &gotFilter); err != nil || !reflect.DeepEqual(gotFilter, wantFilter) {
			t.Fatalf("host %d acknowledged filter changed downstream: %s", i, output.String())
		}
	}

	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s,"reason":"server ended a"}}`, forwarded[0].ID))
	framesA := modernSharingFrames(t, outA.String())
	if len(framesA) != 3 || framesA[2].Method != "notifications/cancelled" || string(framesA[2].Params.RequestID) != "1" {
		t.Fatalf("server subscription termination was not restored to host a: %s", outA.String())
	}
	o.removeSession(a)
	beforeA := outA.String()
	for _, raw := range []string{
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"uri":"file:///a"}}`, forwarded[0].ID),
		`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"unowned"},"uri":"file:///foreign"}}`,
		`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}`,
	} {
		modernSharingDeliver(t, o, raw)
	}
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"uri":"file:///b"}}`, forwarded[1].ID))
	framesB := modernSharingFrames(t, outB.String())
	if len(framesB) != 3 || framesB[2].Params.URI != "file:///b" || string(framesB[2].Params.Meta[modernSharingSubscriptionID]) != "1" {
		t.Fatalf("remaining host received stale/unowned traffic or lost its live subscription: %s", outB.String())
	}
	cancel := parseMessage([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"host b"}}`))
	if err := o.handleDownstreamMessage(b, cancel); err != nil {
		t.Fatal(err)
	}
	if err := o.handleDownstreamMessage(b, cancel); err != nil {
		t.Fatal(err)
	}
	upstreamFrames := modernSharingFrames(t, upstream.String())
	if len(upstreamFrames) != 3 || upstreamFrames[2].Method != "notifications/cancelled" || string(upstreamFrames[2].Params.RequestID) != string(forwarded[1].ID) {
		t.Fatalf("host b cancel did not target its own upstream route exactly once: %s", upstream.String())
	}
	beforeB := outB.String()
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"uri":"file:///b"}}`, forwarded[1].ID))
	modernSharingDeliver(t, o, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s}}`, forwarded[1].ID))
	if waitCondition(t, 100*time.Millisecond, func() bool { return outA.String() != beforeA || outB.String() != beforeB }) {
		t.Fatal("cancelled/detached subscription retained a downstream route")
	}
}

type modernSharingFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Meta          map[string]json.RawMessage `json:"_meta"`
		ProgressToken json.RawMessage            `json:"progressToken"`
		RequestID     json.RawMessage            `json:"requestId"`
		Notifications json.RawMessage            `json:"notifications"`
		Message       string                     `json:"message"`
		URI           string                     `json:"uri"`
	} `json:"params"`
	Result struct {
		Source string `json:"source"`
	} `json:"result"`
}

func modernSharingFrames(t *testing.T, raw string) []modernSharingFrame {
	t.Helper()
	var frames []modernSharingFrame
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var frame modernSharingFrame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode modern frame %q: %v", line, err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func modernSharingDeliver(t *testing.T, o *Owner, raw string) {
	t.Helper()
	if err := o.handleUpstreamMessage(parseMessage([]byte(raw))); err != nil {
		t.Fatalf("deliver modern upstream frame: %v", err)
	}
}
