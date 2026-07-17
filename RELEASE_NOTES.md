# mcp-mux v0.28.0

**Release date:** 2026-07-17

**Type:** Backward-compatible feature release

## Summary

v0.28.0 adds demand-driven upstream materialization for compatible
template-backed owners. After an upstream has published a compatible discovery
template, a later owner can answer cached `initialize`, `tools/list`, and
captured discovery requests without starting an upstream process. The first
uncached request materializes exactly one upstream generation, completes the
MCP initialization handshake, and forwards the original JSON-RPC request on the
same open transport.

Template reuse is fail-closed: authorization requires the full SHA-256 identity
of the effective security-relevant environment and, for isolated templates, the
exact canonical working directory. Missing, incompatible, or repeatedly racing
templates take one bounded cold/eager path rather than replaying discovery from
the wrong context.

## Lifecycle and restart safety

An installed generation remains authoritative during retirement until both
`Process.Done` and process-group or Windows Job authority retirement are proven.
If that proof is unavailable, muxcore reports `FINALIZE_BLOCKED`, retries the
same generation, and does not admit an overlapping replacement.

Graceful-restart negotiation remains pre-detach: listener, spawn, accept,
version, or token failures leave the predecessor serving. A later receipt or
final-ack failure must prove the failed successor exited, finalize the detached
generation, rewrite the pinned snapshot, and pre-start exactly one clean
snapshot successor before predecessor shutdown.

## Upgrade

Upgrade muxcore consumers to the released tag:

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.28.0
```

For the product binary, use the documented versioned-engine upgrade path:

```powershell
.\mcp-mux.exe upgrade --restart
```

Ordinary `engine.New` consumers require no source changes. Do not add
consumer-local spawn locks, retry loops, PID sweeps, stale-process kill loops,
or parallel lifecycle mechanisms; they undermine muxcore's single
process-tree-authority contract.

## Compatibility and rollback

v0.28.0 is backward compatible for ordinary `engine.New` consumers. Persistent
owners and callers that explicitly require eager startup continue to
materialize without waiting for a request.

To roll back, pin `muxcore/v0.27.2` or restore the previous product binary.
That removes cache-only startup and demand-driven materialization; it does not
change the v0.27.2 template background-spawn ownership fix. Do not compensate
with consumer-local process cleanup or retry mechanisms; diagnose the rollback
reason and upgrade again when resolved.

## Verification

Release verification covers cache-only discovery replay, first-demand
materialization on the original transport, exact environment and isolated-CWD
template authorization, bounded revision mismatch handling, failure cleanup,
and process-tree retirement. It also covers pre-detach restart aborts,
post-detach single-successor fallback, and transactional snapshot activation.

Before release closeout, run the current root and muxcore suites, `go vet`, the
repository critical suite, applicable native-consumer and lifecycle-playbook
evidence, independent review, published artifact/tag parity checks, and public
muxcore module resolution.

## Consumer handoff

This release affects muxcore consumers that restore owners, use discovery
caches, or manage daemon restart. After `muxcore/v0.28.0` resolves, update and
re-read the Aimux and Engram adoption issues with the tagged version, cache-only
startup behavior, single-authority invariant, forbidden local workarounds,
customer-mode smoke expectations, rollback notes, and provider evidence. If
that cannot be completed, record `CONSUMER_HANDOFF_BLOCKED` and do not call the
full critical muxcore scope shipped.
