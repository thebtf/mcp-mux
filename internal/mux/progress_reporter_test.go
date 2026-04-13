package mux

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// buildSyntheticProgress — unit tests
// ---------------------------------------------------------------------------

func TestBuildSyntheticProgress_WithTool(t *testing.T) {
	data := buildSyntheticProgress(`"tok-1"`, "tavily_search", 10)
	if data == nil {
		t.Fatal("expected non-nil bytes")
	}

	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			ProgressToken string `json:"progressToken"`
			Progress      int    `json:"progress"`
			Message       string `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v\ndata: %s", err, data)
	}

	if msg.JSONRPC != "2.0" {
		t.Errorf("jsonrpc: got %q, want %q", msg.JSONRPC, "2.0")
	}
	if msg.Method != "notifications/progress" {
		t.Errorf("method: got %q, want %q", msg.Method, "notifications/progress")
	}
	if msg.Params.ProgressToken != "tok-1" {
		t.Errorf("progressToken: got %q, want %q", msg.Params.ProgressToken, "tok-1")
	}
	if msg.Params.Progress != 10 {
		t.Errorf("progress: got %d, want 10", msg.Params.Progress)
	}
	if !strings.Contains(msg.Params.Message, "tavily_search") {
		t.Errorf("message missing tool name: %q", msg.Params.Message)
	}
	if !strings.Contains(msg.Params.Message, "10s") {
		t.Errorf("message missing elapsed: %q", msg.Params.Message)
	}
}

func TestBuildSyntheticProgress_WithoutTool(t *testing.T) {
	// When Tool is empty, Method is used as the label.
	data := buildSyntheticProgress(`"tok-2"`, "tools/call", 5)
	if !bytes.Contains(data, []byte("tools/call")) {
		t.Errorf("expected message to contain method name; got: %s", data)
	}
}

func TestBuildSyntheticProgress_LongElapsed(t *testing.T) {
	data := buildSyntheticProgress(`"tok-3"`, "slow_tool", 3661)
	var msg struct {
		Params struct {
			Progress int    `json:"progress"`
			Message  string `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Params.Progress != 3661 {
		t.Errorf("progress: got %d, want 3661", msg.Params.Progress)
	}
	if !strings.Contains(msg.Params.Message, "3661s") {
		t.Errorf("message should contain elapsed seconds: %q", msg.Params.Message)
	}
}

// ---------------------------------------------------------------------------
// emitSyntheticProgress — integration-style tests using a real Owner
// ---------------------------------------------------------------------------

// buildTestOwnerWithSession creates a minimal Owner with one live session backed
// by a bytes.Buffer, and seeds one inflight request with a progress token.
// Returns the owner, the session, and the buffer receiving writes.
func buildTestOwnerWithSession(startedAgo time.Duration) (*Owner, *Session, *bytes.Buffer) {
	o := newMinimalOwner()

	// Create a session backed by a buffer so we can inspect writes.
	var buf bytes.Buffer
	s := NewSession(strings.NewReader(""), &buf)

	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()

	const reqID = `"req-progress-001"`
	const token = `"tok-progress-001"`

	// Seed inflight request with the desired start time.
	req := &InflightRequest{
		Method:    "tools/call",
		Tool:      "slow_tool",
		SessionID: s.ID,
		StartTime: time.Now().Add(-startedAgo),
	}
	o.inflightTracker.Store(reqID, req)

	// Seed progress token mapping.
	o.mu.Lock()
	o.progressOwners[token] = s.ID
	o.progressTokenRequestID[token] = reqID
	o.requestToTokens[reqID] = []string{token}
	o.mu.Unlock()

	return o, s, &buf
}

func TestProgressReporter_EmitsAfterThreshold(t *testing.T) {
	// Request has been running 10 seconds — well above the 5s interval.
	o, _, buf := buildTestOwnerWithSession(10 * time.Second)

	o.emitSyntheticProgress(5 * time.Second)

	written := buf.String()
	if written == "" {
		t.Fatal("expected synthetic progress notification to be written, got nothing")
	}
	if !strings.Contains(written, "notifications/progress") {
		t.Errorf("written data missing notifications/progress: %q", written)
	}
	if !strings.Contains(written, "slow_tool") {
		t.Errorf("written data missing tool name: %q", written)
	}
}

func TestProgressReporter_NoEmitWhenYoung(t *testing.T) {
	// Request started only 1 second ago — below the 5s interval.
	o, _, buf := buildTestOwnerWithSession(1 * time.Second)

	o.emitSyntheticProgress(5 * time.Second)

	if buf.Len() > 0 {
		t.Errorf("expected no emission for young request, got: %q", buf.String())
	}
}

func TestProgressReporter_StopsOnCancel(t *testing.T) {
	o := newMinimalOwner()
	o.progressIntervalNs.Store(int64(50 * time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.runProgressReporter(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// goroutine exited cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("runProgressReporter did not stop after context cancellation")
	}
}
