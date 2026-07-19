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
