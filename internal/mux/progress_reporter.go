package mux

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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

// buildSyntheticProgress builds JSON-RPC notification bytes for synthetic progress.
// token: from CC's SDK (tracked in requestToTokens)
// toolOrMethod: tool name (e.g. "tavily_search") or method (e.g. "tools/call")
// elapsedSeconds: seconds since request start, used as monotonically increasing progress counter
func buildSyntheticProgress(token string, toolOrMethod string, elapsedSeconds int) []byte {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			ProgressToken string `json:"progressToken"`
			Progress      int    `json:"progress"`
			Message       string `json:"message,omitempty"`
		} `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  "notifications/progress",
	}
	msg.Params.ProgressToken = token
	msg.Params.Progress = elapsedSeconds
	msg.Params.Message = fmt.Sprintf("%s: %ds elapsed", toolOrMethod, elapsedSeconds)

	data, _ := json.Marshal(msg)
	return data
}

// runProgressReporter scans inflightTracker every interval and sends synthetic
// notifications/progress for long-running requests without recent real progress.
func (o *Owner) runProgressReporter(ctx context.Context) {
	interval := o.progressInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
			data := buildSyntheticProgress(token, toolOrMethod, elapsedSec)
			if err := session.WriteRaw(data); err != nil {
				o.logger.Printf("session %d: synthetic progress write error: %v", req.SessionID, err)
			}
		}

		return true
	})
}
