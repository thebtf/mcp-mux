# Upstream maintenance hold release evidence

## Milestone and scope

This is the T029 evidence artifact for GitHub #135. The accepted ADR-015 full-slice scope inherits ordinary release intent. It is not source-only work.

| State | Recorded outcome |
| --- | --- |
| Implemented | Current source `435bcfa70da85f3763f1ddbc861016f2e18b07c4` adds only the deterministic `owner_test.go` fixture correction, 26 additions/6 deletions, atop native-family production 6fc7. No production changes follow 6fc7; its scoped static assurance still applies to those runtime bytes. Public request counters/status/schema remain unchanged. |
| Verified | Native-family normal/race: 7 top-level/45 named PASS rows, including parents. Corrected fixture focus PASS 0.409s and full owner suite PASS 54.973s. Whole muxcore at 435bcfa exits 0, all 24 tested packages PASS. Earlier root-full/both-vet 6fc7 exits 0 remain scoped receipts, not reruns after fixture commit. Linux/callers/CI and final review pending. |
| Current-head acceptance | Native-family finding is repaired/dispositioned: root posted/read back the 435bcfa reply and native resolution succeeded; all twelve known threads are root-resolved. T031 still awaits Linux native-family/built Windows/Linux caller/CI proof and fresh final observer readback. No current all-diff CLEAN, final acceptance/freeze, production-ready, or delivered-canary claim. |
| Delivered | No. Binary/library `0.31.0` is prepared and PR #150 remains open, not merged. T032 is in progress; no new release tags, module/binary publication, post-merge fresh-clone canary, or consumer handoff completion is recorded. T033 remains pending. |

This update inspected receipts and root-provided observations only, without checks, builds, Git, or external effects. Exact-source proof, finding disposition, technical acceptance, and delivery remain separate. Root owns the notification/lifecycle callback-family repair and later freeze. Post-merge/tag/canary evidence belongs in primary-checkout evidence, without editing merged source to record delivery.

## Source and artifact identity

All local raw-evidence paths below are relative to the **primary** checkout `D:/Dev/mcp-mux`, not the candidate's inner `.agent` directory.

| Identity | Value and relation |
| --- | --- |
| Source base | `3881f27b931f6b9d0467c0a15dd4e1824969e125` |
| Candidate root | `D:/Dev/mcp-mux/.agent/worktrees/upstream-maintenance-01a0fb9a` |
| Historical full tests/vet source | `42ef546d146b1e0bc9541f64e9c228d6ba5d3358` |
| Historical live replacement source | `45d4f93ec7c2703db1a57dfa48efc477accb1386` |
| Earlier accepted source milestone | `5b07ecd8f531891fbb7a42353200850baef7d274` |
| `42ef546` to `45d4f93` | Known complete changed-path set is only `scripts/smoke-upstream-maintenance.ps1`. Go product and regression source are unchanged across this interval. |
| `45d4f93` to `1f3c119` | Known complete changed-path set is only `muxcore/daemon/maintenance_test.go`, the TTL fixture repair, with 7 additions and 1 deletion. Go product source remains unchanged; the test corpus is not identical. |
| `1f3c119` to `0b443b0` | Root committed only `specs/002-upstream-maintenance-hold/{release-evidence.md,tasks.md,quickstart.md,spec.md}`. Product and regression source are unchanged. |
| `0b443b0` to `7cd4b2d` | Root's owning-milestone documentation commit only. Product and regression source are unchanged. |
| `7cd4b2d` to `5b07ecd` | Exactly `experiments/current-topology-poc/main.go`, `scripts/run-current-topology-poc.ps1`, and `tests/critical/run-all.ps1` commit the owned oracle repair. Root ran the corrected suite on `7cd4b2d` plus those exact working-tree bytes before committing them as `5b07ecd`; this is tested-source equality, not a gate launched after that commit. |
| Current candidate source | `435bcfa70da85f3763f1ddbc861016f2e18b07c4`, fixture-only successor to unchanged native-family production 6fc7. Corrected fixture/full-owner proof and fresh whole-muxcore GREEN are recorded below. 6fc7 normal/race/static/root-full/vet receipts retain their declared scopes; Linux/caller/CI and review disposition remain pending. |

The changed-path relationships are recorded in `agent://MaintenanceReviewCloseout` and root's supplied commit-path evidence. This writer did not rerun Git comparison or rebuild the candidate. Root admits reuse of the `42ef546` full-test/vet and `45d4f93` live receipts within their unchanged-source scope, including the TTL-test-only `1f3c119` and docs-only `0b443b0` successors. This is **not** a claim that those commands ran freshly at exact `1f3c119` or `0b443b0`, nor that independently built binaries at different SHAs have identical hashes. The integrated race ran from exact committed `1f3c119`; the new focused adapter command ran from exact committed `0b443b0`.

The earlier runtime criterion was bound to the oracle repair committed as `5b07ecd`. That does not cover later production review changes or a delivered release artifact. Subsequent private-owner version and actual caller receipts below prove `mux_version=v0.31.0` in their private artifact namespaces; they are not post-merge release canaries or consumer handoffs.

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

