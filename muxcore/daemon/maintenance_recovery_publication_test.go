package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/registry"
)

func maintenancePublicationStatus(t *testing.T, endpoint string) []control.MaintenanceResult {
	t.Helper()
	response, err := control.SendWithTimeout(endpoint, control.Request{Cmd: "status"}, 2*time.Second)
	if err != nil || response == nil || !response.OK {
		t.Fatalf("live maintenance status: %+v %v", response, err)
	}
	var status struct {
		Maintenance []control.MaintenanceResult `json:"maintenance"`
	}
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatal(err)
	}
	return status.Maintenance
}

func TestMaintenanceFailedConstructorCannotExpireRenewedAuthority(t *testing.T) {
	for _, failure := range []string{"control_bind", "registry_publication"} {
		t.Run(failure, func(t *testing.T) {
			d, _, held := maintenanceSecurityHeld(t)
			endpoint := d.ctlSrv.SocketPath()
			cfg := Config{ControlPath: endpoint, Namespace: d.namespace, SkipSnapshot: true, Logger: testLogger(t)}
			if failure == "registry_publication" {
				cfg.Name = d.namespace + "-abandoned"
				cfg.Registry = &registry.Config{}
				descriptor := cfg.Registry.BuildDescriptor(cfg.Name, filepath.Dir(endpoint), endpoint, os.Getpid(), time.Now())
				path, err := registry.DescriptorPath(filepath.Dir(endpoint), descriptor)
				if err != nil {
					t.Fatal(err)
				}
				// A nonempty directory at the real descriptor path makes both
				// publication and its replace fallback fail after control bind.
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(path) })
				if err := os.WriteFile(filepath.Join(path, "occupied"), []byte("registry publication blocker"), 0o600); err != nil {
					t.Fatal(err)
				}
				d.ctlSrv.Close()
			}

			original, err := d.HandleMaintenance(control.Request{Cmd: "renew", HoldID: held.HoldID, HoldTTLMS: maintenanceTTL(5000)})
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			beforeTransaction, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			abandoned, err := New(cfg)
			if abandoned != nil {
				t.Cleanup(func() { abandoned.shutdown(nil) })
			}
			wantFailure := "control server:"
			if failure == "registry_publication" {
				wantFailure = "registry descriptor:"
			}
			if err == nil || abandoned != nil || !strings.Contains(err.Error(), wantFailure) {
				t.Fatalf("constructor did not fail at %s: %+v %v", failure, abandoned, err)
			}
			after, ledgerErr := os.ReadFile(d.maintenancePath)
			afterTransaction, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if ledgerErr != nil || transactionErr != nil || !bytes.Equal(before, after) || !bytes.Equal(beforeTransaction, afterTransaction) {
				t.Fatalf("failed recovery constructor mutated authority: ledger=%v transaction=%v", ledgerErr, transactionErr)
			}
			if failure == "registry_publication" {
				if ipc.IsAvailable(endpoint) {
					t.Fatal("failed registry publication retained control admission")
				}
				server, err := control.NewServer(endpoint, d, testLogger(t))
				if err != nil {
					t.Fatalf("restore authoritative control listener: %v", err)
				}
				d.ctlSrv = server
			}

			response, err := control.SendWithTimeout(endpoint, control.Request{Cmd: "renew", HoldID: held.HoldID, HoldTTLMS: maintenanceTTL(10000)}, 2*time.Second)
			if err != nil || response == nil || !response.OK || response.Maintenance == nil {
				t.Fatalf("authoritative renewal: %+v %v", response, err)
			}
			renewed := *response.Maintenance
			if renewed.HoldID != original.HoldID || renewed.State != control.MaintenanceHeld || !renewed.TreesRetired || !renewed.ExpiresAt.After(original.ExpiresAt) || !renewed.DrainDeadline.Equal(original.DrainDeadline) {
				t.Fatalf("renewal did not retain exact retired lease: original=%+v renewed=%+v", original, renewed)
			}
			renewedLedger, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			renewedTransaction, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}

			// No reaper or manual reconciliation participates: the abandoned
			// constructor's real old-expiry callback is the pre-fix writer.
			timer := time.NewTimer(time.Until(original.ExpiresAt.Add(time.Second)))
			defer timer.Stop()
			<-timer.C
			after, ledgerErr = os.ReadFile(d.maintenancePath)
			afterTransaction, transactionErr = os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if ledgerErr != nil || transactionErr != nil || !bytes.Equal(renewedLedger, after) || !bytes.Equal(renewedTransaction, afterTransaction) {
				t.Fatalf("abandoned constructor overwrote renewed authority after old expiry: ledger=%v transaction=%v\nrenewed=%s\nafter=%s", ledgerErr, transactionErr, renewedLedger, after)
			}
			_, _, ledger, err := readMaintenanceAuthority(d.namespace, endpoint)
			if err != nil || ledger == nil || len(ledger.Leases) != 1 {
				t.Fatalf("renewed durable authority lost: %+v %v", ledger, err)
			}
			record := ledger.Leases[0]
			if record.HoldID != renewed.HoldID || record.State != control.MaintenanceHeld || !record.ExpiresAt.Equal(renewed.ExpiresAt) || !record.DrainDeadline.Equal(renewed.DrainDeadline) {
				t.Fatalf("renewed durable clocks changed: %+v, want %+v", record, renewed)
			}
			status := maintenancePublicationStatus(t, endpoint)
			if len(status) != 1 || status[0].HoldID != renewed.HoldID || status[0].State != control.MaintenanceHeld || !status[0].TreesRetired || !status[0].ExpiresAt.Equal(renewed.ExpiresAt) || !status[0].DrainDeadline.Equal(renewed.DrainDeadline) {
				t.Fatalf("renewed live authority changed: %+v, want %+v", status, renewed)
			}
			if err := CheckMaintenanceForActivation(d.namespace, endpoint); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("old constructor expiry reopened activation: %v", err)
			}
		})
	}
}

