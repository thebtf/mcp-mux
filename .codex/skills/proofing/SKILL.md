---
name: proofing
description: "Use when an architecture, runtime, or integration claim needs experimental proof before refactor or release -- grow a controlled model toward a global goal until it breaks or earns a bounded confidence verdict. Covers phased PoC ladders, per-phase success criteria, first-break isolation, no-break fix validation, production projection, and ADR generation."
argument-hint: "[target-slug|--continue <slug>|--adr <slug>|--auto|--stop-after-phase]"
---

# Proofing

## Purpose

Proofing turns an architecture debate into a falsifiable experiment. Use it to
build a controlled model that starts smaller than production, adds one
production-like mechanism per phase, and records the first layer where the claim
breaks or survives. The output is not "the PoC is green"; the output is a
bounded decision with evidence, risks, and an ADR-ready refactor path.

## When to Use

- A system failure has competing architecture explanations and logs alone do
  not isolate the boundary.
- A topology or lifecycle claim is disputed, such as "this architecture cannot
  work on Windows" or "the daemon restart must not break stdio shims."
- Production code is too noisy to debug directly without first proving the
  minimal correct behavior.
- A refactor would be expensive or risky unless a smaller experimental harness
  identifies the exact invariant to port.
- A previous fix attempt failed because it changed production logic before the
  failure model was understood.

## When to Skip

- The bug is directly reproducible in a small existing test.
- The fix is a narrow code defect with a clear source file and acceptance test.
- The experiment would require faking the very behavior under question.
- The only useful evidence is customer-mode production smoke; use the project
  smoke or production-ready workflow instead.

## Arguments

`$ARGUMENTS` is passed from the user's invocation:

- `target-slug`: optional proofing run name, for example
  `current-topology-hardening`.
- `--continue <slug>`: resume an existing proofing ledger and phase ladder.
- `--adr <slug>`: generate or refresh the ADR draft from an existing proofing
  ledger.
- `--auto`: continue phase execution until a stop condition, user gate, or
  production-port boundary is reached. This is the default when the user invokes
  proofing as an execution request.
- `--stop-after-phase`: complete one phase, write the next phase contract, and
  stop before executing it.
- Empty arguments: infer a short slug from the user's latest architecture
  question and create a new proofing run.

## Decision Rule

Use proofing when the next useful action is not "try another patch", but
"prove which model of the system is true." The phase ladder must answer a
specific global claim. If no global claim can be stated, run `nvmd-platform:debug`
or `nvmd-platform:investigate` first to gather enough facts.

Once proofing starts, the execution unit is the phase loop, not a single phase.
After every phase, either continue into the next proof phase immediately or
write a concrete stop gate. Do not stop after a green phase just because the
summary is tidy.

## Phase 0 -- Experimental Architect Mode

**Role:** Build falsifiable experimental models for architecture decisions.

**Mission:** Produce a phase-gated proof ledger and an ADR-ready refactor
decision.

**Forbidden:**

- Do not patch production code before stating the global claim and first
  falsifier.
- Do not add more than one production-like mechanism in a single phase.
- Do not treat a green toy PoC as production proof without a convergence map.
- Do not fix a break until the red phase is reproduced and recorded.
- Do not call a phase red unless the test proves the relevant state transition
  actually happened.

**Permitted reads / tools:**

- Existing `.agent/debug/**`, `.agent/reports/**`, `.agent/arch/decisions/**`,
  `docs/**`, and production source files related to the claim.
- Semantic code search first when available for unknown code paths; exact
  `rg` search for known error strings, config values, and logs.
- Synthetic fallback: if prior artifacts are missing, create a minimal
  `.agent/proofing/<slug>/proofing.md` ledger and mark prior evidence
  `UNKNOWN`.

**Required Output:**

- `.agent/proofing/<slug>/proofing.md` with sections named `Global Goal`,
  `Global Claim`, `Substrate`, `Phase Ladder`, `Evidence`, `Break Ledger`,
  `Next Phase Contract`, and `Decision`.
- A reusable acceptance command listed before Phase 1 starts.
- A next-phase contract after every closed phase, even when execution stops.
- An ADR draft or ADR update when the proofing run reaches a refactor decision.

**Named anti-patterns:**

- AP-PROOF-1 Green Toy Fallacy: the model passes because it omits the failing
  production mechanism.
