# Technical Debt

All items from the 2026-04-08 debt batch have been resolved:

- ✅ **Progress notifications forwarding** — implemented earlier via
  `Owner.progressOwners`, `trackProgressToken`, and `routeProgressNotification`
  in `internal/mux/owner.go`. `notifications/progress` from upstream is now
  routed to the originating session using the `_meta.progressToken` mapping,
  with broadcast fallback if routing fails.

- ✅ **Tool call timeout** — implemented in v0.10.1 as `x-mux.toolTimeout`
  capability. Upstream servers declare their acceptable tool-call wait time
  (in seconds); mux starts a watchdog goroutine per `tools/call` request.
  If upstream doesn't respond within the window, the watchdog synthesizes
  a JSON-RPC error response (`code -32000`) and delivers it to the session.
  Uses atomic `sync.Map.LoadAndDelete` to race cleanly with the natural
  response path — whichever wins the claim delivers the response.

- ✅ **Inflight progress injection (research)** — moved to engram as a
  research task. The debt entry was explicitly marked "if feasible" and
  requires CC-side investigation of MCP progress-display support. Not
  suitable for implementation without upstream protocol research.

---

## In progress

(none)

## PR #145 review stopline

The final review exposed the following unresolved NVMD-145 contract gaps after
the bounded correction wave. They are recorded rather than expanded in-place;
**PR #145 must not merge or release until each item is fixed or independently
disproved against the accepted specification.**

- **Start preparation cancellation** — daemon preparation receives the attempt
  context but its product callback ignores it, so timeout/cancellation can leave
  late daemon-start side effects after the generation attempt has ended.
- **Bind-failure tree rollback** — the non-supervisor launcher path uses
  `cmd.Process.Kill/Wait` after `BindChildPID` failure instead of full Unix
  process-group / Windows Job authority.
- **Atomic parent receipt** — a successful attestation response can race
  `Parent.Close` before `Verified` is committed, leaving child success and
  parent admission state inconsistent.
- **Strict frame compliance** — reject invalid UTF-8 and non-JSON whitespace,
  enforce MCP object-shaped `params`/`result`, and distinguish JSON-RPC parse
  error `-32700` from structurally invalid request `-32600`.
- **Task capability and terminal lifetime** — task augmentation must follow the
  negotiated `tasks` capability, and terminal `tasks/get`/`tasks/cancel` results
  must retire task/progress correlation without relying on optional status
  notifications.
- **Replay protocol continuity** — a hidden replacement initialize response
  must not silently change the MCP `protocolVersion` already negotiated with
  the host.
- **Unclosed child wait** — supervisor cancellation/finalization needs a bounded
  way to release its reaper goroutine when a custom `Child.Wait` never closes,
  without treating cancellation as terminal process proof.
- **Acceptance evidence gaps** — Windows descendant-retirement tests must fail
  on unexpected `OpenProcess` errors, and the new/new compatibility fixture must
  assert successful attestation rather than infer it from engine version.

## Open items

The following v0.28.0 follow-ups are non-blocking and out of scope for the
current release:

- **Dormant rejection synchronization** — the current owner and control-plane
  safety gates should make READY-with-live-work unreachable; either prove that
  invariant end to end or define an explicit bounded rejection response so a
  misbehaving child cannot wait indefinitely.

- **Cancellation provenance fallback** — unknown or expired cancellation may
  currently target the current generation after inflight provenance is gone;
  consider dropping the fallback or retaining a short-lived generation
  tombstone.
- **Extremal retry coverage** — add a test with two prior general spawn retries,
  followed by two template mismatches and the cold fifth attempt.
- **Detached listener test flake** —
  `TestDetachedProcessListenerAcceptsParentDial` failed once in the full Windows
  muxcore suite with empty helper output but passed 10 focused repetitions;
  investigate separately.
- **Invalid `DisableTree` + `StartSuspended` option pair** — on Windows this
  combination can leave the child suspended because no procgroup Job authority
  performs the resume. Reject the pair explicitly if `StartSuspended` becomes a
  public option outside the supervisor's tree-managed command path.
- **Hostile Unix process-group escape** — NFR-4 deliberately selects portable
  Unix process groups. A deliberately hostile descendant can call
  `setsid`/`setpgid` and leave that authority; stronger containment needs a new
  cross-platform contract (for example Linux cgroup v2 plus an explicit Darwin
  policy), not descendant polling presented as proof.
- **Same-requested-identity fallback recovery** — a healthy stable-launcher
  fallback remains pinned while the resolved requested identity is unchanged;
  add a bounded retry policy before attempting in-place desired-engine recovery.
- **Long Unix attestation paths** — oversized `TMPDIR` values fail closed today;
  a shorter alternate root needs an explicit ownership and permission policy.
- **Attestation cancellation after dial** — child verification bounds dial and
  exchange separately but does not interrupt an established connection when
  the caller context is cancelled; preserve the hard I/O bound while wiring
  cancellation through the connection.
- **Installed-engine TOCTOU** — authorization verifies canonical path nodes and
  content before exec, but does not pin an immutable file/directory handle
  across validation and launch; closing this requires a new execution contract.
- **Signed engine provenance** — parent-side authorization now requires the
  exact active pointer, installed version-store layout, no symlink escape, and
  content matching the 12-hex version directory. Authenticating a malicious
  same-user replacement that creates its own matching hash requires signed
  release provenance and a separate trust-root design.
- **Trust-boundary error minimization** — public `ControlOf` and attestation
  errors may still include caller-supplied method or endpoint detail if a
  consumer logs returned errors verbatim. Consider fixed public
  classifications with private debug wrapping.


<!--
Resolved items are not tracked in this file; see git history + GH releases for audit trail.
- `upstream.Start` Wait-vs-ReadLine race → PR #67 (squash 30d6314), shipped in muxcore/v0.20.1 + v0.9.7.
-->

