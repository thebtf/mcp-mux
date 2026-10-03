# Feature Specification: Upstream maintenance hold

**Feature Branch**: `002-upstream-maintenance-hold`

**Created**: 2026-10-03

**Status**: Draft, requirements reviewed against accepted ADR-015; implementation not started.

**Input**: [GitHub #135](https://github.com/thebtf/mcp-mux/issues/135): release a managed upstream for in-place executable replacement and suppress respawn until explicit resume or safe timeout. Existing stop is not a maintenance hold.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Replace an upstream executable without a respawn race (Priority: P1)

An installer selects an exact managed upstream, requests a bounded maintenance hold, and waits for confirmation that its managed process trees have ended. It replaces the executable without shutting down connected hosts or racing automatic restart.

**Why this priority**: The reported installer failure occurs because automatic restart immediately re-locks the executable. A stop acknowledgement alone does not make replacement safe.

**Independent Test**: Keep two legacy hosts connected to an upstream with a child process. Hold the target, verify every process in the stated scope has ended, overwrite the running version's executable, and observe no replacement process before release of the hold. Also exercise one native modern host with an isolated upstream.

**Acceptance Scenarios**:

1. **Given** an exact managed upstream and connected hosts, **When** an authorized installer requests a hold, **Then** new demand cannot recreate that launch family, existing work gets the requested bounded drain opportunity, and successful held confirmation follows full-tree retirement of every matched generation.
2. **Given** retirement is incomplete or cannot be proven, **When** the drain deadline or hold expiry arrives, **Then** the product reports blocked retirement rather than safe replacement and continues to suppress starts until retirement is proven.
3. **Given** a successful hold, **When** an installer overwrites the executable and hosts continue issuing requests, **Then** the executable remains free of locks from the scoped managed trees and no scoped replacement process starts.
4. **Given** the selected target cannot unambiguously identify its managed launch family, **When** a hold is requested, **Then** the operation refuses before it changes unrelated targets.

### User Story 2 - Receive explicit maintenance errors on the same host connection (Priority: P1)

A connected host receives a clear maintenance error instead of hanging, causing a respawn, or silently replaying work after the update. Its original input and output connection stays open.

**Why this priority**: The installer outcome must not require users to terminate their hosts. Replaying rejected work could repeat a state-changing operation.

**Independent Test**: Send distinguishable numeric and string request IDs while the hold is acquiring and active, including a request buffered during reconnect. Observe one terminal error for each request, no delivery upstream, and successful fresh demand on the same host connection after safe resume where that route supports reconnect.

**Acceptance Scenarios**:

1. **Given** the hold has fenced new demand, **When** a host sends a request, **Then** the product returns a distinct held-for-update error with the original request ID, does not serve a cached success, and does not replay that request later.
2. **Given** a reconnect attempt discovers a hold, **When** a request arrives or was buffered during that held interval, **Then** the host receives an immediate maintenance error without waiting for ordinary reconnect grace, and its transport stays open.
3. **Given** a notification has no request ID, **When** it arrives during maintenance, **Then** no response ID is invented and it cannot start the upstream or become queued replay.
4. **Given** a supported legacy route resumes safely, **When** the host sends fresh demand, **Then** one replacement generation serves that demand over the same host transport; no held or unfinished request is replayed.
5. **Given** a native modern route crosses a loss or replacement boundary, **When** the host continues, **Then** it uses fresh same-era admission or receives an explicit new-launch-required refusal. No legacy fallback, request replay, progress continuation, or subscription replay is introduced.

### User Story 3 - Resume, renew, or expire one exact hold (Priority: P2)

An installer uses the returned hold identity to extend its maintenance window or resume safely. If it disappears, a bounded timeout restores availability only after retirement is proven.

**Why this priority**: File replacement takes variable time. The system needs a deterministic recovery path without giving a competing installer authority over another lease.

**Independent Test**: Exercise explicit resume, renewal, expiry, competing acquisition, stale identities, and failed retirement with controlled deadlines. Observe which exact hold changes and whether a fresh start is allowed.

**Acceptance Scenarios**:

1. **Given** proven retirement and an unexpired hold, **When** its authorized holder resumes using the exact hold identity, **Then** suppression clears for that hold and fresh demand may start one replacement generation.
2. **Given** an active hold, **When** its holder renews with a valid duration, **Then** the new expiry is calculated from the renewal's acceptance time and returned; a competing acquisition or stale hold identity cannot change it.
3. **Given** proven retirement and no renewal, **When** the accepted expiry is reached, **Then** fresh demand may start normally; no rejected or unfinished work is replayed.
4. **Given** retirement is blocked, **When** a holder requests resume or the deadline expires, **Then** the hold remains fail closed and the result explains that retirement has not been proven.
5. **Given** a missing, nonpositive, excessive, or invalid duration, **When** the operation validates input, **Then** it either applies the documented default for omission or rejects invalid input before altering the current hold.

### User Story 4 - Keep maintenance safe across restart and identity changes (Priority: P1)

An operator can inspect the exact maintenance state and trust that a daemon restart, a retry identity, a changed modern session identity, or another security context does not bypass or inherit the hold.

**Why this priority**: A hold that disappears during lifecycle re-entry is unsafe for executable replacement.

**Independent Test**: Restart a hold-aware daemon before fresh demand; attempt same-family retries and modern reconnections, different-credential and different-namespace launches, stale generation callbacks, and a hold-unaware client or daemon. Verify refusal or suppression with no unintended process starts and no secret disclosure.

**Acceptance Scenarios**:

1. **Given** an active hold, **When** controlled daemon restart, handoff, shutdown, or downgrade is requested, **Then** this release refuses terminally without fallback or successor start. After unplanned daemon loss, a hold-aware replacement restores proven held suppression before admission and keeps incomplete retirement blocked.
2. **Given** an incomplete, corrupt, or unreadable saved maintenance state, **When** recovery occurs, **Then** the product does not assume safe retirement or allow a potentially scoped start.
3. **Given** a held launch family, **When** a modern reconnect changes its visible identity or an isolated retry uses a new identity, **Then** matching demand remains held. Scope is the selected owner's finite already-admitted contexts. Different working directories, protocol eras, security contexts, and engine namespaces neither inherit nor clear the hold unless already included explicitly.
4. **Given** a stale lease identity or process-generation callback, **When** it arrives after a newer lease or generation exists, **Then** it cannot clear suppression or retire the newer unrelated generation.
5. **Given** an old daemon or client cannot express maintenance, **When** a hold is attempted, **Then** the product reports unsupported instead of substituting stop, kill, or restart. A hold-aware daemon fences old shims but cannot promise their immediate held errors or non-replay. An arbitrary old binary is outside this feature's control.
6. **Given** an authorized status reader, **When** it inspects maintenance, **Then** it receives the lease state, expiry, and retirement outcome without credentials, environment values, request contents, or private identity material.

### Edge Cases

- A process start and a hold request race. The start must be included in retirement or blocked; successful held confirmation cannot leave a generation outside the claimed scope.
- Several matched managed generations exist. All must retire, or the operation must refuse ambiguity before it begins.
- A leader exits but a descendant still owns the executable. Retirement remains blocked.
- The installer exits after acquisition. Proven held state expires deterministically; unproven retirement does not expire into a start.
- A drain deadline of zero requests force retirement. Positive deadlines report the actual bound used.
- Resume and renewal race with expiry. One serialized outcome applies to the exact current lease; stale operations do not affect a subsequent hold.
- Restart recovery finds expired proven-held state versus expired incomplete retirement. Only the proven-held case may safely release suppression.
- Cached responses, reconnect buffers, persistent recovery, direct owner starts, and sibling launch attempts must not bypass maintenance.
- Independent engines or unmanaged processes also use the executable. The product does not claim those processes have retired or that an unrelated lock was released.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The product MUST provide an authorized hold operation targeting an exact existing managed upstream and return an opaque hold identity, state, accepted expiry, and actual drain deadline.
- **FR-002**: The product MUST establish suppression before drain and serialize it against every managed start of the identified launch family, including retry, reconnect, recovery, and direct launch paths. Success MUST cover every matched extant generation, or ambiguity MUST be rejected before acquisition.
- **FR-003**: Successful held confirmation MUST require proven retirement of the complete scoped process trees. A surviving descendant or failed proof MUST produce blocked retirement and retain suppression.
- **FR-004**: The product MUST let already-forwarded requests finish until the accepted drain deadline, then retire the scoped trees. Zero drain duration MUST skip request-drain grace. Positive grace MUST begin once at fence commitment and MUST NOT reset on retry. Held confirmation MUST require tree death, not transferred live authority, and an unexpired usable lease.
- **FR-005**: Every request received after suppression is established MUST receive a distinct held-for-update error with its original ID rather than upstream delivery, cached success, or delayed ordinary-reconnect failure. Existing host input and output connections MUST remain open.
- **FR-006**: The product MUST NOT replay requests rejected during maintenance or unfinished work ended by retirement. Notifications MUST NOT trigger start, queued replay, or an invented response ID.
- **FR-007**: Resume MUST require the exact current hold identity and proven retirement. It MUST permit fresh demand without proactively replaying work and MUST reject stale or competing identities.
- **FR-008**: Renewal MUST require the exact active hold identity, set expiry from acceptance time plus the accepted duration, and return that expiry. Competing acquisition MUST report conflict instead of silently replacing an active lease.
- **FR-009**: The hold duration MUST be positive and at most one hour, with a five-minute default. Proven held state MUST release suppression at accepted expiry; incomplete retirement MUST remain blocked regardless of deadline.
- **FR-010**: This release MUST refuse controlled restart, handoff, shutdown, and downgrade terminally while any fence remains, without fallback, force bypass, successor start, or idle daemon exit. After unplanned loss, a hold-aware daemon MUST restore proven held fences before admission and retain incomplete retirement as blocked. Failed persistence or recovery MUST fail closed and MUST NOT report safe held confirmation.
- **FR-010a**: Recovery and activation MUST validate the complete paired maintenance authority. Missing, pending, invalid, ambiguous JSON, or mismatched authority MUST fail closed. A persistence error MUST remain an error and retain conservative current-memory admission; it MUST NOT promise rollback of all storage effects. Recovery MAY accept a matching COMMITTED certificate only as proof of earlier acknowledged durable publication. Successful acknowledged release MUST NOT resurrect its predecessor lease.
- **FR-011**: Matching MUST survive retry and modern-session identity changes across the selected owner's finite admitted context set, with exact working-directory, protocol-era, engine-namespace, and security/configuration-context isolation. Incomplete or ambiguous sets MUST be refused before acquisition. Stale callbacks MUST NOT act on a different current generation or lease.
- **FR-012**: Maintenance control and readback MUST use existing local authorization boundaries and MUST NOT expose raw environment values, credentials, request contents, or private matching material.
- **FR-013**: Hold-aware callers contacting an incapable daemon MUST receive unsupported without fallback to stop, kill, restart, or direct execution. Managed standalone paths MUST require coordinated admission or refuse explicitly. An aware daemon MUST fence matching old-shim starts, but immediate held errors and non-replay guarantees require aware shims. Arbitrary old binaries and manual replacement of active launch pointers are unsupported.
- **FR-014**: Maintenance MUST preserve native modern isolation and lifecycle safety. Modern continuation MUST use fresh exact-era admission or explicit refusal, never legacy bootstrap, mixed-era attachment, replay, or automatic subscription restoration.
- **FR-015**: Existing stop, restart, sharing, and request behavior outside active maintenance MUST remain unchanged except where coordinated managed admission is required to prevent standalone bypass. Unsupported standalone paths MUST refuse explicitly. Status MUST expose safe maintenance state and expiry sufficient for an installer to distinguish held, blocked, and released outcomes.
- **FR-016**: Acceptance MUST include a runnable live-process executable-replacement proof with connected hosts and a managed descendant on Windows and Unix, plus focused regression evidence for fencing, identity, TTL, restart, unsupported behavior, and no replay.

### Key Entities *(include if feature involves data)*

- **Maintenance target**: An exact managed upstream and its finite already-admitted launch/reconnect contexts, separated by local namespace, working directory, protocol era, and security/configuration context.
- **Hold lease**: One opaque identity, target scope, maintenance state, expiry, and drain deadline. It authorizes resume and renewal only for the current hold.
- **Process generation**: A specific managed tree whose exact retirement must be proven before replacement is reported safe.
- **Maintenance outcome**: Acquiring, proven held, retirement blocked, or released state, with a safe explanation and accepted deadlines.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With two live legacy hosts and a managed descendant, one installer hold permits an actual overwrite of the previously running executable with zero surviving scoped processes and zero scoped starts during the hold. Demonstrate this on Windows and Unix.
- **SC-002**: Every numeric-ID and string-ID request submitted during maintenance returns exactly one explicit maintenance error carrying its original ID. None reaches the upstream or reappears after resume, including requests buffered during reconnect.
- **SC-003**: Fresh legacy demand after explicit resume and after safe expiry reaches exactly one replacement generation reporting the new executable version on the original host connection.
- **SC-004**: Renewal, conflict, stale identity, expiry, and blocked-retirement cases each produce the specified deterministic state. No incomplete retirement case permits a start.
- **SC-005**: Every controlled lifecycle attempt during a fence refuses without fallback. Unplanned aware recovery, retry-family, modern-session, namespace, working-directory, credential-context, and stale-generation cases preserve suppression or refuse safely, with zero held-family starts.
- **SC-006**: One native modern host using an aware shim demonstrates held-ID errors and then fresh same-era admission or explicit new-launch-required refusal, with zero legacy bootstrap or replay traffic. Hold-aware callers receive unsupported from old daemons without destructive fallback; old-shim and arbitrary-old-binary limitations are documented rather than claimed safe.
- **SC-007**: Maintenance readbacks contain zero secret values or request bodies, and unrelated targets retain their existing operation throughout the hold.

## Assumptions

- Scope is GitHub #135 only. The installer performs replacement itself after proven held confirmation; mcp-mux does not install software or promise rollback of installer changes.
- The local namespace controls only its managed launch family. Independent engines, other credential contexts, and unmanaged processes may hold unrelated file locks.
- The five-minute default and one-hour maximum make abandoned proven holds bounded. A blocked retirement remains fail closed rather than treating elapsed time as process proof.
- Existing local authorization and process-tree authority remain prerequisites. This specification requires their externally observable guarantees without selecting their technical mechanism.
- Original-ID immediate-error and no-replay guarantees apply to maintenance-aware shims. Older shims retain only the start fence enforced by the aware daemon; unmanaged old binaries cannot be fenced by this feature.
- Active-lease transfer is excluded. Controlled restart, handoff, shutdown, downgrade, and empty-daemon idle exit stay blocked until safe release. Incomplete retirement after unplanned loss stays blocked; metadata does not recreate lost process-tree authority.
- Released modern lifecycle quarantine remains in force. Same-host transparent modern restoration is not promised where existing native admission requires a new launch.
- Acceptance and release require the repository's existing cross-platform regression, production, and consumer-delivery gates. This SpecKit artifact does not claim implementation or release completion.
