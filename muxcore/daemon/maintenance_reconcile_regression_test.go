package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestMaintenanceReaperNamespaceContentionDuringDrain(t *testing.T) {
	d := maintenanceDaemon(t)
	req, started, release, _ := maintenanceHelperRequest(t)
	path, sid, token, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	entry := d.Entry(sid)
	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"reaper-contention","version":"1"}}}`)
	readDaemonResponseID(t, scanner, "1")
	pid, _ := entry.Owner.Status()["upstream_pid"].(int)
	if !daemonTestProcessAlive(pid) {
		t.Fatal("fixture did not materialize a live upstream")
	}
	identity := captureOwnerEntryIdentity(entry)
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"work","method":"maintenance/wait","params":{}}`)
	waitForDaemonCondition(t, 5*time.Second, func() bool {
		_, err := os.Stat(started)
		return err == nil && entry.Owner.PendingRequests() == 1
	}, "helper did not enter admitted work")
	type outcome struct {
		result *control.MaintenanceResult
		err    error
	}
	held := make(chan outcome, 1)
	go func() {
		result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: sid, DrainTimeoutMs: 10000, HoldTTLMS: maintenanceTTL(30000)}, 20*time.Second)
		held <- outcome{result, err}
	}()
	waitMaintenanceState(t, d, control.MaintenanceHolding)
	before := maintenancePublicationStatus(t, d.ctlSrv.SocketPath())
	if len(before) != 1 || before[0].State != control.MaintenanceHolding || before[0].TreesRetired {
		t.Fatalf("drain did not expose incomplete authority: %+v", before)
	}
	probe, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if probe != nil {
		_ = probe.Close()
	}
	if !errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("active drain did not own the actual namespace lock: %v", err)
	}
	r := &Reaper{daemon: d, logger: d.logger}
	swept := make(chan struct{})
	go func() {
		for range 16 {
			r.sweep()
		}
		close(swept)
	}()
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("actual reaper sweeps waited for the draining namespace owner")
	}
	current := maintenancePublicationStatus(t, d.ctlSrv.SocketPath())
	if len(current) != 1 || current[0] != before[0] {
		t.Fatalf("contended sweeps changed the original incomplete lease or clocks: %+v", current)
	}
	spawn := req
	spawn.Cmd = "spawn"
	response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), spawn, 2*time.Second)
	if err != nil || response == nil || !errors.Is(response.Err(), control.ErrMaintenanceHeld) {
		t.Fatalf("contention changed the admission fence: %+v %v", response, err)
	}
	if err := os.WriteFile(release, []byte("complete admitted work"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := readDaemonResponseID(t, scanner, `"work"`)
	if !bytes.Contains(work, []byte(`"completed":true`)) {
		t.Fatalf("reaper contention interrupted admitted work: %s", work)
	}
	var result outcome
	select {
	case result = <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not settle after actual work returned")
	}
	if result.err != nil || result.result == nil || result.result.State != control.MaintenanceHeld || !result.result.TreesRetired ||
		result.result.HoldID != before[0].HoldID || !result.result.ExpiresAt.Equal(before[0].ExpiresAt) || !result.result.DrainDeadline.Equal(before[0].DrainDeadline) {
		t.Fatalf("contention poisoned retirement or reset authority: %+v %v", result.result, result.err)
	}
	if daemonTestProcessAlive(pid) || !identity.matches(entry) || !entry.Owner.MaintenanceRetired() || d.Entry(sid) != nil {
		t.Fatal("successful hold lost exact-generation whole-tree retirement proof")
	}
	_, _, ledger, err := readMaintenanceAuthority(d.namespace, d.ctlSrv.SocketPath())
	if err != nil || ledger == nil || len(ledger.Leases) != 1 || ledger.Leases[0].State != control.MaintenanceHeld ||
		ledger.Leases[0].HoldID != before[0].HoldID || !ledger.Leases[0].ExpiresAt.Equal(before[0].ExpiresAt) || !ledger.Leases[0].DrainDeadline.Equal(before[0].DrainDeadline) {
		t.Fatalf("contention lost durable original-clock authority: %+v %v", ledger, err)
	}
	resumed, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "resume", HoldID: result.result.HoldID}, 5*time.Second)
	if err != nil || resumed == nil || resumed.State != control.MaintenanceReleased || !resumed.TreesRetired {
		t.Fatalf("namespace contention was latched as a permanent persistence failure: %+v %v", resumed, err)
	}
}

func TestMaintenanceReaperNamespaceFailureStaysFenced(t *testing.T) {
	d, req, held := maintenanceSecurityHeld(t)
	ledgerBefore, err := os.ReadFile(d.maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if err != nil {
		t.Fatal(err)
	}
	lockPath := d.maintenanceLockPath
	saved := lockPath + ".reconcile-original"
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
	// A directory at the real namespace lock path is an OS failure, not a
	// competing namespace owner. No injected lock error or timer inspection.
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	probe, err := ipc.AcquireFileLock(lockPath)
	if probe != nil {
		_ = probe.Close()
	}
	if err == nil || errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("fixture did not produce a non-contention lock failure: %v", err)
	}
	r := &Reaper{daemon: d, logger: d.logger}
	r.sweep()
	restore()
	// The next real sweep sees a usable lock. It must not silently clear the
	// persistence latch or acknowledge a new grant after authority failed.
	r.sweep()
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
	if status.Code != control.ErrMaintenancePersistenceFailed.Code || len(status.Maintenance) != 1 || status.Maintenance[0] != held {
		t.Fatalf("namespace failure was not durably fail-closed in live status: %+v", status)
	}
	for _, command := range []string{req.Command, req.Command + "-unrelated"} {
		spawn := req
		spawn.Cmd, spawn.Command = "spawn", command
		response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), spawn, 2*time.Second)
		if err != nil || response == nil || !errors.Is(response.Err(), control.ErrMaintenancePersistenceFailed) || response.Token != "" || response.IPCPath != "" {
			t.Fatalf("namespace failure admitted %q after the path recovered: %+v %v", command, response, err)
		}
	}
	for _, cmd := range []string{"renew", "resume"} {
		result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: cmd, HoldID: held.HoldID}, 2*time.Second)
		if !errors.Is(err, control.ErrMaintenancePersistenceFailed) || result == nil || *result != held {
			t.Fatalf("namespace failure allowed %s to mutate authority: %+v %v", cmd, result, err)
		}
	}
	ledgerAfter, ledgerErr := os.ReadFile(d.maintenancePath)
	transactionAfter, transactionErr := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if ledgerErr != nil || transactionErr != nil || !bytes.Equal(ledgerBefore, ledgerAfter) || !bytes.Equal(transactionBefore, transactionAfter) {
		t.Fatalf("failed namespace reconciliation changed durable predecessor bytes: ledger=%v transaction=%v", ledgerErr, transactionErr)
	}
}
