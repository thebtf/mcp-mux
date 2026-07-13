# mcp-mux v0.27.1

**Release date:** 2026-07-14

**Type:** Non-breaking patch release

## Summary

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
can become dormant, exact-owner wake remains available, persistent owners stay
alive, and full subprocess trees are finalized when their owner is removed.

## Upgrade

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.27.1
```

Ordinary `engine.New` consumers need no source changes. Existing v0.27.0 shim
engines must enter the v0.27.1 binary generation before the retry fix applies;
the stable launcher and active-engine update path perform this replacement.
Do not add product-local retry loops, token indexes, PID-only cleanup, or stale
process sweeps.

## Serena dashboard

Serena dashboard policy is independent of mux process lifecycle:

- `--open-web-dashboard false` or
  `web_dashboard_open_on_launch: false` prevents automatic browser opening but
  leaves the dashboard available.
- `web_dashboard: false` disables the dashboard service.

## Verification

The candidate passed:

- root and muxcore full suites, `go vet`, and focused Windows race suites with
  isolated build/temp caches;
- full root and muxcore race suites under WSL/Linux;
- a real v0.26.13-daemon/v0.27.1-shim smoke that observed one permanent gate
  probe, no later retry across 12 seconds, a successful request over the same
  host stdio afterward, and zero run-scoped survivors;
- the Windows eight-session lifecycle smoke through initial start,
  launcher-only dormancy, wake, active-engine switch, and final cleanup: 8/8
  responses in all three phases, three launcher-only convergences, and zero
  stale descendants or survivors.

GitHub issue [#138](https://github.com/thebtf/mcp-mux/issues/138) tracks the
regression and release proof.

## Consumer handoff closeout

This release affects every muxcore consumer that adopted v0.27.0 lifecycle
behavior. The final release closeout must update and re-read the existing aimux
and engram adoption issues with `muxcore/v0.27.1`, compatibility notes, and
customer-mode verification. If that cannot be completed, record
`CONSUMER_HANDOFF_BLOCKED` and do not call the full critical muxcore scope
shipped.
