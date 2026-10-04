package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestMaintenanceRetirementPublicationRetriesNamespaceContentionWithoutReaper(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		renewBlocked bool
	}{{"original", false}, {"renewed_blocked", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			d := maintenanceDaemon(t) // New starts the real daemon, not a reaper.
			b := newMaintenanceNativeBarrier(t)
			d.sessionHandler = &maintenanceNotificationWork{barrier: b}
			initial, entry, conn, frames := maintenanceNativeIPC(t, d, era.EraLegacy)
			identity := captureOwnerEntryIdentity(entry)
			maintenanceNativeSend(t, conn, era.EraLegacy, "1", "initialize")
			maintenanceNativeRead(t, frames, "1")
			maintenanceNativeSend(t, conn, era.EraLegacy, "", "maintenance/native-notification")
			maintenanceNativeEntered(t, b)

			published := make(chan maintenanceRecord, 3)
			d.maintenanceGate.Lock()
			d.maintenanceCommit = func(data []byte) error {
				var ledger maintenanceLedger
				if err := json.Unmarshal(data, &ledger); err != nil {
					return err
				}
				if err := writeMaintenanceLedger(d.maintenancePath, data); err != nil {
					return err
				}
				if len(ledger.Leases) == 1 && ledger.Leases[0].State == control.MaintenanceHeld {
					published <- ledger.Leases[0]
				}
				return nil
			}
			d.maintenanceGate.Unlock()
			result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: initial.ServerID, HoldTTLMS: maintenanceTTL(60000)}, 5*time.Second)
			if !errors.Is(err, control.ErrMaintenanceRetirementBlocked) || result == nil || result.State != control.MaintenanceRetirementBlocked || result.TreesRetired {
				t.Fatalf("live native producer did not retain blocked authority: %+v %v", result, err)
			}
			maintenanceNativeRetained(t, d, entry)
			ledgerBefore, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lock.Close() })
			b.open()
			select {
			case <-entry.Owner.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("actual native return did not settle owner Done")
			}
			waitForDaemonCondition(t, 5*time.Second, func() bool {
				return d.Entry(entry.ServerID) == nil && entry.Owner.MaintenanceRetired()
			}, "existing exact-entry finalization retry did not retire the real owner")
			if !identity.matches(entry) || b.entered.Load() != 1 || b.returned.Load() != 1 {
				t.Fatal("native retirement changed generation or replayed work")
			}
			d.maintenanceGate.Lock()
			entry.OwnerGeneration = "superseded-generation"
			d.maintenanceGate.Unlock()
			d.maintenanceRetirementChanged(entry)
			d.maintenanceGate.Lock()
			entry.OwnerGeneration = identity.ownerGeneration
			d.maintenanceGate.Unlock()

			// Observe the retirement callback's contention verdict directly, rather
			// than relying on a sleep after the earlier registry-removal event.
			callbackDone := make(chan struct{})
			go func() {
				d.maintenanceRetirementChanged(entry)
				close(callbackDone)
			}()
			select {
			case <-callbackDone:
			case <-time.After(5 * time.Second):
				t.Fatal("retirement callback waited while the namespace lock was held")
			}
			current := d.maintenanceResults()
			if len(current) != 1 || current[0] != *result {
				t.Fatalf("namespace contention changed the original blocked lease: %+v", current)
			}
			_, _, ledger, err := readMaintenanceAuthority(d.namespace, d.ctlSrv.SocketPath())
			if err != nil || ledger == nil || len(ledger.Leases) != 1 || ledger.Leases[0].State != control.MaintenanceRetirementBlocked || !ledger.Leases[0].ExpiresAt.Equal(result.ExpiresAt) || !ledger.Leases[0].DrainDeadline.Equal(result.DrainDeadline) {
				t.Fatalf("contended retirement lost durable original-clock authority: %+v %v", ledger, err)
			}
			assertBytes := func(wantLedger, wantTransaction []byte) {
				t.Helper()
				gotLedger, ledgerErr := os.ReadFile(d.maintenancePath)
				gotTransaction, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
				if ledgerErr != nil || transactionErr != nil || !bytes.Equal(wantLedger, gotLedger) || !bytes.Equal(wantTransaction, gotTransaction) {
					t.Fatalf("callback changed durable bytes: ledger=%v transaction=%v", ledgerErr, transactionErr)
				}
			}
			assertBytes(ledgerBefore, transactionBefore)
			select {
			case proof := <-published:
				t.Fatalf("contended callback published HELD: %+v", proof)
			default:
			}
			if scenario.renewBlocked {
				beforeRenew := *result
				entered := make(chan maintenanceRecord, 1)
				release := make(chan struct{})
				var releaseOnce sync.Once
				t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
				type renewalOutcome struct {
					result *control.MaintenanceResult
					err    error
				}
				renewed := make(chan renewalOutcome, 1)
				func() {
					d.maintenanceGate.Lock()
					defer d.maintenanceGate.Unlock()
					originalCommit := d.maintenanceCommit
					d.maintenanceCommit = func(data []byte) error {
						var ledger maintenanceLedger
						if err := json.Unmarshal(data, &ledger); err != nil {
							return err
						}
						if err := originalCommit(data); err != nil {
							return err
						}
						if len(ledger.Leases) == 1 && ledger.Leases[0].HoldID == beforeRenew.HoldID &&
							ledger.Leases[0].State == control.MaintenanceRetirementBlocked &&
							!ledger.Leases[0].ExpiresAt.Equal(beforeRenew.ExpiresAt) {
							entered <- ledger.Leases[0]
							<-release
						}
						return nil
					}
					if err := lock.Close(); err != nil {
						t.Fatal(err)
					}
					go func() {
						result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "renew", HoldID: beforeRenew.HoldID, HoldTTLMS: maintenanceTTL(120000)}, 5*time.Second)
						renewed <- renewalOutcome{result, err}
					}()
					// The timer cannot pass its gate check while renewal acquires the
					// namespace. Observe that real lock acquisition before releasing the
					// gate; the writer barrier then keeps publication excluded.
					waitForDaemonCondition(t, 5*time.Second, func() bool {
						probe, err := ipc.AcquireFileLock(d.maintenanceLockPath)
						if errors.Is(err, ipc.ErrFileLocked) {
							return true
						}
						if err != nil {
							t.Fatal(err)
						}
						_ = probe.Close()
						return false
					}, "real blocked renewal did not acquire the namespace before publication")
				}()
				var clocked maintenanceRecord
				select {
				case clocked = <-entered:
				case outcome := <-renewed:
					t.Fatalf("renewal missed the controlled blocked writer: %+v %v", outcome.result, outcome.err)
				case <-time.After(5 * time.Second):
					t.Fatal("renewal did not reach its actual namespace-owned writer barrier")
				}
				if !clocked.DrainDeadline.Equal(beforeRenew.DrainDeadline) || clocked.ExpiresAt.Equal(beforeRenew.ExpiresAt) {
					t.Fatalf("blocked renewal changed drain or failed to extend expiry: %+v", clocked)
				}
				select {
				case proof := <-published:
					t.Fatalf("retirement published while renewal owned the namespace: %+v", proof)
				default:
				}
				releaseOnce.Do(func() { close(release) })
				select {
				case outcome := <-renewed:
					if outcome.err != nil || outcome.result == nil || outcome.result.State != control.MaintenanceRetirementBlocked || outcome.result.TreesRetired ||
						outcome.result.HoldID != beforeRenew.HoldID || !outcome.result.ExpiresAt.Equal(clocked.ExpiresAt) || !outcome.result.DrainDeadline.Equal(beforeRenew.DrainDeadline) {
						t.Fatalf("real blocked renewal changed accepted authority: %+v %v", outcome.result, outcome.err)
					}
					result = outcome.result
				case <-time.After(5 * time.Second):
					t.Fatal("real blocked renewal did not commit")
				}
				// No new retirement callback or status mutation: the replacement
				// lease must inherit pending work using its own timer authority.
			} else if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case proof := <-published:
				if proof.HoldID != result.HoldID || !proof.ExpiresAt.Equal(result.ExpiresAt) || !proof.DrainDeadline.Equal(result.DrainDeadline) {
					t.Fatalf("automatic publication resampled the original lease: %+v", proof)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("proven retirement was dropped after namespace contention without a reaper")
			}
			// The writer receipt precedes FINALIZE; admission readback waits for the
			// real transaction to settle without initiating reconciliation.
			current = d.maintenanceResults()
			if len(current) != 1 || current[0].State != control.MaintenanceHeld || !current[0].TreesRetired || current[0].HoldID != result.HoldID || !current[0].ExpiresAt.Equal(result.ExpiresAt) || !current[0].DrainDeadline.Equal(result.DrainDeadline) {
				t.Fatalf("automatic retirement did not publish original-clock HELD: %+v", current)
			}
			_, _, ledger, err = readMaintenanceAuthority(d.namespace, d.ctlSrv.SocketPath())
			if err != nil || ledger == nil || len(ledger.Leases) != 1 || ledger.Leases[0].State != control.MaintenanceHeld {
				t.Fatalf("automatic HELD lacks valid aggregate durable proof: %+v %v", ledger, err)
			}
			if _, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "renew", HoldID: result.HoldID, HoldTTLMS: maintenanceTTL(60000)}, time.Second); err != nil {
				t.Fatal(err)
			}
			ledgerRenewed, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			transactionRenewed, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			d.maintenanceRetirementChanged(entry)
			assertBytes(ledgerRenewed, transactionRenewed)
			if _, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "resume", HoldID: result.HoldID}, time.Second); err != nil {
				t.Fatal(err)
			}
			ledgerReleased, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			transactionReleased, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			d.maintenanceRetirementChanged(entry)
			assertBytes(ledgerReleased, transactionReleased)
			if len(d.maintenanceResults()) != 0 {
				t.Fatal("late old-entry callback resurrected a released lease")
			}
			d.sessionHandler = &maintenanceCallbackBarrier{}
			fresh, freshEntry, freshConn, freshFrames := maintenanceNativeIPC(t, d, era.EraLegacy)
			if freshEntry.OwnerGeneration == identity.ownerGeneration || freshEntry.Owner == entry.Owner {
				t.Fatal("replacement reused the retired generation")
			}
			maintenanceNativeSend(t, freshConn, era.EraLegacy, "2", "initialize")
			maintenanceNativeRead(t, freshFrames, "2")
			replacement, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: fresh.ServerID, HoldTTLMS: maintenanceTTL(60000)}, 5*time.Second)
			if err != nil || replacement == nil || replacement.State != control.MaintenanceHeld || !replacement.TreesRetired || replacement.HoldID == result.HoldID || !freshEntry.Owner.MaintenanceRetired() {
				t.Fatalf("replacement did not establish its own real retired lease: %+v %v", replacement, err)
			}
			ledgerReplacement, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			transactionReplacement, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			d.maintenanceRetirementChanged(entry)
			assertBytes(ledgerReplacement, transactionReplacement)
			current = d.maintenanceResults()
			if len(current) != 1 || current[0] != *replacement {
				t.Fatalf("obsolete generation changed the replacement lease: %+v", current)
			}
			if _, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "resume", HoldID: replacement.HoldID}, time.Second); err != nil {
				t.Fatal(err)
			}
			maintenanceNativeNoReply(t, freshFrames)
			maintenanceNativeNoReply(t, frames)
		})
	}
}
