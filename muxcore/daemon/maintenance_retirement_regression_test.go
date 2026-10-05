package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func TestMaintenanceRestartRejectsLostExactEntry(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprintf("replaced_%t", replaced), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			d := maintenanceDaemon(t)
			d.persistent = true
			req, _, _, effects := maintenanceHelperRequest(t)
			req.Mode = "cwd"
			path, sid, token, err := d.Spawn(req)
			if err != nil {
				t.Fatal(err)
			}
			entry := d.Entry(sid)
			conn, scanner := connectSpawnedOwner(t, path, token)
			defer conn.Close()
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"restart-race","version":"1"}}}`)
			readDaemonResponseID(t, scanner, "1")
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":2,"method":"maintenance/write","params":{"marker":"original"}}`)
			if daemonRespawnGeneration(t, readDaemonResponseID(t, scanner, "2")) != 1 {
				t.Fatal("original helper generation was not live")
			}
			pid, _ := entry.Owner.Status()["upstream_pid"].(int)
			if !daemonTestProcessAlive(pid) {
				t.Fatal("restart fixture had no live upstream")
			}
			replacementReq := req
			replacementReq.Env = mergeEnv(req.Env)
			replacementReq.Env["MCPMUX_MAINTENANCE_CONTEXT"] = "replacement"
			type replacement struct {
				entry   *OwnerEntry
				path    string
				token   string
				pending int
				err     error
			}
			swapped := make(chan replacement, 1)
			original := finalizeOwnerForRemoval
			var once atomic.Bool
			finalizeOwnerForRemoval = func(o *owner.Owner, soft bool) (int, bool, error) {
				exit, finalized, err := original(o, soft)
				if o != entry.Owner || !finalized || !once.CompareAndSwap(false, true) {
					return exit, finalized, err
				}
				// Settle a competing exact-generation removal after real tree death,
				// but before the restart's removal performs its final registry CAS.
				d.mu.Lock()
				if d.owners[sid] != entry {
					d.mu.Unlock()
					failure := errors.New("original entry changed before the race barrier")
					swapped <- replacement{err: failure}
					return exit, finalized, failure
				}
				prepared := d.prepareOwnerRemovalLocked(sid, entry, ownerRemovalReasonOperatorHard, false)
				d.mu.Unlock()
				if removeErr := d.finishOwnerRemoval(prepared); removeErr != nil {
					swapped <- replacement{err: removeErr}
					return exit, finalized, removeErr
				}
				var next replacement
				if replaced {
					var replacementSID string
					next.path, replacementSID, next.token, next.err = d.Spawn(replacementReq)
					if next.err == nil && replacementSID != sid {
						next.err = fmt.Errorf("replacement ID %q differs from %q", replacementSID, sid)
					}
					if next.err == nil {
						next.entry = d.Entry(sid)
						next.pending = next.entry.Owner.SessionMgr().PendingCount()
					}
				}
				swapped <- next
				return exit, finalized, errors.Join(err, next.err)
			}
			t.Cleanup(func() {
				d.shutdown(nil)
				finalizeOwnerForRemoval = original
			})

			resp, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "restart_owner", ServerID: sid}, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var next replacement
			select {
			case next = <-swapped:
				if next.err != nil {
					t.Fatal(next.err)
				}
			default:
				t.Fatal("restart did not reach the exact-entry race barrier")
			}
			wantErr := control.ErrMaintenanceNotFound
			wantOwners, wantGeneration := 0, "1"
			if replaced {
				wantErr = control.ErrMaintenanceConflict
				wantOwners, wantGeneration = 1, "2"
			}
			if resp.OK || !errors.Is(resp.Err(), wantErr) || resp.Token != "" || resp.IPCPath != "" {
				t.Errorf("stale restart response = %+v, want typed %s without a new admission", resp, wantErr.Code)
			}
			if daemonTestProcessAlive(pid) || d.OwnerCount() != wantOwners {
				t.Errorf("stale restart changed process ownership: original_live=%t owners=%d want=%d", daemonTestProcessAlive(pid), d.OwnerCount(), wantOwners)
			}
			if replaced {
				if d.Entry(sid) != next.entry || next.entry.Owner == entry.Owner || next.entry.Owner.SessionMgr().PendingCount() != next.pending {
					t.Error("stale restart removed/reused the replacement or minted another reservation")
				}
				freshConn, freshScanner := connectSpawnedOwner(t, next.path, next.token)
				defer freshConn.Close()
				fmt.Fprintln(freshConn, `{"jsonrpc":"2.0","id":3,"method":"maintenance/write","params":{"marker":"replacement"}}`)
				if daemonRespawnGeneration(t, readDaemonResponseID(t, freshScanner, "3")) != 2 {
					t.Error("replacement's actual upstream generation did not survive stale restart")
				}
			}
			generation, err := os.ReadFile(req.Env["MCPMUX_MAINTENANCE_GENERATION"])
			if err != nil || string(generation) != wantGeneration {
				t.Errorf("stale restart started another upstream: generation=%q want=%q err=%v", generation, wantGeneration, err)
			}
			wantEffects := "original\n"
			if replaced {
				wantEffects += "replacement\n"
			}
			data, err := os.ReadFile(effects)
			if err != nil || string(data) != wantEffects {
				t.Errorf("stale restart produced unexpected effects: %q %v", data, err)
			}
		})
	}
}

