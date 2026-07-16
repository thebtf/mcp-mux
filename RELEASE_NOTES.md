# mcp-mux v0.27.2

**Release date:** 2026-07-17

**Type:** Non-breaking patch release

## Summary

v0.27.2 fixes a process-authority race for snapshot/template owners. A restored
owner can begin materializing its cached upstream in the background while its
first uncached request arrives. Before this patch, the request path could start
a second respawn before the background start published readiness. Both paths
could then install different processes into the same owner slot, leaving the
losing upstream generation outside normal finalization.

The request path now joins an already-pending template background start before
deciding whether respawn is necessary. The wait uses the existing bounded
upstream readiness timeout and owner shutdown signal. If the completed
background start produced a writable upstream, the request uses it; otherwise
the existing request-respawn path remains available.

This is the conservative race repair. It does not change when template owners
are eagerly materialized; demand-driven upstream materialization remains a
separate architecture change and release track.

## What changes for users

- Snapshot/template background startup and first-request recovery no longer
  create competing upstream generations for one owner.
- Windows source-checkout upstreams no longer receive duplicate concurrent
  launches from this race, avoiding locked entrypoint replacement failures and
  the associated crash/respawn storm.
- Timeout and shutdown remain explicit request errors. The fix does not add
  unbounded waits or replay already-sent requests.
- Ordinary `engine.New` and direct muxcore consumers require no source changes.

## Upgrade

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.27.2
```

Consumers should remove or quarantine any workaround added specifically for
duplicate background/request starts. Do not add product-local spawn locks,
file-replacement retries, PID sweeps, stale-process kill loops, or parallel
lifecycle mechanisms; they compete with muxcore's Job Object/process-group
authority and make failures harder to attribute.

## Verification

The release candidate includes the deterministic
`TestSnapshotBackgroundSpawnBlocksRequestRespawn` regression. The deployed
repair completed a `32h 18m` post-cutover Windows soak with zero actual
runtime locked-entrypoint errors and zero request-triggered competing-respawn
markers. The same daemon generation stayed live; unrelated network-failure
respawns are classified separately in
`.agent/reports/2026-07-17-v0.27.2-soak.md`.

Release closeout still requires the current root and muxcore suites, `go vet`,
the repository critical suite, applicable native-consumer and lifecycle
playbook evidence, independent review, remote tag parity, published artifact
verification, and public muxcore module resolution.

## Consumer handoff closeout

This release affects every muxcore consumer that can restore snapshot/template
owners and accept requests while background startup is pending. After
`muxcore/v0.27.2` resolves, release closeout must update and re-read the Aimux
and Engram adoption issues with the tagged version, the single-authority
invariant, forbidden local workarounds, customer-mode smoke expectations,
rollback notes, and provider evidence. If that cannot be completed, record
`CONSUMER_HANDOFF_BLOCKED` and do not call the full critical muxcore scope
shipped.

## Rollback

Consumers can pin `muxcore/v0.27.1` or restore the previous product binary. That
rollback reintroduces the snapshot/template background-start race. Do not mask
it with consumer-local retry or process cleanup loops; prefer upgrading again
to v0.27.2 after diagnosing the rollback reason.
