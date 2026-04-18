package owner

import (
	"bytes"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer is a mutex-guarded bytes.Buffer safe for concurrent Write from a
// log.Logger (running on a background goroutine) and String reads from the
// test goroutine. Standard bytes.Buffer is not safe for that pattern — under
// `-race` the unsynchronised access is a verified data race, not a false
// positive. Shared between rejection_logger_test.go and accept_loop_test.go.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *safeBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *safeBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

// shortSocketPath returns a short Unix-socket path in the OS temp root
// (avoids macOS's 104-byte sockaddr_un.sun_path limit that t.TempDir() can
// easily exceed when the test name is long). Mirrors the daemon_test helper.
// The returned path does NOT exist on disk — the caller's Listen creates it.
func shortSocketPath(t interface {
	Helper()
	Fatalf(format string, args ...interface{})
	Cleanup(f func())
}) string {
	t.Helper()
	f, err := os.CreateTemp("", "mux-test-*.sock")
	if err != nil {
		t.Fatalf("shortSocketPath: CreateTemp: %v", err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestRejectionLogger_RateLimit(t *testing.T) {
	var buf safeBuffer
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

	var buf safeBuffer
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
