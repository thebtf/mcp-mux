# mcp-mux v0.27.1

**Release date:** 2026-07-14

**Type:** Non-breaking patch release

## Summary

Persistent daemon/owner state and downstream transport lifetime are now
separate. Persistent products such as Aimux and Engram retain their transport
by default because MCP permits background notifications. They may opt into
`AllowPersistentIdleSuspend` only after proving no-background-events or a
buffer/replay policy; that opt-in still requires muxcore's exact-owner daemon
gate. Ordinary `engine.New` users remain source-compatible and receive that
gate automatically when they enable `IdleSuspendDelay`.

The installed stable launcher and active versioned engine are distinct binaries
and may differ byte-for-byte. Private dormant frames require protocol-v2
target-bound attestation over a one-shot current-user-only local IPC endpoint.
The engine verifies that the endpoint server PID is its direct parent and keeps
the proof bytes separate from host stdio. Provider-derived version-store layout,
active-engine pointer, and direct-parent executable path checks remain
mandatory; custom, copied, or environment-forwarded engine paths fail closed.
A verified engine can update the stable launcher only for future invocations
through the existing two-rename swap. An already-running v0.27.0-or-older
launcher lacks the v2 attestation server and needs one explicit host/session
restart (or exact scoped maintenance cleanup). A silent host has no MCP
completion signal, so full dormant exit is the explicit
`MCPMUX_LAUNCHER_DORMANT_LEASE` opt-in, not a default promise.

v0.27.1 fixes a control-plane retry herd introduced by the v0.27.0 idle-shim
lifecycle. During rolling coexistence, a new shim could ask a v0.26.13 daemon
whether it was safe to suspend. The old daemon correctly replied
`unknown command: can_suspend`, but the shim treated that permanent response as
transient and opened another named-pipe control connection every five seconds.
Hundreds of retained Desktop/CLI-worker transports could therefore consume
multiple daemon CPU cores even though their upstream owners were idle.

## What changes for users

- Unsupported, unknown-token, owner-gone, persistent-owner, malformed, and
  other non-transient gate outcomes are one-shot and fail closed. The shim keeps
  its existing daemon IPC session connected and stops polling.
- Actual transport failure and daemon shutdown remain recoverable. Retries use
  capped, per-token jittered exponential backoff instead of a synchronized fixed
  cadence.
- Busy, pending-request, and active-progress denials remain recheckable because
  those conditions can clear safely.
- Product shims include the spawn-returned owner ID in the gate request. The
  daemon checks that owner's token history directly instead of scanning every
  owner. A forged owner ID or a stale token after same-ID owner recreation is
  rejected.

The lifecycle behavior shipped in v0.27.0 remains unchanged: disposable shims
can become dormant, exact-owner wake remains available, persistent owners retain
transport by default, and full subprocess trees are finalized when their owner
is removed.

## Upgrade

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.27.1
```

Ordinary `engine.New` consumers need no source changes. Existing v0.27.0 shim
engines must enter the v0.27.1 binary generation before the retry fix applies.
The active engine can bootstrap the stable launcher for future invocations, but
an already-running v0.27.0-or-older launcher cannot acquire the target-bound v2
attestation server in memory; restart that host/session before expecting
launcher dormancy. Do not add product-local retry loops, token indexes, PID-only
cleanup, or stale process sweeps.

## Serena dashboard

Serena dashboard policy is independent of mux process lifecycle:

- `--open-web-dashboard false` or
  `web_dashboard_open_on_launch: false` prevents automatic browser opening but
  leaves the dashboard available.
- `web_dashboard: false` disables the dashboard service.

## Verification

Before release, the final candidate SHA must pass root and muxcore full suites,
`go vet`, focused Windows races, Linux races, cross-platform builds, and the
Windows lifecycle smoke. Earlier Windows artifacts are useful regression
evidence only; they do not certify a later repaired SHA.

GitHub issue [#138](https://github.com/thebtf/mcp-mux/issues/138) tracks the
regression and release proof.

## Consumer handoff closeout

This release affects every muxcore consumer that adopted v0.27.0 lifecycle
behavior. The final release closeout must update and re-read the existing aimux
and engram adoption issues with `muxcore/v0.27.1`, compatibility notes, and
customer-mode verification. If that cannot be completed, record
`CONSUMER_HANDOFF_BLOCKED` and do not call the full critical muxcore scope
shipped.
