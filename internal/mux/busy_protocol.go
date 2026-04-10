package mux

import (
	"encoding/json"
	"time"
)

// busyNotificationParams is the payload shape for notifications/x-mux/busy.
//
// Contract: upstream sends this to signal the mux layer that it is executing
// long-running background work that is NOT represented by a pending JSON-RPC
// request or an active progress token. Canonical use case: an async job
// queue (e.g. aimux.exec with async=true) that returns a job_id synchronously
// and then runs for minutes/hours in a background goroutine.
//
// The mux reaper must not evict the owner while any non-expired busy
// declaration exists. Upstream clears the declaration with
// notifications/x-mux/idle{id=<same id>}. A safety hard-cap equal to
// estimatedDurationMs * 2 ensures forgotten declarations eventually expire
// even if upstream never sends idle.
//
//	{
//	  "jsonrpc": "2.0",
//	  "method": "notifications/x-mux/busy",
//	  "params": {
//	    "id": "job-019d78e3",
//	    "startedAt": "2026-04-11T00:34:07Z",        // RFC3339, optional (default: now)
//	    "estimatedDurationMs": 1800000,              // optional (default: 10 min)
//	    "task": "research: subprocess management"   // optional, for observability
//	  }
//	}
//
// The id is opaque to mux; upstream picks any stable token it will reuse in
// the corresponding idle notification.
type busyNotificationParams struct {
	ID                  string `json:"id"`
	StartedAt           string `json:"startedAt,omitempty"`           // RFC3339
	EstimatedDurationMs int64  `json:"estimatedDurationMs,omitempty"` // optional
	Task                string `json:"task,omitempty"`
}

// idleNotificationParams is the payload shape for notifications/x-mux/idle.
// Upstream sends this to clear a previous busy declaration.
//
//	{
//	  "jsonrpc": "2.0",
//	  "method": "notifications/x-mux/idle",
//	  "params": { "id": "job-019d78e3" }
//	}
type idleNotificationParams struct {
	ID string `json:"id"`
}

type genericNotification struct {
	Params json.RawMessage `json:"params"`
}

// handleBusyNotification parses notifications/x-mux/busy and registers a
// busy declaration on the owner. Called from handleUpstreamMessage when the
// method matches; the notification is consumed at the mux layer and not
// forwarded to downstream sessions.
func (o *Owner) handleBusyNotification(raw []byte) {
	var env genericNotification
	if err := json.Unmarshal(raw, &env); err != nil {
		o.logger.Printf("x-mux busy: failed to parse envelope: %v", err)
		return
	}
	var p busyNotificationParams
	if len(env.Params) > 0 {
		if err := json.Unmarshal(env.Params, &p); err != nil {
			o.logger.Printf("x-mux busy: failed to parse params: %v", err)
			return
		}
	}
	if p.ID == "" {
		o.logger.Printf("x-mux busy: missing id, ignoring")
		return
	}

	startedAt := time.Time{}
	if p.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, p.StartedAt); err == nil {
			startedAt = t
		} else {
			o.logger.Printf("x-mux busy: invalid startedAt %q (%v), using now", p.StartedAt, err)
		}
	}
	const maxBusyDurationMs = int64(86_400_000) // 24 h — matches maxBusyDuration in owner.go
	if p.EstimatedDurationMs > maxBusyDurationMs {
		p.EstimatedDurationMs = maxBusyDurationMs
	}
	estimated := time.Duration(p.EstimatedDurationMs) * time.Millisecond

	o.RegisterBusy(p.ID, startedAt, estimated, p.Task, -1)
	o.logger.Printf(
		"x-mux busy: id=%s task=%q estimated=%s",
		p.ID, p.Task, estimated,
	)
}

// handleIdleNotification parses notifications/x-mux/idle and clears the
// matching busy declaration.
func (o *Owner) handleIdleNotification(raw []byte) {
	var env genericNotification
	if err := json.Unmarshal(raw, &env); err != nil {
		o.logger.Printf("x-mux idle: failed to parse envelope: %v", err)
		return
	}
	var p idleNotificationParams
	if len(env.Params) > 0 {
		if err := json.Unmarshal(env.Params, &p); err != nil {
			o.logger.Printf("x-mux idle: failed to parse params: %v", err)
			return
		}
	}
	if p.ID == "" {
		o.logger.Printf("x-mux idle: missing id, ignoring")
		return
	}
	o.ClearBusy(p.ID)
	o.logger.Printf("x-mux idle: id=%s", p.ID)
}
