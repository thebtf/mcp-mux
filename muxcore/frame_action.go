package muxcore

// FrameAction is the verdict returned by engine.Config.OnFrameReceived for
// each inbound frame. Numeric ordering is intentional: FramePass == 0 keeps
// the zero-value FrameAction in the "transparent" state, matching the
// fail-open semantics on timeout/panic (NFR-2 / FR-4).
type FrameAction int

const (
	// FramePass dispatches the frame normally. Default behaviour and the
	// zero value — produced by a no-op callback, by a callback that returns
	// the zero value explicitly, by the 1 ms timeout fallback, and by the
	// panic-recovered fallback. Consumer policy MUST tolerate FramePass
	// being chosen for them in any of those degraded paths.
	FramePass FrameAction = 0

	// FrameDrop silently discards the frame. No dispatch happens, no JSON-RPC
	// error response is written to the client; the client observes only the
	// absence of a response (matched against its own request ID timeout).
	// Use for hostile or rate-limited tenants where a response would be
	// pure protocol noise.
	FrameDrop FrameAction = 1

	// FrameError responds with a JSON-RPC -32004 ('rate limited') error
	// preserving the request's id, then skips dispatch. The client receives
	// a structured failure it can correlate with the originating request.
	// Use when the caller deserves an explanation — interactive UIs, paying
	// tenants near quota, or anywhere absence of response would degrade UX.
	FrameError FrameAction = 2
)
