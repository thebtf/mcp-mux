# Contract: Owner Status and Read-Only Operational Truth

## Scope

R1 exposes the minimum readback needed to prove modern admission and support rollback. The exact modern control echo proves admission. The following existing projections carry the R1 owner facts when they carry `OwnerInfo`:

- `owner.Owner.Status`;
- `daemon.Daemon.HandleStatus` and `HandleListOwners`;
- CLI `status`; and
- `internal/mcpserver` `mux_list`.

R1 does not add a registry descriptor schema or capability, a `mux_engines` or topology contract, a lifecycle-state taxonomy, or a safe-counter model. Those observability changes are R3 scope.

## Required R1 facts

| Field | Modern R1 value | Legacy presentation | Notes |
| --- | --- | --- | --- |
| `protocol_era` | `2026-07-28` | Existing legacy-compatible value | Explicit wire-era fact. Do not infer it from a server ID. |
| `sharing_policy` | `forced-isolated` | Existing released behavior | Distinct from protocol era. |
| `cache_policy` | `off` | Existing released behavior | Covers response cache, template reuse, and replay. |
| `lifecycle_policy` | `r1-quarantine` | Existing released behavior | Shows the R1 exclusion, cold-start, or fail-closed rule. |
| Existing readiness field | Existing value and meaning, where the projection already provides it | Existing value and meaning | R1 adds no readiness field or lifecycle-state vocabulary. |

Each listed projection that carries `OwnerInfo` must expose the four R1 policy facts. Standard logging remains a runtime behavior tested under FR-011. It is not a required `OwnerInfo` field.

## Control echo

The successful modern spawn response echoes `protocol_era=2026-07-28` before the shim attaches. [`daemon-control.md`](daemon-control.md) defines the request/response validation. The control echo is admission proof. It is not a substitute for the owner readback above.

## Redaction contract

No required R1 `OwnerInfo`, status, list, CLI, or `mux_list` projection may include any of the following, including nested structures, formatted strings, logs, errors, derived hashes, or stable abbreviations:

- host or upstream request/response bodies;
- opaque MRTR `requestState`, `inputRequests`, or `inputResponses`;
- credentials, token values, bound-token history, or raw environment values;
- client/server self-reported identity used as a key;
- authorization, tenant, or partition keys, digests, compatibility hashes, or linkable fingerprints;
- cache/template content or cache keys; or
- subscription IDs, progress tokens, original/remapped request IDs, or route identifiers.

Existing unrelated fields retain their current redaction behavior. A modern projection omits or replaces a legacy field if copying it would expose prohibited modern material.

## Agreement and rollback

For one active R1 owner, every required `OwnerInfo` projection agrees on these values:

```text
protocol_era = 2026-07-28
sharing_policy = forced-isolated
cache_policy = off
lifecycle_policy = r1-quarantine
```

Where a projection already has a readiness field, it keeps that field's existing meaning. If a projection cannot determine the required facts reliably, it returns its existing unavailable/unknown read result. It does not fabricate a legacy value.

Status supports an operator deciding to stop new modern admissions and drain/remove modern owners. It is informational. An operator cannot mutate an era through status, list, CLI, or `mux_list` readback. A rollback must preserve the R1 rule that live modern work is not converted to legacy or replayed.

## Testable read-side assertions

Implementation proof must inspect the exact control echo and every available direct owner status, daemon status/list, CLI status, and `mux_list` projection that carries `OwnerInfo` for one modern owner. It must compare the four required policy fields, preserve existing readiness semantics where present, and scan output and relevant errors for the redaction exclusions above. It must separately prove that legacy status retains released behavior.
