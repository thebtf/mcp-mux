package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestMaintenanceRetirementPublicationLockFailureStaysFencedWithoutReaper(t *testing.T) {
	for _, scenario := range []string{"callback", "retirement_timer"} {
		t.Run(scenario, func(t *testing.T) {
			d := maintenanceDaemon(t) // Direct New, without NewReaper or reconciliation.
			b := newMaintenanceNativeBarrier(t)
			d.sessionHandler = &maintenanceNotificationWork{barrier: b}
			initial, entry, conn, frames := maintenanceNativeIPC(t, d, era.EraLegacy)
			identity := captureOwnerEntryIdentity(entry)
			maintenanceNativeSend(t, conn, era.EraLegacy, "1", "initialize")
			maintenanceNativeRead(t, frames, "1")
			maintenanceNativeSend(t, conn, era.EraLegacy, "", "maintenance/native-notification")
			maintenanceNativeEntered(t, b)
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
			var retry *maintenanceLease
			if scenario == "retirement_timer" {
				lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
				b.open()
				waitForDaemonCondition(t, 5*time.Second, func() bool {
					return d.Entry(entry.ServerID) == nil && entry.Owner.MaintenanceRetired()
				}, "actual callback return did not retire the exact owner")
				// Pause only an automatically owned retry that has not fired, so the
				// real namespace path can be replaced without an acquisition race.
				waitForDaemonCondition(t, 5*time.Second, func() bool {
					d.maintenanceGate.Lock()
					defer d.maintenanceGate.Unlock()
					lease := d.maintenanceLeases[result.HoldID]
					if lease == nil || lease.result != *result || !lease.retirementRetry || lease.timer == nil || !lease.timer.Stop() {
						return false
					}
					retry = lease
					return true
				}, "real retirement callback did not retain an exact-lease contention retry")
				if d.maintenanceStatusCode() != "" {
					t.Fatal("namespace contention poisoned persistence authority")
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			}
			lockPath := d.maintenanceLockPath
			saved := lockPath + ".retirement-original"
			if err := os.Rename(lockPath, saved); err != nil {
				t.Fatal(err)
			}
			restored := false
			restore := func() {
				t.Helper()
				if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := os.Rename(saved, lockPath); err != nil {
					t.Fatal(err)
				}
				restored = true
			}
			t.Cleanup(func() {
				if !restored {
					restore()
				}
			})
			// A directory at the private daemon's actual namespace lock path fails
			// os.OpenFile on both platforms; this is not an injected error verdict.
			if err := os.Mkdir(lockPath, 0o700); err != nil {
				t.Fatal(err)
			}
			probe, lockErr := ipc.AcquireFileLock(lockPath)
			if probe != nil {
				_ = probe.Close()
			}
			if lockErr == nil || errors.Is(lockErr, ipc.ErrFileLocked) {
				t.Fatalf("fixture did not produce an actual non-contention lock error: %v", lockErr)
			}
			d.maintenanceRetirementChanged(&OwnerEntry{ServerID: identity.serverID, ProtocolEra: identity.protocolEra, OwnerGeneration: identity.ownerGeneration})
			if d.maintenanceStatusCode() != "" {
				t.Fatal("a noncurrent entry latched another generation's authority")
			}
			if retry != nil {
				func() {
					d.maintenanceGate.Lock()
					defer d.maintenanceGate.Unlock()
					if d.maintenanceLeases[result.HoldID] != retry || retry.result != *result {
						t.Fatal("namespace failure fixture lost the current retry lease or its clocks")
					}
					// Rearm the same production timer/lease after the path swap, without
					// changing expiry/drain or relying on another retirement callback.
					d.scheduleMaintenanceTimerLocked(retry, 100*time.Millisecond, true)
				}()
			} else {
				b.open() // The existing exact-entry finalizer invokes the real callback.
			}
			waitForDaemonCondition(t, 5*time.Second, func() bool {
				return d.maintenanceStatusCode() == control.ErrMaintenancePersistenceFailed.Code
			}, "non-contention retirement publication lock failure did not latch persistence authority without a reaper")
			waitForDaemonCondition(t, 5*time.Second, func() bool {
				return d.Entry(entry.ServerID) == nil && entry.Owner.MaintenanceRetired()
			}, "persistence failure lost already-owned exact-entry finalization")
			if !identity.matches(entry) || b.entered.Load() != 1 || b.returned.Load() != 1 {
				t.Fatal("lock failure changed the retired generation or replayed native work")
			}
			restore()
			d.maintenanceRetirementChanged(entry)
			response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "status"}, 2*time.Second)
			if err != nil || response == nil || !response.OK {
				t.Fatalf("failed-authority status unavailable: %+v %v", response, err)
			}
			var status struct {
				Code        control.MaintenanceErrorCode `json:"maintenance_error_code"`
				Maintenance []control.MaintenanceResult  `json:"maintenance"`
			}
			if err := json.Unmarshal(response.Data, &status); err != nil {
				t.Fatal(err)
			}
			if status.Code != control.ErrMaintenancePersistenceFailed.Code || len(status.Maintenance) != 1 || status.Maintenance[0] != *result {
				t.Fatalf("recovered lock path cleared the latch or changed accepted lease clocks: %+v", status)
			}
			for _, command := range []string{entry.Command, entry.Command + "-unrelated"} {
				spawn := control.Request{Cmd: "spawn", Command: command, Mode: "isolated", Cwd: t.TempDir()}
				response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), spawn, 2*time.Second)
				if err != nil || response == nil || !errors.Is(response.Err(), control.ErrMaintenancePersistenceFailed) || response.Token != "" || response.IPCPath != "" {
					t.Fatalf("lock failure admitted %q after the path recovered: %+v %v", command, response, err)
				}
			}
			for _, cmd := range []string{"renew", "resume"} {
				updated, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: cmd, HoldID: result.HoldID}, 2*time.Second)
				if !errors.Is(err, control.ErrMaintenancePersistenceFailed) || updated == nil || *updated != *result {
					t.Fatalf("lock failure allowed %s to mutate accepted authority: %+v %v", cmd, updated, err)
				}
			}
			if _, err := d.HandleShutdownWithError(0); !errors.Is(err, control.ErrMaintenancePersistenceFailed) || d.shuttingDown.Load() {
				t.Fatalf("lock failure lost the lifecycle fence: %v", err)
			}
			ledgerAfter, ledgerErr := os.ReadFile(d.maintenancePath)
			transactionAfter, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if ledgerErr != nil || transactionErr != nil || !bytes.Equal(ledgerBefore, ledgerAfter) || !bytes.Equal(transactionBefore, transactionAfter) {
				t.Fatalf("lock failure changed durable predecessor bytes: ledger=%v transaction=%v", ledgerErr, transactionErr)
			}
			if _, _, ledger, err := readMaintenanceAuthority(d.namespace, d.ctlSrv.SocketPath()); err != nil || ledger == nil || len(ledger.Leases) != 1 || ledger.Leases[0].State != control.MaintenanceRetirementBlocked {
				t.Fatalf("failed publication lost durable blocked recovery authority: %+v %v", ledger, err)
			}
			maintenanceNativeNoReply(t, frames)
		})
	}
}

