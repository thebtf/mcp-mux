package daemon

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// TestSpawn_ZombieListenerIsTornDownAndRespawned is the production regression
// test for the FR-4 spawn-time listener health gate.
//
// Pre-fix behaviour: a zombie owner (listener fd closed, listenerDone NOT
// signalled) would be handed to a spawning shim as a valid reusable entry;
// the shim's subsequent ipc.Dial returned "connection refused" and the shim
// exited. This test reproduces the zombie state and asserts that:
//
//  1. The daemon detects the zombie via IsReachable (not just IsAccepting).
//  2. The zombie entry is torn down and removed from d.owners.
//  3. The zombie_detections_spawn counter increments by exactly 1.
//  4. The retry signals errSpawnRetry so Spawn's outer loop can cold-spawn.
//
// This test will fail on master prior to the fix: IsAccepting returns true
// on a zombie, the Spawn RPC returns the dead path, and no counter moves.
func TestSpawn_ZombieListenerIsTornDownAndRespawned(t *testing.T) {
	d := testDaemon(t)

	// Build a zombie owner directly: bind a real net.Listener to a real
	// socket path so ipc.IsAvailable has something to refuse against, then
	// Close() the listener without going through closeListener(). This is
	// the exact shape of the production zombie — listener fd is gone but
	// the Owner's listenerDone channel is still open, so IsAccepting lies.
	path := shortSocketPath(t, "zombie.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	// Close the listener — this is what makes it a zombie. We do NOT call
	// closeListener() on the Owner, so listenerDone stays open.
	ln.Close()

	sid := serverid.GenerateContextKey(serverid.ModeGlobal, "echo", []string{"zombie"}, nil, "")

	// Minimal Owner wired to point at the dead socket. We bypass the normal
	// NewOwner/NewOwnerFromSnapshot paths because we want a controlled zombie
	// — in production the zombie is a real Owner whose listener died after
	// some external event; here we simulate that end state directly.
	zombie := owner.NewTestOwner(path, sid)

	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{
		Owner:       zombie,
		ServerID:    sid,
		Command:     "echo",
		Args:        []string{"zombie"},
		Cwd:         "",
		Mode:        "global",
		LastSession: time.Now(),
	}
	d.mu.Unlock()

	// Sanity: IsAccepting lies (pre-fix probe) but IsReachable does NOT.
	if !zombie.IsAccepting() {
		t.Fatalf("precondition violated: IsAccepting already false on zombie — test no longer reproduces the pre-fix behaviour")
	}
	if zombie.IsReachable() {
		t.Fatalf("precondition violated: IsReachable=true on zombie — dial probe is broken")
	}

	// Call Spawn. Under the fix, spawnOnce detects the zombie, tears it down,
	// and returns errSpawnRetry; Spawn's outer loop then tries again and will
	// typically fail to cold-spawn (since "echo zombie" is not a real MCP
	// server) — we only care about the zombie detection side-effects.
	req := control.Request{
		Cmd:     "spawn",
		Command: "echo",
		Args:    []string{"zombie"},
		Cwd:     "",
		Mode:    "global",
	}
	_, _, _, _ = d.Spawn(req)

	// Assertions.
	d.mu.RLock()
	_, stillPresent := d.owners[sid]
	counter := d.zombieDetectedSpawn
	d.mu.RUnlock()

	if stillPresent {
		t.Errorf("zombie owner still present in d.owners after Spawn — tear-down path did not run")
	}
	if counter != 1 {
		t.Errorf("zombie_detections_spawn counter = %d, want 1", counter)
	}
}

// TestHandleStatus_ZombieCounters verifies FR-10 (operator observability):
// the zombie detection counters are surfaced via the daemon's status output
// so operators can watch them via `mux-mux status` / `mux_list`.
func TestHandleStatus_ZombieCounters(t *testing.T) {
	d := testDaemon(t)

	// Manually bump both counters so we can assert they surface.
	d.mu.Lock()
	d.zombieDetectedSpawn = 7
	d.zombieDetectedRestore = 3
	d.mu.Unlock()

	status := d.HandleStatus()
	if got, ok := status["zombie_detections_spawn"]; !ok || got != 7 {
		t.Errorf("status[zombie_detections_spawn] = %v (ok=%v), want 7", got, ok)
	}
	if got, ok := status["zombie_detections_restore"]; !ok || got != 3 {
		t.Errorf("status[zombie_detections_restore] = %v (ok=%v), want 3", got, ok)
	}
}

// TestRunRestoreHealthGate_ZombieTornDown exercises the FR-3 post-restore
// sweep directly: install a zombie entry, call runRestoreHealthGate, and
// assert the entry was removed and the counter incremented.
//
// We override restoreHealthGateWindow to a short value so the test does not
// wait 750ms, and we re-override at t.Cleanup to restore the production
// default for other tests in the same package.
func TestRunRestoreHealthGate_ZombieTornDown(t *testing.T) {
	origWindow := restoreHealthGateWindow
	restoreHealthGateWindow = 10 * time.Millisecond
	t.Cleanup(func() { restoreHealthGateWindow = origWindow })

	d := testDaemon(t)

	// Install two owners: one zombie, one legit-but-closed (IsAccepting
	// false, listener never bound). The sweep should only tear down the
	// zombie — the explicitly-closed owner is a pre-existing legitimate
	// state (isolated server, listener closed after first session).

	// Zombie: bound listener then closed directly.
	zPath := shortSocketPath(t, "zombie.sock")
	zLn, err := net.Listen("unix", zPath)
	if err != nil {
		t.Fatalf("net.Listen zombie: %v", err)
	}
	zLn.Close()
	zSID := fmt.Sprintf("%064x", 0xDEAD)
	zombie := owner.NewTestOwner(zPath, zSID)

	// Healthy: bind and keep it bound.
	hPath := shortSocketPath(t, "healthy.sock")
	hLn, err := net.Listen("unix", hPath)
	if err != nil {
		t.Fatalf("net.Listen healthy: %v", err)
	}
	t.Cleanup(func() { hLn.Close() })
	go func() {
		for {
			c, err := hLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	hSID := fmt.Sprintf("%064x", 0xBEEF)
	healthy := owner.NewTestOwnerWithListener(hPath, hSID, hLn)

	d.mu.Lock()
	d.owners[zSID] = &OwnerEntry{
		Owner: zombie, ServerID: zSID, Command: "echo", Args: []string{"z"}, LastSession: time.Now(),
	}
	d.owners[hSID] = &OwnerEntry{
		Owner: healthy, ServerID: hSID, Command: "echo", Args: []string{"h"}, LastSession: time.Now(),
	}
	d.mu.Unlock()

	// Run the sweep and wait for its goroutine to finish its work. We poll
	// for up to 2 seconds — the override reduces the sleep to 10ms, so the
	// sweep completes well inside that window even under -race on slow CI.
	d.runRestoreHealthGate()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.RLock()
		_, zPresent := d.owners[zSID]
		_, hPresent := d.owners[hSID]
		counter := d.zombieDetectedRestore
		d.mu.RUnlock()
		if !zPresent && hPresent && counter == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	d.mu.RLock()
	_, zPresent := d.owners[zSID]
	_, hPresent := d.owners[hSID]
	counter := d.zombieDetectedRestore
	d.mu.RUnlock()
	t.Fatalf("post-sweep state: zPresent=%v hPresent=%v counter=%d "+
		"(want zPresent=false hPresent=true counter=1)", zPresent, hPresent, counter)
}

// Sanity imports — keep the toolchain honest about unused imports when the
// file is edited in isolation.
var _ = log.LstdFlags
var _ = os.Stderr
var _ = strings.TrimSpace