func TestMaintenanceHoldAttemptsEveryPinnedGeneration(t *testing.T) {
	for _, placeholders := range []bool{false, true} {
		t.Run(fmt.Sprintf("placeholders_%t", placeholders), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			d := maintenanceDaemon(t)
			d.persistent = true
			req, started, _, effects := maintenanceHelperRequest(t)
			req.Mode = "cwd"
			other := req
			other.Mode, other.Cwd = "isolated", t.TempDir()
			same := req
			same.Mode = "isolated"
			requests := []control.Request{req, other, same}
			entries := make([]*OwnerEntry, len(requests))
			pids := make([]int, len(requests))
			probes := make([]func(bool), len(requests))
			var attempts [3]atomic.Int32
			for i, launch := range requests {
				path, sid, token, err := d.Spawn(launch)
				if err != nil {
					t.Fatal(err)
				}
				entries[i] = d.Entry(sid)
				conn, scanner := connectSpawnedOwner(t, path, token)
				defer conn.Close()
				fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"multi-pin-hold","version":"1"}}}`)
				readDaemonResponseID(t, scanner, "1")
				fmt.Fprintf(conn, "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"maintenance/write\",\"params\":{\"marker\":\"pin-%d\"}}\n", i)
				if daemonRespawnGeneration(t, readDaemonResponseID(t, scanner, "2")) != i+1 {
					t.Fatal("independent helper generation was not materialized")
				}
				pids[i], _ = entries[i].Owner.Status()["upstream_pid"].(int)
				if !entries[i].Persistent || !daemonTestProcessAlive(pids[i]) {
					t.Fatal("fixture must have independent live persistent pins")
				}
				fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"work","method":"maintenance/wait","params":{}}`)
				waitForDaemonCondition(t, 3*time.Second, func() bool {
					_, err := os.Stat(started)
					return err == nil && entries[i].Owner.PendingRequests() == 1
				}, "helper did not reserve delivered work")
				probes[i] = func(blocked bool) {
					maintenanceResponseCode(t, readDaemonResponseID(t, scanner, `"work"`), `"work"`, -32005)
					if blocked {
						fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":3,"method":"maintenance/write","params":{"marker":"after-block"}}`)
						maintenanceResponseCode(t, readDaemonResponseID(t, scanner, "3"), "3", -32005)
					}
				}
			}
			contextToken, err := generateToken()
			if err != nil {
				t.Fatal(err)
			}
			if admitted, err := d.admitOwner(entries[0], contextToken, other.Cwd, mergeEnv(other.Env), false); err != nil || !admitted {
				t.Fatalf("selected owner's second finite context was not admitted: %t %v", admitted, err)
			}
			beforeEffects, err := os.ReadFile(effects)
			if err != nil {
				t.Fatal(err)
			}
			original := finalizeOwnerForRemoval
			var allow atomic.Bool
			var blockedOwner atomic.Pointer[owner.Owner]
			finalizeOwnerForRemoval = func(o *owner.Owner, soft bool) (int, bool, error) {
				for i, entry := range entries {
					if o == entry.Owner {
						attempts[i].Add(1)
						if !placeholders && !allow.Load() {
							blockedOwner.CompareAndSwap(nil, o)
							if blockedOwner.Load() == o {
								return 0, false, errors.New("first pin's tree proof remains unavailable")
							}
						}
						break
					}
				}
				return original(o, soft)
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			var creators sync.WaitGroup
			t.Cleanup(func() {
				allow.Store(true)
				releaseOnce.Do(func() { close(release) })
				creators.Wait()
				d.shutdown(nil)
				finalizeOwnerForRemoval = original
			})
			spawned := make(chan error, 2)
			if placeholders {
				entered := make(chan struct{}, 2)
				barrier := func() { entered <- struct{}{}; <-release }
				d.beforeColdOwnerPromotion = func(*owner.Owner) { barrier() }
				d.beforeTemplatePromotion = barrier
				for _, launch := range []control.Request{other, same} {
					creators.Add(1)
					go func() {
						defer creators.Done()
						_, _, _, err := d.Spawn(launch)
						spawned <- err
					}()
				}
				for range 2 {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("placeholder did not reach promotion barrier")
					}
				}
			}

			result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: entries[0].ServerID, DrainTimeoutMs: 100, HoldTTLMS: maintenanceTTL(600000)}, 10*time.Second)
			if !errors.Is(err, control.ErrMaintenanceRetirementBlocked) || result == nil || result.State != control.MaintenanceRetirementBlocked || result.TreesRetired {
				t.Fatalf("aggregate hold did not remain blocked: %+v %v", result, err)
			}
			if time.Now().Before(result.DrainDeadline) {
				t.Error("unfinished generation work lost the accepted drain deadline")
			}
			for i, entry := range entries {
				if attempts[i].Load() == 0 {
					t.Errorf("live persistent generation %d was never attempted after the first blocker", i)
				}
				if entry.Owner != blockedOwner.Load() && (!entry.Owner.MaintenanceRetired() || daemonTestProcessAlive(pids[i]) || d.Entry(entry.ServerID) != nil) {
					t.Errorf("independent generation %d survived without proven retirement", i)
				}
				if entry.Owner == blockedOwner.Load() {
					d.mu.RLock()
					retrying := entry.removalRetrying
					d.mu.RUnlock()
					if !retrying || entry.Owner.MaintenanceRetired() || d.Entry(entry.ServerID) != entry || !daemonTestProcessAlive(pids[i]) {
						t.Error("blocked generation lost its exact entry, live authority, or existing retry owner")
					}
				}
				if attempts[i].Load() > 0 {
					probes[i](entry.Owner == blockedOwner.Load())
				}
			}
			d.maintenanceGate.RLock()
			lease := d.maintenanceLeases[result.HoldID]
			wantPins := 3
			if placeholders {
				wantPins += 2
			}
			if lease == nil || len(lease.pins) != wantPins || len(lease.record.Keys) != 2 || !lease.result.DrainDeadline.Equal(result.DrainDeadline) {
				d.maintenanceGate.RUnlock()
				t.Fatal("aggregate hold lost a captured generation, finite context, or original deadline")
			}
			d.maintenanceGate.RUnlock()
			beforeLedger, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			beforeTransaction, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			var ledger maintenanceLedger
			if err := json.Unmarshal(beforeLedger, &ledger); err != nil || len(ledger.Leases) != 1 || ledger.Leases[0].State != control.MaintenanceRetirementBlocked || !ledger.Leases[0].DrainDeadline.Equal(result.DrainDeadline) {
				t.Fatalf("blocked retirement was not durable: %+v %v", ledger, err)
			}
			if _, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "resume", HoldID: result.HoldID}, time.Second); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
				t.Errorf("resume released an unproven captured generation: %v", err)
			}
			for _, launch := range requests {
				if _, _, _, err := d.Spawn(launch); !errors.Is(err, control.ErrMaintenanceHeld) {
					t.Errorf("blocked hold allowed a finite-context start: %v", err)
				}
			}
			if err := CheckMaintenanceForActivation(d.namespace, d.ctlSrv.SocketPath()); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
				t.Errorf("durable blocked authority permitted activation: %v", err)
			}
			lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
			if err != nil {
				t.Fatal(err)
			}
			d.maintenanceGate.Lock()
			err = d.expireMaintenanceLocked(result.ExpiresAt.Add(time.Second))
			d.maintenanceGate.Unlock()
			_ = lock.Close()
			if err != nil || len(d.maintenanceResults()) != 1 {
				t.Error("TTL released an incompletely retired multi-context hold")
			}
			afterLedger, ledgerErr := os.ReadFile(d.maintenancePath)
			afterTransaction, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if ledgerErr != nil || transactionErr != nil || !bytes.Equal(beforeLedger, afterLedger) || !bytes.Equal(beforeTransaction, afterTransaction) {
				t.Error("refused resume/start/TTL altered durable blocked authority")
			}
			afterEffects, err := os.ReadFile(effects)
			generation, generationErr := os.ReadFile(req.Env["MCPMUX_MAINTENANCE_GENERATION"])
			if err != nil || generationErr != nil || !bytes.Equal(beforeEffects, afterEffects) || string(generation) != "3" {
				t.Errorf("blocked demand produced effects or another process: effects=%q generation=%q err=%v %v", afterEffects, generation, err, generationErr)
			}

			allow.Store(true)
			if placeholders {
				releaseOnce.Do(func() { close(release) })
				for range 2 {
					select {
					case err := <-spawned:
						if !errors.Is(err, control.ErrMaintenanceHeld) {
							t.Fatalf("late placeholder promotion crossed the fence: %v", err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("fenced placeholder did not settle")
					}
				}
				d.reconcileMaintenance()
			}
			waitMaintenanceState(t, d, control.MaintenanceHeld)
			for i, entry := range entries {
				if !entry.Owner.MaintenanceRetired() || daemonTestProcessAlive(pids[i]) || d.Entry(entry.ServerID) != nil {
					t.Errorf("exact retirement retry did not prove generation %d dead", i)
				}
			}
			if _, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "resume", HoldID: result.HoldID}, time.Second); err != nil {
				t.Fatalf("proven multi-generation hold could not resume: %v", err)
			}
		})
	}
}
