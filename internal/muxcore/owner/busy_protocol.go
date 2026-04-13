package owner

import (
	"github.com/thebtf/mcp-mux/internal/muxcore/busy"
)

// handleBusyNotification parses notifications/x-mux/busy and registers a
// busy declaration on the owner. Called from handleUpstreamMessage when the
// method matches; the notification is consumed at the mux layer and not
// forwarded to downstream sessions.
func (o *Owner) handleBusyNotification(raw []byte) {
	p, err := busy.ParseBusyNotification(raw)
	if err != nil {
		o.logger.Printf("x-mux busy: %v", err)
		return
	}
	if p.EstimatedDuration == 0 {
		// Zero duration is valid from the parser (omitted field); the Owner
		// method will apply the 10-minute default.
	}
	o.RegisterBusy(p.ID, p.StartedAt, p.EstimatedDuration, p.Task, -1)
	o.logger.Printf(
		"x-mux busy: id=%s task=%q estimated=%s",
		p.ID, p.Task, p.EstimatedDuration,
	)
}

// handleIdleNotification parses notifications/x-mux/idle and clears the
// matching busy declaration.
func (o *Owner) handleIdleNotification(raw []byte) {
	p, err := busy.ParseIdleNotification(raw)
	if err != nil {
		o.logger.Printf("x-mux idle: %v", err)
		return
	}
	o.ClearBusy(p.ID)
	o.logger.Printf("x-mux idle: id=%s", p.ID)
}