Root's historical T031 acceptance below incorporated this bounded source closeout together with completed T029/T030 evidence. Later review reopened T031 for admitted production defects. The reviewer's source-only receipt is not independent execution of the critical suite or final release approval.

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

At the `5b07ecd` milestone, root admitted T030 complete and T031 technical implementation acceptance, including named behavior/proof and 7/7 original review corrections. That historical acceptance and its receipt limits remain preserved. Later PR review found additional production issues; current-head freeze acceptance is reopened as REPAIRING below. Additive minor `0.31.0` remains selected for binary and muxcore library through the normal direct release flow.

## Earlier premerge snapshot: d72

The following historical snapshot covered pushed HEAD `d72b7127f7996e1eb6793553dfae11daf791cb96`. It records the repaired first two production groups and the subsequent timing/RPC findings before their fixes. The later b5cb snapshot below supersedes its pending gate/finding state, without rewriting its failures as passes. PR #150 was open, not merged. The existing HTTPS route succeeded on retry for ls-remote and push without new credentials or global configuration; this was source publication, not release delivery.

### Version linking and production review repairs

Root records the workflow-linker/owner-initialization fix as `c599`, followed by prepared release-version docs in `c645f9417257f42b13dbe5245ca222a6c5bcf200`. The initial fixed baked-owner run used source `334cdc801e4b59bf6a858af5c4a2449288fdcc59` plus the two owned patches committed as `c599`; it was not launched after that commit. `P/version-proof-031-fixed/summary.json` records PASS, 1149/1149 assertions, empty error, zero cleanup errors, and artifact SHA-256 `C9C3ECC42B275AACDE6F988666D285500F57364E678B34B93DC345AC191F9C0C`. Its actual modern-owner observation reports `mux_version=v0.31.0`, protocol era `2026-07-28`, and cache off. Earlier unversioned or incorrectly linked artifacts are not relabeled as `v0.31.0` proof.

The configured external review at `c645f941` used Codex and CodeRabbit. Two causal production groups were repaired in `a4aa42e7313514ad68eaf40c526e2b89980b2d49`:

| Group | Repair and recorded regression |
| --- | --- |
| Restart after exact-entry loss | `Removed=false` returns typed not-found/conflict before Spawn, preserving the replacement and avoiding stale-context admission. Both absent/replaced real-helper cases fail before and pass after. |
| Retirement of all captured live pins | Placeholder timeout or one blocked removal no longer returns before all remaining captured live generations receive retirement attempts. One absolute drain deadline, aggregate latches, durable fencing, and existing deduplicated retry remain. Both multi-owner cases, with and without paused creators, fail before and pass after. |

Before evidence uses `P/maintenance-before-overlay.json` to replace only `maintenance.go` with preserved `f3dd69b` bytes while running the new regression source. The source checker independently binds overlay blob `ae3e629c182b086d986c6d60474a923c50b46c7d` to that baseline. `P/pr-retirement-before.log` has 4/4 failing leaf cases; `P/pr-retirement-after.log` has 4/4 passing leaf cases under two parent tests. This is actual isolated RED/GREEN for these repairs, not a claim that all regression files existed at the historical baseline.

`agent://MaintenanceFinalSourceCheck` now reports bounded source PASS at exact `d72b712`, no findings in its assigned scope, and unchanged production repair/regression source from `a4aa42e` through `d72b712`. It also source-challenges the disputed ordinary-finalization latch allegation: the retained Owner pointer observes unconditional latch publication before ordinary shutdown completion, while committed handoff is deliberately not tree death. This does not prove every handoff schedule or close the remote disputed thread by itself. No latch bypass was added. The checker ran no new tests, builds, CI, or release verification; model-family independence is unknown.

### Earlier da7 version-baked caller proof

Both latest builds target `github.com/thebtf/mcp-mux/muxcore/owner.Version=v0.31.0` with `-trimpath`, and both report actual modern owner version `v0.31.0`, era `2026-07-28`, cache off, original-pipe continuation, full-tree retirement, actual overwrite, and no held-frame replay.

| Latest live observation | Windows | Debian Linux |
| --- | --- | --- |
| JSON source SHA | `da7e3f85e4e6ccef9108012857a07f7f93810198` | `da7e3f85e4e6ccef9108012857a07f7f93810198` |
| Candidate SHA-256 | `578D151E09F58ED28CD15F3E14AD9F3D8E12558DEDBE25AE005FEA35C910065C` | `D824F2CC2129C702C34A8740E9282FD46EA1EA4B926835374EFCE5472A1FE2A4` |
| Actual assertions | 1149/1149 PASS, 0 failed | 1182/1182 PASS, 0 failed |
| Error / cleanup errors | Empty / 0 | Empty / 0 |
| Namespace shutdowns | 3 successful | 3 successful |
| Primary authoritative JSON | `P/windows-live-da7.json` | `P/docker/linux-live-da7/linux-live-da7.json` |
| Finished UTC | `2026-10-03T13:51:52.7830693Z` | `2026-10-03T13:53:16.6546179Z` |

