// Package busy implements the x-mux busy/idle notification protocol.
//
// Upstream MCP servers send notifications/x-mux/busy to signal that they are
// executing long-running background work not represented by an active JSON-RPC
// request or progress token. The mux reaper must not evict an owner while any
// non-expired busy declaration exists. Upstream clears the declaration with
// notifications/x-mux/idle{id=<same id>}.
package busy

import (
	"encoding/json"
	"errors"
	"time"
)

// errMissingID is returned by parse functions when the required id field is absent.
var errMissingID = errors.New("busy: missing required id field")

// MaxBusyDurationMs is the hard cap on estimatedDurationMs accepted from
// upstream notifications. Matches the maxBusyDuration constant in owner.go.
const MaxBusyDurationMs = int64(86_400_000) // 24 hours

// Declaration is a signal from upstream that it is doing long-running work
// without an active JSON-RPC request or progress token. Used by the reaper
// to exempt owners from idle-based eviction.
type Declaration struct {
	StartedAt     time.Time
	EstimatedEnd  time.Time // StartedAt + estimatedDuration
	HardExpiresAt time.Time // safety cap: StartedAt + estimatedDuration * 2
	Task          string
	SessionID     int // session that declared the work (-1 if unattributed)
}

// BusyParams is the parsed payload of a notifications/x-mux/busy notification.
type BusyParams struct {
	ID                  string
	StartedAt           time.Time
	EstimatedDuration   time.Duration
	Task                string
}

// IdleParams is the parsed payload of a notifications/x-mux/idle notification.
type IdleParams struct {
	ID string
}

// busyNotificationParams is the raw JSON shape for notifications/x-mux/busy.
type busyNotificationParams struct {
	ID                  string `json:"id"`
	StartedAt           string `json:"startedAt,omitempty"`           // RFC3339
	EstimatedDurationMs int64  `json:"estimatedDurationMs,omitempty"` // optional
	Task                string `json:"task,omitempty"`
}

// idleNotificationParams is the raw JSON shape for notifications/x-mux/idle.
type idleNotificationParams struct {
	ID string `json:"id"`
}

// genericNotification is used to extract the params field from any notification.
type genericNotification struct {
	Params json.RawMessage `json:"params"`
}

// ParseBusyNotification parses the raw bytes of a notifications/x-mux/busy
// message. Returns the parsed BusyParams and nil on success, or a non-nil
// error if the JSON is malformed or the id field is missing.
func ParseBusyNotification(raw []byte) (BusyParams, error) {
	var env genericNotification
	if err := json.Unmarshal(raw, &env); err != nil {
		return BusyParams{}, err
	}
	var p busyNotificationParams
	if len(env.Params) > 0 {
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return BusyParams{}, err
		}
	}
	if p.ID == "" {
		return BusyParams{}, errMissingID
	}

	startedAt := time.Time{}
	if p.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, p.StartedAt); err == nil {
			startedAt = t
		}
		// Callers that need to log a parse warning can detect startedAt.IsZero()
		// after a non-empty input string.
	}

	if p.EstimatedDurationMs > MaxBusyDurationMs {
		p.EstimatedDurationMs = MaxBusyDurationMs
	}
	estimated := time.Duration(p.EstimatedDurationMs) * time.Millisecond

	return BusyParams{
		ID:                p.ID,
		StartedAt:         startedAt,
		EstimatedDuration: estimated,
		Task:              p.Task,
	}, nil
}

// ParseIdleNotification parses the raw bytes of a notifications/x-mux/idle
// message. Returns the parsed IdleParams and nil on success, or a non-nil
// error if the JSON is malformed or the id field is missing.
func ParseIdleNotification(raw []byte) (IdleParams, error) {
	var env genericNotification
	if err := json.Unmarshal(raw, &env); err != nil {
		return IdleParams{}, err
	}
	var p idleNotificationParams
	if len(env.Params) > 0 {
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return IdleParams{}, err
		}
	}
	if p.ID == "" {
		return IdleParams{}, errMissingID
	}
	return IdleParams{ID: p.ID}, nil
}
