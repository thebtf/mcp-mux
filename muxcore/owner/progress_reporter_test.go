package owner

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/progress"
)

// ---------------------------------------------------------------------------
// BuildSyntheticNotification — smoke tests via the muxcore/progress package.
// Full unit coverage lives in muxcore/progress/progress_test.go.
// ---------------------------------------------------------------------------

func TestBuildSyntheticProgress_WithTool(t *testing.T) {
	data := progress.BuildSyntheticNotification(`"tok-1"`, "tavily_search", 10)
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
	// When the label is a method name it must still appear in the message.
	data := progress.BuildSyntheticNotification(`"tok-2"`, "tools/call", 5)
	if !bytes.Contains(data, []byte("tools/call")) {
		t.Errorf("expected message to contain method name; got: %s", data)
	}
}

func TestBuildSyntheticProgress_LongElapsed(t *testing.T) {
	data := progress.BuildSyntheticNotification(`"tok-3"`, "slow_tool", 3661)
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

// ---------------------------------------------------------------------------
// Dedup tests — Phase 3
// ---------------------------------------------------------------------------

// buildDedupOwner builds an Owner with one session and exactly one inflight request
// seeded 10 s ago. The request has a single progress token. Returns the owner, the
// buffer receiving writes from that session, and the token string.
func buildDedupOwner(t *testing.T) (*Owner, *bytes.Buffer, string) {
	t.Helper()
	const reqID = `"req-dedup-001"`
	const token = `"tok-dedup-001"`

	o := newMinimalOwner()

	var buf bytes.Buffer
	s := NewSession(strings.NewReader(""), &buf)

	o.mu.Lock()
	o.sessions[s.ID] = s
	o.progressOwners[token] = s.ID
	o.progressTokenRequestID[token] = reqID
	o.requestToTokens[reqID] = []string{token}
	o.mu.Unlock()

	req := &InflightRequest{
		Method:    "tools/call",
		Tool:      "slow_tool",
		SessionID: s.ID,
		StartTime: time.Now().Add(-10 * time.Second),
	}
	o.inflightTracker.Store(reqID, req)

	return o, &buf, token
}

// TestDedup_RealProgressSuppressesSynthetic verifies that when real progress was
// received within the current interval, emitSyntheticProgress skips that token.
func TestDedup_RealProgressSuppressesSynthetic(t *testing.T) {
	o, buf, token := buildDedupOwner(t)

	// Record real progress just now — within any reasonable interval.
	o.recordRealProgress(token, false)

	o.emitSyntheticProgress(5 * time.Second)

	if buf.Len() > 0 {
		t.Errorf("expected no synthetic emission when real progress active for %s; got: %q",
			token, buf.String())
	}
}

// TestDedup_SyntheticResumesAfterSilence verifies that when real progress is older
// than one interval, synthetic progress resumes.
func TestDedup_SyntheticResumesAfterSilence(t *testing.T) {
	o, buf, token := buildDedupOwner(t)

	// Record real progress that is 10 seconds in the past — older than the 5s interval.
	o.progressTracker.RecordRealProgressAt(token, time.Now().Add(-10*time.Second), false)

	o.emitSyntheticProgress(5 * time.Second)

	if buf.Len() == 0 {
		t.Error("expected synthetic emission to resume after real progress went silent")
	}
	if !strings.Contains(buf.String(), "notifications/progress") {
		t.Errorf("written data missing notifications/progress: %q", buf.String())
	}
}

// TestDedup_DeterminatePermanentlySuppresses verifies that once real progress with
// a total field is recorded, synthetic progress is never emitted for that token —
// even after the interval has elapsed.
func TestDedup_DeterminatePermanentlySuppresses(t *testing.T) {
	o, buf, token := buildDedupOwner(t)

	// Record real progress with hasTotalField=true (determinate bar), placing
	// the timestamp 1 hour in the past so the time-based guard alone would not
	// suppress — only the determinate flag must suppress.
	o.progressTracker.RecordRealProgressAt(token, time.Now().Add(-1*time.Hour), true)

	o.emitSyntheticProgress(5 * time.Second)

	if buf.Len() > 0 {
		t.Errorf("expected no synthetic emission for determinate token; got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// T013 — configurable progressInterval tests (Phase 4)
// ---------------------------------------------------------------------------

// TestProgressInterval_CustomValue verifies that loadProgressInterval returns
// the interval stored in progressIntervalNs when it is explicitly set.
func TestProgressInterval_CustomValue(t *testing.T) {
	o := newMinimalOwner()
	o.progressIntervalNs.Store(int64(10 * time.Second))

	got := o.loadProgressInterval()
	if got != 10*time.Second {
		t.Errorf("loadProgressInterval() = %v, want %v", got, 10*time.Second)
	}
}

// TestProgressInterval_DefaultWhenAbsent verifies that loadProgressInterval
// returns the 5 s default when no interval has been stored (zero value).
func TestProgressInterval_DefaultWhenAbsent(t *testing.T) {
	o := newMinimalOwner()
	// progressIntervalNs is zero-valued — default fallback must apply.

	got := o.loadProgressInterval()
	if got != 5*time.Second {
		t.Errorf("loadProgressInterval() = %v, want %v", got, 5*time.Second)
	}
}

// TestDedup_CleanupOnRequestComplete verifies that clearProgressTokensForRequest
// removes dedup state from the Tracker and all main progress maps.
func TestDedup_CleanupOnRequestComplete(t *testing.T) {
	const reqID = `"req-cleanup-001"`
	const token = `"tok-cleanup-001"`

	o := newMinimalOwner()

	o.mu.Lock()
	o.progressOwners[token] = 1
	o.progressTokenRequestID[token] = reqID
	o.requestToTokens[reqID] = []string{token}
	o.mu.Unlock()

	// Seed the tracker with real progress so there is state to clean up.
	o.progressTracker.RecordRealProgress(token, true)

	// Verify state is suppressing before cleanup.
	if !o.progressTracker.ShouldSuppress(token, 5*time.Second) {
		t.Fatal("expected token to be suppressed before cleanup")
	}

	o.clearProgressTokensForRequest(reqID)

	// After cleanup the tracker must no longer suppress.
	if o.progressTracker.ShouldSuppress(token, 1*time.Hour) {
		t.Errorf("token should not suppress after clearProgressTokensForRequest")
	}

	// Also verify the main progress maps were cleaned up.
	o.mu.RLock()
	_, hasOwner := o.progressOwners[token]
	_, hasReqID := o.progressTokenRequestID[token]
	_, hasTokens := o.requestToTokens[reqID]
	o.mu.RUnlock()

	if hasOwner {
		t.Errorf("progressOwners[%s] not cleaned up", token)
	}
	if hasReqID {
		t.Errorf("progressTokenRequestID[%s] not cleaned up", token)
	}
	if hasTokens {
		t.Errorf("requestToTokens[%s] not cleaned up", reqID)
	}
}
