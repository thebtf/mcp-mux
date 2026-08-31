# R1 independent exact-evidence check

**Checker:** `R1ExactEvidenceChecker` (`nvmd-checker`)

**Verdict:** `PASS`

The checker was distinct from the Windows and Unix runner makers. It read only the frozen final evidence, the pinned MCP `2026-07-28` sources, the candidate source, and `release-evidence.md`. It ignored `.agent/evidence/r1-native-isolation/failed/` and `superseded/`.

## Frozen identities

- Product source SHA: `a1e816339170416d9a7a956e882f9119f99396a8`
- Protocol revision: `2026-07-28`
- Protocol source manifest SHA-256: `187566e4c4fcd65208178748f94e6d179765b7764381d10b327a3c944f478551`
- Normative contract SHA-256: `d1908bcb5d3a847432be22596df988083aade9b810606ae75b8e3e5d4339f303`
- Frozen proof manifest SHA-256: `cbd84797a65eff8f22c2d178dafcc7d37a64f0ded057bbefdb3c8542abcdf8c4`
- Corpus SHA-256: `b61ae9d4eef44d153b3b0e88f2ce717a811bf06bef4f1baf87dca428753514eb`
- Corpus result: `100/100 PASS` on Windows and Unix
- Scenario result: Scenarios `1` through `8` are `PASS` on Windows and Unix

## Platform and build hashes

| Field | Windows | Unix |
| --- | --- | --- |
| Platform ID | `windows-10.0.26200.0-x64-powershell-5.1.26100.9278` | `linux-x86_64`, WSL2 `6.6.87.2-microsoft-standard-WSL2` |
| Binary SHA-256 | `4d0c9d656bec81f61a0f4a28f869c0be0720cd2c283955b3331d203849a638ad` | `149c46a4743638b3eded2e8682bc93d183236777e8a14c9cc1ce1b81343085d2` |
| Modern fixture SHA-256 | `d4a57a591dbe09fecf8458cd2f826e1cc59a289efdb9bb9ba283fa1876474eb8` | `429db97f7dbca5f085a954e67692ef37a4a872eb930983e02d86f433fa8722cd` |
| Legacy fixture SHA-256 | `64e22f3ee2a425d10e60b587af5788232c291a85258a5212bd468feae829afc0` | `f278398b6e3b82e1db71fd63ef82122dfd7fc4441d03f9eafa8373717c5100a1` |
| Summary SHA-256 | `1ef821b227077b33f7c66f534ae2069c9162bc00090404fe011a1be1431f9cc5` | `40ebc848217e11383c23ad49f427a717d8516d7785f6680530f3bbf55851bc2b` |
| Transcript SHA-256 | `f30b03b5644ebc2dafd2c0e20207e7c877332e07f80ad3bccc9a3713cd460f67` | `a39ea27f15ec5ebd5f6a09b9cc3a29b947c652f4040f24fedf114ef43844eb89` |

## Required artifact hashes

| Artifact | Windows SHA-256 | Unix SHA-256 |
| --- | --- | --- |
| Modern | `4f2314db15870cb6fd3ce6ac25326ecfdd752f299bcbf21e81f140201b214158` | `5380ef92d0e4b2a90a5b82c7986d7c8547fd91577a66cf97e1b129e3abb8b97b` |
| Admission | `1216695193540cbcc09a3dfabddf3eb2e7c82014cf925bb41130304022493943` | `fb1fe7c6904d977e4d20b8096152c3b2a9916cf7866ea8b93c54c790cb0c1994` |
| Lifecycle | `731d4db2d7c718cd1ee3fcd41d0e1b91cbfa2caa5176621d906d3554205dfdac` | `7fa72a746a540a4b3546922c3c065dfdf54d11a7ffce55c9b202b5d44922f6ed` |
| Readback | `4decffb1828b92ce04f8b2c5b36988c541e7b455582be855fdb96c40916327d4` | `c6f489ea093a015072562e565bb0864b3fda78b2038dadf2e7a3906c2c79c11e` |
| Legacy | `4d2a06955aa19324cf76faa6346efe123bef2e029a7442b26aa4ecafef24d602` | `3703a75fd00a55b944818ff239e262eb1cf03b1247152ad0ebcd1527a0889b4a` |
| Rollback | `ef7a12d9cb595db832998243ae46751f6078672c7283ea7bdf629c026b759fbe` | `a6a09e5bb5f24ca851cd4470a47b58ee34921617b5827b3a76be0ec648227eb9` |

## Proof script hashes

| Script | SHA-256 |
| --- | --- |
| `verify-r1-native-isolation.contract.ps1` | `ec03d3e4c56ac94b81cdcfc029e199e462adf26ce13f7846136d172b86a706d6` |
| `verify-r1-native-isolation.contract.sh` | `132320f580d56d24e62935a0dc7494c5ab078c617e332281965ec6f434cb77dd` |
| `verify-r1-native-isolation.ps1` | `977fcefbd02c46099f544fd5f71fe459861f0ec1750b7dccfb063e2a148ed168` |
| `verify-r1-native-isolation.sh` | `39f602b268bac316c5c3842abfbc370511dfca09a42b0dc7ce81c38aa39ccdbb` |

