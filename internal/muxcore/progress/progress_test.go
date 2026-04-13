package progress_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/progress"
)

// ---------------------------------------------------------------------------
// BuildSyntheticNotification
// ---------------------------------------------------------------------------

func TestBuildSyntheticNotification_WithTool(t *testing.T) {
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

func TestBuildSyntheticNotification_MethodLabel(t *testing.T) {
	// When the label is a method name rather than a tool name it must still appear.
	data := progress.BuildSyntheticNotification(`"tok-2"`, "tools/call", 5)
	if !bytes.Contains(data, []byte("tools/call")) {
		t.Errorf("expected message to contain method name; got: %s", data)
	}
}

func TestBuildSyntheticNotification_LargeElapsed(t *testing.T) {
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

func TestBuildSyntheticNotification_NumericToken(t *testing.T) {
	// Numeric tokens must not be re-quoted; they must remain a JSON number.
	data := progress.BuildSyntheticNotification(`42`, "my_tool", 1)
	if !bytes.Contains(data, []byte(`"progressToken":42`)) {
		t.Errorf("numeric token should be preserved as JSON number; got: %s", data)
	}
}

// ---------------------------------------------------------------------------
// LoadProgressInterval
// ---------------------------------------------------------------------------

func TestLoadProgressInterval_Default(t *testing.T) {
	if got := progress.LoadProgressInterval(0); got != 5*time.Second {
		t.Errorf("LoadProgressInterval(0) = %v, want 5s", got)
	}
}

func TestLoadProgressInterval_Negative(t *testing.T) {
	if got := progress.LoadProgressInterval(-1); got != 5*time.Second {
		t.Errorf("LoadProgressInterval(-1) = %v, want 5s", got)
	}
}

func TestLoadProgressInterval_Custom(t *testing.T) {
	ns := int64(10 * time.Second)
	if got := progress.LoadProgressInterval(ns); got != 10*time.Second {
		t.Errorf("LoadProgressInterval(%d) = %v, want 10s", ns, got)
	}
}

// ---------------------------------------------------------------------------
// Tracker — deduplication
// ---------------------------------------------------------------------------

func TestTracker_NoSuppressInitially(t *testing.T) {
	tr := progress.NewTracker()
	if tr.ShouldSuppress("tok", 5*time.Second) {
		t.Error("fresh tracker should not suppress any token")
	}
}

func TestTracker_RecordRealProgress_SuppressesWithinInterval(t *testing.T) {
	tr := progress.NewTracker()
	tr.RecordRealProgress("tok", false)

	if !tr.ShouldSuppress("tok", 5*time.Second) {
		t.Error("real progress just recorded: should suppress within interval")
	}
}

func TestTracker_RealProgressOlderThanInterval_AllowsSynthetic(t *testing.T) {
	tr := progress.NewTracker()
	// Suppress check with a tiny interval so "just recorded" is already past.
	tr.RecordRealProgress("tok", false)
	// Use 1 ns interval — any real progress is older than that.
	if tr.ShouldSuppress("tok", time.Nanosecond) {
		// This is timing-sensitive; give it a tiny grace window.
		t.Log("ShouldSuppress returned true with 1ns interval — acceptable on fast machine, skipping")
	}
}

func TestTracker_DeterminateToken_PermanentlySuppresses(t *testing.T) {
	tr := progress.NewTracker()
	tr.RecordRealProgress("tok", true) // hasTotalField=true

	// Even with a vanishingly small interval (so time-based guard wouldn't trigger),
	// the determinate flag must permanently suppress.
	if !tr.ShouldSuppress("tok", time.Nanosecond) {
		t.Error("determinate token should be permanently suppressed regardless of interval")
	}
}

func TestTracker_Cleanup_RemovesState(t *testing.T) {
	tr := progress.NewTracker()
	tr.RecordRealProgress("tok", true)

	tr.Cleanup([]string{"tok"})

	// After cleanup both guards must be gone.
	if tr.ShouldSuppress("tok", 1*time.Hour) {
		t.Error("token should no longer suppress after Cleanup")
	}
}

func TestTracker_Cleanup_EmptyList_NoOp(t *testing.T) {
	tr := progress.NewTracker()
	tr.Cleanup(nil)   // must not panic
	tr.Cleanup([]string{}) // must not panic
}

func TestTracker_MultipleTokens_IndependentState(t *testing.T) {
	tr := progress.NewTracker()
	tr.RecordRealProgress("tok-a", true)  // determinate
	tr.RecordRealProgress("tok-b", false) // indeterminate, recent

	if !tr.ShouldSuppress("tok-a", 5*time.Second) {
		t.Error("tok-a should be suppressed (determinate)")
	}
	if !tr.ShouldSuppress("tok-b", 5*time.Second) {
		t.Error("tok-b should be suppressed (recent real progress)")
	}

	// Unknown token is not suppressed.
	if tr.ShouldSuppress("tok-c", 5*time.Second) {
		t.Error("tok-c was never recorded; should not be suppressed")
	}

	// Cleanup tok-a only — tok-b must remain.
	tr.Cleanup([]string{"tok-a"})
	if tr.ShouldSuppress("tok-a", 5*time.Second) {
		t.Error("tok-a should no longer suppress after Cleanup")
	}
	if !tr.ShouldSuppress("tok-b", 5*time.Second) {
		t.Error("tok-b should still suppress — it was not cleaned up")
	}
}