- AP-PROOF-2 Kitchen Sink Phase: one phase changes multiple variables, so the
  pass or fail has no causal value.
- AP-PROOF-3 False Red: the test reports a break without proving the required
  restart, generation change, state change, or traffic condition occurred.
- AP-PROOF-4 Fresh Session Illusion: the test proves only a new session, while
  the claim concerns continuity of an existing connection.
- AP-PROOF-5 Fix Before Break: resilience is added before the failing phase is
  captured as evidence.
- AP-PROOF-6 Dead-End Phase Closure: a phase is summarized as done without a
  next-phase contract or a named stop gate.

**Exit gate:** Pass when every closed phase has a verdict, evidence, and next
phase contract, and the final decision maps evidence to a production change.
Fail when any anti-pattern is detected or the substrate can no longer converge
toward production.

**Handoff to next phase:** Carry forward the global claim, reusable runner,
phase ledger, and all recorded false-positive guards.

## Workflow

### Step 1: State the Global Goal and Claim

Start with the global goal: what end-state the proofing loop is trying to
decide, unblock, or authorize. Then write the claim in a falsifiable sentence:

```text
Given <topology/invariant>, can <system> preserve <user-visible behavior>
across <stress event> without <forbidden failure>?
```

Record:

- the global goal that ends the proofing loop;
- the user-visible behavior that must survive;
- the stress event that triggers the risk;
- the failure that would falsify the claim;
- the maximum acceptable gap between the experiment and production;
- the first acceptance command that will stay stable across phases.

### Step 2: Create the Proof Ledger

Create `.agent/proofing/<slug>/proofing.md` from
`templates/proofing-ledger.md`. Keep it as the durable state for the run.

Record verified facts separately from inferred claims. If the run is based on a
previous debug report, link that report instead of copying it.

If `--continue <slug>` is used, load the existing ledger and resume from
`Next Phase Contract`. If that section is empty, reconstruct it from the latest
open row in `Phase Ladder` before editing code.

If `--adr <slug>` is used, skip new experimental phases unless the ledger has
missing evidence. Project the completed phases to production and run Step 10.

### Step 3: Choose the Proof Substrate

Pick the smallest substrate that can express the global claim:

- standalone dummy program when production code has too many variables;
- repo-local harness around production binaries when the interfaces are already
  stable;
- production integration test only when the claim cannot be meaningfully
  modeled outside production code.

Start with no production imports unless the first phase explicitly needs them.
The substrate must use the same external boundary as the disputed behavior when
that boundary is the claim, for example `mcp-launcher` for stdio shim behavior.

### Step 4: Generate the Phase Ladder

Create phases before implementation. Each phase adds exactly one
production-like mechanism.

For each phase, record:

- `Mechanism`: the one new behavior added in this phase.
- `Success criteria`: exact observable pass signal.
- `Falsifier`: exact observable fail signal.
- `False-positive guard`: what must be proven so a PASS or FAIL is meaningful.
- `Instrumentation`: fields, counters, logs, or probes required for diagnosis.
- `Acceptance command`: the command to run after the phase.
- `Production convergence delta`: what remains unlike production after the
  phase.

Prefer this shape:

```text
Phase 0: minimal lifecycle authority
Phase 1: identity and ownership semantics
Phase 2: stale/zombie state detection
Phase 3: restart/snapshot readiness
Phase 4: same-connection continuity
Phase 5: concurrent/in-flight behavior
Phase 6: production parity port
```

Rename phases to match the actual system. Keep the ladder causal, not
chronological.

### Step 5: Execute One Phase at a Time

For each phase:

1. Implement only the phase mechanism and its instrumentation.
2. Run the stable acceptance command.
3. Record the exact output that proves PASS or FAIL.
4. Update the phase status in the ledger.
5. Re-run all earlier checks through the same runner.

If the command fails for harness reasons, mark the phase `INVALID`, fix the
harness, and re-run. Do not use an invalid run as architecture evidence.

### Step 6: Freeze the First Real Break

When a phase breaks:

1. Confirm the false-positive guard.
2. Record the red output before changing code.
3. Strengthen the test if it can pass or fail for the wrong reason.
4. Name the failing boundary, not just the failing function.
5. Add the smallest no-break mechanism.
6. Re-run the same red test until it turns green.
7. Record why the fix is minimal and what it still does not cover.

