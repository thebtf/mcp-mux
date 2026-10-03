# Upstream maintenance hold release evidence

## Milestone and scope

This is the T029 evidence artifact for GitHub #135. The accepted ADR-015 full-slice scope inherits ordinary release intent. It is not source-only work.

| State | Recorded outcome |
| --- | --- |
| Implemented | Core, optional public APIs, CLI/MCP adapters, consumer docs, fixtures, and cross-platform live runner are implemented through `1f3c119ef6841b219412e4e6a93f6a452b7339a8`; current candidate `0b443b0ea51078814e76d12af0632fb9b5d52cc0` adds only the four owning milestone documents. |
| Verified | Root admits T029 complete: focused core and adapter maintenance proof, Windows/Linux live replacement, and selected Go 1.25.12 R1 parity on both platforms. Full tests/vet, integrated race, and bounded seven-finding source closeout retain their recorded scope below. |
| Technically accepted | Not yet. T030 critical aggregate and Scenario 5b/8, including native Unix coverage, remain pending; T031 final root acceptance awaits those outcomes. T029 completion is not feature acceptance. |
| Delivered | No. No release version has been selected, no PR or release tags created, and no current-module publication or consumer delivery recorded. T032/T033 remain incomplete. |

This documentation update inspected existing receipts only. It ran no tests, builds, vet, linters, formatters, or repeated gates and made no commit. The plan, acceptance scenarios, and required commands in [tasks](tasks.md), [specification](spec.md), and [quickstart](quickstart.md) remain authoritative. Later root gate outcomes must be appended before acceptance or delivery is claimed.

## Source and artifact identity

All local raw-evidence paths below are relative to the **primary** checkout `D:/Dev/mcp-mux`, not the candidate's inner `.agent` directory.

| Identity | Value and relation |
| --- | --- |
| Source base | `3881f27b931f6b9d0467c0a15dd4e1824969e125` |
| Candidate root | `D:/Dev/mcp-mux/.agent/worktrees/upstream-maintenance-01a0fb9a` |
| Full tests/vet source | `42ef546d146b1e0bc9541f64e9c228d6ba5d3358` |
| Live replacement source | `45d4f93ec7c2703db1a57dfa48efc477accb1386` |
| Current source candidate | `0b443b0ea51078814e76d12af0632fb9b5d52cc0` |
| `42ef546` to `45d4f93` | Known complete changed-path set is only `scripts/smoke-upstream-maintenance.ps1`. Go product and regression source are unchanged across this interval. |
| `45d4f93` to `1f3c119` | Known complete changed-path set is only `muxcore/daemon/maintenance_test.go`, the TTL fixture repair, with 7 additions and 1 deletion. Go product source remains unchanged; the test corpus is not identical. |
| `1f3c119` to `0b443b0` | Root committed only `specs/002-upstream-maintenance-hold/{release-evidence.md,tasks.md,quickstart.md,spec.md}`. Product and regression source are unchanged. |

The changed-path relationships are recorded in `agent://MaintenanceReviewCloseout` and root's supplied commit-path evidence. This writer did not rerun Git comparison or rebuild the candidate. Root admits reuse of the `42ef546` full-test/vet and `45d4f93` live receipts within their unchanged-source scope, including the TTL-test-only `1f3c119` and docs-only `0b443b0` successors. This is **not** a claim that those commands ran freshly at exact `1f3c119` or `0b443b0`, nor that independently built binaries at different SHAs have identical hashes. The integrated race ran from exact committed `1f3c119`; the new focused adapter command ran from exact committed `0b443b0`.

