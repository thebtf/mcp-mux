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
)

func TestMaintenanceHoldDurableClockWriterDelays(t *testing.T) {
	for _, test := range []struct {
		name       string
		write      int32
		ttl        int64
		drain      int
		windowLost bool
	}{
		{name: "initial_slow_ack", write: 1, ttl: 5000, drain: 1000},
		{name: "clocked_slow_write", write: 2, ttl: 5000, drain: 1000},
		{name: "clocked_write_exhausts_ttl", write: 2, ttl: 1000, drain: 500, windowLost: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := maintenanceDaemon(t)
			req, started, releaseWork, _ := maintenanceHelperRequest(t)
			path, sid, token, err := d.Spawn(req)
			if err != nil {
				t.Fatal(err)
			}
			entry := d.Entry(sid)
			conn, scanner := connectSpawnedOwner(t, path, token)
			defer conn.Close()
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"durable-clock","version":"1"}}}`)
			readDaemonResponseID(t, scanner, "1")
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"work","method":"maintenance/wait","params":{}}`)
			waitForDaemonCondition(t, 3*time.Second, func() bool {
				_, err := os.Stat(started)
				return err == nil && entry.Owner.PendingRequests() == 1
			}, "clock fixture did not deliver its admitted work")

			var holdingWrites atomic.Int32
			clocked := make(chan maintenanceRecord, 1)
			entered := make(chan struct{}, 1)
			acknowledged := make(chan time.Time, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			d.maintenanceCommit = func(data []byte) error {
				var ledger maintenanceLedger
				if err := json.Unmarshal(data, &ledger); err != nil {
					return err
				}
				var ordinal int32
				if len(ledger.Leases) == 1 && ledger.Leases[0].State == control.MaintenanceHolding {
					ordinal = holdingWrites.Add(1)
					if ordinal == 2 {
						clocked <- ledger.Leases[0]
					}
				}
				if err := writeMaintenanceLedger(d.maintenancePath, data); err != nil {
					return err
				}
				if ordinal == test.write {
					entered <- struct{}{}
					<-release
					acknowledged <- time.Now().UTC()
				}
				return nil
			}
			type outcome struct {
				result *control.MaintenanceResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: sid, DrainTimeoutMs: test.drain, HoldTTLMS: maintenanceTTL(test.ttl)}, 10*time.Second)
				done <- outcome{result, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("hold did not reach the controlled durable writer")
			}
			delay := time.Duration(test.drain)*time.Millisecond + 200*time.Millisecond
			if test.windowLost {
				delay = time.Duration(test.ttl)*time.Millisecond + 200*time.Millisecond
			}
			<-time.After(delay)
			releaseOnce.Do(func() { close(release) })
			ack := <-acknowledged
			if test.write == 1 {
				waitMaintenanceState(t, d, control.MaintenanceHolding)
				if err := os.WriteFile(releaseWork, []byte("complete admitted work"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			work := readDaemonResponseID(t, scanner, `"work"`)
			if test.write == 1 {
				if !bytes.Contains(work, []byte(`"completed":true`)) {
					t.Fatalf("first writer latency consumed admitted drain grace: %s", work)
				}
			} else {
				maintenanceResponseCode(t, work, `"work"`, -32005)
			}
			var held outcome
			select {
			case held = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("hold did not finish after the writer acknowledgment")
			}
			if held.result == nil {
				t.Fatalf("hold lost its safe result: %v", held.err)
			}
			if test.windowLost {
				if held.err == nil || held.result.State != control.MaintenanceReleased || !held.result.TreesRetired || len(d.maintenanceResults()) != 0 {
					t.Fatalf("clocked writer delay manufactured a usable window: %+v %v", held.result, held.err)
				}
			} else if held.err != nil || held.result.State != control.MaintenanceHeld || !held.result.TreesRetired {
				t.Fatalf("hold failed inside its accepted window: %+v %v", held.result, held.err)
			}
			if holdingWrites.Load() != 2 {
				t.Fatalf("acquisition wrote HOLDING %d times, want seed plus one clocked commit", holdingWrites.Load())
			}
			accepted := <-clocked
			ttl := time.Duration(test.ttl) * time.Millisecond
			drain := time.Duration(test.drain) * time.Millisecond
			origin := accepted.ExpiresAt.Add(-ttl)
			if !accepted.DrainDeadline.Equal(origin.Add(drain)) || !held.result.ExpiresAt.Equal(accepted.ExpiresAt) || !held.result.DrainDeadline.Equal(accepted.DrainDeadline) {
				t.Fatalf("acquisition or retirement restarted the accepted clock: accepted=%+v result=%+v", accepted, held.result)
			}
			if test.write == 1 && origin.Before(ack) {
				t.Fatalf("accepted window started before initial durable writer acknowledgment: origin=%s ack=%s", origin, ack)
			}
			if test.write == 2 && (!origin.Before(ack.Add(-delay)) || ack.Before(accepted.DrainDeadline)) {
				t.Fatalf("second writer latency did not consume the single clock: origin=%s ack=%s deadline=%s", origin, ack, accepted.DrainDeadline)
			}
			if !entry.Owner.MaintenanceRetired() || d.Entry(sid) != nil {
				t.Fatal("hold result did not preserve exact tree retirement")
			}
		})
	}
}

