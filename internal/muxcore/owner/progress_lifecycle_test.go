package owner

import (
	"testing"
)

// ---------------------------------------------------------------------------
// progressOwners lifecycle (FIX 1)
//
// ActiveProgressTokens() must return 0 after:
//   (a) the upstream response for the request arrives   → clearProgressTokensForRequest
//   (b) the client sends notifications/cancelled        → clearProgressTokensForRequest
//   (c) the owning session is removed                   → removeSession cleanup
// ---------------------------------------------------------------------------

const (
	testRequestID = `"req-remap-001"`
	testToken     = `"tok-abc"`
)

// seedProgressToken registers a single progress-token entry via the internal
// maps, bypassing the JSON-parsing path so the test stays fast and isolated.
func seedProgressToken(o *Owner, sessionID int, requestID, token string) {
	o.mu.Lock()
	o.progressOwners[token] = sessionID
	o.progressTokenRequestID[token] = requestID
	o.requestToTokens[requestID] = append(o.requestToTokens[requestID], token)
	o.mu.Unlock()
}

func TestProgressLifecycle_ResponseClearsToken(t *testing.T) {
	o := newMinimalOwner()
	seedProgressToken(o, 1, testRequestID, testToken)

	if got := o.ActiveProgressTokens(); got != 1 {
		t.Fatalf("want 1 active token before response, got %d", got)
	}

	o.clearProgressTokensForRequest(testRequestID)

	if got := o.ActiveProgressTokens(); got != 0 {
		t.Fatalf("want 0 active tokens after response, got %d", got)
	}
}

func TestProgressLifecycle_CancelledClearsToken(t *testing.T) {
	o := newMinimalOwner()
	seedProgressToken(o, 2, testRequestID, testToken)

	if got := o.ActiveProgressTokens(); got != 1 {
		t.Fatalf("want 1 active token before cancel, got %d", got)
	}

	o.clearProgressTokensForRequest(testRequestID)

	if got := o.ActiveProgressTokens(); got != 0 {
		t.Fatalf("want 0 active tokens after cancel, got %d", got)
	}
}

func TestProgressLifecycle_RemoveSessionClearsToken(t *testing.T) {
	o := newMinimalOwner()

	// Register a live session
	s := &Session{ID: 7}
	o.mu.Lock()
	o.sessions[s.ID] = s
	o.mu.Unlock()

	seedProgressToken(o, s.ID, testRequestID, testToken)

	if got := o.ActiveProgressTokens(); got != 1 {
		t.Fatalf("want 1 active token before session removal, got %d", got)
	}

	// removeSession must clear progress tokens for the dead session
	o.removeSession(s)

	if got := o.ActiveProgressTokens(); got != 0 {
		t.Fatalf("want 0 active tokens after removeSession, got %d", got)
	}
}

func TestProgressLifecycle_MultipleTokensSameRequest(t *testing.T) {
	o := newMinimalOwner()

	// One request ID, two progress tokens (edge-case: parallel streams)
	seedProgressToken(o, 1, testRequestID, `"tok-1"`)
	seedProgressToken(o, 1, testRequestID, `"tok-2"`)

	if got := o.ActiveProgressTokens(); got != 2 {
		t.Fatalf("want 2 active tokens, got %d", got)
	}

	o.clearProgressTokensForRequest(testRequestID)

	if got := o.ActiveProgressTokens(); got != 0 {
		t.Fatalf("want 0 active tokens after clearing request, got %d", got)
	}
}

func TestProgressLifecycle_IdempotentClear(t *testing.T) {
	o := newMinimalOwner()
	seedProgressToken(o, 1, testRequestID, testToken)

	o.clearProgressTokensForRequest(testRequestID)
	// Second call on unknown ID must not panic.
	o.clearProgressTokensForRequest(testRequestID)

	if got := o.ActiveProgressTokens(); got != 0 {
		t.Fatalf("want 0 active tokens, got %d", got)
	}
}
