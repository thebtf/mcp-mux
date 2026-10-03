package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func stopAdmissionLiveOwner(t *testing.T, d *Daemon, req control.Request) (*OwnerEntry, net.Conn) {
	t.Helper()
	path, sid, token, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	entry := d.Entry(sid)
	conn, scanner := connectSpawnedOwner(t, path, token)
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"stop-admission","version":"1"}}}`)
	readDaemonResponseID(t, scanner, "1")
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":2,"method":"maintenance/write","params":{"marker":"before-stop"}}`)
	readDaemonResponseID(t, scanner, "2")
	pid, _ := entry.Owner.Status()["upstream_pid"].(int)
	if !daemonTestProcessAlive(pid) || entry.Owner.SessionCount() != 1 {
		t.Fatal("stop fixture has no actual live upstream and session")
	}
	return entry, conn
}

func TestMaintenanceStopOwnerHoldingAdmission(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending_%t/force_%t", pending, force), func(t *testing.T) {
				d := maintenanceDaemon(t)
				d.persistent = true
				req, started, releaseWork, _ := maintenanceHelperRequest(t)
				entry, conn := stopAdmissionLiveOwner(t, d, req)
				otherEntry, _ := stopAdmissionLiveOwner(t, d, req)
				pid, _ := entry.Owner.Status()["upstream_pid"].(int)
				if pending {
					fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"delivered-work","method":"maintenance/wait","params":{}}`)
					waitForDaemonCondition(t, 3*time.Second, func() bool {
						_, err := os.Stat(started)
						return err == nil && entry.Owner.PendingRequests() == 1
					}, "actual upstream did not reserve pending work")
				}

				// An actual creator callback keeps a captured placeholder unfinished.
				// HOLDING is durable before the hold joins that creator, hence no
				// live pin has begun retirement. Owner fences are already published.
				entered, releaseCreator := make(chan struct{}, 1), make(chan struct{})
				var releaseOnce sync.Once
				barrier := func() { entered <- struct{}{}; <-releaseCreator }
				d.beforeColdOwnerPromotion = func(*owner.Owner) { barrier() }
				d.beforeTemplatePromotion = barrier
				t.Cleanup(func() {
					_ = os.WriteFile(releaseWork, []byte("release"), 0o600)
					releaseOnce.Do(func() { close(releaseCreator) })
				})
				created := make(chan error, 1)
				go func() { _, _, _, err := d.Spawn(req); created <- err }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("real placeholder did not reach the promotion barrier")
				}
				d.beforeColdOwnerPromotion, d.beforeTemplatePromotion = nil, nil
				d.mu.RLock()
				var placeholder *OwnerEntry
				for _, candidate := range d.owners {
					if candidate.creating != nil {
						placeholder = candidate
					}
				}
				d.mu.RUnlock()
				if placeholder == nil || placeholder.Owner != nil {
					t.Fatal("creator barrier did not retain an inert exact registry entry")
				}
				type holdOutcome struct {
					result *control.MaintenanceResult
					err    error
				}
				held := make(chan holdOutcome, 1)
				holdSettled := false
				t.Cleanup(func() {
					_ = os.WriteFile(releaseWork, []byte("release"), 0o600)
					releaseOnce.Do(func() { close(releaseCreator) })
					if !holdSettled {
						select {
						case <-held:
						case <-time.After(10 * time.Second):
							t.Error("stop fixture hold did not settle after creator release")
						}
					}
					for _, result := range d.maintenanceResults() {
						if result.State == control.MaintenanceHeld {
							_, _ = d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID})
						}
					}
				})
				go func() {
					result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: entry.ServerID, DrainTimeoutMs: 30000, HoldTTLMS: maintenanceTTL(600000)}, 40*time.Second)
					held <- holdOutcome{result, err}
				}()
				waitMaintenanceState(t, d, control.MaintenanceHolding)
				before := d.maintenanceResults()[0]
				ledgerBefore, err := os.ReadFile(d.maintenancePath)
				if err != nil {
					t.Fatal(err)
				}
				transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
				if err != nil {
					t.Fatal(err)
				}
				_, _, ledger, err := readMaintenanceAuthority(d.namespace, d.ctlSrv.SocketPath())
				if err != nil || ledger == nil || len(ledger.Leases) != 1 || ledger.Leases[0].State != control.MaintenanceHolding || !ledger.Leases[0].DrainDeadline.Equal(before.DrainDeadline) {
					t.Fatalf("HOLDING barrier lacks committed durable authority: %+v %v", ledger, err)
				}
				drain := 30000
				if force {
					drain = 0
				}
				// Selected identity, finite-context sibling and unpublished placeholder
				// all arbitrate at the daemon registry rather than their owner fences.
				for _, target := range []*OwnerEntry{entry, otherEntry, placeholder} {
					response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "stop_owner", ServerID: target.ServerID, DrainTimeoutMs: drain}, 2*time.Second)
					d.mu.RLock()
					claimed := target.removalInProgress || target.removalRetrying || target.terminationHint == HintOperatorStop
					d.mu.RUnlock()
					if claimed {
						t.Fatal("stop claimed operator retirement after durable HOLDING")
					}
					if err != nil || response.OK || !errors.Is(response.Err(), control.ErrMaintenanceHeld) || response.Maintenance == nil || *response.Maintenance != before {
						t.Fatalf("stop %s lost typed hold admission or original clocks: %+v %v", target.ServerID, response, err)
					}
					if d.Entry(target.ServerID) != target || (target.Owner != nil && !target.Owner.IsAccepting()) {
						t.Fatal("refused stop tore down a captured entry or listener")
					}
				}
				if !daemonTestProcessAlive(pid) || entry.Owner.SessionCount() != 1 || (pending && entry.Owner.PendingRequests() != 1) {
					t.Fatal("refused stop disposed upstream work before accepted maintenance drain")
				}
				ledgerAfter, ledgerErr := os.ReadFile(d.maintenancePath)
				transactionAfter, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
				if ledgerErr != nil || transactionErr != nil || !bytes.Equal(ledgerBefore, ledgerAfter) || !bytes.Equal(transactionBefore, transactionAfter) {
					t.Fatal("refused stop altered durable hold authority")
				}

				unrelated, _, _, _ := maintenanceHelperRequest(t)
				unrelatedEntry, _ := stopAdmissionLiveOwner(t, d, unrelated)
				response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "stop_owner", ServerID: unrelatedEntry.ServerID, DrainTimeoutMs: drain}, 5*time.Second)
				if err != nil || !response.OK || d.Entry(unrelatedEntry.ServerID) != nil {
					t.Fatalf("unrelated owner could not stop: %+v %v", response, err)
				}
				wantReason := ownerRemovalReasonOperatorSoft
				if force {
					wantReason = ownerRemovalReasonOperatorHard
				}
				if d.ownerRemoval.ByReason[wantReason] != 1 || unrelatedEntry.terminationHint != HintOperatorStop {
					t.Fatalf("unrelated stop lost soft/hard semantics: %+v", d.ownerRemoval)
				}

				_ = os.WriteFile(releaseWork, []byte("release"), 0o600)
				releaseOnce.Do(func() { close(releaseCreator) })
				select {
				case err := <-created:
					if !errors.Is(err, control.ErrMaintenanceHeld) {
						t.Fatalf("late promotion crossed hold: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("pinned creator did not settle")
				}
				select {
				case result := <-held:
					holdSettled = true
					if result.err != nil || result.result == nil || result.result.State != control.MaintenanceHeld || !result.result.DrainDeadline.Equal(before.DrainDeadline) || !result.result.ExpiresAt.Equal(before.ExpiresAt) {
						t.Fatalf("maintenance-owned retirement lost clocks or deadlocked: %+v %v", result.result, result.err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("maintenance-owned retirement deadlocked after refused stop")
				}
			})
		}
	}
}

func TestMaintenanceStopOwnerPersistenceFailureAdmission(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force_%t", force), func(t *testing.T) {
			d := maintenanceDaemon(t)
			d.persistent = true
			req, _, _, _ := maintenanceHelperRequest(t)
			entry, _ := stopAdmissionLiveOwner(t, d, req)
			pid, _ := entry.Owner.Status()["upstream_pid"].(int)
			d.maintenanceGate.Lock()
			d.maintenanceCommit = func([]byte) error { return errors.New("authority writer unavailable") }
			d.maintenanceGate.Unlock()
			t.Cleanup(func() {
				d.maintenanceGate.Lock()
				d.maintenanceCommit, d.maintenanceFailed = nil, false
				d.maintenanceGate.Unlock()
			})
			if _, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: entry.ServerID}); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
				t.Fatalf("real failed hold did not latch persistence failure: %v", err)
			}
			drain := 30000
			if force {
				drain = 0
			}
			response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "stop_owner", ServerID: entry.ServerID, DrainTimeoutMs: drain}, 2*time.Second)
			if err != nil || response.OK || !errors.Is(response.Err(), control.ErrMaintenancePersistenceFailed) {
				t.Fatalf("failed-persistence stop did not fail closed: %+v %v", response, err)
			}
			if d.Entry(entry.ServerID) != entry || !entry.Owner.IsAccepting() || !daemonTestProcessAlive(pid) {
				t.Fatal("failed-persistence stop tore down actual owner authority")
			}
		})
	}
}
