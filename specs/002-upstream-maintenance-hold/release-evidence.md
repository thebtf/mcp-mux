# Upstream maintenance hold release evidence

## Milestone and scope

This is the T029 evidence artifact for GitHub #135. The accepted ADR-015 full-slice scope inherits ordinary release intent. It is not source-only work.

| State | Recorded outcome |
| --- | --- |
| Implemented | Complete maintenance behavior is implemented. Source milestone `5b07ecd8f531891fbb7a42353200850baef7d274` commits the three owned critical-oracle fixes on `7cd4b2d`; the earlier maintenance product source and recorded source relationships remain unchanged. |
| Verified | T029 and T030 are root-admitted complete. The corrected overall critical suite exits 0 with 5/5 PASS and no failures, skips, or errors, supplementing the focused, race, live Windows/Unix, R1, Scenario 5b/8, and seven-finding receipts below. |
| Technically accepted | Yes. Root admits T031 complete and accepts the complete technical implementation, named behavior/proof, and 7/7 known source corrections. Historical receipt limits remain explicit; acceptance does not turn old aborted or setup-failed attempts into passes. |
| Delivered | No. Root selects additive minor `0.31.0` for both binary and muxcore library through the normal direct release flow. T032 is in progress, not complete; no PR, merge, release tags, publication, or post-merge consumer delivery is recorded. T033 remains pending. |

This documentation update inspected existing receipts only. It ran no tests, builds, vet, linters, formatters, or repeated gates and made no commit. The plan, acceptance scenarios, and required commands in [tasks](tasks.md), [specification](spec.md), and [quickstart](quickstart.md) remain authoritative. Root has accepted T029-T031; subsequent release and consumer evidence must be appended before delivery is claimed.

## Source and artifact identity

All local raw-evidence paths below are relative to the **primary** checkout `D:/Dev/mcp-mux`, not the candidate's inner `.agent` directory.

| Identity | Value and relation |
| --- | --- |
| Source base | `3881f27b931f6b9d0467c0a15dd4e1824969e125` |
| Candidate root | `D:/Dev/mcp-mux/.agent/worktrees/upstream-maintenance-01a0fb9a` |
| Full tests/vet source | `42ef546d146b1e0bc9541f64e9c228d6ba5d3358` |
| Live replacement source | `45d4f93ec7c2703db1a57dfa48efc477accb1386` |
| Accepted source milestone | `5b07ecd8f531891fbb7a42353200850baef7d274` |
| `42ef546` to `45d4f93` | Known complete changed-path set is only `scripts/smoke-upstream-maintenance.ps1`. Go product and regression source are unchanged across this interval. |
| `45d4f93` to `1f3c119` | Known complete changed-path set is only `muxcore/daemon/maintenance_test.go`, the TTL fixture repair, with 7 additions and 1 deletion. Go product source remains unchanged; the test corpus is not identical. |
| `1f3c119` to `0b443b0` | Root committed only `specs/002-upstream-maintenance-hold/{release-evidence.md,tasks.md,quickstart.md,spec.md}`. Product and regression source are unchanged. |
| `0b443b0` to `7cd4b2d` | Root's owning-milestone documentation commit only. Product and regression source are unchanged. |
| `7cd4b2d` to `5b07ecd` | Exactly `experiments/current-topology-poc/main.go`, `scripts/run-current-topology-poc.ps1`, and `tests/critical/run-all.ps1` commit the owned oracle repair. Root ran the corrected suite on `7cd4b2d` plus those exact working-tree bytes before committing them as `5b07ecd`; this is tested-source equality, not a gate launched after that commit. |

The changed-path relationships are recorded in `agent://MaintenanceReviewCloseout` and root's supplied commit-path evidence. This writer did not rerun Git comparison or rebuild the candidate. Root admits reuse of the `42ef546` full-test/vet and `45d4f93` live receipts within their unchanged-source scope, including the TTL-test-only `1f3c119` and docs-only `0b443b0` successors. This is **not** a claim that those commands ran freshly at exact `1f3c119` or `0b443b0`, nor that independently built binaries at different SHAs have identical hashes. The integrated race ran from exact committed `1f3c119`; the new focused adapter command ran from exact committed `0b443b0`.

The accepted runtime criterion is bound to the tested working-tree oracle repair now committed as `5b07ecd`. The separate release workflow's one-line version-embedding correction and owning-document updates do not imply a fresh runtime run or completed release-artifact delivery. Root reports private-owner `v0.31.0` artifact proof, which is not a post-merge release canary or consumer handoff.

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

Root's later T031 acceptance below incorporates this bounded source closeout together with completed T029/T030 evidence. The reviewer's source-only receipt is not relabeled as independent execution of the new critical suite or final release approval.

## Recorded Scenario 5b and Scenario 8 outcomes

Root supplied these new observed receipts without asking this writer to rerun any check. They close the scenario evidence gaps in T030, not its overall critical-suite gate.

