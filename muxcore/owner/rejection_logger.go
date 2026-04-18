package owner

import (
	"log"
	"sync"
	"time"
)

// rejectionLogger rate-limits per-rejection log entries (C4: max 10 per 60s window).
// The rejection itself is never rate-limited — only the log emission.
type rejectionLogger struct {
	mu         sync.Mutex
	timestamps [10]time.Time // ring buffer of the last 10 logged rejections
	pos        int           // next write position in ring buffer
	suppressed int64         // count of suppressed entries since last summary
	done       chan struct{}
}

var rejectionLoggerNewTicker = time.NewTicker

func newRejectionLogger(logger *log.Logger) *rejectionLogger {
	rl := &rejectionLogger{
		done: make(chan struct{}),
	}
	go rl.summaryLoop(logger)
	return rl
}

// Log emits a rejection log entry if under the 10-per-60s cap; otherwise
// increments the suppressed counter. Never logs the token value (C1).
func (rl *rejectionLogger) Log(logger *log.Logger, pid int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	count := 0
	for _, ts := range rl.timestamps {
		if ts.After(cutoff) {
			count++
		}
	}

	if count < 10 {
		rl.timestamps[rl.pos] = now
		rl.pos = (rl.pos + 1) % 10
		logger.Printf("accept: rejected connection from pid=%d (invalid/missing token)", pid)
	} else {
		rl.suppressed++
	}
}

// Close stops the background summary goroutine.
func (rl *rejectionLogger) Close() {
	close(rl.done)
}

func (rl *rejectionLogger) summaryLoop(logger *log.Logger) {
	ticker := rejectionLoggerNewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			n := rl.suppressed
			rl.suppressed = 0
			rl.mu.Unlock()
			if n > 0 {
				logger.Printf("accept: rate-limited: %d rejections suppressed in last 60s", n)
			}
		case <-rl.done:
			return
		}
	}
}
