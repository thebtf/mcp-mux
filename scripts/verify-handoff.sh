#!/usr/bin/env sh
# verify-handoff.sh — post-deploy verification for upstream-survives-daemon-restart (T036).
#
# Asserts that `mcp-mux upgrade --restart` preserves every upstream PID across
# the daemon restart (v0.21.0+ handoff path) OR documents the legacy fallback
# path when the old daemon predates the handoff protocol.
#
# Exit codes:
#   0 = PASS (all upstream PIDs survived OR operator explicitly ran against
#            legacy daemon — FR-9 zero-deployment-impact guarantee held)
#   1 = FAIL (PIDs diverged without an FR-8 fallback signal — regression)
#   2 = SETUP ERROR (mcp-mux not on PATH, no daemon running, jq missing, etc.)
#
# Usage:
#   scripts/verify-handoff.sh                     # use default mcp-mux binary
#   MCP_MUX_BINARY=/opt/mcp-mux scripts/verify-handoff.sh
#   VERIFY_TIMEOUT=30 scripts/verify-handoff.sh   # wait-for-ready budget in seconds

set -eu

BINARY="${MCP_MUX_BINARY:-mcp-mux}"
TIMEOUT="${VERIFY_TIMEOUT:-15}"

log()  { printf '[verify-handoff] %s\n' "$*" >&2; }
die()  { log "FATAL: $*"; exit 2; }
fail() { log "FAIL: $*"; exit 1; }

command -v "$BINARY" >/dev/null 2>&1 || die "mcp-mux binary not found: $BINARY"
command -v jq        >/dev/null 2>&1 || die "jq not found on PATH (required for JSON parsing)"

status_json() {
  "$BINARY" status 2>/dev/null || die "mcp-mux status failed — is the daemon running?"
}

# Snapshot of pid<->server_id pairs, sorted by sid for stable diff.
pids_snapshot() {
  status_json | jq -r '
    (.servers // [])
    | map(select(.pid != null) | {sid: .server_id, pid: .pid, cmd: .command})
    | sort_by(.sid)
    | .[]
    | "\(.sid)\t\(.pid)\t\(.cmd)"
  '
}

handoff_counter() {
  status_json | jq -r --arg k "$1" '(.handoff // {})[$k] // 0'
}

log "step 1/5: capturing pre-restart state"
BEFORE="$(pids_snapshot)"
if [ -z "$BEFORE" ]; then
  die "no upstream PIDs reported before restart — start at least one upstream before verifying"
fi
BEFORE_ATTEMPTED="$(handoff_counter attempted)"
BEFORE_TRANSFERRED="$(handoff_counter transferred)"
BEFORE_FALLBACK="$(handoff_counter fallback)"
BEFORE_COUNT="$(printf '%s\n' "$BEFORE" | wc -l | tr -d ' ')"
log "  observed $BEFORE_COUNT upstream(s); pre counters: attempted=$BEFORE_ATTEMPTED transferred=$BEFORE_TRANSFERRED fallback=$BEFORE_FALLBACK"

log "step 2/5: invoking mcp-mux upgrade --restart"
"$BINARY" upgrade --restart

log "step 3/5: waiting up to ${TIMEOUT}s for successor daemon ready"
ELAPSED=0
while [ "$ELAPSED" -lt "$TIMEOUT" ]; do
  if "$BINARY" status >/dev/null 2>&1; then
    log "  daemon ready after ${ELAPSED}s"
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done
[ "$ELAPSED" -lt "$TIMEOUT" ] || fail "successor daemon did not come up within ${TIMEOUT}s"

log "step 4/5: capturing post-restart state"
AFTER="$(pids_snapshot)"
AFTER_ATTEMPTED="$(handoff_counter attempted)"
AFTER_TRANSFERRED="$(handoff_counter transferred)"
AFTER_FALLBACK="$(handoff_counter fallback)"
AFTER_COUNT="$(printf '%s\n' "$AFTER" | wc -l | tr -d ' ')"
log "  observed $AFTER_COUNT upstream(s); post counters: attempted=$AFTER_ATTEMPTED transferred=$AFTER_TRANSFERRED fallback=$AFTER_FALLBACK"

log "step 5/5: comparing PID sets"
DIFF="$(diff -u <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$AFTER") || true)"
if [ -z "$DIFF" ]; then
  log "PASS: all $AFTER_COUNT upstream PIDs survived the restart"
  log "       handoff counters delta: attempted+$((AFTER_ATTEMPTED - BEFORE_ATTEMPTED)) transferred+$((AFTER_TRANSFERRED - BEFORE_TRANSFERRED)) fallback+$((AFTER_FALLBACK - BEFORE_FALLBACK))"
  exit 0
fi

# PIDs diverged. If the fallback counter increased, that's FR-8 — expected for legacy.
if [ "$AFTER_FALLBACK" -gt "$BEFORE_FALLBACK" ]; then
  log "PASS (fallback): handoff_fallback incremented by $((AFTER_FALLBACK - BEFORE_FALLBACK))"
  log "       this is the FR-8 zero-deployment-impact path (old daemon had no handoff code,"
  log "       or protocol versions mismatched). Upstreams were respawned — next restart"
  log "       will use the handoff path."
  log "PID changes:"
  printf '%s\n' "$DIFF" | sed 's/^/  /' >&2
  exit 0
fi

log "PID diff (unexpected — no fallback counter bump):"
printf '%s\n' "$DIFF" | sed 's/^/  /' >&2
fail "upstream PIDs diverged without a fallback signal — handoff regression suspected"