### Scenario 5b direct native update

The direct command ran from exact candidate `0b443b0ea51078814e76d12af0632fb9b5d52cc0` with Go 1.25.12 and primary `P` scratch settings for `TMP`, `TEMP`, and `GOTMPDIR`:

```text
pwsh -NoProfile -File C/scripts/smoke-native-sessionhandler-update.ps1 -RunDir P/native-update-0b4 -EvidencePath P/native-update-0b4.json -TimeoutSeconds 120
```

The worker and root receipts record exit 0 and PASS. `P/native-update-0b4.json` records runtime 16.919s, old daemon generation `daemon_267e71a65e4d` and new generation `daemon_05f53523a13a`. The original open session and a fresh session both report `new`, for 2/2 post-update sessions. Restored owners are 1, reconnect refreshed is 1, fallback spawned is 0, and give-up is 0. Graceful restart, replacement started, and replacement ready are true; fallback shutdown is false. This is the native SessionHandler fixture's update behavior, not transparent modern MCP protocol restoration or a delivered-consumer canary.

The evidence JSON SHA-256 is `89d88d38f77466f615e59db2ff79e97a1406428706e6a7e7c34df26179dcf745`. The old/new fixture binary hashes are `b7ab9a21aa8a81a838980e664d00e6c29dad878a85bd2a5b6b4f2f8758e4a1ca` and `c2cca07a60adc972f40850a8c688bfb06047000ebbbf99f2991e8f5a22b25a88`, not the CLI/R1 binary hashes. Raw companions are `P/native-update-0b4/{fixture.log}`, `P/native-update-0b4.stdout.log`, and `P/native-update-0b4.stderr.log`.

The worker's cleanup receipt reports no matching process for private fixture PIDs 113064, 115388, 97636, and 115440, and fixture-log shutdown completion. The supplementary `P/native-update-0b4-process-check.json`, SHA-256 `456990ddfa00ebad7bc237d603ef2a9069f16e1c8642af2dc0c03dbda5b0355f`, labels each query `ABSENT_OR_UNREADABLE`. That label alone is not an independent process-exit proof; the cleanup claim is bounded to the worker's exact-PID receipt and fixture log. No unrelated process cleanup is implied.

### Scenario 8 native Unix held-reaper and full-tree cases

The selected gate ran at source `7cd4b2dcaab4f626f2b6868f491085ee604b3e64`, working directory `/repo/c7cd/muxcore`, using the direct cached Go 1.25.12 executable. Its recorded command is:

```text
/repo/.agent/tmp/u/gomod/golang.org/toolchain@v0.0.1-go1.25.12.linux-amd64/bin/go test ./daemon ./upstream ./procgroup -run '^(TestMaintenanceSafeTTLReleasesDurablyWithoutIdleExitBypass|TestMaintenanceBlockedTTLDoesNotReleaseUnprovenTree|TestMaintenanceIncompleteLedgerRecoveryCannotInventTreeDeath|TestMaintenanceTreeDeathExcludesCommittedTransfer|TestConcurrentTreeFinalizationIsIdempotent|TestGracefulKill_KillsTree)$' -count=1 -timeout=120s -v
```

The receipt records `GOTOOLCHAIN=local`, `TMPDIR=/repo/c7cd/.agent/tmp/u`, `GOTMPDIR=/repo/c7cd/.agent/tmp/u/go-tmp`, `GOCACHE=/repo/c7cd/.agent/tmp/u/go-cache`, and `GOMODCACHE=/repo/.agent/tmp/u/gomod`. These are observed container paths, not portable defaults. Exit is 0: 6/6 selected top-level tests, 2/2 incomplete-recovery subcases, and 3/3 packages PASS. The incomplete-recovery states are HOLDING and RETIREMENT_BLOCKED. Raw `P/docker/unix-lifecycle-7cd.log` has SHA-256 `793cf620bff598b69bae1a5e1ba02cc648770816a493e30e7acde47bb8ab4958`. Its package durations are daemon 2.076s, upstream 0.019s, and procgroup 0.006s.

The initial attempt exited 1 before any test because GOTMPDIR did not exist. `P/docker/unix-lifecycle-7cd-setup.log` remains preserved and is not a zero-test success. The completed selected run supplies the previously missing named/source/toolchain-bound Unix coverage. Older `linux-focused-valid.log` remains a failing historical receipt; `linux-full-fixture-green.log` remains package-only and source-unbound. Neither is upgraded by the new run.

### Scenario 8 retained Windows lifecycle child

