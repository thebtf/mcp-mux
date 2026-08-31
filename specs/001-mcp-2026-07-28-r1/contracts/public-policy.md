# Contract: Public Library and CLI Policy

## Purpose

This contract defines the public R1 selection boundary for a known MCP `2026-07-28` host. It is additive: callers that do not choose the new policy retain released legacy behavior.

The semantic values below are required. Their final Go field and CLI flag spelling are implementation-level compatibility details and must remain additive; this plan recommends `ProtocolPolicy` and `Modern20260728` to avoid reusing historical sharing terminology.

## Public policy values

| Semantic policy | Availability | Meaning |
| --- | --- | --- |
| `LegacyOnly` | Existing/default | Released initialization-based MCP behavior. This is the library zero value and the CLI behavior when no R1 policy is selected. |
| `Modern20260728` | New additive R1 opt-in | Pinned native MCP `2026-07-28` route. It is for a known modern host and same-era upstream. |

No R1 public policy means `Auto`, `Probe`, `DualSameEra`, `ShareModern`, `CacheModern`, `PersistModern`, or `ResumeModern`. Historical sharing flags (`cwd`, `git`, `global`, `isolated`, including any historical stateless terminology) retain their released meaning and are not protocol-era selectors.

## Library caller contract

### Input

A caller configures the policy before `engine.New`/`(*MuxEngine).Run` begins client/shim admission.

| Input | `LegacyOnly` | `Modern20260728` |
| --- | --- | --- |
| Configuration zero value | Valid; preserves released behavior | Not selected |
| First host frame | Released legacy flow | One newline-delimited JSON-RPC **request** with valid modern required metadata |
| `clientInfo` | Existing legacy semantics | Optional; absent is accepted; present must be valid |
| Sharing input | Released behavior | Ignored as authorization for reuse; resulting owner is forced isolated |

The implementation must not wait until `RunResilientClient` has connected/admitted an IPC session to make modern selection. Current `engine.New`, `(*MuxEngine).Run`, and `RunResilientClient` callers were mapped with LSP before this contract was written.

### Success result

For `Modern20260728`, the caller gets a route only after all of the following are true:

1. the opening request metadata is valid;
2. control has echoed exactly `2026-07-28`;
3. the daemon has selected a new forced-isolated modern owner rather than a legacy/cache/template owner; and
4. the upstream receives the original opening frame byte-for-byte, without a mux-generated legacy frame before it.

The public route provides native same-era traffic only. It does not promise caching, sharing, automatic fallback, replay, persistence, or automatic subscription continuation.

### Failure result

- Missing/null/non-object/malformed required modern metadata, or malformed present `clientInfo`, is `-32602` when the request ID permits a JSON-RPC error.
- A valid declared version other than the pinned R1 version is `-32022` with `data.supported` and `data.requested`.
- A conflicting modern/legacy opener, absent/unknown/mismatched control echo, or unsafe lifecycle reconnect is an explicit safe refusal. A local refusal must not pretend to be `-32022` unless an unsupported declared version caused it.
- No failure falls through to the legacy route, starts an unsafe upstream, or attaches an existing legacy owner.

## CLI caller contract

The CLI exposes the same semantic choice as the library. The recommended customer-visible shape is an explicit protocol selector whose value is `2026-07-28`; its exact spelling must be chosen as an additive CLI API review and documented alongside the release. It must not be inferred from command arguments, upstream capabilities, a discovery response, or a failed request.

A successful explicit-modern invocation has these observable properties:

- exactly one isolated modern owner is created for the host;
- `server/discover` is forwarded unchanged if the host sends it, but is never injected by mcp-mux;
- a direct valid modern request is also a valid opener;
- mcp-mux sends no legacy `initialize`, `notifications/initialized`, roots/list, list-change, cache, template, or replay frame to the modern upstream;
- after request opt-in, a valid request-scoped standard log reaches the sole recipient once and is never synthesized or broadcast; and
- status exposes the R1 policy facts defined in [`owner-status.md`](owner-status.md).

## Compatibility rules

| Consumer or behavior | Required R1 result |
| --- | --- |
| Existing library caller with no policy field set | Released legacy behavior. Legacy identity remains byte-identical to the released baseline. |
| Existing CLI invocation without modern selector | Released legacy behavior. |
| Known MCP 2026-07-28 host with same-era upstream and explicit modern policy | Supported through one forced-isolated native route. |
| Legacy host on modern policy | Explicit refusal; no translation or fallback. |
| Modern host against a legacy owner/upstream identity | Explicit refusal; no translation. |
| Modern host against unknown, absent, or control-era-mismatched upstream identity | Explicit refusal before unsafe attachment or fallback. |
| Dual-era host requiring same-shim probe/fallback | Not supported automatically in R1; operator uses an explicit known-legacy or known-modern invocation. |
| Older daemon that cannot echo era | Modern caller fails closed; legacy caller continues existing compatibility behavior. |
| Any omitted combination using explicit modern policy | Unsupported and refused fail-closed before unsafe attachment. |

## Non-goals and security boundary

`clientInfo`, `serverInfo`, command similarity, CWD, and post-connect authorization never authorize modern sharing. R1 publishes no tenant/authorization partition, raw environment value, token, request payload, opaque `requestState`, cache key, digest, or linkable compatibility fingerprint.

The caller owns the choice to retry after a loss. For `subscriptions/listen`, the caller must issue a new native listen request after a terminated/re-established route. mcp-mux does not author a request, retry, or subscription on the caller's behalf.

## Rollback behavior

To roll back, an operator stops admitting new explicit-modern launches and drains/removes existing modern owners through the lifecycle-quarantine contract. Rollback never converts live modern work to legacy, hands it to a legacy owner, or replays unfinished work.
