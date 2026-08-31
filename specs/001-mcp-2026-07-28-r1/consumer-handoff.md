# R1 Consumer-Handoff Preparation

**Status**: Candidate-owned preparation only. This file does not publish, file, send, or update an external consumer handoff.

## Purpose

R1 changes the `mcp-mux` modern-admission boundary while keeping released legacy behavior as the default. This artifact prepares the consumer record required before release for `aimux`, `engram`, and every other `muxcore` consumer discovered during release inventory.

The R1 public change is additive. A consumer receives modern behavior only when it explicitly selects the pinned `2026-07-28` policy for a known modern host and same-era upstream. Consumers that do not opt in retain released legacy behavior. Legacy identity remains byte-identical to the released baseline.

## Consumer map

| Consumer | R1 impact | Additive migration | Rollback | Required evidence |
| --- | --- | --- | --- | --- |
| `mcp-mux` | Owns the R1 selector, forced isolation, lifecycle quarantine, and minimal readback. | Ship the exact candidate only after the customer-proof and independent-check receipts are complete. Use the explicit modern policy only for a known modern host. | Stop new explicit-modern admissions. Drain/remove modern owners through R1 quarantine. Restore the prior product binary if a product rollback is required. | Candidate source/binary SHA, SC-001 corpus result, quickstart transcripts, status/redaction evidence, and rollback result. |
| `aimux` | No behavior changes by default. It can encounter the new boundary only if it adopts the candidate `muxcore` revision or invokes `mcp-mux` with the explicit modern policy. | Keep the existing zero-value/legacy configuration. If adopting R1, opt in per known modern route and prove the same-era customer matrix without copying mcp-mux private control or lifecycle logic. | Pin or restore the prior compatible `muxcore`/product revision. Do not force a mixed-era live handoff. | Consumer revision, opt-in decision, legacy-parity result, any modern proof, and rollback result. |
| `engram` | No behavior changes by default. The R1 selector remains opt-in and does not alter existing legacy engines. | Retain the existing legacy configuration unless a known modern upstream is explicitly selected. Use the released public policy only; do not add a consumer-local retry, replay, stale-process, or lifecycle controller. | Pin or restore the prior compatible `muxcore`/product revision. Do not convert live modern work to legacy. | Consumer revision, opt-in decision, legacy-parity result, any modern proof, and rollback result. |
| Other `muxcore` consumers | The release inventory determines the complete set. No unknown consumer receives a silent behavior change because the default remains legacy. | Keep existing configuration unless the consumer explicitly adopts the modern policy for a known same-era route. Do not copy private mcp-mux protocol, owner, or lifecycle behavior. | Restore the consumer's prior compatible dependency/product revision. Do not operate a mixed-era live handoff. | Inventory identity, dependency revision, opt-in decision, legacy-parity result, any modern proof, and rollback result. |

## Additive migration boundary

A consumer migration is valid only when all of the following are true:

1. The consumer identifies a known MCP `2026-07-28` host and same-era upstream.
2. The consumer explicitly selects the R1 modern policy before its opening frame.
3. The consumer accepts a control response only when it echoes the exact modern era.
4. The consumer treats loss/reconnect as fresh exact-era admission or explicit new-launch failure. It does not add replay, automatic re-listen, or an implicit legacy fallback.
5. The consumer verifies its released legacy path before and after the change. Its legacy identity remains byte-identical to the released baseline.

No migration is implied by a shared-mode flag, `clientInfo`, a discovery response, an upstream name, or a failed modern attempt.

## Rollback boundary

Rollback stops new explicit-modern admissions and uses the existing R1 quarantine path to drain or remove modern owners. It never converts live modern work to legacy, hands it to a legacy owner, or replays unfinished work.

For a consumer dependency rollback, restore the prior compatible module/product revision. Do not force a mixed-version, mixed-era, or live state transfer.

## Evidence record for release-stage handoff

The release record must preserve these facts for each mapped consumer:

- consumer identity and revision;
- whether it opted into the R1 modern policy;
- exact candidate source and binary SHA;
- legacy-parity evidence and, when applicable, modern customer-proof evidence;
- quickstart scenario, fixture, platform, transcript, and evidence-hash references;
- rollback result; and
- disposition: no action needed, additive migration verified, deferred, or blocked.

`release-evidence.md` records the candidate-level proof. `independent-check.md` records an independent re-derivation of the R1 boundary. A release-stage handoff may use these records, but this implementation-phase artifact does not contact external systems or publish consumer notices.
