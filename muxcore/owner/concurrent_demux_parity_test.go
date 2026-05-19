package owner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/remap"
)

func TestConcurrentDemuxParityRoutesOutOfOrderResponsesByID(t *testing.T) {
	o := newMinimalOwner()
	upstream := &safeBuf{}
	o.upstreamWriter = upstream

	slowSession, slowBuf := newTestSession("/project/slow")
	fastSession, fastBuf := newTestSession("/project/fast")

	o.mu.Lock()
	o.sessions[slowSession.ID] = slowSession
	o.sessions[fastSession.ID] = fastSession
	o.mu.Unlock()
	o.sessionMgr.RegisterSession(slowSession, slowSession.Cwd)
	o.sessionMgr.RegisterSession(fastSession, fastSession.Cwd)

	slowReq := parseMessage([]byte(`{"jsonrpc":"2.0","id":101,"method":"tools/call","params":{"name":"slow"}}`))
	fastReq := parseMessage([]byte(`{"jsonrpc":"2.0","id":202,"method":"tools/call","params":{"name":"fast"}}`))

	if err := o.handleDownstreamMessage(slowSession, slowReq); err != nil {
		t.Fatalf("handleDownstreamMessage slow: %v", err)
	}
	if err := o.handleDownstreamMessage(fastSession, fastReq); err != nil {
		t.Fatalf("handleDownstreamMessage fast: %v", err)
	}

	slowRemapped := string(remap.Remap(slowSession.ID, slowReq.ID))
	fastRemapped := string(remap.Remap(fastSession.ID, fastReq.ID))
	upstreamLines := nonEmptyLines(upstream.String())
	if len(upstreamLines) != 2 {
		t.Fatalf("upstream request count = %d, want 2; upstream=%q", len(upstreamLines), upstream.String())
	}
	if !strings.Contains(upstream.String(), slowRemapped) || !strings.Contains(upstream.String(), fastRemapped) {
		t.Fatalf("upstream did not receive both remapped IDs slow=%s fast=%s; upstream=%q", slowRemapped, fastRemapped, upstream.String())
	}
	if got := o.pendingRequests.Load(); got != 2 {
		t.Fatalf("pendingRequests before responses = %d, want 2", got)
	}

	fastResp := parseMessage([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%s,"result":{"label":"fast","successor_owner_generation":"successor-owner-2"}}`,
		fastRemapped,
	)))
	if err := o.handleUpstreamMessage(fastResp); err != nil {
		t.Fatalf("handleUpstreamMessage fast: %v", err)
	}

	if got := countID(fastBuf.String(), 202); got != 1 {
		t.Fatalf("fast response id count = %d, want 1; fastBuf=%q", got, fastBuf.String())
	}
	if !strings.Contains(fastBuf.String(), `"successor_owner_generation":"successor-owner-2"`) {
		t.Fatalf("fast response missing successor owner generation evidence: %q", fastBuf.String())
	}
	if got := countID(slowBuf.String(), 101); got != 0 {
		t.Fatalf("slow response arrived before slow upstream response, count=%d; slowBuf=%q", got, slowBuf.String())
	}

	slowResp := parseMessage([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%s,"result":{"label":"slow","successor_owner_generation":"successor-owner-2"}}`,
		slowRemapped,
	)))
	if err := o.handleUpstreamMessage(slowResp); err != nil {
		t.Fatalf("handleUpstreamMessage slow: %v", err)
	}

	if got := countID(slowBuf.String(), 101); got != 1 {
		t.Fatalf("slow response id count = %d, want 1; slowBuf=%q", got, slowBuf.String())
	}
	if got := countID(fastBuf.String(), 202); got != 1 {
		t.Fatalf("fast response id count after slow = %d, want 1; fastBuf=%q", got, fastBuf.String())
	}
	if strings.Contains(fastBuf.String(), `"id":101`) || strings.Contains(slowBuf.String(), `"id":202`) {
		t.Fatalf("responses crossed sessions: fastBuf=%q slowBuf=%q", fastBuf.String(), slowBuf.String())
	}
	if got := o.pendingRequests.Load(); got != 0 {
		t.Fatalf("pendingRequests after responses = %d, want 0", got)
	}
	if _, ok := o.inflightTracker.Load(fastRemapped); ok {
		t.Fatalf("fast inflight entry %s still present", fastRemapped)
	}
	if _, ok := o.inflightTracker.Load(slowRemapped); ok {
		t.Fatalf("slow inflight entry %s still present", slowRemapped)
	}
	if ctx := o.sessionMgr.ResolveCallback(); ctx != nil {
		t.Fatalf("session manager still has inflight callback context for session %d", ctx.Session.ID)
	}
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func countID(s string, id int) int {
	return strings.Count(s, fmt.Sprintf(`"id":%d`, id))
}
