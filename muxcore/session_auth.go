package muxcore

// AuthDecision is the verdict returned by engine.Config.AuthorizeSession.
// Numeric ordering is part of the public API: AuthAllow == 0 keeps the
// zero-value SessionAuth in the "allow" state, but Owner only treats a
// SessionAuth as "ran successfully" when it has been returned by an
// AuthorizeSession callback — the discriminator on SessionMeta lives in
// SessionMeta.AuthorizedAt, not here.
type AuthDecision int

const (
	// AuthAllow lets the session proceed to AddSession + dispatch. TenantID
	// (free-form, may be empty) is recorded on SessionMeta; AuthorizedAt is
	// stamped to time.Now().
	AuthAllow AuthDecision = 0

	// AuthDeny closes the connection with a JSON-RPC -32000 error carrying
	// SessionAuth.Reason as the message before any upstream spawn or
	// session-table insertion. TenantID is ignored on AuthDeny.
	AuthDeny AuthDecision = 1
)

// SessionAuth is the verdict structure returned by an AuthorizeSession
// callback. Decision is mandatory; TenantID and Reason are optional and
// carry consumer-defined meaning.
//
//   - On AuthAllow: TenantID is copied to SessionMeta.TenantID. An empty
//     TenantID is legitimate (FR-3 amendment CHK013) — it means "authorized
//     but no tenant assignment" and is distinguished from "AuthorizeSession
//     not configured" by SessionMeta.IsAuthorized() returning true. Reason
//     is unused on AuthAllow.
//
//   - On AuthDeny: Reason is sent to the client as the message field of a
//     JSON-RPC -32000 error before the connection is closed. TenantID is
//     ignored. An empty Reason produces a generic error message; consumers
//     are encouraged to set a stable machine-readable reason (e.g.
//     "tenant_not_enrolled") so downstream operators can categorise denials.
type SessionAuth struct {
	Decision AuthDecision
	TenantID string
	Reason   string
}