| Live artifact | SHA-256 |
| --- | --- |
| Windows candidate `build/mcp-mux-45d.exe` | `A35902E70AA406227334F9BB4ACB413FC3A891972594414433A0FFA161068483` |
| Linux candidate `/repo/c45d/.agent/tmp/u/mcp-mux-45d` | `3D64B408AA338A4A5E27177946C33389169142D167561BD1DED49E61F7F1F8B3` |
| Windows lifecycle v1 | `3FB7315F3F044A159A802458BF895FE1ABE7256108AC3A7302E2FF94875B3902` |
| Windows lifecycle v2 | `5AD5086F3030D1A19F454F5A17666F95427FAA7D371CCFF98029A311E3750207` |
| Windows existing modern fixture | `6A965B3CE8B7181C46B5CCB63035A91ADF3AB46E5DDD0E4A0710964D4855070F` |
| Linux lifecycle v1 | `D6C0F7D9CCC344EF0244D886E5CBDE06EAF65FD92729B75D1661A501764DDB53` |
| Linux lifecycle v2 | `C6B1796B4E0B9A8A9BA37C615756C5BDAEA757F0C52D9FC869CA9FEE166DFDBF` |
| Linux existing modern fixture | `196FA96C33BEE1716840C4923ED714F03C24D5CA6CF013B03D64D43E8FAB6074` |

Windows JSON labels the candidate externally built and delegates source/binary binding to root. `WindowsMaintenanceLiveProof` supplies the exit-zero build receipt at `45d4f93` and the same candidate hash. Linux JSON labels its artifact a candidate-root binary; `DockerLinuxMaintenanceProof` supplies its `/repo/c45d` build receipt and source SHA. These bindings come from recorded builds and hashes, not this writer's rebuild.

## Observed live replacement commands

The following are recorded invocations, not instructions to rerun them. For the Windows commands, `C` denotes the candidate root above and `P` denotes `D:/Dev/mcp-mux/.agent/tmp/m135-proof-01a0fb9a`. The Windows worker recorded `GOTOOLCHAIN=go1.25.12`, `TMP=P`, and `TEMP=P` for both commands. Its reported runtime is `go1.25.12 windows/amd64`.

```text
go build -trimpath -o P/build/mcp-mux-45d.exe ./cmd/mcp-mux
pwsh -NoProfile -File C/scripts/smoke-upstream-maintenance.ps1 -SourceRoot C -CandidateBinary P/build/mcp-mux-45d.exe -ScratchRoot P -OutputDir P/windows-live-45d -EvidencePath P/windows-live-45d.json -TimeoutSeconds 120
```

Both commands exited 0. Build output is in `P/build-45d.stdout.log` and `P/build-45d.stderr.log`. The smoke's authoritative JSON is `P/windows-live-45d.json`; companion artifacts are `P/windows-live-45d/{summary.json,transcript.ndjson}`, `P/windows-live-45d.stdout.log`, and `P/windows-live-45d.stderr.log`.

The Linux commands ran in `/repo/c45d` in Docker container `54909ae8de2b19a9b7f2b36858376a31453dc9dee7a097e81b007475c74a4bf3`, on Debian GNU/Linux 13 trixie. The build receipt records `GOTOOLCHAIN=go1.25.12` and `/usr/local/go/bin` on `PATH`; both commands use `TMPDIR=/repo/c45d/.agent/tmp/u`.

```text
go build -trimpath -o /repo/c45d/.agent/tmp/u/mcp-mux-45d ./cmd/mcp-mux
pwsh -NoProfile -File scripts/smoke-upstream-maintenance.ps1 -SourceRoot /repo/c45d -CandidateBinary /repo/c45d/.agent/tmp/u/mcp-mux-45d -ScratchRoot /repo/c45d/.agent/tmp/u -OutputDir /repo/c45d/.agent/tmp/u/linux-live-45d -EvidencePath /repo/c45d/.agent/tmp/u/linux-live-45d.json -TimeoutSeconds 180
```

Both commands exited 0. Copied primary evidence is `P/docker/linux-live-45d/{linux-live-45d.json,linux-live-45d.build.log,linux-live-45d.stdout.log,summary.json,transcript.ndjson}` plus the retained fixture records. The JSON SHA-256 is `b6ddbe672a17433b6fbdddffa421b6a00c0046aa6058b36822600114fd325561`; transcript SHA-256 is `9133f2c1262f13e950a6838c992c30e305acd8d2eaf0e760b4736765db0b6efd`, as recorded by the Linux worker.

### Live denominators and observations