func TestMaintenancePublishedRecoveryPreservesExpiryAndBlockedFence(t *testing.T) {
	for _, state := range []control.MaintenanceState{control.MaintenanceHeld, control.MaintenanceHolding, control.MaintenanceRetirementBlocked} {
		t.Run(string(state), func(t *testing.T) {
			d, req, held := maintenanceSecurityHeld(t)
			endpoint := d.ctlSrv.SocketPath()
			d.shutdown(nil)
			lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			d.maintenanceGate.Lock()
			fixture := *d.maintenanceLeases[held.HoldID]
			fixture.result.State = state
			fixture.result.ExpiresAt = time.Now().UTC().Add(time.Second)
			fixture.result.TreesRetired = state == control.MaintenanceHeld
			next := copyMaintenanceLeases(d.maintenanceLeases)
			next[held.HoldID] = &fixture
			// Shorten only the durable recovery fixture, using the production
			// writer under its namespace lock. The stopped predecessor's live
			// lease and timer retain the original ten-minute expiry, so only
			// the reconstructed constructor can produce this test's release.
			err = d.persistMaintenanceLocked(next)
			d.maintenanceGate.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			beforeTransaction, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := New(Config{ControlPath: endpoint, Namespace: d.namespace, Name: d.namespace, SkipSnapshot: true, Registry: &registry.Config{}, Logger: testLogger(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { recovered.shutdown(nil) })
			if _, err := registry.ReadDescriptor(recovered.registryDescriptorPath); err != nil {
				t.Fatalf("successful constructor did not publish registry: %v", err)
			}
			expectedState := state
			if state != control.MaintenanceHeld {
				expectedState = control.MaintenanceRetirementBlocked
			}
			status := maintenancePublicationStatus(t, endpoint)
			if len(status) != 1 || status[0].HoldID != held.HoldID || status[0].State != expectedState || !status[0].ExpiresAt.Equal(fixture.result.ExpiresAt) || !status[0].DrainDeadline.Equal(held.DrainDeadline) || status[0].TreesRetired != (state == control.MaintenanceHeld) {
				t.Fatalf("recovery changed original authority or clocks: %+v, want %+v", status, fixture.result)
			}
			timer := time.NewTimer(time.Until(fixture.result.ExpiresAt.Add(150 * time.Millisecond)))
			defer timer.Stop()
			<-timer.C
			after, ledgerErr := os.ReadFile(d.maintenancePath)
			afterTransaction, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if ledgerErr != nil || transactionErr != nil || !bytes.Equal(before, after) || !bytes.Equal(beforeTransaction, afterTransaction) {
				t.Fatalf("recovery expiry mutated authority while starter owns namespace lock: ledger=%v transaction=%v", ledgerErr, transactionErr)
			}
			if _, _, _, err := recovered.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("startup lock/expired recovery fence admitted demand: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			if state == control.MaintenanceHeld {
				waitForDaemonCondition(t, 3*time.Second, func() bool { return len(recovered.maintenanceResults()) == 0 }, "published recovery did not expire through its existing timer lifecycle")
				_, _, ledger, err := readMaintenanceAuthority(d.namespace, endpoint)
				if err != nil || ledger == nil || len(ledger.Leases) != 0 {
					t.Fatalf("published recovery did not durably release: %+v %v", ledger, err)
				}
				if err := CheckMaintenanceForActivation(d.namespace, endpoint); err != nil {
					t.Fatalf("safely expired reconstruction retained activation fence: %v", err)
				}
				if len(maintenancePublicationStatus(t, endpoint)) != 0 {
					t.Fatal("safely expired reconstruction retained live lease")
				}
				return
			}
			// Allow the existing lock-contention retry to run after expiry;
			// lost tree authority must remain blocked, never TTL-open.
			time.Sleep(300 * time.Millisecond)
			status = maintenancePublicationStatus(t, endpoint)
			if len(status) != 1 || status[0].State != control.MaintenanceRetirementBlocked || status[0].TreesRetired || !status[0].ExpiresAt.Equal(fixture.result.ExpiresAt) || !status[0].DrainDeadline.Equal(held.DrainDeadline) {
				t.Fatalf("expired blocked reconstruction changed authority: %+v", status)
			}
			after, ledgerErr = os.ReadFile(d.maintenancePath)
			afterTransaction, transactionErr = os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if ledgerErr != nil || transactionErr != nil || !bytes.Equal(before, after) || !bytes.Equal(beforeTransaction, afterTransaction) {
				t.Fatalf("expired blocked reconstruction rewrote durable authority: ledger=%v transaction=%v", ledgerErr, transactionErr)
			}
			if _, err := recovered.HandleMaintenance(control.Request{Cmd: "resume", HoldID: held.HoldID}); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
				t.Fatalf("expired blocked reconstruction resumed: %v", err)
			}
			if _, _, _, err := recovered.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("expired blocked reconstruction admitted demand: %v", err)
			}
			if err := CheckMaintenanceForActivation(d.namespace, endpoint); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
				t.Fatalf("expired blocked reconstruction reopened activation: %v", err)
			}
		})
	}
}
