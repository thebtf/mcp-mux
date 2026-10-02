# Plan Quality Checklist: Upstream maintenance hold

**Purpose**: Review accepted-design completeness before task generation.
**Created**: 2026-10-03
**Feature**: [plan.md](../plan.md)
**Review ownership**: SpecKit local design review; root retains acceptance. Checked means requirements/design quality, never implemented or released.

## Accepted architecture

- [x] CHK001 F1 finite admitted context set includes exact CWD, era, namespace/endpoint, argv boundaries, and strict full environment identity, without nonce/version/retry suffix or wildcard.
- [x] CHK002 F2 shared start gate begins before owner locks and ends after all nonnil Process authority installation, including failed starts and placeholders.
- [x] CHK003 F3 actual request drain begins once at durable commitment; HELD requires dead trees, no detach, durable state, and usable TTL.
- [x] CHK004 F4 persistent atomic ledger loads before listener/restore/start independently of SkipSnapshot; incomplete/invalid authority stays fail closed.
- [x] CHK005 Controlled restart/handoff/shutdown/downgrade and idle daemon exit refuse while fenced; update/launcher fallbacks cannot bypass refusal.
- [x] CHK006 F5 typed engine/CLI adapters and ingress-to-connected disposition prevent held queue replay and silent request loss.

## Implementation readiness

- [x] CHK007 `contracts/maintenance.md` freezes shared additive fields, optional interfaces, methods, result names, error codes/sentinels, CLI/MCP inputs, and local held error.
- [x] CHK008 Core contract commit is prerequisite for disjoint CLI/MCP adapters; source ownership is explicit.
- [x] CHK009 Scope preserves modern same-era quarantine and documents unsupported old-daemon, old-shim, standalone, and arbitrary-old-binary boundaries.
- [x] CHK010 Public readback is safe; durable records exclude raw env/commands/payloads and private matching material is not emitted.
- [x] CHK011 Quickstart records runnable planned runner commands, exact live executable replacement, Windows/Unix trees, original IDs, unchanged pipes, new version, and every SC mapping.
- [x] CHK012 Root integration/release commands and consumer handoff/fresh-delivery requirements are explicit; this child runs none of them.
- [x] CHK013 No unresolved clarification markers, guessed architecture mechanisms, implementation-complete checkboxes, or new dependency/supervisor remain.
- [x] CHK014 Constitution gates are checked before and after design without an exception.

## Notes

- Local review iteration 1 passed all 14 checks after root's accepted ADR-015 correction message.
- No independent root acceptance or runtime verification is claimed. The plan calls for real proof only after implementation.
- Installed PowerShell workflow helpers/templates are the actual execution route; there is no invented external SpecKit CLI. Candidate extension hooks are absent.