The assertion counts below were read from the complete JSON arrays, not the reader's collapsed preview or a maker's confidence statement.

| Observation | Windows | Debian Linux |
| --- | --- | --- |
| JSON verdict | PASS | PASS |
| Named assertion instances | 1158 passed / 1158 total, 0 failed | 1191 passed / 1191 total, 0 failed |
| Recorded child commands | 35 | 44 |
| Expected refusal commands | 5 exit-one commands, validated by passing refusal assertions | 5 exit-one commands, validated by passing refusal assertions |
| Original host records | 5, including both legacy hosts | 5, including both legacy hosts |
| Exact owned process identity records | 23 | 23 |
| Cleanup | 3 successful namespace shutdown acknowledgements, 0 cleanup errors | 3 successful namespace shutdown acknowledgements, 0 cleanup errors |
| JSON `error` | Empty | Empty |
| Finished UTC | `2026-10-03T04:11:23.6903717Z` | `2026-10-03T04:11:15.3842276Z` |

The five expected nonzero commands on each platform are competing `hold`, stale `resume`, `stop --force`, `upgrade --restart-active`, and `upgrade --restart`. They are required refusal observations, not failed smoke runs. Assertion totals include repeated state, pipe, redaction, and process observations; they are not counts of independent scenarios or package tests.

The retained JSON and transcripts record:

- Two legacy hosts share one leader/descendant tree. Windows original legacy host PIDs are 106792 and 60656, with stdin/stdout handle pairs 2336/2284 and 2468/2476. Linux original legacy host PIDs are 16754 and 16784, with handle pairs 162/170 and 172/174. Fresh demand after resume and safe TTL expiry reports v2 on those original pipes from one fresh shared generation.
- HELD requires retired trees and future expiry. Exact scoped identities are observed before retirement and confirmed dead. The executed fixture path is actually overwritten, changing the v1 hash to the v2 hash listed above. This is not rename-only Unix replacement proof.
- Numeric-ID and string-ID held requests get original-ID errors. Held request/notification and recovery-buffered markers have zero upstream receipts or replay. Already forwarded markers occur once, and the retired unfinished operation has zero completion/replay. The drain completion precedes its accepted single deadline.
- Renewal, competing acquisition, stale lease, terminal lifecycle refusal, aware daemon-loss recovery, unrelated-context continuity, and safe readback assertions pass in their recorded live scope.
- Native modern discovery and fresh same-era admission on the original pipe pass without legacy bootstrap or held replay. This does not promise transparent restoration of progress or subscriptions.
- The workers' terminal receipts record 5/5 host exits and closure of all 23 owned process identities. Cleanup is scoped to fixture-owned resources, not unrelated live MCP owners.

Both JSON files explicitly limit their scope to live managed upstream replacement, **not full feature or release acceptance**. They also list remaining focused obligations, including deterministic queue/reconnect and start/install/activation races, blocked retirement, invalid authority, unsupported endpoints, update-helper refusal, finite context sets, and duration boundaries. A live PASS does not remove those obligations or the separate R1 gates.

## Root full tests, vet, and race evidence

Root supplied the command/exit receipt titled `Binding exact declared full checks to immutable final source`, retained as `artifact://188`, and the raw logs below. All four `42ef546` command exits are explicitly 0 in that receipt. Vet success is not inferred from its silent log.