The assertion totals were parsed from complete JSON arrays. They count repeated assertions, not independent scenarios. Latest Linux executed fixture overwrite changes SHA-256 `C9A32E93551AB39B1D98F3B630AAC35DD738C7033D299208646211BA4E26601B` to `99168BBF34841E84731C195FE95FDF3041FC8AA4866E4B7BFA1EA3C6AAC09105`; Windows uses the recorded v1/v2 hashes above.

The Windows worker records exit 0 for `go build -trimpath -ldflags '-X github.com/thebtf/mcp-mux/muxcore/owner.Version=v0.31.0' -o P/build/mcp-mux-da7.exe ./cmd/mcp-mux`, then the maintenance runner with `SourceRoot=C`, `CandidateBinary=P/build/mcp-mux-da7.exe`, `ScratchRoot=P`, `OutputDir=P/windows-live-da7`, `EvidencePath=P/windows-live-da7.json`, and `TimeoutSeconds=180`. It records Go 1.25.12 and primary TMP/TEMP/GOTMPDIR. The actual-owner companion is `P/windows-live-da7-observed-owner-status.json`, SHA-256 `a7fd5dd7e2854ad2c54b068bc285b25b926523b22d6abbd22fe8badd980eb0c6`. Worker cleanup records 23/23 owned identities closed and 5/5 host exits 0.

The Linux worker records exit 0 for the direct Go 1.25.12 toolchain build under `/repo/cda7` with the same linker target, producing `/repo/cda7/.agent/tmp/u/mcp-mux-da7`. Its runner uses `SourceRoot=/repo/cda7`, that CandidateBinary, `ScratchRoot=/repo/cda7/.agent/tmp/u`, `OutputDir=/repo/cda7/.agent/tmp/u/linux-live-da7`, `EvidencePath=/repo/cda7/.agent/tmp/u/linux-live-da7.json`, and `TimeoutSeconds=180`. Raw build/stdout/transcript/fixture files remain under `P/docker/linux-live-da7/`. The Linux JSON SHA-256 is `74bb69dbe5828d0245c71bf0089dc423c5fdc376a9e020f69520270a72800810`; transcript SHA-256 is `ee98f6a9b5ce008a02f4abc3db7a951f25961e316ae76d38ca19badf439c56cf`.

These are private built-artifact customer journeys, not released binaries. Windows labels its candidate externally built and its worker reports a dirty working tree with preserved root-owned changes; its JSON source field is not a clean-tree attestation. Root retains build/source binding authority. Linux labels a candidate-root binary but does not claim a verified clean tree. Neither receipt establishes fresh full/live execution at `d72b712`, final CI acceptance, or delivered-consumer proof.

### Earlier local gates and external boundaries

| Gate | Observed scope and limit |
| --- | --- |
| Full root/muxcore tests at `da7e3f8` | Root reports PASS. `P/root-full-da7.log` has 2 tested packages PASS and 3 without test files; `P/muxcore-full-da7.log` has 24 tested packages PASS and 1 without test files. No per-case denominator is inferred from nonverbose output. |
| Root/muxcore vet at `da7e3f8` | Root reports PASS. `P/root-vet-da7.log` and `P/muxcore-vet-da7.log` have no diagnostics; success is attributed to the root receipt, not inferred from silence. |
| Retirement race | Root reports race PASS; `P/race-retirement-da7.log` records daemon package PASS in 7.637s. The file alone does not encode flags, source SHA, or individual case counts. |
| Windows startup | `P/startup-private-home-after.log` records both numeric/string opening cases PASS, command package 3.646s. Short namespace and retained stderr preceded separate private HOME/USERPROFILE isolation; HOME is not asserted as the cause of the earlier EOF. |
| Platform compile gates | Root reports FreeBSD and Darwin compilation PASS; raw refs are `P/upstream-freebsd-compile.log`, `P/upstream-darwin-compile.log`, and `P/startup-darwin-arm64-compile.log`. Compilation is not native macOS runtime acceptance. |
| Latest fixture/source continuity | Root records BSD guard `c2`, macOS short namespace `f3dd69b` and private HOME `0e72c53`, README unsupported-environment correction `da7e3f8`, and `d72b712` shared-daemon short unique nonce plus deterministic expired-blocked semantics. Root reports daemon-prefix race/blocked-TTL coverage PASS. The bounded source checker confirms the original production repair carries unchanged into `d72b712`; latest fixtures still require their actual CI outcome. |
| Actual CI | At `da7e3f8`, BSD and Ubuntu PASS; coverage/macOS fixture failures were preserved and repaired for `d72b712`. Windows was matrix-cancelled, not a Windows test failure. Latest `d72b712` CI is pending in root's snapshot. No all-matrix or latest-head CI PASS is claimed. |

Four native repair comment readbacks are retained in `P/pr150-repair-comment-readbacks.json`: [restart repair](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4173490097), [all-pin retirement](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4173490175), [startup fixture repair](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4173490220), and [environment guidance](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4173490276). They prove posted/read-back replies, not blanket final acceptance.

