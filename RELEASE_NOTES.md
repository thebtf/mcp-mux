# mcp-mux v0.29.0

**Release date:** 2026-07-19

**Type:** Backward-compatible feature release

## Summary

v0.29.0 makes the stable-stdio lifecycle boundary reusable. The new public
`muxcore/supervisor` package keeps one MCP host transport attached while a
product starts, parks, wakes, crashes, or replaces a child engine generation.
The `mcp-mux` stable launcher now uses this package for generic transport,
protocol, replay, correlation, and process-tree mechanics while retaining its
product-specific active-engine authorization, version-store, fallback, update,
shared-daemon, and operator policy.

Ordinary `engine.New` consumers require no source changes. Products that need a
stable host transport around replaceable engines can adopt the new public
packages instead of copying the `mcp-mux` launcher protocol or adapter.

## Public muxcore API

- `supervisor.Run` and `supervisor.Config` own one host input/output pair and a
  sequence of product-selected child generations.
- `supervisor.StartCommand` supplies full Unix process-group or Windows Job
  Object authority so the prior child tree is retired before a successor
  starts.
- `supervisor.ProtocolV2()` exposes the supported private lifecycle protocol
  version without exporting product-private method strings or exit codes.
- `supervisor/attest` supplies `StartParent`, `BindChildPID`, and
  `VerifyParent` for one-shot, generation-bound direct-parent and exact-child-
  PID proof on Windows, Linux, and Darwin. Unsupported platforms fail closed
  for private lifecycle control while ordinary MCP supervision remains usable.

The product still owns executable and installed-layout authorization, active
version and fallback selection, command/args/cwd/environment construction,
bootstrap/update policy, shared-daemon placement, and operator exit behavior.

## Transport and lifecycle guarantees

The supervisor provides one serialized host writer and admits at most one live
child process-tree authority. Count-and-byte-bounded FIFO protects first
delivery while a child starts, replays, finalizes, quiesces, or is dormant.
Only the cached `initialize` / `notifications/initialized` handshake is
replayed; arbitrary requests are never replayed. Lifecycle-permitted pings and
server logging prelude remain correlated and ordered.

Delivered requests lost across a generation change receive explicit JSON-RPC
errors with their original IDs. Request, cancellation, progress-token, and MCP
Tasks state is generation fenced, including immutable finalized task status in
the presence of delayed `tasks/get`, `tasks/result`, cancel, or status traffic.
Replacement discovery list-change notifications are emitted only when the
initialize capabilities already visible to the host authorize them.

When `StartCommand` is used, successor start waits for complete process-group
or Job Object retirement. A start path that cannot prove rollback of partial
authority fails closed instead of admitting an overlapping generation.

## mcp-mux product integration

The installed stable launcher owns the host stdio transport and prepares the
shared daemon outside each replaceable child authority. Launcher-only dormancy,
wake-on-demand, and installed active-engine switches therefore preserve the
original host transport. A marked engine generation does not create the shared
daemon inside its own process tree.

Private dormant control is enabled only after protocol-v2 bilateral
attestation succeeds for the exact child generation. Rolling combinations are
fail-closed: a new supervisor runs an old engine as ordinary MCP, an old
launcher runs a new engine without private dormancy, and a colliding private
method from an unattested child is suppressed rather than committed or
restarted in a loop.

## Upgrade

After the tags resolve, upgrade muxcore consumers with:

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.29.0
```

For the product binary, use the versioned-engine upgrade path:

```powershell
.\mcp-mux.exe upgrade --restart
```

Specialized consumers should integrate `supervisor.Run`,
`supervisor.StartCommand`, `supervisor.ProtocolV2`, and
`supervisor/attest`. Do not copy the `mcp-mux` private method strings, exit
code, parser, replay loop, attestation exchange, or launcher adapter.

## Compatibility and rollback

v0.29.0 is backward compatible for ordinary `engine.New` consumers. To roll
back, pin `muxcore/v0.28.0` or restore the previous product binary. Do not
force a mixed-version live supervisor handoff or forward an old attestation
advertisement; run without private dormancy and restart through the product's
bounded replacement path.

## Verification

Release verification covers root and muxcore suites, `go vet`, the supervisor
race suite, strict replay/correlation/Tasks regressions, public API compile
tests, attestation on supported platforms plus unsupported-platform fail-
closed behavior, and the repository critical lifecycle suite. Product smoke
evidence includes parallel host transports reaching launcher-only dormancy,
waking on demand, and surviving an installed active-engine switch on the same
stdio pipes.

## Consumer handoff

This is a consumer-impacting muxcore release for products that adopt stable
stdio supervision or private dormant control. After `muxcore/v0.29.0` resolves,
update and re-read the Aimux and Engram adoption issues with the released tag,
public API boundary, single-authority and replay invariants, forbidden private-
protocol copies, required customer-mode smoke evidence, rollback notes, and
provider commit/test/module-resolution proof. If that cannot be completed,
record `CONSUMER_HANDOFF_BLOCKED` and do not claim the full critical muxcore scope
shipped.