| Source and working directory | Observed command | Exit and denominator | Primary raw log |
| --- | --- | --- | --- |
| `42ef546`, candidate root | `go test ./... -count=1` | 0; 2 tested packages PASS, 3 packages have no test files | `P/root-full-42ef.log` |
| `42ef546`, candidate `muxcore` | `go test ./... -count=1` | 0; 24 tested packages PASS, 1 package has no test files | `P/muxcore-full-42ef.log` |
| `42ef546`, candidate root | `go vet ./...` | 0; no diagnostics | `P/root-vet-42ef.log` |
| `42ef546`, candidate `muxcore` | `go vet ./...` | 0; no diagnostics | `P/muxcore-vet-42ef.log` |
| `45d4f93` plus only the TTL-fixture patch later committed as `1f3c119`, candidate `muxcore` | `go test -race ./control ./daemon ./owner ./engine ./upstream -run ^TestMaintenance -count=1 -timeout=180s -v` | 0; 143 named PASS cases, including subcases, across 5 packages; 1 Windows SKIP | `P/race-maintenance-renew-green.log` |
| Exact committed `1f3c119`, candidate `muxcore` | `go test -race ./control ./daemon ./owner ./engine ./upstream -count=1 -timeout=240s` | 0; 5/5 packages PASS | `P/race-integrated-1f3.log` |
| Exact committed `0b443b0`, candidate root | `go test ./cmd/mcp-mux ./internal/mcpserver -run ^TestMaintenance -count=1 -timeout=120s -v` | 0; 66 named PASS rows across 2/2 packages, no named FAIL or SKIP | `P/maintenance-adapters-0b4.log` |

The race commands' root receipt records `GOTOOLCHAIN=go1.25.12` and `TMP`, `TEMP`, `TMPDIR`, and `GOTMPDIR` bound to primary `P`. No unrecorded environment or timestamp is assigned to the older full-test/vet commands.

The focused log has 46 top-level maintenance PASS rows and 97 passing subcases, for 143 named PASS rows. Its additional named case `TestMaintenanceSecurity001EndpointAliasKeepsRecoveryFence` is SKIP on Windows. That Unix-only case is not counted as a Windows pass. Linux live replacement does not independently attribute a package-level result to that case.

The integrated exact-`1f3c119` race log records control 9.237s, daemon 117.000s, owner 70.791s, engine 11.487s, and upstream 14.370s. It supplies package results, not per-case denominators.

Root's new focused adapter receipt records Go 1.25.12 and primary scratch environment. The raw log records command package 4.637s and MCP server package 0.133s. Its 66 named PASS rows include subcases, not 66 independent top-level tests. This supplies the adapter portion of the now-admitted T029 focused maintenance proof without changing historical RED task claims.

The earlier preserved `P/race-maintenance.log` is RED for the 500ms acquisition fixture. Root's `MaintenanceRaceTTLRepair` source trace found that TTL begins before real race-built helper retirement, so acquisition correctly refused an already-expired usable lease. The fixture now acquires with the default TTL, proves HELD and retired trees, and renews the exact lease to 500ms before testing short expiry. Production code did not change. The subsequent focused GREEN is on those patched working-tree bytes before commit, not a test launched from committed `1f3c119`. This isolated observed RED/GREEN does not reconstruct every historical failing-before task clause.

## Selected R1 parity and T029 completion

Root read back both selected summaries and admits T029 complete. The selected runs use actual Go 1.25.12, not a newer base Go executable with an ineffective toolchain environment override.

| Selected R1 proof | Windows | Unix |
| --- | --- | --- |
| Primary summary | `P/r1-windows-1f3/summary.json` | `P/docker/r1-unix-45d-go125-toolchain/summary.json` |
| Source SHA | `1f3c119ef6841b219412e4e6a93f6a452b7339a8` | `45d4f93ec7c2703db1a57dfa48efc477accb1386` |
| Reported Go | `go version go1.25.12 windows/amd64` | `go version go1.25.12 linux/amd64` |
| Result and exit | PASS, exit 0 | PASS, exit 0 |
| Scenario denominator | 8/8 PASS | 8/8 PASS |
| Modern corpus denominator | 100/100 PASS | 100/100 PASS |
| Candidate SHA-256 | `a35902e70aa406227334f9bb4acb413fc3a891972594414433a0ffa161068483` | `c0ebb16fb8de29799d4e3e1513aa40ebd10d803d4579de5d272b0254d8ac48fa` |
| Modern fixture SHA-256 | `d87681e341df6903862e854f84bf5cf5454c899ba22ce4c49cf1ab6053346e16` | `ae7172808b45dd6317ee901055a471e187033d0685cdd668b04c9650ef96e56f` |
| Corpus SHA-256 | `e616d970bde2282ca97dfe4db2acb434008baf75cecade002f190938ce22588a` | `b61ae9d4eef44d153b3b0e88f2ce717a811bf06bef4f1baf87dca428753514eb` |
| Transcript SHA-256 | `5ee775a5841153e866e8dacdf2a49a5529a158952965c6cc7f7644fa53ed9786` | `bd22375492b36dc013cb1e687a7c8e6c10d5685f9507fa64875c39fbd8b060d4` |
| Base directory readback | `base_dir_removed=true`, `base_dir_preserved=false` | `base_preserved=false`; no separate removed flag in this schema |

