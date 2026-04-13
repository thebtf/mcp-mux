// Package progress provides pure progress-notification helpers for mcp-mux.
//
// It contains two independently usable pieces:
//
//   - BuildSyntheticNotification — constructs an MCP notifications/progress
//     JSON-RPC message from a token, a tool/method label, and an elapsed counter.
//
//   - Tracker — thread-safe deduplication state that prevents synthetic progress
//     ticks from racing with real upstream progress notifications.
//
// Neither type depends on Owner or any other mux-internal type; they can be
// tested and reused without the full owner lifecycle.
package progress

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// BuildSyntheticNotification constructs JSON-RPC notification bytes for a
// synthetic notifications/progress message.
//
//   - token is the raw JSON progress token (e.g. `"tok-1"` or `42`); stored as
//     json.RawMessage so numeric and string tokens are preserved without
//     re-quoting.
//   - toolOrMethod is the tool name (e.g. "tavily_search") or MCP method
//     (e.g. "tools/call") used as the human-readable label in the message field.
//   - elapsedSeconds is the seconds elapsed since the request started; used as
//     a monotonically increasing progress counter.
func BuildSyntheticNotification(token string, toolOrMethod string, elapsedSeconds int) []byte {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			ProgressToken json.RawMessage `json:"progressToken"`
			Progress      int             `json:"progress"`
			Message       string          `json:"message,omitempty"`
		} `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  "notifications/progress",
	}
	msg.Params.ProgressToken = json.RawMessage(token)
	msg.Params.Progress = elapsedSeconds
	msg.Params.Message = fmt.Sprintf("%s: %ds elapsed", toolOrMethod, elapsedSeconds)

	data, _ := json.Marshal(msg)
	return data
}

// LoadProgressInterval converts a nanosecond duration stored atomically to a
// time.Duration. Returns the default 5 s when ns is zero or negative.
//
// Callers should read their atomic int64 and pass the raw value here rather
// than calling this function in a hot loop with reflection.
func LoadProgressInterval(ns int64) time.Duration {
	if ns <= 0 {
		return 5 * time.Second
	}
	return time.Duration(ns)
}

// Tracker is thread-safe deduplication state for synthetic progress emission.
//
// It tracks the last time a real upstream progress notification was received
// for each progress token, and whether that token has ever carried a total
// field (i.e. it is a determinate progress bar). Synthetic progress is
// suppressed for determinate tokens or when real progress arrived within the
// current interval.
type Tracker struct {
	mu                sync.Mutex
	lastRealProgress  map[string]time.Time // progressToken → last real-progress timestamp
	determinateTokens map[string]bool      // progressToken → true when total field seen
}

// NewTracker allocates a ready-to-use Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		lastRealProgress:  make(map[string]time.Time),
		determinateTokens: make(map[string]bool),
	}
}

// RecordRealProgress notes that a real upstream progress notification was
// received for token at the current time. When hasTotalField is true the
// token is marked determinate and synthetic progress is permanently
// suppressed for it.
func (t *Tracker) RecordRealProgress(token string, hasTotalField bool) {
	t.RecordRealProgressAt(token, time.Now(), hasTotalField)
}

// RecordRealProgressAt is like RecordRealProgress but records the event at
// an explicit time. Useful when replaying historical progress state (e.g.
// snapshot restore) or in tests that need precise timing control.
func (t *Tracker) RecordRealProgressAt(token string, at time.Time, hasTotalField bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastRealProgress[token] = at
	if hasTotalField {
		t.determinateTokens[token] = true
	}
}

// ShouldSuppress returns true when synthetic progress should NOT be emitted
// for token. Suppression occurs when:
//
//   - The token has a determinate progress bar (real progress with a total
//     field was received at any point).
//   - Real progress was received within the given interval (the upstream-driven
//     bar is still active; back off to avoid flickering).
func (t *Tracker) ShouldSuppress(token string, interval time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.determinateTokens[token] {
		return true
	}
	if last, ok := t.lastRealProgress[token]; ok {
		if time.Since(last) < interval {
			return true
		}
	}
	return false
}

// Cleanup removes deduplication state for all tokens in the provided list.
// Call this when the associated request completes so the maps do not grow
// without bound.
func (t *Tracker) Cleanup(tokens []string) {
	if len(tokens) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, token := range tokens {
		delete(t.lastRealProgress, token)
		delete(t.determinateTokens, token)
	}
}
