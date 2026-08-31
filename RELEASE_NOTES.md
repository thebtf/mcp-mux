# mcp-mux v0.30.0

**Release date:** 2026-08-31

**Type:** Additive, opt-in minor release

## Summary

R1 adds `--mcp-protocol=2026-07-28` for a known MCP `2026-07-28` host and a
same-era upstream. The selected route opens one dedicated native modern owner
and forwards the opening request unchanged. It is a safety-contained boundary,
not a legacy-to-modern gateway, a sharing release, or an automatic upgrade.

## Explicit modern route

The modern route is available only when a host explicitly selects
`--mcp-protocol=2026-07-28` before its opening request. It requires the pinned
modern request metadata and an exact-era admission result; it does not probe,
infer, or fall back to legacy.

- A valid opening request reaches the same-era upstream without mux-authored
  legacy initialization, discovery, list, cache, template, or replay traffic.
- Modern owners are forced isolated: they serve the selected downstream route
  and are never reused as a shared owner.
- Upstream JSON-RPC requests are contained rather than forwarded to a host.
  Eligible standard logs are forwarded only on the opted-in request path to
  its sole downstream recipient; mcp-mux neither synthesizes nor broadcasts
  them.

## Compatibility and migration

Existing installations need no configuration or source changes. Legacy remains
the default, and its observable behavior and owner identity remain unchanged.

To adopt R1, use the explicit selector only for a known modern host and
same-era upstream. Treat a refused, absent, or mismatched era confirmation as
a failed modern admission, not as permission to retry the route as legacy.
R1 deliberately provides no automatic probing, fallback, semantic protocol
translation, modern sharing, or persisted modern handoff.

## Operational readback

Existing owner, daemon, list, CLI, and `mux_list` views that project
`OwnerInfo` identify an active R1 owner with four redacted policy facts:

- `protocol_era=2026-07-28`
- `sharing_policy=forced-isolated`
- `cache_policy=off`
- `lifecycle_policy=r1-quarantine`

Existing readiness information remains available where it already exists. The
R1 readback does not expose request content, credentials, opaque state, or
linkable compatibility material.

## Lifecycle safety

R1 keeps process-generation and `RetirementProven` authority in the existing
owner lifecycle. A modern owner may continue only with its exact selected era;
an unsafe snapshot, handoff, reaper, zero-session, retry, respawn, loss, or
reconnect transition drains, cold-starts, or fails closed instead of becoming
legacy.

After upstream or daemon loss, unfinished modern requests, progress,
subscriptions, and replay state are not restored or replayed. A host uses a
fresh exact-era admission and issues a new retry or listen request when it is
ready to resume work.

## Validation scope

Validation covers the 100-frame native opening corpus, same-era byte
preservation, forced isolation, minimal redacted readback, legacy parity,
lifecycle quarantine and loss behavior, rollback, Windows and Unix customer
proof, full Go test and vet suites, and the repository critical suite.

## Rollback

Rollback stops new explicit-modern admissions and drains or removes modern
owners through R1 quarantine. It never downgrades live modern work to legacy,
hands it to a legacy owner, or replays unfinished work. If a product or
dependency rollback is required, restore the prior compatible revision after
the bounded modern-owner retirement path; do not force a mixed-era live
handoff.

---

# mcp-mux v0.29.1

**Release date:** 2026-07-19

**Type:** Backward-compatible patch release

## Summary

v0.29.1 adds a public, provider-generic start fallback helper to
`muxcore/supervisor` and makes daemon registry mutations exact-generation
transactions. These changes tighten lifecycle authority without changing the
ordinary `engine.New` path.

## `supervisor.StartWithFallback`

`supervisor.StartWithFallback` starts the requested engine first and tries a
distinct fallback only when that attempt fails cleanly with neither child nor
admission authority.

- If a failed attempt retains child or admission authority, the helper returns
  that authority to `supervisor.Run` for finalization instead of starting a
  second generation.
- `ErrStartRollbackUnproven` is terminal even when only admission cleanup is
  available: closing admission is not proof that a process tree was retired.
  The supervisor therefore remains fail-closed rather than overlapping
  authorities.
- Cancellation is preserved. A canceled requested attempt does not start a
  fallback; cancellation from a fallback attempt is also returned, with any
  retained authority still available for supervisor finalization.
- Returned error classifications do not expose product engine identities.

## Exact-generation daemon registry updates

Owner-originated persistence, template-cache, zero-session, and upstream-exit
callbacks now update the daemon registry through one daemon-owned transaction.
The transaction applies only when the originating owner is still the current
registry generation for its server ID. Stale generations are no-ops, and
process-generation authority remains in `muxcore/owner`.

## Compatibility

This patch is backward compatible for ordinary `engine.New` consumers and for
existing supervisor users. Products that need requested/fallback start policy
can adopt `supervisor.StartWithFallback`; they should continue to keep product
engine selection and policy outside muxcore.

## Upgrade

After the tags resolve, upgrade muxcore consumers with:

```bash
go get github.com/thebtf/mcp-mux/muxcore@v0.29.1
```

For the product binary, use the versioned-engine upgrade path:

```powershell
.\mcp-mux.exe upgrade --restart
```

## Rollback

To roll back this patch, pin `muxcore/v0.29.0` or restore the previous product
binary. Do not force a mixed-version live handoff; use the product's bounded
replacement path.

## Verification scope

The release verification scope covers focused supervisor fallback behavior
(clean failure, retained authority, rollback-unproven, and cancellation),
exact-generation daemon registry mutation behavior, and the relevant public
API and lifecycle regression coverage. These notes prepare the release; they
do not claim a final tag or publication.

## Post-publication consumer handoff

After publication, Aimux and Engram require fresh handoff against the exact
released version, including their module-resolution, provider commit, and
consumer verification evidence. Their adoption follows publication and is not
represented as completed by these notes.