The selected Windows command is `pwsh -NoProfile -File C/scripts/verify-r1-native-isolation.ps1 -SourceRoot C -OutputDir P/r1-windows-1f3`, with `GOTOOLCHAIN=go1.25.12`, `TMP=P`, and `TEMP=P`. The existing Windows output directory was prepared before the successful invocation. The receipt supplies exit 0 and retains `transcript.ndjson`, `artifacts/evidence-hashes.json`, and per-scenario artifacts beneath that output directory, plus primary stdout/stderr logs.

The selected Unix command is `bash scripts/verify-r1-native-isolation.sh --source-root /repo/c45d --output-dir /repo/c45d/.agent/tmp/u/r1-unix-45d-go125-toolchain`, with `MCP_MUX_R1_GOMODCACHE=/repo/.agent/tmp/u/gomod` and `MCP_MUX_R1_GO=/repo/.agent/tmp/u/gomod/golang.org/toolchain@v0.0.1-go1.25.12.linux-amd64/bin/go`. Those are observed container paths, not portable quickstart defaults. The receipt supplies exit 0. Its selected primary directory also retains `transcript.ndjson`, six scenario artifacts, and `r1-unix-45d-go125-toolchain.stdout.log`.

Earlier Unix runs in `P/docker/r1-unix-45d/summary.json` and `P/docker/r1-unix-45d-go125/summary.json` produced successful scenario/corpus results under **Go 1.26.6 and are NOT selected Go 1.25.12 evidence**. The verifier pins `GOTOOLCHAIN=local`, so selecting `/usr/local/go/bin/go` did not select the downloaded 1.25.12 toolchain. The final selected summary confirms the direct 1.25.12 binary.

Setup failures remain preserved, not converted to passes: Windows initially exited 2 because OutputDir did not exist; Unix initially exited 2 for unset `MCP_MUX_R1_GOMODCACHE`, then exited 2 for nonempty OutputDir. Unix logs remain at `P/docker/r1-unix-45d/{setup-missing-cache.log,setup-nonempty-output.log}`; Windows initial stdout/stderr references remain in its worker receipt. These attempts are not selected gate evidence.

T029 is now root-admitted complete on the focused core and adapter receipts, both live OS replacement receipts, and both selected R1 parity receipts, with source reuse bounded as declared above. R1's numbered Scenario 8 is modern-owner operator rollback. It does **not** establish the production playbook's Unix held-reaper/full-tree Scenario 8 coverage, which remains a T030 obligation. No T031 technical acceptance or T032/T033 delivery is inferred.

## Seven-finding source review closeout

`agent://MaintenanceReviewCloseout` reports PASS at `1f3c119ef6841b219412e4e6a93f6a452b7339a8`, with **7/7 previously admitted findings fixed with source evidence** and no surviving finding in that bounded review. Its production source is unchanged from `45d4f93`. The reviewer ran no tests, builds, or new repros.