func maintenanceFailAutomaticPublication(t *testing.T, d *Daemon, target control.MaintenanceState) <-chan error {
	t.Helper()
	saved := filepath.Join(t.TempDir(), "ledger.json")
	faulted := make(chan error, 1)
	d.maintenanceGate.Lock()
	d.maintenanceCommit = func(data []byte) error {
		var ledger maintenanceLedger
		if err := json.Unmarshal(data, &ledger); err != nil {
			return err
		}
		matches := target == control.MaintenanceReleased && len(ledger.Leases) == 0 ||
			len(ledger.Leases) == 1 && ledger.Leases[0].State == target
		if !matches {
			return writeMaintenanceLedger(d.maintenancePath, data)
		}
		// Fail the real writer at its actual authority leaf, after PREPARE.
		if err := os.Rename(d.maintenancePath, saved); err != nil {
			return err
		}
		if err := os.Mkdir(d.maintenancePath, 0o700); err != nil {
			return errors.Join(err, os.Rename(saved, d.maintenancePath))
		}
		writeErr := writeMaintenanceLedger(d.maintenancePath, data)
		restoreErr := errors.Join(os.Remove(d.maintenancePath), os.Rename(saved, d.maintenancePath))
		faulted <- writeErr
		return errors.Join(writeErr, restoreErr)
	}
	d.maintenanceGate.Unlock()
	t.Cleanup(func() {
		d.maintenanceGate.Lock()
		d.maintenanceCommit = nil
		d.maintenanceGate.Unlock()
	})
	return faulted
}

