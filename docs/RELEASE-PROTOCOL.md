# mcp-mux Release Protocol

This protocol is mandatory for public `mcp-mux` binary releases and
`muxcore` library releases. It complements `docs/PRODUCTION-TESTING-PLAYBOOK.md`
and the generic NVMD release gate.

## Required Verdicts

Use these verdict names in release evidence:

| Verdict | Meaning |
| --- | --- |
| `PROJECT_RELEASE_PROTOCOL_PASS` | All applicable project gates below have current evidence. |
| `PROJECT_RELEASE_PROTOCOL_BLOCKED` | A required gate could not be completed. Do not report the release as shipped for the blocked scope. |
| `CONSUMER_HANDOFF_NOT_REQUIRED` | The release does not contain a consumer-impacting `muxcore` change. |
| `CONSUMER_HANDOFF_PASS` | Every impacted consumer has a fresh Engram issue or comment with the released version and adoption instructions. |
| `CONSUMER_HANDOFF_BLOCKED` | Engram is unavailable, a required consumer cannot be addressed, or the released version is not yet known. |

## Standard Release Gates

Before tagging or publishing:

1. Work from a clean release branch synchronized with `origin/master`.
2. Run the repository tests from the root:

   ```powershell
   go test ./... -count=1
   ```

3. When `muxcore` changed, run the muxcore module tests:

   ```powershell
   Push-Location muxcore
   go test ./... -count=1
   Pop-Location
   ```

4. Run `go vet ./...` for source changes that affect runtime behavior or public
   API.
5. Run the production playbook scenarios that match the release surface. Use
   Scenario 5b when the release changes `muxcore` library contracts, daemon
   lifecycle, snapshot/cache behavior, reconnect, process management, update
   helpers, registry behavior, namespaces, or consumer-facing docs.
   For v0.27.0 lifecycle convergence, Scenario 8 is mandatory in addition to
   the repository critical suite:

   ```powershell
   .\tests\critical\run-all.ps1 -TimeoutSeconds 120
   ```

   The current-topology oracle also needs `mcp-launcher`. If it is not on
   `PATH`, pass `-Launcher C:\path\to\mcp-launcher.exe` or set
   `MCP_LAUNCHER`; the critical report records the resolved path.

   The existing critical suite is necessary but not sufficient for v0.27.0:
   release evidence must also cover idle-to-dormant wake, full-tree cleanup on
   Windows and Unix, one v1-to-v2 bounded snapshot respawn, and one same-v2
   handoff that retains tree authority. Record each command, exit code, and
   evidence artifact; do not infer cross-platform containment from one OS.
6. Create annotated tags for both surfaces when both are released:
   `vX.Y.Z` and `muxcore/vX.Y.Z`.
7. Verify remote tag parity with `git ls-remote --tags origin`.
8. Verify Go module resolution for muxcore releases:

   ```powershell
   go list -m -json github.com/thebtf/mcp-mux/muxcore@vX.Y.Z
   ```

## Upstream maintenance release gate

