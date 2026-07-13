# mcp-mux v0.27.0

**Release date:** 2026-07-13

**Type:** Non-breaking minor release

## Summary

v0.27.0 closes the process-lifecycle gap that could leave large numbers of
Serena and other MCP subprocess trees running after short-lived CLI workers had
finished. Reusable stateless owners remain reusable, stateful owners can remain
persistent, and disposable trees now converge to zero demand instead of living
forever.

## What changes for users

- A disposable mcp-mux shim safely suspends its daemon session after 10 minutes
  without host-originated traffic, waits 30 seconds for exact-owner demand, and
  then lets the stable launcher become dormant. The original host stdio remains
  available and wakes on the next request.
- An isolated server remains private. A returning consumer inside the grace
  window reconnects to the same owner generation by token; a fresh consumer
  cannot attach to it.
- When an owner is no longer needed, muxcore finalizes the complete subprocess
  tree, including descendants that inherited stdio or outlived their leader.
- Upstream crash and engine update continuity still preserve the MCP host
  transport. In-flight requests are not replayed: each already-sent request
  receives an explicit JSON-RPC error with its original ID.

The product defaults can be adjusted with:

- `MCPMUX_SHIM_IDLE_TIMEOUT`
- `MCPMUX_SHIM_DORMANT_GRACE`

Set a duration to zero or negative to disable the corresponding stage. Keep
`Persistent: true` or advertise `x-mux.persistent: true` for indexes,
background jobs, caches, or other state that must survive zero connected
sessions.

## Serena dashboard

The Serena dashboard is independent of mcp-mux lifecycle management:

- `--open-web-dashboard false` or
  `web_dashboard_open_on_launch: false` prevents automatic browser opening.
- `web_dashboard: false` disables the dashboard service.

Disabling the dashboard reduces UI noise, but v0.27.0 process-tree cleanup is
still required to prevent abandoned Serena helpers and descendants.

## muxcore consumers

Ordinary `engine.New` consumers require no source changes:

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.27.0
```

Direct low-level handoff consumers keep the existing three-argument
`daemon.NewFdTransferMsg` source contract. Protocol-v2 integrations should use
`daemon.NewFdTransferMsgWithStderr` and populate `HandoffUpstream.StderrFD` so
stderr ownership moves with stdin, stdout, and the tree authority.

Consumers should remove product-local stale-process sweeps, PID-only kills,
request replay, shim polling, launcher respawn loops, and handoff retry
protocols that duplicate muxcore lifecycle authority. Consumer-specific
customer-mode smoke remains required after the dependency bump.

## Upgrade and rollback

- The first restart from handoff protocol v1 to v2 rejects live authority
  transfer before detaching anything, then performs one bounded
  snapshot-backed restart.
- Later v2-to-v2 restarts transfer stdin, stdout, stderr, and process-tree authority
  transactionally.
- Rollback to a pre-v0.27 binary is supported through the same bounded
  cross-version snapshot restart. Do not force mixed-version live handoff.

## Release verification

The release candidate passed:

- root and muxcore tests, vet, build, and Windows race detector runs;
- full root and muxcore race detector runs under WSL/Linux;
- the five-step repository critical suite;
- eight parallel real host transports through initial start, launcher-only
  dormancy, wake, active-engine switch, and final zero-survivor cleanup;
- native SessionHandler update recovery with refresh-based reconnect and no
  fallback spawn or give-up.

See [the production testing playbook](docs/PRODUCTION-TESTING-PLAYBOOK.md) for
the lifecycle-convergence scenario and required evidence.

## Consumer handoff closeout

This release impacts `aimux` and `engram`. The final GitHub release closeout
must record `CONSUMER_HANDOFF_PASS` after `muxcore/v0.27.0` resolves and fresh
Engram adoption issues/comments for both consumers have been re-read. If that
cannot be completed, it must record `CONSUMER_HANDOFF_BLOCKED` and must not call
the full critical muxcore scope shipped. Source release notes deliberately do
not predeclare the post-tag result.