func maintenanceAssertAutomaticFailureFenced(t *testing.T, d *Daemon, result control.MaintenanceResult, demand control.Request) {
	t.Helper()
	// This read waits for the automatic transaction, without reconciling it.
	current := d.maintenanceResults()
	if d.maintenanceStatusCode() != control.ErrMaintenancePersistenceFailed.Code || len(current) != 1 || current[0] != result {
		t.Fatalf("automatic failure lost its persistence signal or exact lease without a reaper: %+v code=%s", current, d.maintenanceStatusCode())
	}
	d.maintenanceGate.Lock()
	d.maintenanceCommit = nil // The real writer has recovered; the latch must not.
	d.maintenanceGate.Unlock()
	ledgerBefore, err := os.ReadFile(d.maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if err != nil {
		t.Fatal(err)
	}
	response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "status"}, 2*time.Second)
	if err != nil || response == nil || !response.OK {
		t.Fatalf("failed-authority control status: %+v %v", response, err)
	}
	var status struct {
		Code        control.MaintenanceErrorCode `json:"maintenance_error_code"`
		Maintenance []control.MaintenanceResult  `json:"maintenance"`
	}
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatal(err)
	}
	if status.Code != control.ErrMaintenancePersistenceFailed.Code || len(status.Maintenance) != 1 || status.Maintenance[0] != result {
		t.Fatalf("control status lost failed original-clock authority: %+v", status)
	}
	for _, cmd := range []string{"renew", "resume", "hold"} {
		req := control.Request{Cmd: cmd, HoldID: result.HoldID}
		if cmd == "hold" {
			req.HoldID, req.ServerID = "", result.ServerID
		}
		updated, err := control.SendMaintenance(d.ctlSrv.SocketPath(), req, 2*time.Second)
		if !errors.Is(err, control.ErrMaintenancePersistenceFailed) || updated != nil && *updated != result || cmd != "hold" && updated == nil {
			t.Fatalf("recovered writer allowed %s to mutate failed authority: %+v %v", cmd, updated, err)
		}
	}
	for _, candidate := range []control.Request{demand, {Command: demand.Command + "-unrelated", Mode: "isolated", Cwd: t.TempDir()}} {
		candidate.Cmd = "spawn"
		response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), candidate, 2*time.Second)
		if err != nil || response == nil || !errors.Is(response.Err(), control.ErrMaintenancePersistenceFailed) || response.Token != "" || response.IPCPath != "" {
			t.Fatalf("failed authority admitted %q after writer recovery: %+v %v", candidate.Command, response, err)
		}
	}
	if _, err := d.HandleShutdownWithError(0); !errors.Is(err, control.ErrMaintenancePersistenceFailed) || d.shuttingDown.Load() {
		t.Fatalf("automatic failure lost lifecycle authority: %v", err)
	}
	d.reconcileMaintenance()
	current = d.maintenanceResults()
	ledgerAfter, ledgerErr := os.ReadFile(d.maintenancePath)
	transactionAfter, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if len(current) != 1 || current[0] != result || d.OwnerCount() != 0 || ledgerErr != nil || transactionErr != nil || !bytes.Equal(ledgerBefore, ledgerAfter) || !bytes.Equal(transactionBefore, transactionAfter) {
		t.Fatalf("writer recovery deleted or replaced failed predecessor: leases=%+v owners=%d ledger=%v transaction=%v", current, d.OwnerCount(), ledgerErr, transactionErr)
	}
	if err := CheckMaintenanceForActivation(d.namespace, d.ctlSrv.SocketPath()); !errors.Is(err, control.ErrMaintenanceHeld) && !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("failed automatic mutation granted activation: %v", err)
	}
}