At the d72 snapshot, `P/pr150-review-successor-d72b712.json` was terminal **REVIEW_UNAVAILABLE**, not a successful second configured pass: the installed reviewer had a one-pass role boundary and performed only its original invocation. This was not a provider outage or billing pause. CodeRabbit's automatic no-actionable result selected only `maintenance_test.go`; Codex's d72 review was COMMENTED with new findings. That historical native snapshot had 9 threads, 5 resolved, 4 unresolved, and merge readiness false. The latch thread was remotely unresolved then; its source challenge did not bypass a safety latch. These counts are not asserted as the final b5cb thread state.

| Review group unresolved at d72 | Historical claim and later disposition |
| --- | --- |
| Hold clocks before durable fence commitment | [The timing finding](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4173425241) was source-classified VALID: persistence delay consumed requested grace/TTL. It is now repaired in `5d5a599` with controlled delayed-writer and publication-fault proof below. |
| Lifecycle RPC budget omits finalization/durable-response phases | [Hold](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4173618827) and [restart](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4173618833) budget findings were valid source-traced defects. They are now repaired in `0e573dd` with real delayed-exchange and explicit-timeout proof below. |

At d72, T031 freeze acceptance was REPAIRING. The b5cb timing repairs and completed CI below close those earlier named defects and gate failures. The completed b5cb observer then admitted a new cold-start P1, so T031 remains REPAIRING. Earlier bounded source PASS receipts are not a clean second all-diff review.

## Historical production-source premerge snapshot: b5cb

This exact-source snapshot records `b5cb84157e86787f6e119771e062efbd98aa9b5f` before the newly admitted concurrent cold-start repair. PR #150 is open, not merged. Its timing, full-test, CI, and private live proofs remain valid within that source scope, not proof of a successor or permission to freeze. Later merge/tag/module/binary/canary receipts must stay in primary `.agent` evidence.

### Durable clock and publication repair

`5d5a599f880950eff5b330e97a36795234f23023` establishes a provisional durable HOLDING fence with the first complete aggregate acknowledgement. T is sampled once after that acknowledgement, retained in a copied clocked HOLDING lease, and persisted once. Later clocked-write, retirement, HELD, and response latency consumes that same window; no later stage resamples T. Either acquisition-write failure retains conservative authority, and the internal maintenance-failed guard prevents retirement callbacks from promoting it. The conservative seed remains retained. This changes no public API, state, or schema.

`P/clock-before.log` preserves actual failed clock and publication regressions using the old-source overlay. `P/clock-after-corrected.log` records both top-level tests PASS: three delayed-writer cases and all four first/second-publication before/after-failure cases, seven passing leaf cases in total. Its package exit result is daemon PASS in 6.033s. The related fixture assertion was corrected to treat Removed=true plus authoritative tree proof as retirement even when finalization carries a warning, rather than inventing blocked retirement from the warning alone.

Expanded `P/clock-transaction-boundaries.log` records `TestMaintenanceSecurity004TransactionBoundaries` PASS across initial_hold, clocked_hold, and release. Each covers PREPARE/PUBLISH/FINALIZE before/after faults: 18 passing leaf fault cases, plus three phase parents and one top-level PASS row. The matching valid COMMITTED-recovery allowance and conservative live-memory error semantics remain as specified. `P/clock-security-retirement-race.log` records daemon PASS in 18.013s; root's receipt reports the selected ten-case race gate PASS. The nonverbose raw file itself supplies only package result, not ten individual PASS rows.

### Neutral lifecycle RPC policy and real exchanges

`0e573dd79ad53d68282b5123e9bc399a61fc346e` selects one command-aware default completion allowance of **180 seconds plus exactly one requested drain interval** for hold and restart_owner. Forced restart selects 180s, ordinary 30s drain selects 210s. Positive explicit library timeouts remain unchanged, and other commands keep their existing 5s policy. This finite operational allowance is **not a universal bound** on arbitrary pin counts, stalled storage, or all upstream behavior. Timeout remains an unknown outcome requiring status inspection, not authority to resubmit a mutation, release a lease, cancel server work, or use a fallback. No new flag/API, automatic retry, state, or schema was added.

`P/rpc-default-neutral-after.log` records five top-level PASS tests and twenty named PASS rows including policy subcases. It covers a real control exchange delayed 5200ms, explicit 50ms timeout with no resubmission/release, unchanged other-command policy, and overflow refusal before dial. `P/rpc-default-adapters-after.log` records three top-level PASS cases: delayed CLI hold, real tools/call forced restart, and delayed MCP hold typed refusal, each delayed beyond the former 5s budget while retaining its original outcome/ID. Command package PASS is 5.296s; MCP server package PASS is 10.493s. The nine owning timing docs are committed in b5cb; this writer did not change those external surfaces.

### Exact b5cb artifacts and full gates

Both final live builds use Go 1.25.12, `-trimpath`, and `-X github.com/thebtf/mcp-mux/muxcore/owner.Version=v0.31.0`. Both actual modern owners report `mux_version=v0.31.0`, era `2026-07-28`, and cache off. Complete JSON arrays, not previews, supply the counts below.

