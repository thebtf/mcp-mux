<!--
Sync Impact Report
- Version change: none (initial template) -> 1.0.0
- Modified principles: none (initial ratification; all five template slots filled from
  repo AGENTS.md RULES, docs/RELEASE-PROTOCOL.md, and muxcore v0.19-v0.27 release history)
- Added sections: Core Principles (I-V), Architecture & Stack Constraints,
  Release & Review Workflow, Governance
- Removed sections: none
- Follow-up TODOs: none
-->

# mcp-mux Constitution

## Core Principles

### I. MCP Protocol Spec Is Authoritative

All protocol behavior MUST be verified against the official Model Context Protocol
specification; the spec, not memory or convention, decides any protocol question.
When observed upstream behavior conflicts with an implementation assumption, the
implementation assumption loses. Every protocol-affecting change MUST name the spec
section it relies on in its commit, design note, or release notes.

Rationale: mcp-mux is a transparent proxy; a silent protocol deviation multiplies
across every downstream session and every consumer of muxcore.

### II. Single Process-Tree Authority

muxcore is the sole owner of daemon, owner, upstream, and subprocess lifecycle.
Contributors and consumers MUST NOT add product-local spawn locks, file-replacement
retries, PID sweeps, stale-process kill loops, request replay, launcher respawn
loops, or any lifecycle mechanism parallel to muxcore. Cleanup MUST cover the full
process tree (Unix process groups, Windows Job Objects), finalized exactly once.

Rationale: every historical process-explosion incident came from a second authority
competing with muxcore for the same owner slot or control pipe.

### III. No Stubs, No Guessing, Reasoning First

Only complete, working implementations land; partial scaffolds, placeholder returns,
and "fill in later" code are prohibited. External facts (library APIs, CLI flags,
schemas, platform behavior) MUST be verified with tools or primary docs before use,
never assumed. The WHY of a non-obvious decision MUST be documented before or with
its implementation (design note, ADR under `.agent/arch/decisions/`, or release note).

Rationale: this repository's own RULES table; unverified integration guesses have
repeatedly shipped dead-on-arrival behavior in consumer deployments.

### IV. Every Fix Ships a Regression Test

Each bug fix MUST land with at least one regression test that fails without the fix
and passes with it. Release evidence MUST include the root and muxcore test suites,
`go vet`, the repository critical suite, and the focused tests named by the release
notes for the touched lifecycle area, on Windows and Unix where the change is
platform-relevant.

Rationale: the v0.19.3 concurrency class of bugs only stayed fixed because each fix
carried its regression test; lifecycle races do not stay fixed by inspection.

### V. Consumer-Compatible Versioning

muxcore is a consumed library (aimux, engram, mcp-launcher). Public API changes MUST
be additive by default; any breaking change requires a MAJOR/minor version decision
with explicit migration notes in AGENTS.md and release notes. Releases containing
critical or consumer-impacting muxcore updates MUST complete the consumer handoff
defined in `docs/RELEASE-PROTOCOL.md` (fresh Engram issues or comments for every
impacted consumer) or report `CONSUMER_HANDOFF_BLOCKED` instead of claiming the
scope shipped.

Rationale: consumers pin muxcore and upgrade on explicit guidance; a silent breaking
change is a production incident in someone else's binary.

## Architecture & Stack Constraints

- Stack: Go. The product is a stdio multiplexer/proxy: shim (per-session) -> daemon
  -> owner -> upstream MCP server process.
- Sharing modes (`cwd`, `git`, `global`, `isolated`) are identity inputs, not
  lifecycle policy; per-engine namespaces isolate consumers on one host.
- Platform support is first-class on Windows and Unix: named pipes and Unix sockets,
  Job Objects and process groups. Platform-specific behavior MUST be implemented and
  tested on both families.
- Project artifacts follow the repo convention: investigations in
  `.agent/reports/`, diagnostics in `.agent/data/`, plans in `.agent/plans/`,
  specs in `.agent/specs/`, decisions in `.agent/arch/decisions/NNN-title.md`.
- `AGENTS.md` is the runtime operational contract and muxcore consumer API
  reference; it MUST be updated in the same change that alters consumer-visible
  behavior.

## Release & Review Workflow

- Public releases MUST follow `docs/RELEASE-PROTOCOL.md`; version bumps, changelog,
  and release notes land with the release change.
- Release evidence MUST be produced on the exact reviewed head: full suites and
  focused lifecycle tests run once per admitted SHA, not per attempt.
- Merge is exact-SHA; tagging happens after merge (tag-last); a fresh clone/build
  canary verifies the published artifact when the release ships a binary or module.
- Review findings are dispositioned individually (fixed, challenged with evidence,
  clarified, or deferred with named target); an open finding count is a queue,
  never a gate by itself.
- A release closes a named problem: the release notes MUST be able to state what
  previously failed and now works.

## Governance

This constitution supersedes conflicting practice documents for governance
questions; for protocol behavior the MCP specification supersedes this
constitution; for day-to-day operations `AGENTS.md` remains the binding contract.

Amendments require: a documented rationale, a version bump per semantic versioning
(MAJOR for principle removal or redefinition, MINOR for a new principle or
materially expanded section, PATCH for clarifications), and review through the
normal pull-request path. Compliance is verified at code review and again at the
release gate; a change that violates a principle MUST either be amended or carry an
explicit, reviewed exception recorded in the PR.

**Version**: 1.0.0 | **Ratified**: 2026-08-19 | **Last Amended**: 2026-08-19
