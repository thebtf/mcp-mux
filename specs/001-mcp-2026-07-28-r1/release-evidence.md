# R1 candidate evidence

**Status:** Candidate proof complete. This record does not publish, tag, merge, or claim consumer adoption.

## Candidate and protocol identity

- Product source SHA: `a1e816339170416d9a7a956e882f9119f99396a8`
- Selected policy: `--mcp-protocol=2026-07-28`
- Pinned protocol revision: `2026-07-28`
- Protocol source manifest SHA-256: `187566e4c4fcd65208178748f94e6d179765b7764381d10b327a3c944f478551`
- Normative contract SHA-256: `d1908bcb5d3a847432be22596df988083aade9b810606ae75b8e3e5d4339f303`
- Frozen proof manifest: `D:/Dev/mcp-mux/.agent/evidence/r1-native-isolation/manifest.json`
- Frozen proof manifest SHA-256: `cbd84797a65eff8f22c2d178dafcc7d37a64f0ded057bbefdb3c8542abcdf8c4`
- Acceptance corpus SHA-256: `b61ae9d4eef44d153b3b0e88f2ce717a811bf06bef4f1baf87dca428753514eb`

The manifest re-computes every final summary, transcript, and required artifact hash. It excludes preserved failed and superseded attempts.

## Runner contracts

Both platform contracts first rejected an intentionally incomplete evidence fixture. They then built `mcp-mux`, `mock_modern_server`, and `mock_server` from the candidate source and validated the generated `summary.json`, `transcript.ndjson`, six required artifacts, 100-frame denominator, and eight scenario verdicts.

The Windows run used Windows PowerShell 5.1 and isolated `TEMP`, `TMP`, and `TMPDIR` under the repository's `.agent/tmp` directory. The Unix run used WSL2 Linux, mounted a private tmpfs at `.agent/tmp/u` for AF_UNIX sockets, and kept durable evidence under the repository. It used the cached Go 1.25.4 toolchain with `GOTOOLCHAIN=local` and `GOPROXY=off`; the terminal run made no toolchain or module download.

## Platform build identities

| Field | Windows | Unix |
| --- | --- | --- |
| Platform | `windows-10.0.26200.0-x64-powershell-5.1.26100.9278` | `linux-x86_64` on WSL2 `6.6.87.2-microsoft-standard-WSL2` |
| Go | `go1.25.4 windows/amd64` | `go1.25.4 linux/amd64` |
| Binary SHA-256 | `4d0c9d656bec81f61a0f4a28f869c0be0720cd2c283955b3331d203849a638ad` | `149c46a4743638b3eded2e8682bc93d183236777e8a14c9cc1ce1b81343085d2` |
| Modern fixture SHA-256 | `d4a57a591dbe09fecf8458cd2f826e1cc59a289efdb9bb9ba283fa1876474eb8` | `429db97f7dbca5f085a954e67692ef37a4a872eb930983e02d86f433fa8722cd` |
| Legacy fixture SHA-256 | `64e22f3ee2a425d10e60b587af5788232c291a85258a5212bd468feae829afc0` | `f278398b6e3b82e1db71fd63ef82122dfd7fc4441d03f9eafa8373717c5100a1` |
| Summary SHA-256 | `1ef821b227077b33f7c66f534ae2069c9162bc00090404fe011a1be1431f9cc5` | `40ebc848217e11383c23ad49f427a717d8516d7785f6680530f3bbf55851bc2b` |
| Transcript SHA-256 | `f30b03b5644ebc2dafd2c0e20207e7c877332e07f80ad3bccc9a3713cd460f67` | `a39ea27f15ec5ebd5f6a09b9cc3a29b947c652f4040f24fedf114ef43844eb89` |
| Corpus result | `100/100 PASS` | `100/100 PASS` |
| Scenario result | `1–8 PASS` | `1–8 PASS` |

## Scenario evidence