| Exact b5cb live proof | Windows | Debian Linux |
| --- | --- | --- |
| Source SHA | `b5cb84157e86787f6e119771e062efbd98aa9b5f` | `b5cb84157e86787f6e119771e062efbd98aa9b5f` |
| Binary SHA-256 | `8F696862FF7ACA144DE93E97108A889C1D170DE829E9E09DA9AAB0F4B1760C17` | `3A458ABC3FDAC7E0E98F25040EAF6EC44115CCF8DF6C5326284B8911BECCE14E` |
| Assertions / failed | 1172/1172 PASS / 0 | 1191/1191 PASS / 0 |
| Error / cleanup errors | Empty / 0 | Empty / 0 |
| Private namespace shutdowns | 3 successful | 3 successful |
| Primary JSON | `P/windows-live-b5cb.json` | `P/docker/linux-live-b5cb/linux-live-b5cb.json` |
| Finished UTC | `2026-10-03T17:03:34.8547489Z` | `2026-10-03T17:04:25.9161562Z` |

The Windows worker records build and smoke exit 0, reports a clean worktree, and binds its externally built candidate to the above source/hash. Its exact build is `go build -trimpath -ldflags '-X github.com/thebtf/mcp-mux/muxcore/owner.Version=v0.31.0' -o P/build/mcp-mux-b5cb.exe ./cmd/mcp-mux`; the runner selects C, that CandidateBinary, P scratch, `P/windows-live-b5cb` output, `P/windows-live-b5cb.json` evidence, and 180s timeout. Go 1.25.12 and primary TMP/TEMP/GOTMPDIR are recorded. Actual owner status is `P/windows-live-b5cb-observed-owner-status.json`, SHA-256 `be619fe8c61b5caf3034ec5e741715986c448040c1f4ca0469382bd2ab5d4a5c`. Cleanup records 23/23 owned identities closed and 5/5 host exits 0.

The Linux worker records build and smoke exit 0 from `/repo/cb5cb`, using the direct cached Go 1.25.12 toolchain and the same linker target. Candidate is `/repo/cb5cb/.agent/tmp/u/mcp-mux-b5cb`; scratch is `/repo/cb5cb/.agent/tmp/u`, output `/repo/cb5cb/.agent/tmp/u/linux-live-b5cb`, evidence that output's sibling `linux-live-b5cb.json`, timeout 180s. Primary build/stdout/transcript/fixture records are under `P/docker/linux-live-b5cb/`. JSON SHA-256 is `9cbd45b80694ccf0a7191e055a9ca12da5075332aae6fe0294a4f60d36673b1f`; transcript SHA-256 is `a42bd19b3006fe3898a67ab901e4af7103fecf3273126f5b5cb3dbc29e6d5afb`. Linux worktree cleanliness is not independently claimed by its receipt.

These b5cb binary signatures supersede the older da7/d72 signatures only within the recorded source scope. Original pipes, hold/drain/renew/resume/short TTL, actual overwrite, fresh same-era generation, and no replay pass on both platforms. Assertion totals include repeated observations, not independent scenario counts. These private artifacts do not prove the cold-start correction, a successor head, released binaries, or delivered-consumer canaries.

