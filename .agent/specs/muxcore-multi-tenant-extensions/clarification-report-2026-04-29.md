# Clarification Report — muxcore-multi-tenant-extensions

**Date:** 2026-04-29
**Mode:** `--auto` (recommendations applied without user prompts)
**Spec:** `.agent/specs/muxcore-multi-tenant-extensions/spec.md`

## Coverage by Category

| # | Category | Status |
|---|----------|--------|
| 1 | Functional Scope | Clear |
| 2 | User Roles | Clear |
| 3 | Domain/Data Model | **Resolved** (C1 — SessionMeta with embedded ConnInfo replaces ConnInfo.TenantID) |
| 4 | Data Lifecycle | Clear |
| 5 | Interaction & UX Flow | Clear |
| 6 | Non-Functional Perf/Scale | Clear |
| 7 | Non-Functional Reliability | Clear |
| 8 | Non-Functional Security | Clear |
| 9 | Integration | **Resolved** (C3 — aimux migration is documentation snippet only) |
| 10 | Edge Cases | Clear |
| 11 | Constraints & Tradeoffs | **Resolved** (C2 — `GetNamedPipeClientProcessId` via x/sys/windows OR NewLazySystemDLL fallback) |
| 12 | Terminology | Clear |
| 13 | Completion Signals | Clear |
| 14 | Misc | **Resolved** (3/3 [NEEDS CLARIFICATION] markers cleared) |

## Resolutions Tracking

| # | Category | Question | Resolution | Severity |
|---|----------|----------|------------|----------|
| C1 | Domain Model | TenantID placement | Option B — `SessionMeta` with embedded `ConnInfo`. ConnInfo = OS facts only. | HIGH |
| C2 | Constraints | `GetNamedPipeClientProcessId` import path | Try `x/sys/windows` first; fall back to `NewLazySystemDLL("kernel32.dll")` if absent. | LOW |
| C3 | Functional Scope | aimux migration acceptance | AGENTS.md migration snippet only; actual aimux PR is separate task. | MEDIUM |

## Spec Mutations Applied

- **Added:** `## Clarifications` section with tracking table (C1, C2, C3)
- **Updated FR-1:** ConnInfo + SessionMeta types defined; `NotificationHandlerWithSessionMeta` interface
- **Updated FR-2:** `SessionHandlerWithSessionMeta` interface
- **Updated FR-3:** AuthorizeSession constructs `SessionMeta{Conn, TenantID, AuthorizedAt}` on AuthAllow
- **Updated FR-5:** `GetNamedPipeClientProcessId` fallback path documented (C2)
- **Updated FR-6:** Cached `SessionMeta` (not just ConnInfo); single mutation on AuthAllow then immutable
- **Updated FR-7:** Backward compat references `*WithSessionMeta` interfaces
- **Updated US1:** Acceptance reads `meta.Conn.PeerPid`
- **Updated US2:** Acceptance includes `!meta.AuthorizedAt.IsZero()` discriminator
- **Updated EC-7:** `*WithSessionMeta` precedence over legacy
- **Replaced Open Questions section:** all 3 OQs marked RESOLVED with cross-reference to C1/C2/C3

## Architecture Mutations Applied

- **Updated ADR-004:** Revised from Option A (`ConnInfo.TenantID`) to Option C (`SessionMeta` with embedded `ConnInfo`). Full Go type signatures included. Discriminator semantics documented.
- **Updated Component Map:** `muxcore.ConnInfo` (OS only), `muxcore.SessionMeta` (new), `*WithSessionMeta` interfaces (renamed from `*WithConnInfo`), `Session.meta` cached field.
- **Updated Deployment:** Backward-compat references `*WithSessionMeta`.

## Pipeline Status

**Questions asked/answered:** 3/3 (all auto-applied)
**Spec status:** Ready for planning
**Forward target:** `nvmd-plan` (next pipeline step)

## Verification

- [x] All CRITICAL ambiguities resolved (1 HIGH + 1 MED + 1 LOW — no CRITICAL)
- [x] Coverage Report saved: `.agent/specs/muxcore-multi-tenant-extensions/clarification-report-2026-04-29.md`
- [x] spec.md updated with `## Clarifications` tracking table
- [x] No unresolved `[NEEDS CLARIFICATION]` markers at any severity

## Auto-Forward

`Skill("nvmd-platform:nvmd-plan", "muxcore-multi-tenant-extensions")` invoked next.
