---
feature_id: ~
slug: proofing-muxcore-production-port
status: Synthetic
created: 2026-05-19
evidence_sources:
  - current user incident thread
  - .agent/proofing/current-topology-poc/proofing.md
  - .agent/arch/decisions/011-proofing-to-muxcore-production-port.md
---

# User Job Statement: Proofing to Muxcore Production Port

> SYNTHETIC -- no `docs/USER-INTERVIEWS/`, `.agent/opportunities.md`, or
> GitHub issues labelled `user-feedback` were found. This job statement uses
> the current feature-request thread as primary evidence and should be
> re-audited if real interview/support evidence is added.

## Current Struggle

The user is operating multiple Claude Code/Codex MCP sessions and sees random
startup and reconnect failures in servers routed through `mcp-mux` or through
`muxcore` consumers.

Evidence:

- Q1: "в каждой новой сессии часть mcp серверов которые идут через mcp-mux - фейлится"
- Q2: "mcp-mux сейчас ненадежный мультиплексор, не выполняющий основную идею"
- Q3: "даже со стартом проблемы, всегда"

The job is not merely to make one test green. The user needs the current
topology to prove that a consumer-facing shim can stay attached while daemon,
owner, and upstream lifecycle events happen behind it.

## Workaround

The current workaround is manual investigation and session hygiene: inspect
logs, restart mux slots, close old sessions, avoid trusting a single runtime
smoke, and keep adding experimental PoC phases until the failure boundary is
visible.

Evidence:

- Q4: "можем создать отдельную экспериментальную dummy программу, вся задача которой - проверить возможную ПРАВИЛЬНУЮ реализацию"
- Q5: "давай попробуем развить PoC: поэтапно атомарно добавлять в него \"мясо\""
- Q6: "давай доведем до поломки. иначе непонятно, почему ломается текущий слой"

This helped discover the right invariants, but it does not by itself fix the
production `muxcore` runtime.

## Friend-Summary

The user wants `mcp-mux` to behave like a real transparent multiplexer again:
new MCP sessions should attach reliably, restart/handoff should not look like
fresh-spawn success by accident, and production code should inherit only the
PoC mechanisms that survived proofing.

Evidence:

- Q7: "поищем, как теперь добиться результата без поломки?"
- Q8: "доделаем скилл и продолжим основную задачу после"
- Q9: "$nvmd-platform:nvmd-architect пиши ADR портирования"

The desired outcome is a production-ready implementation path: translate the
Phase 6-9 proofing invariants into real `muxcore` tests, refactors, and runtime
smoke evidence, without adding more toy PoC phases first.
