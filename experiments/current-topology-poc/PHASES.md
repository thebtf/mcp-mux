# Current Topology PoC Phase Ladder

The experiment should grow one production-like mechanism at a time. Each phase
must keep the same acceptance command green:

```powershell
.\scripts\run-current-topology-poc.ps1 -WatchSeconds 1
```

## Phase 0 - Minimal Lifecycle Authority

Status: PASS

- one daemon
- one owner
- daemon ready gate
- daemon and owner generations
- one-shot token bound to both generations
- stale generation rejection

## Phase 1 - Owner Registry Identity

Status: PASS

Adds the first muxcore-like ownership layer:

- `spawn` request carries `command`, `args`, `cwd`, and `mode`
- daemon owns an `owners` registry keyed by server identity
- `cwd` mode reuses owner only for identical `command+args+cwd`
- `global` mode ignores cwd and reuses across workspaces
- `isolated` mode creates a fresh owner on every spawn

Pass signal:

- prior launcher checks still pass
- direct `--poc-probe-owner-registry` confirms reuse/split semantics

## Phase 2 - Zombie Listener Spawn Gate

Status: PASS

Adds the next muxcore-like health gate:

- a registered owner can be poisoned by closing its listener while leaving the
  registry entry behind
- the next spawn for the same server identity probes reachability
- unreachable registered owner is treated as zombie, removed, and replaced
- status exposes a `zombie_detected` counter

Pass signal:

- prior launcher checks still pass
- direct `--poc-probe-zombie-owner` confirms same `server_id`, new owner
  generation, and incremented zombie counter

## Phase 3 - Snapshot Restore Ready Gate

Status: PASS

Adds a restart state closer to muxcore:

- `graceful-restart` writes an owner registry snapshot before successor spawn
- successor starts with `ready=false`
- successor restores owner identities with fresh owner generations
- successor flips `ready=true` only after restore completes
- snapshot is removed after successful restore

Pass signal:

- prior launcher checks still pass
- `mcp-launcher phase2` final status reports restored owner registry rather
  than an empty daemon after restart

## Phase 4 - Live Same-Stdio Reconnect

Status: PASS

Adds the first check that keeps the same stdio shim process alive across daemon
restart, then adds the smallest reconnect machinery needed to pass it:

- start one shim process and complete `initialize` + `tools/list`
- trigger daemon `graceful-restart` externally
- wait for successor daemon `ready=true`
- call `tools/call topology_state` through the same stdin/stdout pipes
- shim caches the first `initialize` request
- on owner write/read failure, shim reconnects through daemon `spawn`
- after reconnect, shim replays cached `initialize` to warm the restored owner
- shim retries the current request on the new owner connection

Observed pre-fix break:

- break observed: the simple shim exits when the owner connection closes
- observed output:

```text
"break_observed":true
"error":"write |1: The pipe is being closed."
```

Current pass signal:

```text
"break_observed":false
"probe":"live_reconnect"
```

This identifies the first required no-break mechanism: resilient shim reconnect,
not daemon owner registry basics.

## Phase 5 - Concurrent In-Flight Reconnect

Status: PASS

Adds the next traffic condition without changing the topology:

- start one shim process and complete `initialize` + `tools/list`
- send a slow `tools/call topology_state` request with `sleep_ms=900`
- write a second `tools/call topology_state` request into the same stdin pipe
  before reading the first response
- trigger daemon `graceful-restart` while the slow request is still in-flight
- wait for successor daemon `ready=true`
- read both JSON-RPC responses from the same stdio shim

False-positive guards:

- successor daemon PID/generation must differ from the predecessor
- slow response must report `delay_ms >= 900`
- both slow and buffered responses must report the successor
  `daemon_generation`

Current pass signal:

```text
"probe":"inflight_reconnect"
"phase":"phase5"
"break_observed":false
"slow_payload":{"daemon_generation":"<successor>","delay_ms":900,...}
"buffered_payload":{"daemon_generation":"<successor>","delay_ms":900,...}
```

This shows the minimal serial reconnect loop covers one interrupted in-flight
request plus the next request already buffered into stdin. It does not prove
true concurrent owner dispatch, out-of-order response demux, orphaned response
drain, or refresh-token history.

## Phase 6 - True Concurrent Dispatch and Out-of-Order Response Demux

Status: PASS

Adds the first truly concurrent same-stdio traffic condition:

- owner sessions dispatch JSON-RPC requests concurrently instead of serially
- shim keeps a pending-response map keyed by JSON-RPC ID
- shim writes requests concurrently and demultiplexes out-of-order owner
  responses back to the same stdio stdout
- the probe sends two slow requests, proves `max_concurrent_calls >= 2`, then
  restarts the daemon while both are outstanding
- both requests reconnect to the successor owner and return from the successor
  daemon generation

Observed pre-fix break:

```text
"probe":"concurrent_demux"
"break_observed":true
"error":"owner concurrency did not reach 2"
"max_concurrent_calls":1
```

Current pass signal:

```text
"probe":"concurrent_demux"
"phase":"phase6"
"break_observed":false
"concurrent_status":{"owners":[{"active_calls":2,"max_concurrent_calls":2,...}]}
"response_order":[fast_id,slow_id]
"fast_payload":{"daemon_generation":"<successor>","delay_ms":700,"tag":"fast",...}
"slow_payload":{"daemon_generation":"<successor>","delay_ms":900,"tag":"slow",...}
```

This proves the PoC shim can handle multiple outstanding owner requests and
out-of-order responses by JSON-RPC ID across daemon restart. It still did not,
by itself, model production refresh-token history, real upstream subprocess
side effects, or production `muxcore` code imports.

## Phase 7 - Session Manager History and Refresh-Token Reconnect

Status: PASS

Adds the first token-continuity mechanism:

- daemon records consumed session tokens in a bounded history table
- graceful-restart snapshots owner identities plus consumed-token history
- successor restores owners with fresh owner generations and restores token
  history for those server IDs
- shim remembers the last consumed token for its stdio session
- on reconnect, shim calls control `refresh-token` before using fallback spawn
- daemon mints a fresh one-shot token for the restored owner when the previous
  token is known and the owner is alive
- probe proves the same stdio shim reconnects with refresh-token, replays
  initialize, and completes post-restart traffic without fallback spawn

Observed pre-fix break:

```text
"probe":"refresh_reconnect"
"break_observed":true
"refresh_used":false
"fallback_spawn_used":true
"token_changed":false
"last_reconnect_reason":"fallback_spawn"
```

Current pass signal:

```text
"probe":"refresh_reconnect"
"phase":"phase7"
"break_observed":false
"refresh_used":true
"fallback_spawn_used":false
"final_status":{"refresh_requests":1,"refresh_successes":1,"fallback_spawns":0,...}
"original_token":"<prev>"
"refreshed_token":"<different>"
```

This proves fallback spawn is not required when a restored owner is alive and
the daemon has a consumed-token history entry for the existing shim.

## Phase 8 - Generation-Aware Graceful Restart Handoff

Status: PASS

Adds explicit handoff observability:

- graceful-restart snapshot records predecessor daemon PID/generation
- snapshot records predecessor owner generation/socket per server ID
- successor status reports `handoff:"restored"` after snapshot restore
- successor status reports predecessor fields and `restored_owner_count`
- probe verifies the predecessor owner socket is no longer reachable after
  successor ready
- probe verifies post-restart traffic returns only from the successor daemon and
  restored owner generation

Observed pre-fix break:

```text
"probe":"generation_handoff"
"break_observed":true
"handoff":"none"
"predecessor_generation_matched":false
"old_owner_socket_reachable":false
```

Current pass signal:

```text
"probe":"generation_handoff"
"phase":"phase8"
"break_observed":false
"after_status":{"handoff":"restored","restored_owner_count":1,...}
"old_owner_socket_reachable":false
"payload":{"daemon_generation":"<successor>","owner_generation":"<restored-owner>",...}
```

This proves the PoC can make handoff generation state explicit enough to
distinguish fresh spawn, restored owner, predecessor generation, and successor
generation.

## Phase 9 - Persistent/Idle Reaper and Upstream Lifecycle Classification

Status: PASS

Adds owner lifecycle classification and idle ownership cleanup:

- spawn requests can mark an owner `persistent`
- status reports `persistent`, `active_sessions`, `idle_reaper_ttl_ms`, and
  `reaped_owner_count`
- non-persistent owners with no active sessions are reaped after the configured
  idle TTL
- persistent owners survive the same idle TTL
- non-persistent owners with an active owner session survive the same idle TTL
  and are reaped only after the active session closes
- reaping an owner removes pending spawn tickets for that owner identity
- a non-persistent owner respawns with the same server ID and a new owner
  generation after reaping
- graceful restart snapshots and restores persistent owner classification

Observed pre-fix break:

```text
"probe":"idle_reaper"
"break_observed":true
"non_persistent_reaped":false
"persistent_survived":false
"reaped_owner_count":0
```

Current pass signal:

```text
"probe":"idle_reaper"
"phase":"phase9"
"break_observed":false
"non_persistent_reaped":true
"persistent_survived":true
"active_session_survived":true
"active_reaped_after_close":true
"persistent_restored":true
```

This proves the experimental topology can distinguish expected idle cleanup
from lifecycle corruption. After Phase 9 the remaining uncertainty is no longer
well served by adding more toy mechanisms; the next useful step is production
parity in real `muxcore` tests and refactors, plus a later runtime smoke against
real MCP upstream processes.

## Production Port Boundary

Next action: port the converged Phase 6-9 invariants into production `muxcore`
tests and implementation. Do not treat the standalone PoC as a release gate by
itself.