| Scenario | Observed result |
| --- | --- |
| 1. Direct openers | `70/70` direct corpus frames passed on each platform. The split was 35 with `clientInfo` and 35 without it. Each frame used a fresh daemon and forced-isolated owner, reached the fixture byte-for-byte except for LF transport framing, and returned a native result. |
| 2. Host-sent discovery | `30/30` `server/discover` frames passed on each platform. The split was 15 with `clientInfo` and 15 without it. No mux-authored discovery preceded the host frame. |
| 3. Strict admission | Missing or malformed required metadata returned `-32602`; an unsupported declared version returned `-32022`. No capture file or modern owner appeared, and no legacy fallback ran. Windows exercised six malformed/version cases. Unix exercised three independent representative classes. |
| 4. MRTR and directionality | `input_required` and opaque state reached the host. The retry used a fresh ID. One opted-in standard log reached the sole route once. A fixture server request did not escape to the host. |
| 5. Lifecycle quarantine | The host received the terminal result before fixture loss. The same open host session observed a same-era replacement with an empty capture before fresh traffic, then received a fresh request result. Status caused no bootstrap or replay traffic. Closing the host shim before `stop --force` prevented reconnect from creating another admission. Final status was empty. |
| 6. Readback and redaction | Active modern owners reported `protocol_era=2026-07-28`, `sharing_policy=forced-isolated`, `cache_policy=off`, and `lifecycle_policy=r1-quarantine`. Existing readiness remained `upstream_live=true`. Injected request, opaque-state, credential, environment, progress, subscription, and compatibility sentinels did not appear in status. No R3 registry, topology, taxonomy, or counter fields appeared. |
| 7. Legacy parity | Omitting the selector preserved legacy `initialize`, `notifications/initialized`, `tools/list`, and `tools/call` behavior. Legacy status contained no modern policy fields. Each platform recorded a deterministic legacy identity and output hash in `artifacts/legacy.json`. |
| 8. Rollback | Status identified the active modern forced-isolated owner. The host shim closed, the built `mcp-mux stop --force` path retired the scoped daemon, and final status reported no active owner. No modern work was downgraded to legacy or replayed. |

The private daemon control response is not printed on the customer stdio or status interfaces. Positive attachment therefore records the public consequence of the exact control-era check, not the private raw response bytes: every accepted route attached only under the explicit selector and immediately reported `protocol_era=2026-07-28`. The independent check re-derives the exact request/response echo requirement from the pinned source and candidate code.

## Required artifact hashes

| Artifact | Windows SHA-256 | Unix SHA-256 |
| --- | --- | --- |
| Modern corpus proof | `4f2314db15870cb6fd3ce6ac25326ecfdd752f299bcbf21e81f140201b214158` | `5380ef92d0e4b2a90a5b82c7986d7c8547fd91577a66cf97e1b129e3abb8b97b` |
| Admission | `1216695193540cbcc09a3dfabddf3eb2e7c82014cf925bb41130304022493943` | `fb1fe7c6904d977e4d20b8096152c3b2a9916cf7866ea8b93c54c790cb0c1994` |
| Lifecycle | `731d4db2d7c718cd1ee3fcd41d0e1b91cbfa2caa5176621d906d3554205dfdac` | `7fa72a746a540a4b3546922c3c065dfdf54d11a7ffce55c9b202b5d44922f6ed` |
| Readback | `4decffb1828b92ce04f8b2c5b36988c541e7b455582be855fdb96c40916327d4` | `c6f489ea093a015072562e565bb0864b3fda78b2038dadf2e7a3906c2c79c11e` |
| Legacy | `4d2a06955aa19324cf76faa6346efe123bef2e029a7442b26aa4ecafef24d602` | `3703a75fd00a55b944818ff239e262eb1cf03b1247152ad0ebcd1527a0889b4a` |
| Rollback | `ef7a12d9cb595db832998243ae46751f6078672c7283ea7bdf629c026b759fbe` | `a6a09e5bb5f24ca851cd4470a47b58ee34921617b5827b3a76be0ec648227eb9` |

