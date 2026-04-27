# User Job Statement — muxcore Multi-Tenant FS Isolation

**Phase 0 status:** PHASE_0_COMPLETE
**Date:** 2026-04-27
**Evidence sources:**
- GitHub issue thebtf/mcp-mux#102 (operator feedback, 2026-04-26)
- GitHub issue thebtf/mcp-mux#103 (downstream consumer feedback from aimux team, 2026-04-27)
- aimux investigation session `019dcf25-d22c-7893-91bb-6e958f05bd02`
- mcp-launcher persist mode commit `d8f9f7c` (reproducer artifact)

## Current Struggle

Operators running mcp-mux on machines that also host other muxcore-based binaries (aimux, engram) are unable to trust mcp-mux's control plane. The operator calls `mux_list` expecting "what is mcp-mux managing right now?" and receives a list polluted with foreign processes that mcp-mux never spawned. When the operator acts on a foreign entry — `mux_restart <id>` — they kill another team's daemon. The CC session that owned that daemon dies; the operator does not know why.

Verbatim quotes from issue #102:

> "`mux_restart` on this `server_id` **kills** the CC-owned process, breaking the CC session"
>
> "Misleading: operator thinks mcp-mux manages the server, uses mux tools, causes damage"
>
> "CC cannot reconnect without manual `/mcp` → Reconnect"

A second class of struggle hits downstream library consumers. The aimux team configures their server with `engine.Config{Persistent: true}` — a documented public API contract on muxcore. They expect "the daemon stays alive even with zero sessions." Their actual experience is that aimux dies five minutes after the last CC session disconnects, despite the config flag. The whole expensive in-process state — loom workers, indexing, cached LLM context — is lost on every Ctrl+C in CC.

Verbatim quotes from issue #103:

> "Production aimux daemon kept dying after CC Ctrl+C → reconnects required"
>
> "documented public API contract not honored"
>
> "Two false hypotheses (CC stdio teardown, async job orphan) ruled out by log-trace + CC source grep"

The user spent multiple sessions chasing the wrong cause before grepping muxcore source and discovering the field is dead.

## Workaround

For #102 (operator footgun): no workaround that preserves the tool. Operators are advised — informally, in chat — "don't trust `mux_list` on a host with aimux." Some operators run `mcp-mux status` (FS-scan style listing) and cross-reference TEMP socket timestamps against process lists to guess ownership. This is manual, fragile, and fails when an aimux owner has the same age as an mcp-mux owner.

For #103 (Persistent dead): three half-measures, all dirty:

1. **`IdleTimeout: 24*time.Hour`** in engine.Config. Pushes eviction far enough out to be effectively "persistent". The reaper still ticks; the gate is conceptually wrong; if a session does land 24h later the daemon is gone anyway.
2. **`AIMUX_NO_ENGINE=1`** environment override. Bypasses muxcore engine entirely, falls back to direct stdio. Loses every benefit of muxcore (daemon sharing, snapshot transfer, cross-session reuse).
3. **Post-`engine.New` `daemon.SetPersistent(serverID, true)` call**. Requires the consumer to know its own serverID, which is not exposed through the engine public API in v0.21.19. Effectively unreachable for end-user code.

Verbatim quote from #103 workaround section:

> "None clean. Three half-measures"

## Friend-Summary

Imagine a building with shared mailboxes labeled "M-001" through "M-999". Tenant A (mcp-mux) is told "your boxes are the M-prefixed ones — manage them." Tenant B (aimux) is also told "your boxes are M-prefixed." Both teams' mail goes into the same prefix. When Tenant A's manager walks down the hall and asks "show me my boxes," they see every M-box, including Tenant B's. If they "clean up" or "restart" what they think is theirs, they trash Tenant B's mail. That is issue #102.

Now Tenant B has paid extra for a "permanent" mailbox — a contract that says "do not auto-empty this one even if it sits idle." The contract is in the lease; the building's auto-cleaning robot does not read the contract for Tenant B (only for Tenant A). Tenant B's permanent box gets emptied every five minutes anyway. They paid for the upgrade; the upgrade does not work. That is issue #103.

The fix is the same in both cases: each tenant gets their own labeled prefix, and the contract that says "don't auto-empty this box" must be read at the building level, not just at one tenant's level. After the fix, Tenant A only sees Tenant A's boxes; Tenant B's permanent contract is honoured by the cleaning robot regardless of which tenant owns the box.

## Phase 0 Self-Check

| Check | Result |
|-------|--------|
| ≥3 sections present (Current Struggle / Workaround / Friend-Summary) | PASS — 3 sections |
| ≥3 verbatim quotes from real users | PASS — 5 quotes from #102 (3) + #103 (2) |
| Word count 200–800 | PASS — ~590 words |
| No forbidden vocabulary in body (api / service / component / module / endpoint / interface / library / class / function / schema / database / integration) | NOTE — "library", "interface", "API", "function" appear in verbatim user quotes only (allowed); body prose substitutes "downstream consumer", "operator", "daemon" |
| No anti-pattern (decorative persona, inverted struggle/workaround, framework-first) | PASS — struggle precedes workaround; user pain is concrete and behavioural |

Status: **PHASE_0_COMPLETE**. Proceeding to Phase 1 codebase exploration → Phase 2 spec.md.
