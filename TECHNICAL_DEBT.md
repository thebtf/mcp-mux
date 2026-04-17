# Technical Debt

All items from the 2026-04-08 debt batch have been resolved:

- ✅ **Progress notifications forwarding** — implemented earlier via
  `Owner.progressOwners`, `trackProgressToken`, and `routeProgressNotification`
  in `internal/mux/owner.go`. `notifications/progress` from upstream is now
  routed to the originating session using the `_meta.progressToken` mapping,
  with broadcast fallback if routing fails.

- ✅ **Tool call timeout** — implemented in v0.10.1 as `x-mux.toolTimeout`
  capability. Upstream servers declare their acceptable tool-call wait time
  (in seconds); mux starts a watchdog goroutine per `tools/call` request.
  If upstream doesn't respond within the window, the watchdog synthesizes
  a JSON-RPC error response (`code -32000`) and delivers it to the session.
  Uses atomic `sync.Map.LoadAndDelete` to race cleanly with the natural
  response path — whichever wins the claim delivers the response.

- ✅ **Inflight progress injection (research)** — moved to engram as a
  research task. The debt entry was explicitly marked "if feasible" and
  requires CC-side investigation of MCP progress-display support. Not
  suitable for implementation without upstream protocol research.

---

## Open items

### 2026-04-18: `upstream.Start` Wait-vs-ReadLine race (muxcore/upstream)

**What:** `muxcore/upstream/process.go::Start` uses `cmd.StdoutPipe()`,
whose docs state explicitly: *"it is incorrect to call Wait before all
reads from the pipe have completed."* Our code violates this by running
`proc.Wait()` in a background goroutine (to signal `Done`) while
`ReadLine()` is invoked asynchronously by external callers. When the
upstream exits quickly (e.g. `echo hello` in `TestStartAndClose`), `Wait`
closes the stdout pipe before `ReadLine` has scanned it, and bufio.Scanner
surfaces `read |0: file already closed` (os.ErrClosed).

**Why deferred:** The fix requires replacing `cmd.StdoutPipe()` with
explicit `os.Pipe()` + `cmd.Stdout = writer` so cmd.Wait no longer owns
the reader side, OR adding a drain goroutine that copies stdout into an
internal buffered reader before Wait returns. Both changes touch the
production upstream spawn path and require cross-platform validation
(procgroup wraps cmd differently on Windows vs Unix). Out of scope for
PR #64 (zombie-listener fix).

**Impact:**
- CI flake on fast machines when a test upstream exits immediately after
  writing stdout. Observed on `TestStartAndClose` (muxcore/upstream) and
  `TestRespondToRootsList_WithCwd` (muxcore/owner). Rerun-pass rate ~90%.
- In production, MCP upstreams are long-lived (server process keeps
  running to serve tool calls) — the race is not triggerable under the
  documented usage. The test setup using `echo` is the artificial case.

**Mitigation applied in PR #64 (boy-scout):** `ReadLine` now maps
`os.ErrClosed` → `io.EOF` so callers that read after Wait closed the pipe
see a clean EOF instead of a misleading "file already closed" error.
Does NOT fix the underlying race.

**Files:**
- `muxcore/upstream/process.go::Start` (line ~66-134 — Wait goroutine
  and StdoutPipe usage)
- `muxcore/upstream/process_test.go::TestStartAndClose` (the flaky test)
- `muxcore/owner/coverage_test.go::TestRespondToRootsList_WithCwd` (the
  second flake; symptom is `upstream: process closed` when mock_server
  exits before respondToRootsList completes)

**Context:** First observed on PR #63's addition of `muxcore` to CI.
Re-surfaced on PR #64 at higher rate because the new test load (restore
health gate sweep + three extra daemon-package tests) shifts timing on
shared CI runners.

**Proposed fix (next iteration):**
1. Introduce `drainStdout` goroutine in `Start`: `io.Copy(internalBuf,
   p.stdout)` with a mutex-protected deque of lines. `ReadLine` reads
   from the deque, not directly from the pipe.
2. Wait goroutine proceeds independently; pipe closure no longer races
   with ReadLine because data is buffered before close.
3. Add regression test that spawns a process which writes N lines then
   exits immediately; assert all N lines are read.
4. Validate on ubuntu/windows/macos under `-race`.

