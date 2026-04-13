# Standard Requirements Quality Checklist

**Feature:** Graceful Daemon Restart with State Snapshot Transfer
**Focus:** Full spec quality (serialization, loading, control protocol, edge cases)
**Created:** 2026-04-07
**Depth:** Standard

## Requirement Completeness

- [ ] CHK001 Are all snapshot fields enumerated exhaustively in FR-1? [Completeness, Spec §FR-1]
- [ ] CHK002 Is the snapshot file path (well-known location) explicitly specified? [Completeness, Spec §FR-2]
- [ ] CHK003 Are drain semantics for graceful-restart defined (timeout, in-flight handling)? [Completeness, Spec §FR-3]
- [ ] CHK004 Is the upstream background spawn trigger defined (on first request vs on snapshot load)? [Completeness, Spec §FR-4]
- [ ] CHK005 Are all 5 cached response types (init, tools, prompts, resources, resource_templates) included in snapshot schema? [Completeness, Spec §FR-1]

## Requirement Clarity

- [ ] CHK006 Is "< 2 seconds" in NFR-1 measured from IPC EOF or from CC detecting server failure? [Clarity, Spec §NFR-1]
- [ ] CHK007 Is "valid snapshot" precisely defined (schema version match + freshness + parseable JSON)? [Clarity, Spec §FR-2]
- [ ] CHK008 Is "cache-first mode" behavior for non-init requests (tools/call, sampling) specified? [Clarity, Spec §FR-4]
- [ ] CHK009 Is the atomic write mechanism (temp file + rename) specified for all platforms? [Clarity, Spec §NFR-4]

## Requirement Consistency

- [ ] CHK010 Does FR-1 snapshot state align with plan.md OwnerSnapshot schema? [Consistency, Spec §FR-1 vs Plan §Data Model]
- [ ] CHK011 Does FR-3 drain behavior align with existing HandleShutdown drain semantics? [Consistency, Spec §FR-3]
- [ ] CHK012 Is the "5 minutes" staleness in FR-5 consistent with the clarification C1 (hardcoded)? [Consistency, Spec §FR-5 vs Clarifications]

## Acceptance Criteria Quality

- [ ] CHK013 Can US1 AC "completes in < 5 seconds" be measured automatically? [Measurability, Spec §US1]
- [ ] CHK014 Can US1 AC "no failed status" be verified programmatically (mux_list check)? [Measurability, Spec §US1]
- [ ] CHK015 Can US2 AC "starts in < 3 seconds" be measured from process start to first control socket ready? [Measurability, Spec §US2]
- [ ] CHK016 Are US3 ACs testable without 4 real CC sessions (mock sessions in test)? [Measurability, Spec §US3]

## Scenario Coverage

- [ ] CHK017 Are requirements defined for partial snapshot (some owners serialized, others failed)? [Coverage, Edge Case]
- [ ] CHK018 Are requirements for snapshot during high-load (many pending requests) specified? [Coverage, Spec §FR-3]
- [ ] CHK019 Are requirements for snapshot with zero owners (fresh daemon, nothing to save) defined? [Coverage, Edge Case]
- [ ] CHK020 Is behavior specified when new binary has different mux_version than snapshot? [Coverage, Spec Edge Case]

## Edge Case Coverage

- [ ] CHK021 Is the snapshot version field format and comparison logic defined? [Edge Case, Spec §Edge Cases]
- [ ] CHK022 Is behavior for snapshot file locked by another process (Windows) specified? [Edge Case, Gap]
- [ ] CHK023 Is behavior for disk full during snapshot write specified? [Edge Case, Gap]

## Non-Functional Requirements

- [ ] CHK024 Is the < 1 MB snapshot size threshold validated against actual cached response sizes? [NFR, Spec §NFR-3]
- [ ] CHK025 Is cross-platform atomic rename behavior verified (os.Rename on Windows with existing target)? [NFR, Spec §NFR-4]

## Dependencies and Assumptions

- [ ] CHK026 Is the assumption "shims auto-reconnect via resilient_client" validated against current implementation? [Dependency, Spec §Dependencies]
- [ ] CHK027 Is the assumption "daemon lock file prevents double-start" verified against daemon.go? [Dependency, Spec §Edge Cases]