func TestMaintenanceHoldClockPublicationFailuresRemainFenced(t *testing.T) {
	for _, write := range []int{1, 2} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("write_%d_after_%t", write, after), func(t *testing.T) {
				d := maintenanceDaemon(t)
				req, _, _, _ := maintenanceHelperRequest(t)
				path, sid, token, err := d.Spawn(req)
				if err != nil {
					t.Fatal(err)
				}
				entry := d.Entry(sid)
				conn, scanner := connectSpawnedOwner(t, path, token)
				defer conn.Close()
				fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"failed-clock","version":"1"}}}`)
				readDaemonResponseID(t, scanner, "1")
				pid, _ := entry.Owner.Status()["upstream_pid"].(int)
				if !daemonTestProcessAlive(pid) {
					t.Fatal("fault fixture has no live tree authority")
				}
				var seed maintenanceRecord
				writes, faulted := 0, false
				d.maintenanceCommit = func(data []byte) error {
					var ledger maintenanceLedger
					if err := json.Unmarshal(data, &ledger); err != nil {
						return err
					}
					writes++
					if writes == 1 {
						seed = ledger.Leases[0]
					}
					if writes != write {
						return writeMaintenanceLedger(d.maintenancePath, data)
					}
					transactionData, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
					if err != nil {
						return err
					}
					var transaction maintenanceTransaction
					if err := json.Unmarshal(transactionData, &transaction); err != nil {
						return err
					}
					if len(transaction.Predecessor) != write-1 || write == 2 && (transaction.Predecessor[0].HoldID != seed.HoldID || transaction.Predecessor[0].State != control.MaintenanceHolding || !transaction.Predecessor[0].ExpiresAt.Equal(seed.ExpiresAt) || !transaction.Predecessor[0].DrainDeadline.Equal(seed.DrainDeadline)) {
						return errors.New("clock commit lost its durable seed predecessor")
					}
					if after {
						if err := writeMaintenanceLedger(d.maintenancePath, data); err != nil {
							return err
						}
					}
					faulted = true
					return errors.New("injected HOLDING publication failure")
				}
				result, err := control.SendMaintenance(d.ctlSrv.SocketPath(), control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(5000), DrainTimeoutMs: 1000}, 10*time.Second)
				d.maintenanceGate.RLock()
				seedSnapshot, writeCount, reachedFault := seed, writes, faulted
				d.maintenanceGate.RUnlock()
				if !reachedFault || writeCount != write || !errors.Is(err, control.ErrMaintenancePersistenceFailed) || result == nil || result.State != control.MaintenanceHolding || result.TreesRetired || !result.ExpiresAt.Equal(seedSnapshot.ExpiresAt) || !result.DrainDeadline.Equal(seedSnapshot.DrainDeadline) {
					t.Fatalf("failed acquisition did not return its conservative seed: fault=%t writes=%d result=%+v err=%v", reachedFault, writeCount, result, err)
				}
				states := d.maintenanceResults()
				if len(states) != write-1 || write == 2 && (states[0].HoldID != seedSnapshot.HoldID || states[0].State != control.MaintenanceHolding || states[0].TreesRetired || !states[0].ExpiresAt.Equal(seedSnapshot.ExpiresAt) || !states[0].DrainDeadline.Equal(seedSnapshot.DrainDeadline)) {
					t.Fatalf("failed clock mutation installed an unacknowledged candidate: %+v", states)
				}
				for _, demand := range []control.Request{req, {Command: req.Command, Args: req.Args, Mode: req.Mode, Cwd: t.TempDir(), Env: req.Env}} {
					if _, _, _, err := d.Spawn(demand); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
						t.Fatalf("failed acquisition reopened admission: %v", err)
					}
				}
				fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"fenced","method":"tools/list","params":{}}`)
				maintenanceResponseCode(t, readDaemonResponseID(t, scanner, `"fenced"`), `"fenced"`, -32005)
				if d.HandleStatus()["maintenance_error_code"] != control.ErrMaintenancePersistenceFailed.Code || entry.Owner.MaintenanceRetired() || !daemonTestProcessAlive(pid) || d.Entry(sid) != entry {
					t.Fatal("failed fence lost fail-closed status or invented tree retirement")
				}
				endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
				if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
					t.Fatalf("pending acquisition permitted activation: %v", err)
				}
				// Even a later real tree-death callback cannot convert a failed
				// clock commit into HELD or retry/replace the pending authority.
				removed, err := d.removeOwnerIfCurrent(sid, entry, ownerRemovalReasonMaintenance, false)
				if !removed.Removed || d.Entry(sid) != nil || !entry.Owner.MaintenanceRetired() || daemonTestProcessAlive(pid) {
					t.Fatalf("exact fault-fixture retirement: %+v %v", removed, err)
				}
				// Zero-grace SoftClose can warn after proving the entire tree dead.
				if err != nil && err.Error() != "upstream: forced kill after soft-close timeout" {
					t.Fatalf("unexpected fault-fixture finalization error: %v", err)
				}
				d.reconcileMaintenance()
				states = d.maintenanceResults()
				if len(states) != write-1 || write == 2 && (states[0].State != control.MaintenanceHolding || states[0].TreesRetired || !states[0].ExpiresAt.Equal(seedSnapshot.ExpiresAt) || !states[0].DrainDeadline.Equal(seedSnapshot.DrainDeadline)) || d.HandleStatus()["maintenance_error_code"] != control.ErrMaintenancePersistenceFailed.Code {
					t.Fatalf("tree-death callback bypassed the failed-acquisition latch: %+v", states)
				}
				if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
					t.Fatalf("tree-death callback repaired an unacknowledged transaction: %v", err)
				}
				d.shutdown(nil)
				recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
				if recovered != nil {
					t.Cleanup(func() { recovered.shutdown(nil) })
				}
				if !errors.Is(err, control.ErrMaintenancePersistenceFailed) || ipc.IsAvailable(endpoint) {
					t.Fatalf("pending clock transaction opened recovery: err=%v available=%t", err, ipc.IsAvailable(endpoint))
				}
			})
		}
	}
}
