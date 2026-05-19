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
"buffered_payload":{"daemon_generation":"<successor>","delay_ms":0,...}
```

This shows the minimal serial reconnect loop covers one interrupted in-flight
request plus the next request already buffered into stdin. It does not prove
true concurrent owner dispatch, out-of-order response demux, orphaned response
drain, or refresh-token history.

## Candidate Next Phases

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
out-of-order responses by JSON-RPC ID across daemon restart. It still does not
model production refresh-token history, real upstream subprocess side effects,
or production `muxcore` code imports.

## Candidate Next Phases

- Phase 7: session manager history and refresh-token reconnect path
- Phase 8: generation-aware graceful restart handoff
- Phase 9: persistent/idle reaper and upstream process lifecycle