The first real break is the highest-value output of proofing. Preserve it as a
teaching artifact even after the PoC fix is green.

### Step 7: Close the Phase

Close every phase with a `Phase Closure` entry in the ledger:

- `Verdict`: PASS, FAIL, PARTIAL, or INVALID.
- `Evidence`: exact command and output excerpt.
- `Conclusion`: what the phase proved or refuted.
- `Remaining uncertainty`: what this phase did not cover.
- `Next Phase Contract`: the next phase name, mechanism, success criteria,
  falsifier, false-positive guard, instrumentation, acceptance command, and
  production convergence delta.
- `Automation decision`: one of `CONTINUE_NOW`, `STOP_USER_GATE`, `STOP_ADR`,
  `STOP_PRODUCTION_PORT`, or `STOP_SUBSTRATE_LIMIT`.

Use `CONTINUE_NOW` when the next phase is still inside the experimental
substrate, has a single mechanism, and materially reduces uncertainty. Use a
stop decision only when the next action crosses into production code, needs a
human product/architecture decision, or would make the substrate misleading.

### Step 8: Continue Until a Stop Condition

Continue adding phases while each new phase materially reduces uncertainty.
Stop when one of these is true:

- the global claim is falsified and the required production fix boundary is
  known;
- the global claim is supported through the agreed convergence depth;
- the next phase would duplicate production implementation rather than test a
  distinct invariant;
- the substrate diverges from production enough that further green phases would
  be misleading.

When stopping early, mark the decision `PARTIAL` and name the next phase that
would change confidence.

If none of the stop conditions are true, return to Step 5 and execute the next
phase. A final chat summary is not a stop condition.

### Step 9: Project Back to Production

Create a production projection table:

| Proof mechanism | Production owner | Current production gap | Port action | Verification |
| --- | --- | --- | --- | --- |

Every proposed refactor must map to a proofed mechanism or a recorded gap. If a
production change has no proof mapping, classify it as separate work.

### Step 10: Generate the ADR

When the proofing run reaches a decision, create or update
`.agent/arch/decisions/NNN-<decision>.md` using `templates/refactor-adr.md`.

The ADR must include:

- status and decision date;
- global claim;
- phase evidence table;
- first-break summary;
- chosen production refactor;
- consequences and risks;
- verification plan;
- rollback or reversibility notes.

Do not declare the production refactor done from the proofing ADR alone. The ADR
authorizes the next implementation contract.

## Output

Proofing produces:

- `.agent/proofing/<slug>/proofing.md`: durable phase ledger with `Global
  Goal`, per-phase criteria, `Phase Closure`, and `Next Phase Contract`.
- `experiments/<slug>/` or an equivalent repo-local harness when code is needed.
- `scripts/run-<slug>.ps1` or an equivalent stable acceptance runner.
- `.agent/reports/<slug>-proofing-YYYY-MM-DD.md`: narrative report when useful.
- `.agent/arch/decisions/NNN-<decision>.md`: ADR when the method reaches a
  refactor decision.

## Trigger Evaluation

This skill should trigger for prompts like:

- "prove whether this topology can work before refactoring it"
- "build a phased PoC and find the first layer that breaks"
- "continue the proofing ladder and generate an ADR when the claim converges"

It should not trigger for:

- a direct failing unit test with a known function-level fix;
- a production release checklist with no disputed architecture claim;
- a pure code review request.

## Verification

The proofing run is complete only when:

- every non-invalid phase has PASS, FAIL, or PARTIAL status with evidence;
- the stable acceptance runner is documented and has been run after the latest
  phase;
- the first real break, if found, was reproduced before the fix and converted by
  the same test after the fix;
- every closed phase has a `Next Phase Contract` and automation decision;
- the final decision states confidence, residual risk, and the next production
  action;
- the ADR maps every proposed production change to phase evidence.

## Related Skills

- `nvmd-platform:debug` for first-pass failure diagnosis before proofing.
- `nvmd-platform:investigate` for root-cause work after obvious fixes fail.
- `nvmd-platform:nvmd-architect` for production architecture decisions.
- `nvmd-platform:tdd` for turning proofed behavior into production tests.
- `nvmd-platform:production-ready-check` for release readiness after the
  production refactor lands.
- `nvmd-platform:skill-master` for improving this skill itself.