| Exact b5cb root gate | Observed result and primary log |
| --- | --- |
| Root full tests at b5cb | Root command receipt exit 0; 2 tested packages PASS, 3 without test files. `P/root-full-b5cb.log`. |
| Muxcore full tests at b5cb | Root command receipt exit 0; 24 tested packages PASS, 1 without test files. `P/muxcore-full-b5cb.log`. |
| Root and muxcore vet at b5cb | Root command receipts exit 0, no diagnostics. `P/root-vet-b5cb.log` and `P/muxcore-vet-b5cb.log`; silence alone is not the exit proof. |
| Native CI at b5cb | Root readback records all 5 jobs PASS: coverage, BSD, Ubuntu, Windows, macOS. [Run 37138998740](https://github.com/thebtf/mcp-mux/actions/runs/37138998740). Earlier matrix cancellations, compile/setup failures, and coverage/macOS fixture failures remain historical, not current pending gates or retroactive passes. |

Root admits required T029/T030 premerge gates PASS at b5cb, retaining historical R1/critical/scenario signatures rather than claiming every older runner was repeated there. Those results remain recorded historical proof. A repaired successor requires root-provided exact-head receipts before final freeze; a later docs-only commit is not new runtime execution or a released canary.

### Completed observer and reopened T031

Three native posted/read-back replies in `P/pr150-timing-comment-readbacks.json` cover [clock repair](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174070866), [hold RPC policy](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174071036), and [restart RPC policy](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174071155). The final observer confirms all nine earlier threads resolved. Their admitted valid groups are repaired; the false ordinary-finalization latch claim remains source-challenged without weakening tree/latch proof.

`P/pr150-review-final-b5cb841.json` records the final observer as COMPLETED with terminal `REVIEW_BUDGET_EXHAUSTED`. Both automatic b5cb reviews completed. The closing readback has ten threads: nine resolved and one unresolved MAJOR/P1, [PRRT_kwDORq0kOM6opvtg](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174063941). Concurrent ordinary legacy `engine.New` cold starters can return terminal `ErrFileLocked` while the winning starter holds the namespace lock until daemon readiness. This is a source-backed finding, not a rerun reproduction or intentional maintenance refusal.

At b5cb, T031 was REPAIRING under root and `CooperativeColdStartFix`. The observer's one-pass observation budget limited that observer, not root repair or new-head proof. CodeRabbit's bounded maintenance assessment did not refute the Codex engine finding. No all-diff `REVIEW_CLEAN`, optional second configured invocation, current-head acceptance, source freeze, or released canary was claimed.

The initial correction changed only cooperative `ipc.ErrFileLocked` to use the existing bounded exact-control readiness wait, while other lock errors propagated. It added no lock retry, competing spawn, or maintenance/era bypass. At that documentation snapshot, checks and P1 disposition were pending; the successor receipts below now close those specific cold-start obligations.

## Historical successor proof: 28947

Root records committed/pushed source `28947dca06047bbb342600153f7c39c8868c00e8`. README restart tables and the prepared cold-start changelog correction are committed there. This historical section preserves that successor proof; it does not cover the later native-handler correction or final documentation freeze.

| Proof | Recorded result and primary evidence |
| --- | --- |
| Windows actual cold-start RED/GREEN | Corrected real `engine.New`/`Run` fixture with old-source overlay fails both legacy and modern losers with `namespace file locked`, exit 1 in `P/coldstart-before-corrected-fixture.log`. Both pass after repair, exit 0, package 0.746s in `P/coldstart-after-corrected-fixture.log`. Earlier fixture-failure logs remain preserved, not selected GREEN. |
| Cold-start safety | Root records occupied-but-unresponsive readiness failing closed after the existing 10s bound without a competing daemon, HELD refusal preserved in both eras, and non-contention lock errors terminal. Linux exact-28947 `P/docker/linux-live-28947/linux-live-28947.focused.log` independently records 4/4 top-level tests and 8 named PASS rows, 0 SKIP, exit 0, package 11.638s. |
| Root and muxcore full tests | Exact-28947 logs `P/root-full-28947.log` and `P/muxcore-full-28947.log` explicitly record `EXIT_CODE=0`: root 2 tested packages PASS plus 3 without tests; muxcore 24 tested packages PASS plus 1 without tests. |
| Root and muxcore vet | `P/root-vet-28947.log` and `P/muxcore-vet-28947.log` explicitly record `EXIT_CODE=0`, with no diagnostics. |
| CI at 28947 | Root's exact-SHA API query and native `run_watch` readback record run [37142995133](https://github.com/thebtf/mcp-mux/actions/runs/37142995133) completed SUCCESS, all five jobs PASS: coverage, Ubuntu, macOS, BSD, Windows. The initial `gh run list` timeout was a lookup failure, not a CI failure; the narrowed query supplied fresh facts. This does not cover the native-handler successor. |

Root parsed the complete compatibility-smoke JSON arrays: `P/windows-live-28947.json` has 1149/1149 assertions PASS; `P/docker/linux-live-28947/linux-live-28947.json` has 1173/1173 PASS. Both have zero errors and zero cleanup errors. These counts include repeated observations, not independent scenarios. They are private compatibility proofs, not release canaries or substitutes for the separate engine regression.

The Windows JSON declares source 28947 and SHA-256 `8F696862FF7ACA144DE93E97108A889C1D170DE829E9E09DA9AAB0F4B1760C17`, matching b5cb. Root reports all 23 linked muxcore dependencies are candidate-rooted, but the CLI does not link `muxcore/engine`; that smoke cannot prove the cold-start correction. Embedded VCS identifies an older revision beginning `d3c5` with `vcs.modified=true`. That unexplained discrepancy remains an explicit artifact-identity limit, not exact embedded-28947 proof or a manufactured failure of the separately exercised engine regression.

Linux `P/docker/linux-live-28947/linux-proof-receipt.json` binds a clean independent clone at `/repo/.agent/tmp/c28947`, Go 1.25.12, and version-baked owner `v0.31.0`. SHA-256 is `81f7fcf134966745d8b9c16884cf7322050628b9a4c3eb13b7d07a48b22421c2`; `linux-live-28947.build-metadata.log` records exact embedded revision 28947 and `vcs.modified=false`. The build and selected caller exit 0. Two earlier Linux preflights exited 1 with zero checks/commands/process observations for scratch ownership and output-containment configuration. Their retained `linux-live-28947.preflight.json` and `linux-live-28947.output-preflight.stdout.log` are setup failures before runtime, not product failures or zero-case passes.

Root posted/read back [cold-start reply 4174265488](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174265488) in `P/pr150-coldstart-comment-readback.json`, then resolved the tenth thread. The eleventh native-handler finding was subsequently reproduced and repaired in 8f09 as recorded below; its known-thread resolution is now recorded, while fresh final review/readback remains pending.

At 28947, the scout traced handler dispatch reserving `pendingRequests` without tracker-backed request IDs. Process-free removal could mark retirement proved while callbacks remained active. Reconnect supplied original-ID generic `-32603` lost-request errors, while newly fenced requests received `-32005`; no duplicate final delivery was source-proven. Managed `SessionHandler` owners were not explicitly excluded by FR-004/005/006 or compatibility limits, and modern MCP-era coverage was not handler-topology proof. The later repair preserves that scope and terminal-error distinction.

## Native request callback repair: Windows focused proof

Source `8f09dc08bf16fac511b1c43d94ce22c4ecf3f261` commits the three-line `owner.go` gate after the finalization probe: maintenance on a `SessionHandler` also requires `PendingRequests()==0` before retirement is proved. Its only other changed path is the 382-line daemon regression fixture. Existing blocked-finalization/exact-entry retry is reused, with no new state, API, counter, schema, shim behavior, or unsupported topology. Root accepts this bounded technical correction; remaining release gates are separate.

`P/native-handler-before.log` records the old-source overlay exiting 1: six blocking cases falsely reach `HELD` with `pending=2`, `actual_work=2`, `returned=0`; both finish-before-deadline cases pass. `P/native-handler-after.log` records exit 0 and all eight leaf scenarios PASS, daemon package 5.114s. Actual Windows shim `io.Pipe` and control IPC cover legacy and modern routes, each with zero drain, short drain, expiry while blocked, and completion before deadline.

Root records counted request callbacks retaining `RETIREMENT_BLOCKED`, even when they ignore cancellation; TTL/resume cannot release their active work. Existing exact-entry retry proves retirement after actual request return subject to the original clock. Numeric/string pre-fence requests each receive one terminal result, with legal generic `-32603` on orphaned work; newly fenced requests receive `-32005`. Late request callbacks produce no duplicate/replay, and fresh work succeeds on original pipes after safe release. This request proof does not establish notification/lifecycle settlement or add a stronger pre-fence error code/topology exclusion.

Root records eight request-callback leaves PASS under `-race`, exit 0, package 6.824s in `P/native-handler-race.log`. `P/root-full-native-final.log` exits 0 with 2 tested packages PASS, 3 without tests, command/internal durations 37.254s/11.979s. `P/muxcore-full-native-final.log` exits 0 with 24 tested packages PASS, 1 without tests, including daemon 108.213s and engine 20.530s. Both `P/{root,muxcore}-vet-native-final.log` explicitly record exit 0 without diagnostics. Root binds these precommit checks to identical runtime bytes committed as 8f09, not a doc-only base or post-commit rerun. Later exact-8f09 caller/Linux/CI receipts below supplement that request scope only.

Root pushed 8f09 and posted/read back [native request-handler reply 4174464694](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174464694) in `P/pr150-native-handler-comment-readback.json`; native resolution of `PRRT_kwDORq0kOM6oqR42` succeeded. Those eleven earlier known threads remain root-resolved. The reply preserves legal pre-fence `-32603` versus post-fence `-32005`, with no request-ID registry. The new twelfth finding below reopens T031; earlier closure is not a current all-diff CLEAN or a new configured invocation.

## Exact 8f09 caller/CI proof and new callback-family boundary

Root parsed complete live JSON arrays: Windows `P/windows-live-8f09.json` has 1167/1167 PASS; Linux `P/docker/linux-live-8f09/linux-live-8f09.json` has 1191/1191 PASS, both with zero errors/cleanup errors. These repeated-observation totals are not independent scenario counts or delivered-artifact canaries. Linux `focused.stdout.log` records all eight native request-handler leaves PASS, 0 SKIP, package 5.208s. Root's native CI readback records [run 37146727650](https://github.com/thebtf/mcp-mux/actions/runs/37146727650), all five jobs PASS.

Windows `P/windows-build-8f09-provenance.json` records a successful candidate-rooted build with all 25 packages, binary SHA-256 `1e381814187bab592e42400e43d9076191edcfc62d84f426b2819d6c8ff0152a`, and owner-source SHA-256 `0a0d0c6a2d198d69dc07c5b978d998276bd18d666cc4c76700699694e235ece1`. Embedded VCS remains `d3c5f918cedcc872468c4a1d82dabba222634271`, modified=true, so embedded metadata alone does not prove exact 8f09. Linux `linux-proof-receipt.json` binds a clean clone and exact embedded 8f09, modified=false, binary SHA-256 `dec7bac02a4c00ecf89e3fae0cf325ccb06761bf5af025e1bb606918e9cce940`. These identities/proofs remain valid for their request-callback scope.

At 8f09, twelfth P1 [notification finding](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174480314), thread `PRRT_kwDORq0kOM6oqvOV`, identified uncounted callbacks. Family repair in 6fc7 plus fixture-only successor 435bcfa now closes that admitted finding; root's reply/readback and native resolution are recorded below. Earlier request-only proof remains scoped, not broadened to stand in for family verification.

## Native callback-family source: 6fc7

Root commits one private nonrequest counter and retained native sessions as teardown producers. Connect is reserved at registration; disconnect reservation/unlink is serialized under the owner lock. Notifications through both interfaces, lifecycle connect/disconnect, authentication, and frame hooks stay accounted through actual return. Session/owner closure cancels notification context but does not prove settlement. Modern native notification non-dispatch remains unchanged. Both ordinary and maintenance `FinalizeForRemoval`, `Done`, and `MaintenanceRetired` require quiescence; otherwise ordinary registry removal could orphan work before a later hold can pin it. Existing blocked finalization/exact-entry retry is reused, without a new waiter, manager, public API/status, or schema. Public `PendingRequests` stays request-only.

`P/native-family-before.log` exits 1 on the original three-production-file overlay: 4 top-level tests and 28 named FAIL rows. `P/native-family-after.log` exits 0, package 7.959s, with 7 top-level tests and 45 named PASS rows; `P/native-family-race.log` repeats those 45 PASS rows, exit 0, package 9.144s. Named totals include parents, not 45 independent cases. Root records actual reader/disconnect and registration barriers, both notification interfaces, lifecycle connect/disconnect, authentication, a >1ms frame hook, ordinary-removal versus hold pinning, and the existing `SetNotifier` creating barrier.

`agent://NativeQuiescenceChecker` reports bounded D1 static assurance at exact 6fc7: 3/3 production paths, 6/6 nonrequest callback routes, 2 registration paths plus reader-removal producer, and 4 proof predicates source-inspected. No concrete unaccounted callback, bypass, or deadlock survived that scoped ordering check. It ran no runtime proof and is not an all-diff CLEAN verdict.

`P/root-full-6fc7.log`, `P/root-vet-6fc7.log`, and `P/muxcore-vet-6fc7.log` explicitly exit 0. Historical `P/muxcore-full-6fc7.log` exits 1 for `TestNewOwner_SessionHandlerOnly_NoUpstream`; other packages pass. Root identified its one-shot `Shutdown` expectation before pipe-reader/disconnect settlement, not a missing closer: `NewSession` adopts the `PipeReader` closer. The fixture-only correction is now committed as 435bcfa, with no production change: `P/native-owner-fixture-focused.log` exits 0, package 0.409s, and `P/owner-suite-native-fixture.log` exits 0 in 54.973s. `P/muxcore-full-435bcfa.log` now exits 0 with all 24 tested packages PASS and 1 without tests, including daemon 87.329s and engine 16.456s. Historical RED is preserved, not rewritten. Root binds corrected fixture checks to their stable bytes before commit; no post-fixture root/vet rerun invented. Unchanged 6fc7 production retains the scoped static assurance. Linux native-family/built Windows/Linux callers/CI and final review/acceptance remain pending.

Root pushed 435bcfa to PR #150, posted/read back [native-family reply 4174748321](https://github.com/thebtf/mcp-mux/pull/150#discussion_r4174748321) in `P/pr150-native-family-comment-readback.json`, and resolved `PRRT_kwDORq0kOM6oqvOV` through the native route. All twelve **known** threads are root-resolved; the fresh final observer still supplies the later current-head readback. This closes finding disposition, not pending Linux native-family/built Windows/Linux caller/CI or release gates. Modern native notification non-dispatch remains unchanged. No old owner-fixture or known family-code defect is left pending in this snapshot; no all-diff CLEAN or final acceptance inferred.

## Remaining release and consumer outcomes

Source 435bcfa is pushed with the repaired/dispositioned family finding, corrected fixture, and whole-muxcore GREEN. Unchanged 6fc7 production retains scoped normal/race/static/root-full/vet proof. Remaining work is exact-source Linux native-family/built Windows/Linux caller/CI and fresh observer readback, not unfixed family code or the old owner fixture. Final acceptance/freeze and delivery remain unclaimed.

| Task / gate | Current recorded state | Outcome still required |
| --- | --- | --- |
| T032 release | `0.31.0` prepared, PR #150 open, 435bcfa pushed. Family and fixture repaired; whole muxcore GREEN and earlier scoped proofs retained. All twelve known threads root-resolved. Linux/callers/CI and fresh observer pending; no final freeze/merge/publication/canary. | Admit remaining exact-source verification/readback, then owning-doc freeze and authorized integration/tag-last/module/binary delivery. Later receipts remain primary-only; do not edit merged source to record delivery. |
| T033 consumers and canary | Post-merge fresh-clone/tag/module/binary and fresh-session delivered hold/replace/resume proof remain pending. Engram native issue capability is unmounted with no proxy backend; its concrete handoff boundary becomes active after the delivered version/tag target is resolved, not as a global premerge hold. | Perform real target-bound consumer handoffs and delivered-artifact canaries after release-version/tag resolution. Claim `PROJECT_RELEASE_PROTOCOL_PASS` and `CONSUMER_HANDOFF_PASS` only on actual completed criteria. |

Neither `PROJECT_RELEASE_PROTOCOL_PASS` nor `CONSUMER_HANDOFF_PASS` is claimed. Source implementation, bounded verification, technical acceptance, and consumer delivery remain separate states.