## Proof script identities

| Script | SHA-256 |
| --- | --- |
| `scripts/verify-r1-native-isolation.contract.ps1` | `ec03d3e4c56ac94b81cdcfc029e199e462adf26ce13f7846136d172b86a706d6` |
| `scripts/verify-r1-native-isolation.contract.sh` | `132320f580d56d24e62935a0dc7494c5ab078c617e332281965ec6f434cb77dd` |
| `scripts/verify-r1-native-isolation.ps1` | `977fcefbd02c46099f544fd5f71fe459861f0ec1750b7dccfb063e2a148ed168` |
| `scripts/verify-r1-native-isolation.sh` | `39f602b268bac316c5c3842abfbc370511dfca09a42b0dc7ce81c38aa39ccdbb` |

## Consumer handoff and rollback boundary

`consumer-handoff.md` identifies `mcp-mux`, `aimux`, `engram`, and other discovered `muxcore` consumers. It keeps legacy as the default, requires an explicit route-by-route opt-in after publication, forbids consumer-local lifecycle and replay workarounds, and reserves issue creation or consumer contact for the release stage. No external issue, comment, release, tag, or consumer update occurred in this phase.

Rollback stops new explicit-modern admissions, closes the active host route, and uses the existing owner lifecycle path. It does not convert an active modern owner to legacy, transfer opaque state, or replay unfinished requests or subscriptions.

## Repository verification

- `go test ./testdata/mock_modern_server_test.go -count=1`: PASS.
- Root `go test ./...`: PASS.
- `muxcore` `go test ./...`: PASS, 24 packages with tests passed and one package had no tests.
- Root `go vet ./...`: PASS.
- `muxcore` `go vet ./...`: PASS.
- Windows R1 runner contract: PASS.
- Unix R1 runner contract: PASS with a local Go 1.25.4 toolchain, `GOPROXY=off`, and Linux tmpfs process state.
- Independent checker `R1ExactEvidenceChecker`: PASS. Receipt: `independent-check.md`.
- Repository critical suite `tests/critical/run-all.ps1`: PASS, 5/5 steps. The entrypoint named in `docs/RELEASE-PROTOCOL.md` built the isolated candidate and passed process lifecycle convergence, real time-upstream reconnect, the current-topology oracle, and native `SessionHandler` update.

Critical suite report: `D:/Dev/mcp-mux/.agent/reports/critical-suite-20260831-175641.json`, SHA-256 `d3c7ef2db6ca2e02750afb718e86e971edbb35e79730020289eec09e0730fd90`.

| Critical evidence | SHA-256 |
| --- | --- |
| Process lifecycle | `4940a992d9a5315b7c34746e154fc05dcbe6671bf00ee9ccae847198d985c06e` |
| Time-upstream reconnect | `ba740c3315d73758eea54b61927907cb1845c38e6a0a82defe825dae35bafac9` |
| Native `SessionHandler` update | `b6473143b952c59e9f2f060cb70ea3cf1545e5bb9440858b8289f6d8a74c7fa5` |

Critical harness compatibility support is commit `acb7825`. It changes only verification scripts; the built product source remains `a1e816339170416d9a7a956e882f9119f99396a8`.

The independent critical-script review found no blocking defect. One `MINOR` was deferred: the Windows PowerShell 5.1 timeout fallback uses direct `Process.Kill()` and then the existing run-scoped CIM cleanup for the known launcher, engine, and fixture process names. The focused lifecycle smoke and the full critical suite both ended with zero scoped survivors. A future patch may replace that bounded fallback with a generic descendant walk. The remaining findings were non-blocking portability or diagnostic nits.

## Verdict

`PASS` for candidate customer proof, independent exact-evidence checking, full Go test and vet suites, and the repository critical suite. Publication and consumer adoption remain outside this phase.
