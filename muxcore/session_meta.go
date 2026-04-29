package muxcore

import "time"

// SessionMeta combines OS-derived peer identity (Conn) with consumer-policy
// metadata produced by the optional engine.Config.AuthorizeSession callback.
// Owner caches one SessionMeta per session at accept time and passes it
// unchanged to every dispatch via the *WithSessionMeta interface upgrades.
//
// Discriminator semantics (CHK013 / EC-12):
//   - AuthorizedAt.IsZero() == true   → AuthorizeSession callback was not
//     configured for this engine; TenantID is also empty by construction.
//   - AuthorizedAt.IsZero() == false  → callback ran and returned AuthAllow.
//     TenantID may legitimately be empty: an AuthAllow with empty TenantID
//     means "authorized but no tenant assignment" and is distinct from
//     "unauthorized / no callback".
//
// Use IsAuthorized() to distinguish the two cases without comparing strings.
type SessionMeta struct {
	// Conn carries OS-level peer identity captured at accept time.
	Conn ConnInfo

	// TenantID is the consumer-defined identifier returned by AuthorizeSession.
	// Empty when AuthorizeSession is not configured OR when the callback
	// returned AuthAllow with an empty TenantID (legitimate per FR-3).
	TenantID string

	// AuthorizedAt is the wall-clock time AuthorizeSession returned AuthAllow.
	// Zero when AuthorizeSession is not configured or returned AuthDeny
	// (in the latter case the session is closed before SessionMeta is dispatched).
	AuthorizedAt time.Time
}

// IsAuthorized reports whether the AuthorizeSession callback produced an
// AuthAllow verdict for this session. Returns false when the callback was
// not configured. Empty TenantID with non-zero AuthorizedAt still returns
// true — that is a legitimate AuthAllow without tenant assignment.
func (sm SessionMeta) IsAuthorized() bool {
	return !sm.AuthorizedAt.IsZero()
}