## Independently re-derived boundary

1. `--mcp-protocol=2026-07-28` selects the CLI modern route. `engine.Config.ProtocolPolicy = era.PolicyModern20260728` selects the embedded route. The zero value remains `era.PolicyLegacyOnly`.
2. Every modern request requires `_meta.io.modelcontextprotocol/protocolVersion=2026-07-28` and an object-valued `_meta.io.modelcontextprotocol/clientCapabilities`. The pinned contract identifies these as required. The admission artifacts record `-32602` for missing or malformed required metadata and `-32022` for an unsupported declared version.
3. `control.Request.ProtocolEra` and `control.Response.ProtocolEra` carry the exact era before attachment. The candidate source refuses a missing or mismatched successful echo. Accepted evidence then reports a `native-*` owner with `protocol_era=2026-07-28`.
4. Every modern owner is forced isolated. The readback artifacts consistently report `sharing_policy=forced-isolated` and `cache_policy=off`.
5. Modern shim and capture evidence contains no mux-authored legacy `initialize`, `notifications/initialized`, roots/list, list-change, cache, template, or replay traffic.
6. MRTR remains native. `input_required` reaches the host, the retry uses a fresh JSON-RPC ID, opaque `requestState` is echoed unchanged, one opted-in standard log reaches the sole route once, and an upstream JSON-RPC request does not escape to the host.
7. After upstream loss, the same host session receives the terminal result, observes an empty replacement capture before fresh traffic, and sends a fresh request to the same-era replacement. No request or subscription replay is accepted as continuity.
8. R1 status adds only `protocol_era`, `sharing_policy`, `cache_policy`, and `lifecycle_policy`. Existing readiness remains truthful. Injected private sentinels do not appear in the owner status, and no R3 registry, topology, taxonomy, or counter fields appear.
9. Omitting the selector keeps the legacy initialize and tool flow. The legacy artifacts contain no modern policy fields.
10. Rollback identifies the active modern owner, closes the host route, retires the scoped daemon with the built `stop --force` path, and ends with no active owner. The evidence records no downgrade or replay.

## Scenario checks

| Scenario | Verdict | Independent evidence check |
| --- | --- | --- |
| 1 | PASS | Recomputed `70` direct frames and the 35/35 `clientInfo` split. Spot-checked corpus line 1 against both platform captures and the native response. |
| 2 | PASS | Recomputed `30` discovery frames and the 15/15 `clientInfo` split. Spot-checked a host-sent `server/discover` capture with full required metadata. |
| 3 | PASS | Checked six Windows cases and three independent Unix classes. Codes, absent captures, empty status, and no fallback matched the contract. |
| 4 | PASS | Checked `input_required`, fresh retry ID, unchanged `requestState`, one opted-in log, and contained server request artifacts. |
| 5 | PASS | Checked terminal result, generation-two same-era replacement, empty pre-fresh capture, fresh request success, shim-first cleanup, and empty final status. |
| 6 | PASS | Checked all four policy facts, `upstream_live=true`, sentinel redaction, and absence of R3 fields. |
| 7 | PASS | Checked legacy initialize and tools flow, deterministic identity/output hashes, and absent modern policy fields. |
| 8 | PASS | Checked active modern identification, built force-stop evidence, empty final status, and no downgrade or replay flags. |

## Findings and dispositions

| ID | Severity | Disposition | Finding |
| --- | --- | --- | --- |
| F-1 | INFO | Accepted | Unix has an extra `cleanup.json`; it is outside the six required artifacts and does not change acceptance. |
| F-2 | INFO | Accepted | `release-evidence.md` is candidate-relative in the worktree. Its content matches every frozen hash. |
| F-3 | INFO | Accepted | Phase 7 documentation and proof scripts were uncommitted during the built proof. Product `HEAD` was exactly the recorded source SHA; the proof manifest separately binds the final script bytes. |
| F-4 | INFO | Accepted | Captured `mux_version` values include `-dirty` because Phase 7 documentation and proof-script changes were present. The product source SHA remained exact. |
| F-5 | INFO | Accepted | The corpus partitions exactly into 70 direct and 30 discovery frames, with 50 present and 50 absent `clientInfo` cases. |
| F-6 | INFO | Accepted | A cleanup message uses legacy wording for a shutdown signal. Modern attachment, status, captures, and rollback still prove modern semantics and no downgrade. |

No blocking or corrective finding remains.

## Checker commands and limits

The checker recomputed SHA-256 values for the pinned protocol files, proof manifest, both summaries and transcripts, all required artifacts, all four proof scripts, and the corpus. It also read representative captures, responses, status snapshots, shim logs, legacy evidence, rollback evidence, and the candidate source.

The checker did not re-fetch the live MCP website. It verified the exact frozen `SOURCE-MANIFEST.json` bytes and used the cached primary-source files named by that manifest. Removed temporary build directories were not regenerated; the checker relied on the frozen binary hashes and re-computed every retained evidence-file hash.

## Final verdict

`PASS`. The selected-era boundary, legacy default, customer scenarios, platform evidence, rollback, and proof hashes are internally consistent and match the pinned MCP `2026-07-28` contract. No finding blocks the Phase 7 proof commit.
