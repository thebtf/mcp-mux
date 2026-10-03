# Maintenance control contract

This is the implementation contract for accepted ADR-015 and feature002. Types below are planned additive API, not claims that these symbols exist at the source base. Core implements and commits this contract before adapter makers use it.

## Shared Go API in muxcore/control

Keep `DaemonHandler`, `CommandHandler`, `Send`, and `SendWithTimeout` signatures unchanged. Extend `Request` with:

```go
HoldID string `json:"hold_id,omitempty"`
HoldTTLMS *int64 `json:"hold_ttl_ms,omitempty"`
```

Reuse `Cmd`, `ServerID`, and `DrainTimeoutMs`. `hold` requires an exact `ServerID`; substring lookup and the historical `Command` fallback are not accepted. `resume` and `renew` require `HoldID` and do not re-resolve a server ID. Omitted TTL defaults to 300000ms; explicit zero, negative, or more than 3600000ms is invalid. Negative drain is invalid; zero skips grace. CLI hold defaults to 10s drain. Expiry starts at durable fence commitment, not at response delivery.

Add these types and methods in `muxcore/control/protocol.go` or the same package's `maintenance.go`:

```go
type MaintenanceState string
const (
    MaintenanceHolding MaintenanceState = "HOLDING"
    MaintenanceHeld MaintenanceState = "HELD"
    MaintenanceRetirementBlocked MaintenanceState = "RETIREMENT_BLOCKED"
    MaintenanceReleased MaintenanceState = "RELEASED"
)
type MaintenanceResult struct {
    HoldID string `json:"hold_id"`
    ServerID string `json:"server_id"`
    State MaintenanceState `json:"state"`
    ExpiresAt time.Time `json:"expires_at"`
    DrainDeadline time.Time `json:"drain_deadline"`
    TreesRetired bool `json:"trees_retired"`
}
type MaintenanceHandler interface {
    HandleMaintenance(req Request) (MaintenanceResult, error)
}
type ShutdownWithErrorHandler interface {
    HandleShutdownWithError(drainTimeoutMs int) (string, error)
}
type OwnerRestartHandler interface {
    HandleRestartOwner(req Request) (Response, error)
}
func (r *Response) Err() error
func SendMaintenance(socketPath string, req Request, timeout time.Duration) (*MaintenanceResult, error)
```

`Response` gains `Maintenance *MaintenanceResult` with `json:"maintenance,omitempty"` and `ErrorCode MaintenanceErrorCode` with `json:"error_code,omitempty"`. Existing successful nonmaintenance responses remain byte-compatible when the new fields are absent. `OwnerInfo` gains `Maintenance *MaintenanceResult` with the same optional JSON field. Daemon status also exposes a redacted `maintenance` list so leases remain inspectable after their owners are removed. Do not add raw context keys or environment fields to either projection.

Use `MaintenanceErrorCode` as a string type with the following wire values and sentinels:

| Wire code | Exported sentinel | Meaning |
| --- | --- | --- |
| `maintenance_held` | `ErrMaintenanceHeld` | Matching request/start fenced, or lifecycle operation refused because an active fence exists. |
| `maintenance_conflict` | `ErrMaintenanceConflict` | Scope overlaps an active different lease, or a stale lease attempts to alter a current lease. |
| `maintenance_not_found` | `ErrMaintenanceNotFound` | Exact target/lease absent; no mutation. |
| `maintenance_retirement_blocked` | `ErrMaintenanceRetirementBlocked` | Full tree death not proven; suppression remains. |
| `maintenance_unsupported` | `ErrMaintenanceUnsupported` | Endpoint/path cannot perform this contract; no fallback. |
| `maintenance_persistence_failed` | `ErrMaintenancePersistenceFailed` | Authority could not be durably read/committed; admission stays closed. |
| `maintenance_invalid` | `ErrMaintenanceInvalid` | Invalid duration, identity input, ambiguous scope, or malformed maintenance response; no unsafe mutation. |

Define `MaintenanceError` with `Code MaintenanceErrorCode`, `Result *MaintenanceResult`, `Error() string`, and `Is(target error) bool`. Each sentinel is a `*MaintenanceError` of its code. Wrapping retains `errors.Is` and `errors.As`; error text is a fixed safe message, never an arbitrary internal error/reason. Unknown nonempty error codes fail closed as `ErrMaintenanceInvalid`. `Response.Err()` returns nil only for `OK=true`, recognizes known typed codes for failures, and otherwise retains ordinary nonmaintenance error semantics. `SendMaintenance` uses `SendWithTimeout`, validates the command and typed result, and maps an untyped unsuccessful old-endpoint response or missing optional handler/result to `ErrMaintenanceUnsupported` without text matching. It must not retry with stop or direct execution.