| Finding | Recorded source resolution | Proof limit |
| --- | --- | --- |
| ContractF1 | Request-admission leases end before transport writes; pending/inflight process ownership remains reserved; retirement closes the exact installed generation. | Deterministic blocked-writer seam, not an independent live OS pipe-saturation repro. Historical `request-seam-green.log` has an owner PASS subsection but a later upstream failure; it is not wholly GREEN. Later `softclose-green.log` and full `42ef546` package receipts cover upstream success. |
| ContractF2 | EOF-dequeued frames return to reconnect disposition and rejected deferred work is cleared/accounted once. | Existing numeric/string/notification seam receipts, not all disconnect schedules. |
| ContractF3 | Consumed native opening is retained, typed held startup errors enter maintenance, and host ingress follows maintenance setup. | Real-main test executable covers numeric/string openings; the live smoke alone is not proof of this exact startup seam. |
| S3-M135-001 | Endpoint identity canonicalizes the existing parent and preserves the endpoint leaf for both recovery and activation. | Unix-only source/regression inspected; no new symlink/stale-socket repro or individually attributable Unix execution receipt in this closeout. |
| S3-M135-002 | Persisted context keys require canonical `v1:` plus lowercase full hexadecimal digest. | Canonical schema rejection, not authentication against arbitrary same-principal replacement. |
| S3-M135-003 | Ledger and certificate decoding rejects duplicate/case-aliased object members before typed decoding. | Bounded source and transaction regression evidence, not an exhaustive JSON parser proof. |
| S3-M135-004 | Durable preparation precedes target publication; only acknowledged target publication can receive a COMMITTED certificate. Uncertain pairs fail closed. | No physical filesystem/fsync fault injection. The accepted finalize-after-publication amendment permits valid COMMITTED recovery while the API still reports persistence failure and live memory remains conservative. |

The closeout is only the seven-finding source portion of T031. It is not final root review of frozen evidence, implementation acceptance, or release approval. Reviewer model family is unknown; cross-family independence is not claimed. Its prior assertion-count limitation is not silently upgraded into reviewer execution: this documentation writer separately read the complete live JSON arrays for the counts above.

## Pending gates and exact remaining outcomes

These rows are factual pending outcomes at this milestone snapshot, not a generic blocked verdict. Existing GREEN receipts remain valid within their recorded bounds. Root retains the full release outcome and adds later observations through the same evidence writer.

| Task / gate | Current recorded state | Outcome still required |
| --- | --- | --- |
| T030 critical suite | Prior child execution was interrupted/aborted after roughly 30 minutes. `CriticalExecutionRecovery` is recovering actual exit/hang evidence. No critical PASS is recorded. | Terminal outcome and exact raw evidence for `tests/critical/run-all.ps1 -TimeoutSeconds 120`, including required launcher configuration and any recovery disposition. |
| T030 Scenario 5b | Evidence recovery is pending. No completed native-update scenario receipt is incorporated. | Observed native-sessionhandler update result, original connection/no-replay evidence, exact source/binary binding and exit. |
| T030 Scenario 8 | Native Unix held-reaper/full-tree coverage remains pending. R1 Scenario 8 is operator rollback, not this coverage. Earlier `P/docker/linux-focused-valid.log` reports failed whole-tree death and a too-long engine socket path; `linux-full-fixture-green.log` has only package PASS rows without source SHA or named cases. | Applicable source/toolchain-bound Windows and Unix process-group/Job/lifecycle evidence and explicit scenario disposition, including named Unix held-reaper/full-tree proof. Stale failing or unbound package-only receipts do not establish it. |
| T031 final root review | Seven source findings closed within their assigned scope; T029 admitted complete. The complete candidate/frozen evidence is not technically accepted while T030 remains pending. | Root review of all requirements, public APIs, migration/rollback docs, T030 outcomes, and any residual findings. |
| T032 release | Ordinary release intent retained. No selected version, PR, merge, tags, current-module publication, or binary delivery recorded. | Post-acceptance version decision, changelog/release notes, authorized exact-SHA merge/tag-last flow, remote tag parity and module/binary delivery. |
| T033 consumers and canary | Current-version handoffs, fresh-clone/module/binary canary, and fresh-session delivered hold/replace/resume proof are not recorded. Engram native issue capability is unmounted; no backend substitute is invented. | Target-authorized consumer handoff/readback through the owning capability, plus delivered-artifact and fresh-session proof. Record the exact missing capability boundary if it remains unavailable. |

Neither `PROJECT_RELEASE_PROTOCOL_PASS` nor `CONSUMER_HANDOFF_PASS` is claimed. Source implementation, bounded verification, technical acceptance, and consumer delivery remain separate states.