The independently completed child report is primary `.agent/reports/critical-process-lifecycle-20261003-064612.json`, not a file beneath `P`. It records PASS for 8 parallel isolated host transports with initial, wake, and post-upgrade observations of 8 trees each, 3 launcher-only convergences, and zero stale descendants or scoped/captured survivors before and after cleanup. Its candidate SHA-256 is `b5f847deb5bf13ee3d991a3a14b6347611ebc0b867076a60cbd47e0d2447acc7`, distinct from the parity and live-maintenance binaries. The JSON contains no source-SHA field; this writer does not label it a fresh `7cd4b2d` execution. Root retains it as historical bounded Windows Scenario 8 evidence from the interrupted critical run. This child report alone did not establish the original aggregate suite exit or verdict; the corrected complete suite below supplies a new terminal result.

## Corrected critical suite and root technical acceptance

Root executed the existing complete critical command from the candidate root with Go 1.25.12 and primary scratch environment:

```text
pwsh -NoProfile -File tests/critical/run-all.ps1 -TimeoutSeconds 120 -Launcher D:/Dev/mcp-launcher/mcp-launcher.exe -ArtifactRoot D:/Dev/mcp-mux/.agent
```

The root receipt records **exit 0**. Raw output is `P/critical-topology-repaired.log`; the aggregate is primary `.agent/reports/critical-suite-20261003-080036.json`, run ID `94c18a13-0c44-43a8-abe4-62be8e844bbb`. It records PASS, total 5, passed 5, failed 0, skipped 0, errored 0, runtime 282.405s, empty error and missing-coverage arrays, and successful isolated-process teardown. Start/finish timestamps are `2026-10-03T05:00:36.2901549Z` and `2026-10-03T05:05:18.6948835Z`.

| Critical step | Verdict | Duration ms |
| --- | --- | --- |
| Build isolated mcp-mux binary | PASS | 1121 |
| Process lifecycle convergence smoke | PASS | 168541 |
| Real time upstream reconnect smoke | PASS | 79871 |
| Current topology proofing oracle | PASS | 21224 |
| Native SessionHandler update smoke | PASS | 11628 |

Companion reports are `.agent/reports/critical-process-lifecycle-20261003-080036.json`, `critical-smoke-time-upstream-20261003-080036.json`, and `critical-native-sessionhandler-update-20261003-080036.json`. The aggregate identifies isolated binary `.agent/tmp/critical-suite-20261003-080036/mcp-mux.exe`; its lifecycle companion records SHA-256 `a35902e70aa406227334f9bb4acb413fc3a891972594414433a0ffa161068483`. This matches the recorded Windows candidate hash, not the older interrupted critical run's `b5f847...` hash.

Root binds the executed source to `7cd4b2d` plus exactly the three owned oracle-fix files committed as `5b07ecd8f531891fbb7a42353200850baef7d274`. No post-commit rerun is fabricated. The original roughly 30-minute aborted critical attempt remains preserved without terminal exit or aggregate verdict. Its scout initially could not distinguish launcher capture from shutdown. Later exact-PID evidence showed control shutdown exit 0 and daemon PID 43148 OS exit in 160ms; source established inherited detached-daemon stderr keeping launcher capture EOF open. The fixture's stderr lifetime, timeout propagation, and typed cleanup were repaired. The new complete suite closes that pending gate; it does not rewrite the original attempt as PASS.

Root now admits T030 complete and T031 technical implementation acceptance, including all named behavior/proof and 7/7 known review corrections. T029 remains complete. Release selection is additive minor **0.31.0 for both binary and muxcore library**, using the normal direct release flow, not a compare-and-swap release route. Selection and private versioned-artifact proof do not complete T032 or T033.

## Remaining release and consumer outcomes

Technical acceptance is complete. The remaining rows describe delivery effects, not pending runtime gates or a generic blocked verdict. Root retains the ordinary full release outcome and appends exact delivery/readback evidence before completion.

| Task / gate | Current recorded state | Outcome still required |
| --- | --- | --- |
| T032 release | In progress. Root selects binary/library `0.31.0` and is preparing version/docs/artifact delivery through the normal direct release flow. No PR, merge, tags, current-module publication, or delivered binary is recorded. Root reports private-owner `v0.31.0` proof, not a release canary. | Complete owning release-version docs and correct artifact version embedding, authorized exact-SHA merge/tag-last flow, remote parity, and module/binary delivery. Root's fresh GitHub API read works; HTTPS Git ls-remote encountered a connection timeout, which is transport evidence, not publication success. |
| T033 consumers and canary | Pending external handoffs and post-merge fresh-clone/module/binary/fresh-session proof. Engram native issue capability remains unmounted; only that handoff effect is held, not the completed technical work or other reachable delivery effects. | Target-authorized handoff/readback and post-merge delivered-artifact canaries, including fresh-session hold/replace/resume. Record `PROJECT_RELEASE_PROTOCOL_PASS` and `CONSUMER_HANDOFF_PASS` only after their actual criteria pass. Do not invent an Engram backend substitute. |

Neither `PROJECT_RELEASE_PROTOCOL_PASS` nor `CONSUMER_HANDOFF_PASS` is claimed. Source implementation, bounded verification, technical acceptance, and consumer delivery remain separate states.