`HandleMaintenance` accepts `hold`, `resume`, and `renew`. `hold` reports success only for durable, unexpired `HELD` with `TreesRetired=true`; blocked/persistence/elapsed-window failure returns an error and safe current result when available. `resume` reports durable `RELEASED` only after all trees are dead. `renew` can extend the exact current fenced lease without changing its retirement state, and calculates expiry from serialized renewal acceptance plus TTL. A released or expired lease is not renewed. A stale identity returns conflict if a replacement lease exists, otherwise not-found. Safe HELD expiry atomically releases suppression only after a successful durable release commit.

The optional shutdown extension lets control return a typed refusal while preserving existing consumer interfaces. An aware daemon implements it; control prefers it when present. Graceful-restart handlers already return errors and must preserve the same typed refusal. Refusal is checked inside the serialized lifecycle transaction, not only by a preflight read. Empty-daemon idle exit is fenced too. `restart_owner` is an additive daemon command for managed restart from the exact daemon-owned context and era; its existing spawn-shaped response is used by adapters. During maintenance it returns `maintenance_held`; it never uses public `OwnerInfo` plus ambient adapter credentials to reconstruct a launch.

## Local wire example

One NDJSON control request/response pair uses the existing authenticated local endpoint. This protocol is not MCP JSON-RPC.

```json
{"cmd":"hold","server_id":"exact-owner-id","hold_ttl_ms":300000,"drain_timeout_ms":10000}
{"ok":true,"maintenance":{"hold_id":"opaque-lease","server_id":"exact-owner-id","state":"HELD","expires_at":"2026-10-03T12:05:00Z","drain_deadline":"2026-10-03T12:00:10Z","trees_retired":true}}
{"cmd":"renew","hold_id":"opaque-lease","hold_ttl_ms":300000}
{"cmd":"resume","hold_id":"opaque-lease"}
```

Times are UTC RFC3339 timestamps, encoded by `time.Time`. The opaque identifiers above are illustrative, not executable selectors. `ServerID` remains a display target; private finite context keys remain ledger-only.

`ServerID` is the original selected display ID while retained in daemon memory. After unplanned recovery it may be empty because the durable ledger deliberately stores only opaque context keys; hold ID, state, deadlines, and fence remain inspectable. Adapters must not reconstruct a target from that empty display ID. `OwnerRestartHandler` is optional; an absent handler returns typed unsupported, and a successful result carries the daemon-selected exact ProtocolEra plus spawn-shaped endpoint/token fields.

## CLI and MCP adapters

```text
mcp-mux hold <exact-server-id> --ttl 5m --drain-timeout 10s --json
mcp-mux resume <hold-id> --json
mcp-mux renew <hold-id> --ttl 5m --json
```

CLI accepts flags after the positional identifier exactly as shown, returns nonzero on typed refusal, and emits one JSON object with `ok`, optional `maintenance`, and optional `error_code`. Safe readback is available through existing status/list paths. No substring, PID, foreign-engine lookup, or automatic daemon replacement is added.

MCP tools use the selected local daemon endpoint and delegate to `SendMaintenance`:

- `mux_hold`: required `server_id`, optional integer `hold_seconds` default 300 and range 1..3600, optional integer `drain_timeout_ms` default 10000 and minimum zero.
- `mux_resume`: required `hold_id`.
- `mux_renew`: required `hold_id`, optional integer `hold_seconds` with the same default and bounds.

Tool failures retain existing MCP tool-error framing and carry the stable `error_code` plus safe maintenance result when available. No foreign engine writes, substring targeting, arbitrary reason field, or direct-owner fallback. Existing `mux_restart` uses `restart_owner` rather than stop-and-exec reconstruction and treats typed maintenance refusal as terminal.

## Aware host request behavior

Use product JSON-RPC error code `-32005`, message `upstream held for update`, and data containing `error_code=maintenance_held` plus safe hold state/expiry when known. This is a local product error, not a newly assigned MCP protocol method or version error. Preserve the original raw numeric/string ID exactly. Notification/response frames receive no invented reply and are not replayed.

