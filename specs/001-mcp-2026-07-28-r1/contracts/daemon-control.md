# Contract: Daemon Control Spawn and Exact-Era Echo

## Scope

The existing control plane is one NDJSON request/response exchange over a local authenticated same-user endpoint (`muxcore/control/protocol.go:Request` and `Response`). R1 extends the spawn path additively so a modern shim can prove that the daemon selected the era it requested before it dials an owner.

This is a local control contract, not MCP JSON-RPC and not protocol negotiation.

## Additive request/response fields

| DTO | Recommended field | Presence and meaning |
| --- | --- | --- |
| Spawn request | `protocol_era` | Empty/absent means existing legacy request behavior. Explicit modern value is the exact wire string `2026-07-28`. Unknown/non-string/ambiguous value is rejected before election. |
| Spawn response | `protocol_era` | For a successful R1 modern spawn, echoes exactly `2026-07-28`. Legacy response compatibility remains unchanged when no modern request was made. |
| Spawn/local error | stable local reason/category | Safe diagnostic only; contains no request bytes, token, environment, credential, partition material, or private compatibility fingerprint. |

An eventual typed internal era may be an enum; the control wire remains explicit and strictly parsed. The field does not reuse `mode`, and `mode` remains the existing sharing-mode input.

## Modern spawn preconditions

Before the control request is issued, ingress has already:

1. received an explicit `Modern20260728` public policy selection;
2. buffered one opening JSON-RPC request;
3. validated required modern `_meta` fields and version; and
4. retained the original bytes for one later upstream write.

The daemon must independently validate the requested control era. It must not trust a CLI-provided string solely because ingress validated a frame.

## Required spawn state machine

```text
ValidatedModernOpening
  -> send spawn(protocol_era=2026-07-28)
  -> daemon validates exact era
  -> daemon derives distinct safe modern identity
  -> daemon creates forced-isolated modern owner
  -> response echoes protocol_era=2026-07-28
  -> shim attaches and forwards retained opening bytes

any failure before echo
  -> refuse
  -> no modern-to-legacy attach, no implicit fallback
```

The daemon performs identity/reuse selection only after modern era validation. For R1, the selection result is always a new or exact reconnectable **forced-isolated** modern owner; `findSharedOwnerLocked`-style broad reuse cannot cross the era boundary.

## Echo validation at the shim

A modern shim accepts a successful control result only when all conditions hold:

- response success is true;
- response includes a syntactically valid modern era echo;
- echoed era exactly equals the caller's `Modern20260728` selection; and
- returned owner endpoint/identity is designated modern and forced isolated by daemon policy.

An absent response field, a legacy value, unknown value, or mismatch is a local `ControlEraMismatch` and fails closed before the host is attached. It is not automatically an MCP `UnsupportedProtocolVersionError`.

## Error contract

| Failure class | Boundary | Result |
| --- | --- | --- |
| Invalid modern frame/metadata | MCP ingress | MCP parse/invalid request or `-32602`; do not send spawn. |
| Valid unsupported declared wire version | MCP ingress | `-32022` with `supported` and `requested`; do not send spawn. |
| Invalid/unknown control-era field | Local control | Refuse before owner election; safe local reason. |
| Old daemon/no modern echo | Local control | Refuse modern admission; leave legacy compatibility unchanged. |
| Echo mismatch or legacy owner target | Local control | Refuse before IPC attach; do not downgrade/fallback. |
| Control transport failure | Local control | Explicit modern admission failure; no speculative owner/legacy retry. |

The control response may not include host payload, opaque metadata, token history, raw environment, credentials, or private compatibility inputs as part of an error.

## Identity and owner election requirements

- `ProtocolEra` participates in every exact lookup, retry/respawn identity, reconnect selection, and lifecycle constructor input that could otherwise attach an owner.
- Legacy `serverid.GenerateContextKey` bytes are unchanged.
- Modern physical identity is domain-separated and safe in Windows/Unix IPC paths; a raw delimiter such as `|` is prohibited.
- `ProtocolEra` and sharing policy remain two distinct data fields.
- R1 modern has `ForcedIsolated` sharing regardless of command/args/CWD/environment similarity.

## Reconnect and compatibility

The existing refresh-token and fallback-spawn mechanism retains its process/token safety gates. A modern reconnect must carry and revalidate the exact modern era, receive an exact echo, and use the existing single-recipient path with no preserved ephemeral session, progress, or subscription state. Otherwise it returns an explicit new-launch failure. It must not invoke legacy `replayInit`, synthesize list change, attach through an old token to a legacy owner, or reuse old ephemeral state.

## Testable control assertions

Implementation proof must show:

1. zero-value/absent `protocol_era` retains legacy request/response compatibility;
2. a valid modern echo permits only an isolated modern owner;
3. absent, malformed, unknown, old-daemon, and mismatched echo each refuse before attach;
4. an existing legacy owner with matching launch inputs is never returned to a modern request;
5. legacy ID bytes remain unchanged and modern endpoint derivation is valid on Windows and Unix; and
6. control/status errors redact every prohibited value.
