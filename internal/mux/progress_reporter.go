package mux

import (
	"context"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/progress"
)

// doneContext wraps a done channel into a context.Context.
// The returned context is cancelled when the channel is closed.
func doneContext(done <-chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-done
		cancel()
	}()
	return ctx
}

// recordRealProgress notes that a real upstream progress notification was received
// for the given token. If hasTotalField is true the token is marked determinate
// and synthetic progress is permanently suppressed for it.
func (o *Owner) recordRealProgress(token string, hasTotalField bool) {
	o.progressTracker.RecordRealProgress(token, hasTotalField)
}

// loadProgressInterval reads the current interval from the atomic field with a
// fallback to 5 s when the stored value is zero or negative.
func (o *Owner) loadProgressInterval() time.Duration {
	return progress.LoadProgressInterval(o.progressIntervalNs.Load())
}

// runProgressReporter scans inflightTracker every interval and sends synthetic
// notifications/progress for long-running requests without recent real progress.
// The ticker is reset whenever the configured interval changes so that updates
// from the initialize response take effect without restarting the goroutine.
func (o *Owner) runProgressReporter(ctx context.Context) {
	interval := o.loadProgressInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := o.loadProgressInterval()
			if current != interval {
				interval = current
				ticker.Reset(interval)
			}
			o.emitSyntheticProgress(interval)
		}
	}
}

// emitSyntheticProgress scans inflight requests and sends synthetic notifications
// for any that have been running longer than the reporting interval.
func (o *Owner) emitSyntheticProgress(interval time.Duration) {
	now := time.Now()

	o.inflightTracker.Range(func(key, value any) bool {
		reqID := key.(string)
		req := value.(*InflightRequest)

		elapsed := now.Sub(req.StartTime)
		if elapsed < interval {
			return true // too young, skip
		}

		// Look up progress tokens for this request
		o.mu.RLock()
		tokens := o.requestToTokens[reqID]
		o.mu.RUnlock()

		if len(tokens) == 0 {
			return true // no progress token, can't send notification
		}

		elapsedSec := int(elapsed.Seconds())
		toolOrMethod := req.Tool
		if toolOrMethod == "" {
			toolOrMethod = req.Method
		}

		// Send to the owning session
		o.mu.RLock()
		session, ok := o.sessions[req.SessionID]
		o.mu.RUnlock()

		if !ok {
			return true // session gone
		}

		for _, token := range tokens {
			if o.progressTracker.ShouldSuppress(token, interval) {
				continue
			}

			data := progress.BuildSyntheticNotification(token, toolOrMethod, elapsedSec)
			if err := session.WriteRaw(data); err != nil {
				o.logger.Printf("session %d: synthetic progress write error: %v", req.SessionID, err)
			}
		}

		return true
	})
}