For the upstream maintenance hold change, run production Scenario 11 on the
exact integrated head on both Windows and Unix. Use the actual candidate binary,
the canonical `SourceRoot`/`CandidateBinary`/`OutputDir`/`TimeoutSeconds` inputs,
and explicit PRIMARY `.agent` `ScratchRoot`/`EvidencePath` inputs from
[`PRODUCTION-TESTING-PLAYBOOK.md`](PRODUCTION-TESTING-PLAYBOOK.md#scenario-11-upstream-maintenance-replacement).
Do not auto-enroll this long live smoke in unrelated test runs.

Release evidence must retain:

- Source SHA, candidate and fixture hashes, command/exit records, and nonzero
  live/focused denominators. Fixture compilation is not live replacement proof.
- Two unchanged legacy host process/pipe identities, managed leader/descendant
  death, actual overwrite of the executed file path, and zero held starts.
- Original numeric/string held errors, no notification reply, no cache success
  or replay, and one new-version generation after exact resume and safe TTL.
- Durable HELD readback after owned unplanned aware-daemon loss, terminal
  controlled restart/handoff/shutdown and staged activation refusal, unchanged
  launcher/pending/layout/pointer files, redaction, and unrelated-context
  continuity. The live runner does not fake blocked/corrupt authority.
- Fresh same-era modern admission with valid metadata and the unchanged modern
  fixture after resume, with zero injected legacy or replay frames. A generic
  reconnect error or EOF is not a successful modern continuation.
- Named focused queue/reconnect, start/install, and hold-versus-activation races,
  blocked retirement that never TTL-clears, corrupt/incomplete/persistence
  failures, finite exact context partitions, stale callbacks/leases, unsupported
  old endpoints, whole-millisecond CLI boundaries, and terminal library
  update-helper refusal. Prove read-only status/startup behavior and locked
  persisted-clear activation with offline/old endpoints. No zero-case or
  timing-only substitute is accepted.
- Scoped lifecycle cleanup and retained failed evidence/blocked authority.
  No production namespace, broad PID cleanup, ledger deletion, or direct
  standalone bypass is part of acceptance.

The existing root/muxcore tests and vet, applicable race/tree tests, critical
suite, native consumer Scenario 5b, Scenario 8 on Windows and Unix, and R1 native
parity runners remain required. A maintenance smoke PASS is one gate, not
`PROJECT_RELEASE_PROTOCOL_PASS` or consumer delivery.

Root selects actual binary/module versions only after acceptance, updates
version/changelog/release targets, integrates the exact accepted SHA, and tags
last. Check remote tag parity and module resolution for that selected version,
then prove a fresh-clone binary/module canary and delivered-artifact same-host
hold/overwrite/resume behavior in a fresh session. Current-version handoffs to
aimux, engram, and other affected consumers must be read back before closeout.

The handoff must name maintenance-aware managed shim semantics, the finite
admitted context scope rather than host-wide executable locking, old-daemon
unsupported behavior, old-shim physical-fence-only limits, fresh native modern
admission, and terminal lifecycle refusal under any fence. Clear all safely
proven holds using the current aware binary before downgrade. Blocked authority
must remain intact, even after TTL; unsupported rollback is an exact held
boundary, not permission to delete the ledger or run the upstream directly.
Controlled installation, launcher swap, layout/bootstrap mutation, and active
pointer updates serialize with hold-ledger mutation under the existing daemon
namespace file lock. Status/pure startup checks remain read-only and do not start
a daemon. Offline/old activation needs locked persisted-clear proof. This is not
a separate updater lease or host-wide executable-lock guarantee.

## Critical Muxcore Consumer Handoff Gate

This gate is required when a release contains a critical or consumer-impacting
`muxcore` update.

Treat a `muxcore` update as critical or consumer-impacting when it touches any
of these surfaces:

- MCP startup, `initialize`, `tools/list`, `prompts/list`,
  `resources/list`, or cache replay behavior.
- Daemon startup, owner spawn, process lifecycle, idle reaping, persistent
  owners, zombie cleanup, or process fanout.
- Shim reconnect, token refresh, snapshot restore, graceful restart, live
  update helpers, or handoff.
- Engine naming, namespace derivation, control sockets, owner sockets, locks,
  registry descriptors, or cross-engine discovery.
- Security, tenant/session authorization, peer identity, frame admission, or
  credential/environment isolation.
- Public `engine`, `daemon`, `owner`, `control`, `registry`, or `upgrade` APIs.
- Consumer-facing documentation that changes the current muxcore target or
  required adoption protocol.

For each critical or consumer-impacting muxcore release:

1. Identify impacted consumers. `aimux` and `engram` are always checked. Add
   any other known muxcore consumer named by the change, test evidence, issue,
   release note, or operator directive.
2. After the released muxcore version is known and resolvable, create or update
   Engram issues for every impacted consumer. Prefer updating an existing open
   adoption issue when it is still the same adoption track; otherwise create a
   new issue.
3. The issue/comment must include:
   - released muxcore tag, for example `muxcore/vX.Y.Z`;
   - severity and why the update is critical or consumer-impacting;
   - affected user-visible symptom or runtime invariant;
   - required consumer implementation steps;
   - forbidden workarounds or obsolete local hacks, when applicable;
   - expected consumer smoke tests and acceptance evidence;
   - rollback or compatibility notes;
   - provider-side evidence: commit, tests, release tag, and module-resolution
     proof.
4. Re-read every touched Engram issue before final release reporting and record
   the issue IDs and latest status in the release evidence.
5. If Engram is unavailable, record `CONSUMER_HANDOFF_BLOCKED`. The tag may
   exist, but the release closeout must say `partially verified` or `blocked`,
   not `SHIPPED` for the full critical muxcore scope.

### Consumer Issue Template

```markdown
## Required muxcore adoption

Provider release: `muxcore/vX.Y.Z`
Severity: critical | high | medium
Reason: <startup/reconnect/update/process/security/API impact>

### Why this consumer is affected

- <symptom or invariant>
- <consumer topology or muxcore surface involved>

### Required implementation

1. Bump `github.com/thebtf/mcp-mux/muxcore` to `vX.Y.Z`.
2. Remove or quarantine local workarounds that duplicate the fixed muxcore
   behavior.
3. Keep product-native lifecycle/status/update surfaces authoritative. Use
   `mcp-mux mux_engines` / scoped `mux_list(engine_name=...)` only when the
   product has opted into registry visibility and only as read-only evidence.

### Required verification

- `go mod verify`
- `go vet ./...`
- `go test ./...`
- Consumer customer-mode smoke through the public MCP/CLI entrypoint.
- If the update touches live restart/reconnect: same open session survives the
  update, reports the new product version, and shows refresh-based reconnect
  without fallback spawn/give-up.

### Provider evidence

- Commit: `<sha>`
- Tags: `vX.Y.Z`, `muxcore/vX.Y.Z`
- Module proof: `go list -m -json github.com/thebtf/mcp-mux/muxcore@vX.Y.Z`
```

## Closeout Checklist

Record this checklist in the release report:

- [ ] `PROJECT_RELEASE_PROTOCOL_PASS` or explicit blocker.
- [ ] Root tests passed.
- [ ] muxcore tests passed when muxcore changed.
- [ ] Repository critical suite passed.
- [ ] Runtime/customer playbook scenarios passed or were explicitly scoped out.
- [ ] Remote tags exist and point at the release commit.
- [ ] muxcore module resolution points at `muxcore/vX.Y.Z` when muxcore was released.
- [ ] `CONSUMER_HANDOFF_PASS` or `CONSUMER_HANDOFF_NOT_REQUIRED`.
- [ ] Engram issue IDs/comments are listed for all impacted consumers when the
      consumer handoff gate was required.
- [ ] Maintenance releases have Scenario 11 Windows/Unix live evidence and
      positive named focused maintenance proof, alongside existing 5b/8/R1 gates.
- [ ] Delivered-artifact and fresh-session maintenance proof use the root-selected
      version; consumer handoffs state aware/old-shim/old-daemon/rollback limits.
