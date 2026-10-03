# Upstream maintenance hold release evidence

## Milestone and scope

This is the T029 evidence artifact for GitHub #135. The accepted ADR-015 full-slice scope inherits ordinary release intent. It is not source-only work.

| State | Recorded outcome |
| --- | --- |
| Implemented | Core, optional public APIs, CLI/MCP adapters, consumer docs, fixtures, and cross-platform live runner are implemented in candidate `1f3c119ef6841b219412e4e6a93f6a452b7339a8`. |
| Verified | Windows and Debian Linux live replacement at `45d4f93`, root/muxcore full tests and vet at `42ef546`, focused maintenance race on the subsequently committed TTL-fixture patch, integrated five-package race at exact `1f3c119`, and bounded seven-finding source closeout. |
| Technically accepted | Not yet. T029 R1 parity, T030 critical/scenario outcomes, and T031 final root review remain pending in this snapshot. |
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
| Current source candidate | `1f3c119ef6841b219412e4e6a93f6a452b7339a8` |
| `42ef546` to `45d4f93` | Known complete changed-path set is only `scripts/smoke-upstream-maintenance.ps1`. Go product and regression source are unchanged across this interval. |
| `45d4f93` to `1f3c119` | Known complete changed-path set is only `muxcore/daemon/maintenance_test.go`, the TTL fixture repair, with 7 additions and 1 deletion. Go product source remains unchanged; the test corpus is not identical. |

The changed-path relationships are recorded in `agent://MaintenanceReviewCloseout` and root's supplied commit-path evidence. This writer did not rerun Git comparison or rebuild the candidate. Reuse of the `42ef546` full-test/vet and `45d4f93` live receipts is bounded to unchanged source. It is **not** a claim that those commands ran freshly at exact `1f3c119`, nor a claim that independently built binaries at different SHAs have identical hashes. Only the integrated race receipt below is an exact committed-`1f3c119` execution.

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

The race commands' root receipt records `GOTOOLCHAIN=go1.25.12` and `TMP`, `TEMP`, `TMPDIR`, and `GOTMPDIR` bound to primary `P`. No unrecorded environment or timestamp is assigned to the older full-test/vet commands.

The focused log has 46 top-level maintenance PASS rows and 97 passing subcases, for 143 named PASS rows. Its additional named case `TestMaintenanceSecurity001EndpointAliasKeepsRecoveryFence` is SKIP on Windows. That Unix-only case is not counted as a Windows pass. Linux live replacement does not independently attribute a package-level result to that case.

The integrated exact-`1f3c119` race log records control 9.237s, daemon 117.000s, owner 70.791s, engine 11.487s, and upstream 14.370s. It supplies package results, not per-case denominators.

The earlier preserved `P/race-maintenance.log` is RED for the 500ms acquisition fixture. Root's `MaintenanceRaceTTLRepair` source trace found that TTL begins before real race-built helper retirement, so acquisition correctly refused an already-expired usable lease. The fixture now acquires with the default TTL, proves HELD and retired trees, and renews the exact lease to 500ms before testing short expiry. Production code did not change. The subsequent focused GREEN is on those patched working-tree bytes before commit, not a test launched from committed `1f3c119`. This isolated observed RED/GREEN does not reconstruct every historical failing-before task clause.

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
| T029 R1 Windows | Assigned parity run is in progress; no final result incorporated here. | Exact candidate command, nonzero denominator, exit/result and raw artifact readback from `verify-r1-native-isolation.ps1`. |
| T029 R1 Unix | Assigned parity run is in progress; no final result incorporated here. | Exact candidate command, nonzero denominator, exit/result and raw artifact readback from `verify-r1-native-isolation.sh`. Linux maintenance live PASS is not R1 parity PASS. |
| T030 critical suite | Prior child execution was interrupted/aborted after roughly 30 minutes. `CriticalExecutionRecovery` is recovering actual exit/hang evidence. No critical PASS is recorded. | Terminal outcome and exact raw evidence for `tests/critical/run-all.ps1 -TimeoutSeconds 120`, including required launcher configuration and any recovery disposition. |
| T030 Scenario 5b | Evidence recovery is pending. No completed native-update scenario receipt is incorporated. | Observed native-sessionhandler update result, original connection/no-replay evidence, exact source/binary binding and exit. |
| T030 Scenario 8 | Applicable lifecycle/tree evidence recovery is pending. Full suite and maintenance smoke receipts do not certify the whole scenario. | Applicable Windows and Unix process-group/Job/lifecycle evidence and its explicit scenario disposition. |
| T030 exact-source reuse | Older full tests/vet ran at `42ef546`; current live proof ran at `45d4f93`; only integrated race ran from exact committed `1f3c119`. | Root must disposition the declared unchanged-source reuse and the test-only successor under its exact-candidate acceptance contract. No fresh-HEAD full/runtime run is fabricated here. |
| T031 final root review | Seven source findings closed within their assigned scope. The complete candidate/frozen evidence is not technically accepted. | Root review of all requirements, public APIs, migration/rollback docs, remaining gate outcomes, and any residual findings. |
| T032 release | Ordinary release intent retained. No selected version, PR, merge, tags, current-module publication, or binary delivery recorded. | Post-acceptance version decision, changelog/release notes, authorized exact-SHA merge/tag-last flow, remote tag parity and module/binary delivery. |
| T033 consumers and canary | Current-version handoffs, fresh-clone/module/binary canary, and fresh-session delivered hold/replace/resume proof are not recorded. Engram native issue capability is unmounted; no backend substitute is invented. | Target-authorized consumer handoff/readback through the owning capability, plus delivered-artifact and fresh-session proof. Record the exact missing capability boundary if it remains unavailable. |

Neither `PROJECT_RELEASE_PROTOCOL_PASS` nor `CONSUMER_HANDOFF_PASS` is claimed. Source implementation, bounded verification, technical acceptance, and consumer delivery remain separate states.
