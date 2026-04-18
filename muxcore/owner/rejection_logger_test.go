package owner

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func TestRejectionLogger_RateLimit(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	rl := newRejectionLogger(logger)
	defer rl.Close()

	for i := 0; i < 15; i++ {
		rl.Log(logger, 1234)
	}

	// Allow logger goroutine to serialize writes.
	time.Sleep(10 * time.Millisecond)

	lines := strings.Count(buf.String(), "accept: rejected connection")
	if lines != 10 {
		t.Fatalf("rejection log lines: got %d, want 10", lines)
	}

	rl.mu.Lock()
	suppressed := rl.suppressed
	rl.mu.Unlock()
	if suppressed != 5 {
		t.Fatalf("suppressed count: got %d, want 5", suppressed)
	}
}

func TestRejectionLogger_Summary(t *testing.T) {
	origTicker := rejectionLoggerNewTicker
	rejectionLoggerNewTicker = func(time.Duration) *time.Ticker {
		return time.NewTicker(10 * time.Millisecond)
	}
	defer func() {
		rejectionLoggerNewTicker = origTicker
	}()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	rl := newRejectionLogger(logger)
	defer rl.Close()

	for i := 0; i < 15; i++ {
		rl.Log(logger, 4321)
	}

	// Poll for summary line instead of hard sleep — avoids CI scheduler flake.
	summary := "accept: rate-limited: 5 rejections suppressed in last 60s"
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), summary) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := strings.Count(buf.String(), summary); got != 1 {
		t.Fatalf("summary count: got %d, want 1", got)
	}
}
