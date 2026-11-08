package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/session"
)

type sessionRoute struct {
	owner    *owner.Owner
	entry    *OwnerEntry
	identity ownerEntryIdentity
	session  *session.Session
}

type sessionNotifier struct{ daemon *Daemon }

// Injection is synchronous: a blocked callback remains owned by New, and a
// panic fails construction before any owner can dispatch.
func injectSessionNotifier(aware muxcore.SessionNotifierAware, notifier muxcore.SessionNotifier) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("daemon: SetSessionNotifier panic: %v", r)
		}
	}()
	aware.SetSessionNotifier(notifier)
	return nil
}

func (d *Daemon) registerSessionRoute(o *owner.Owner, s *session.Session) {
	id := s.Meta().SessionID
	d.mu.Lock()
	entry := d.owners[o.ServerID()]
	if entry != nil && entry.Owner == nil && entry.creating != nil {
		ready := entry.creating
		d.mu.Unlock()
		<-ready
		d.mu.Lock()
		entry = d.owners[o.ServerID()]
	}
	defer d.mu.Unlock()
	if id == "" || d.shuttingDown.Load() || s.IsClosed() || entry == nil || entry.Owner != o || entry.removalInProgress || entry.sessionRoutesRetired || entry.ProtocolEra != era.EraLegacy {
		return
	}
	d.sessionRoutes[id] = sessionRoute{owner: o, entry: entry, identity: captureOwnerEntryIdentity(entry), session: s}
}

func (d *Daemon) unregisterSessionRoute(o *owner.Owner, s *session.Session) {
	id := s.Meta().SessionID
	d.mu.Lock()
	defer d.mu.Unlock()
	if route, ok := d.sessionRoutes[id]; ok && route.owner == o && route.session == s {
		delete(d.sessionRoutes, id)
	}
}

func (n *sessionNotifier) NotifySession(id string, notification []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(notification, &fields); err != nil || fields == nil {
		return fmt.Errorf("invalid JSON-RPC notification")
	}
	var version, method string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" || json.Unmarshal(fields["method"], &method) != nil || method == "" {
		return fmt.Errorf("invalid JSON-RPC notification")
	}
	if _, ok := fields["id"]; ok {
		return fmt.Errorf("JSON-RPC notification has an id")
	}
	if _, ok := fields["result"]; ok {
		return fmt.Errorf("JSON-RPC notification has a result")
	}
	if _, ok := fields["error"]; ok {
		return fmt.Errorf("JSON-RPC notification has an error")
	}
	if params, ok := fields["params"]; ok {
		params = bytes.TrimSpace(params)
		if len(params) == 0 || (params[0] != '{' && params[0] != '[') {
			return fmt.Errorf("invalid JSON-RPC notification params")
		}
	}
	if bytes.ContainsAny(notification, "\r\n") {
		var compact bytes.Buffer
		if err := json.Compact(&compact, notification); err != nil {
			return fmt.Errorf("invalid JSON-RPC notification: %w", err)
		}
		notification = compact.Bytes()
	}
	d := n.daemon
	d.mu.RLock()
	defer d.mu.RUnlock()
	route, ok := d.sessionRoutes[id]
	if !ok || d.shuttingDown.Load() || d.owners[route.identity.serverID] != route.entry || route.entry.Owner != route.owner || !route.identity.matches(route.entry) || route.entry.removalInProgress || route.entry.sessionRoutesRetired || route.entry.ProtocolEra != era.EraLegacy {
		return fmt.Errorf("session unavailable")
	}
	return route.session.EnqueueNotification(notification)
}