func TestMaintenanceRetirementPublicationWriteFailureStaysFencedWithoutReaper(t *testing.T) {
	for _, scenario := range []string{"callback", "retirement_timer"} {
		t.Run(scenario, func(t *testing.T) {
			d := maintenanceDaemon(t) // Direct New, no reaper or reconciliation retry.
			b := newMaintenanceNativeBarrier(t)
			d.sessionHandler = &maintenanceNotificationWork{barrier: b}
			initial, entry, conn, frames := maintenanceNativeIPC(t, d, era.EraLegacy)
			identity := captureOwnerEntryIdentity(entry)
			maintenanceNativeSend(t, conn, era.EraLegacy, "1", "initialize")
			maintenanceNativeRead(t, frames, "1")
			launch := entry.Owner.CurrentLaunchContext()
			demand := control.Request{Command: entry.Command, Args: entry.Args, Cwd: launch.Cwd, Env: launch.Env, Mode: entry.Mode}
			maintenanceNativeSend(t, conn, era.EraLegacy, "", "maintenance/native-notification")
			maintenanceNativeEntered(t, b)
			result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: initial.ServerID, HoldTTLMS: maintenanceTTL(60000)}, 5*time.Second)
			if !errors.Is(err, control.ErrMaintenanceRetirementBlocked) || result == nil || result.State != control.MaintenanceRetirementBlocked || result.TreesRetired {
				t.Fatalf("actual native work did not retain blocked authority: %+v %v", result, err)
			}
			maintenanceNativeRetained(t, d, entry)
			ledgerBefore, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			d.maintenanceGate.RLock()
			lease := d.maintenanceLeases[result.HoldID]
			d.maintenanceGate.RUnlock()
			faulted := maintenanceFailAutomaticPublication(t, d, control.MaintenanceHeld)
			d.maintenanceRetirementChanged(&OwnerEntry{ServerID: identity.serverID, ProtocolEra: identity.protocolEra, OwnerGeneration: identity.ownerGeneration})
			if d.maintenanceStatusCode() != "" {
				t.Fatal("obsolete entry latched current persistence authority")
			}
			if scenario == "retirement_timer" {
				lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
				b.open()
				waitForDaemonCondition(t, 5*time.Second, func() bool {
					d.maintenanceGate.RLock()
					retrying := d.maintenanceLeases[result.HoldID] == lease && lease.retirementRetry
					d.maintenanceGate.RUnlock()
					return d.Entry(entry.ServerID) == nil && entry.Owner.MaintenanceRetired() && retrying
				}, "exact-entry callback did not retain a real contention timer")
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				b.open()
			}
			select {
			case err := <-faulted:
				if err == nil {
					t.Fatal("actual authority-leaf writer did not fail")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("automatic retirement never reached the post-lock durable writer")
			}
			maintenanceAssertAutomaticFailureFenced(t, d, *result, demand)
			d.maintenanceGate.RLock()
			retained := d.maintenanceLeases[result.HoldID] == lease
			d.maintenanceGate.RUnlock()
			if !retained || d.Entry(entry.ServerID) != nil || !entry.Owner.MaintenanceRetired() || !identity.matches(entry) || b.entered.Load() != 1 || b.returned.Load() != 1 {
				t.Fatal("writer failure lost exact retired generation or replayed native work")
			}
			ledgerAfter, err := os.ReadFile(d.maintenancePath)
			if err != nil || !bytes.Equal(ledgerBefore, ledgerAfter) {
				t.Fatalf("failed publication replaced durable predecessor: %v", err)
			}
			transactionData, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			var transaction maintenanceTransaction
			if err := json.Unmarshal(transactionData, &transaction); err != nil {
				t.Fatal(err)
			}
			if transaction.State != "prepared" || len(transaction.Predecessor) != 1 || transaction.Predecessor[0].HoldID != result.HoldID || transaction.Predecessor[0].State != result.State || !transaction.Predecessor[0].ExpiresAt.Equal(result.ExpiresAt) || !transaction.Predecessor[0].DrainDeadline.Equal(result.DrainDeadline) {
				t.Fatalf("failed publication lost prepared original-clock authority: %+v", transaction)
			}
			maintenanceNativeNoReply(t, frames)
		})
	}
}
