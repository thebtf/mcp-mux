package daemon

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// mockServerAbsPath returns the absolute path to the mock server source so
// the upstream subprocess can find it regardless of the per-test working
// directory used in Spawn requests.
func mockServerAbsPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../testdata/mock_server.go")
	if err != nil {
		t.Fatalf("resolve mock_server.go path: %v", err)
	}
	return abs
}

// TestSpawn_ParallelStormSharedConverges is the integration regression test
// for Engram #244 Bug 1 (shared classification does not share). Under the
// post-CR-002-phase-A default Mode flip from ModeCwd to ModeGlobal, N
// concurrent Spawn calls for the same (cmd, args) — regardless of cwd —
// compute the same global sid, hit `d.owners[sid]` exactly, and either
// create-as-placeholder or bind-as-secondary. The end state is ONE owner.
//
// Spec evidence (AC1, verbatim from spec.md):
//
//	"A parallel-spawn storm of N goroutines calling `Spawn` for the same
//	 `(cmd, args)` from N distinct cwds — where the upstream's `tools/list`
//	 classifies as `shared` — produces exactly **1** owner. OwnerCount() == 1,
//	 owner.cwdSet contains all N canonicalized cwds, every Spawn call returns
//	 the same server_id."
//
// I am proving that contract by running 4 goroutines through a barrier so
// they hit Spawn at the same instant (recreating the workstation-startup
// parallel-spawn storm). Each goroutine omits Request.Mode so the daemon
// applies the new ModeGlobal default per CR-002 phase A.
//
// Note: this test does NOT exercise the cross-cwd admission gate (CR-002
// phase B). It exercises the simpler convergence-via-exact-sid path that
// global-first default unlocks. The admission gate matters only for
// upstreams that classify as isolated mid-storm; shareable upstreams
// (which mock_server.go is, per its tools/list with no isolated patterns)
// converge correctly via the direct sid hit.
func TestSpawn_ParallelStormSharedConverges(t *testing.T) {
	orig := concurrentCreateWaitTimeout
	concurrentCreateWaitTimeout = 3 * time.Second
	t.Cleanup(func() { concurrentCreateWaitTimeout = orig })

	const N = 4
	results := make([]string, N)
	errs := make([]error, N)

	var wg sync.WaitGroup
	start := make(chan struct{})

	mockServer := mockServerAbsPath(t)

	// TempDirs allocated BEFORE testDaemon so the daemon's Shutdown LIFO
	// cleanup terminates upstream subprocesses BEFORE TempDir rm-rf runs
	// on Windows. Otherwise the subprocess's open cwd handle fails the
	// rm-rf and marks the test FAIL post-assertions.
	cwds := make([]string, N)
	for i := 0; i < N; i++ {
		cwds[i] = t.TempDir()
	}

	d := testDaemon(t)

	for i := 0; i < N; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			<-start
			req := control.Request{
				Cmd:     "spawn",
				Command: "go",
				Args:    []string{"run", mockServer},
				Cwd:     cwds[i],
				// Mode omitted — relies on CR-002 phase A default flip to
				// ModeGlobal. Under the prior ModeCwd default this storm
				// would produce 4 distinct sids; under ModeGlobal it
				// converges to 1.
			}
			_, sid, _, err := d.Spawn(req)
			results[i] = sid
			errs[i] = err
		}()
	}

	close(start) // release the storm
	wg.Wait()

	// Surface any individual Spawn failures with full context.
	for i, e := range errs {
		if e != nil {
			t.Fatalf("Spawn %d failed: %v", i, e)
		}
	}

	// All goroutines must have converged to a single shared owner sid.
	seen := make(map[string]int)
	for _, sid := range results {
		seen[sid]++
	}
	if len(seen) != 1 {
		t.Errorf("parallel storm produced %d distinct shared-owner sids (want 1): %v",
			len(seen), seen)
	}

	// Live owner count must also be 1 — no extra orphans created on
	// losing-race paths.
	if got := d.OwnerCount(); got != 1 {
		t.Errorf("OwnerCount = %d after storm, want 1", got)
	}

	var sharedSID string
	for sid := range seen {
		sharedSID = sid
	}
	entry := d.Entry(sharedSID)
	if entry == nil || entry.Owner == nil {
		t.Fatalf("shared owner %q missing after storm", sharedSID)
	}
	wantCwds := make(map[string]struct{}, N)
	for _, cwd := range cwds {
		wantCwds[serverid.CanonicalizePath(cwd)] = struct{}{}
	}
	gotCwds := entry.Owner.ExportSnapshot().CwdSet
	if len(gotCwds) != len(wantCwds) {
		t.Fatalf("shared owner cwdSet = %v, want %d distinct roots", gotCwds, len(wantCwds))
	}
	for _, cwd := range gotCwds {
		if _, ok := wantCwds[cwd]; !ok {
			t.Fatalf("shared owner cwdSet contains unexpected root %q; want %v", cwd, wantCwds)
		}
	}
}