Owner ingress rejects before cache, enqueue, materialization, or forwarding after fence commitment. The resilient shim classifies the typed control error immediately. Its ingress and maintenance-to-connected transitions serialize frame disposition: a successful reconnect cannot send an earlier held frame, even when both events are ready. Backpressure must not silently drop IDs at queue capacity. In-flight work may finish until the single drain deadline; unfinished work receives one terminal error and is never replayed. Fresh work after durable release can reach one new generation.

Modern native policy stays isolated/cache-off/replay-off and uses fresh exact-era admission or explicit new-launch-required refusal. Old shims under an aware daemon are physically fenced but do not acquire promised immediate errors or replay semantics. Old daemons are unsupported. Arbitrary old binaries, foreign engines, manual active-pointer swaps, and unmanaged processes are not controlled.

## Authority and lifecycle boundaries

Scope is the exact selected owner's finite admitted context set. Context keys use namespace/canonical endpoint, era, command, length-delimited argv, canonical CWD, and strict normalized `envidentity.Build` fingerprint; exclude product version, nonce, retry suffix, lease/generation IDs, and transient mode promotion. Persistence stores only opaque keys and safe lease/timing metadata under the current user's persistent configuration root. Handoff/snapshot records are not maintenance authority.

The schema-2 store is one logical authority with two mandatory members, `ledger.json` and `transaction.json`, under `os.UserConfigDir()/mcp-mux/maintenance/<full-scope-digest>/`. Startup and `CheckMaintenanceForActivation` use the same aggregate lookup. Canonical endpoint identity resolves the parent directory and preserves the endpoint leaf, with Windows case normalization; creating or removing the socket leaf cannot select a different authority. Only an absent namespace directory with both members absent is a cold start. An existing directory with either or both members missing, pending preparation, unreadable or invalid data, or unknown members fails closed. `.ledger-*` temporary files are not authority.

Opaque keys and transaction digests must be canonical `v1:` plus 64 lowercase hexadecimal characters. Validation rejects duplicate context keys or lease IDs, unknown schema/fields, trailing JSON, and duplicate JSON members including case-equivalent names. Recovery accepts only a COMMITTED certificate whose schema, scope, transaction ID, and target digest match the exact ledger bytes and which contains no predecessor. It never repairs authority during lookup.

Mutations serialize under the existing namespace file lock before the admission gate. Each member uses restrictive same-directory temporary write, flush, and platform replacement, including Unix directory durability. This is not an atomic two-file write or a generic transaction framework. The phases are:

| Phase | Durable ordering and error/crash outcome |
| --- | --- |
| PREPARE | Write `transaction.json` with wire state `prepared`, predecessor leases, and target ledger digest. Do not publish the target until the writer acknowledges durable preparation. Before replacement the previous complete pair may remain valid; after replacement pending preparation refuses recovery. |
| PUBLISH | Replace `ledger.json`. A write error may follow replacement, including Unix directory durability failure. Leave PREPARED in place and fail closed even if target bytes are visible. Never infer rollback or release from those bytes alone. |
| FINALIZE | Only after the target writer acknowledges durable publication, write a matching `committed` certificate without predecessor leases. An error still returns `maintenance_persistence_failed` and retains conservative predecessor authority in current memory. Recovery either refuses pending/invalid data or verifies a matching COMMITTED certificate proving that earlier acknowledged durable publication. That proof does not convert the failed API response into success. |

A successfully acknowledged release leaves a complete committed pair with the released lease absent, retaining any unrelated leases. Recovery must not resurrect its old lease. A finalize error does not promise absence of external storage changes, and callers must not treat it as RELEASED or as a usable HELD grant. The acknowledgment above is the local storage writer's durability result, not a new network acknowledgment service. Caller fields and wire types remain unchanged.

Start admission is held through context election, physical start, and installation of every nonnil process, including partial failed starts. Acquire the daemon-owned gate before owner locks; retries reacquire it. Exclusive acquisition commits the fence and request-admission closure before drain. HELD requires actual tree death and no detach, not merely `RetirementProven` after committed handoff. Controlled restart/handoff/shutdown/downgrade is refused while fences remain, including update-helper and launcher fallback paths. Aware unplanned startup loads authority before listener/restore/spawn, independently of `SkipSnapshot`; incomplete records remain blocked. Standalone launch must use coordinated admission or explicitly refuse.
