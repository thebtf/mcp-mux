package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func maintenanceDaemon(t *testing.T) *Daemon {
	t.Helper()
	config := t.TempDir()
	t.Setenv("HOME", config)
	t.Setenv("USERPROFILE", config)
	t.Setenv("APPDATA", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	path := shortSocketPath(t, "maintenance.ctl.sock")
	namespace := "m-" + strings.TrimPrefix(strings.TrimSuffix(filepath.Base(path), ".sock"), "mux-test-")
	d, err := New(Config{ControlPath: path, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.shutdown(nil) })
	return d
}

func maintenanceHelperRequest(t *testing.T) (control.Request, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	generation := filepath.Join(dir, "generation")
	started, release, effects := filepath.Join(dir, "started"), filepath.Join(dir, "release"), filepath.Join(dir, "effects")
	req := control.Request{Command: os.Args[0], Args: []string{"-test.run=^TestMaintenanceDaemonHelperProcess$"}, Mode: "isolated", Cwd: dir, Env: map[string]string{
		"MCPMUX_MAINTENANCE_HELPER": "1", "MCPMUX_MAINTENANCE_GENERATION": generation,
		"MCPMUX_MAINTENANCE_STARTED": started, "MCPMUX_MAINTENANCE_RELEASE": release, "MCPMUX_MAINTENANCE_EFFECTS": effects,
	}}
	return req, started, release, effects
}

func TestMaintenanceDaemonHelperProcess(t *testing.T) {
	if os.Getenv("MCPMUX_MAINTENANCE_HELPER") != "1" {
		return
	}
	generation := nextDaemonRespawnHelperGeneration(os.Getenv("MCPMUX_MAINTENANCE_GENERATION"))
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Marker string `json:"marker"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			writeDaemonRespawnResult(req.ID, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "maintenance-helper", "version": fmt.Sprint(generation)}})
		case "tools/list":
			writeDaemonRespawnResult(req.ID, map[string]any{"tools": []any{}})
		case "maintenance/wait":
			if os.WriteFile(os.Getenv("MCPMUX_MAINTENANCE_STARTED"), []byte("delivered"), 0o600) != nil {
				os.Exit(3)
			}
			for {
				if _, err := os.Stat(os.Getenv("MCPMUX_MAINTENANCE_RELEASE")); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			writeDaemonRespawnResult(req.ID, map[string]any{"completed": true, "generation": generation})
		case "maintenance/write":
			file, err := os.OpenFile(os.Getenv("MCPMUX_MAINTENANCE_EFFECTS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(4)
			}
			_, err = fmt.Fprintln(file, req.Params.Marker)
			_ = file.Close()
			if err != nil {
				os.Exit(5)
			}
			writeDaemonRespawnResult(req.ID, map[string]any{"generation": generation})
		default:
			writeDaemonRespawnResult(req.ID, map[string]any{"generation": generation})
		}
	}
	os.Exit(0)
}

func maintenanceTTL(ms int64) *int64 { return &ms }

func waitMaintenanceState(t *testing.T, d *Daemon, state control.MaintenanceState) {
	t.Helper()
	waitForDaemonCondition(t, 5*time.Second, func() bool { results := d.maintenanceResults(); return len(results) == 1 && results[0].State == state }, "maintenance state did not reach "+string(state))
}

func maintenanceResponseCode(t *testing.T, raw []byte, id string, code int) {
	t.Helper()
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if string(response.ID) != id || response.Error == nil || response.Error.Code != code {
		t.Fatalf("request disposition: %s, want id %s code %d", raw, id, code)
	}
}

func TestMaintenanceDeadlineDrainsDeliveredWorkAndRejectsCachedIngress(t *testing.T) {
	for _, completes := range []bool{true, false} {
		t.Run(fmt.Sprintf("completes_%t", completes), func(t *testing.T) {
			d := maintenanceDaemon(t)
			req, started, release, _ := maintenanceHelperRequest(t)
			path, sid, token, err := d.Spawn(req)
			if err != nil {
				t.Fatal(err)
			}
			entry := d.Entry(sid)
			conn, scanner := connectSpawnedOwner(t, path, token)
			defer conn.Close()
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"maintenance","version":"1"}}}`)
			readDaemonResponseID(t, scanner, "1")
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
			readDaemonResponseID(t, scanner, "2")
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"work","method":"maintenance/wait","params":{}}`)
			waitForDaemonCondition(t, 5*time.Second, func() bool { _, err := os.Stat(started); return err == nil }, "helper never received work")
			type held struct {
				result control.MaintenanceResult
				err    error
			}
			done := make(chan held, 1)
			go func() {
				result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, DrainTimeoutMs: 1000, HoldTTLMS: maintenanceTTL(5000)})
				done <- held{result, err}
			}()
			waitMaintenanceState(t, d, control.MaintenanceHolding)
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`)
			maintenanceResponseCode(t, readDaemonResponseID(t, scanner, "7"), "7", -32005)
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":"cached-string","method":"tools/list","params":{}}`)
			maintenanceResponseCode(t, readDaemonResponseID(t, scanner, `"cached-string"`), `"cached-string"`, -32005)
			if completes {
				if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			response := readDaemonResponseID(t, scanner, `"work"`)
			if completes {
				var obj map[string]json.RawMessage
				_ = json.Unmarshal(response, &obj)
				if obj["error"] != nil || !bytes.Contains(obj["result"], []byte(`"completed":true`)) {
					t.Fatalf("completed request was lost: %s", response)
				}
			} else {
				maintenanceResponseCode(t, response, `"work"`, -32005)
			}
			select {
			case result := <-done:
				if result.err != nil || result.result.State != control.MaintenanceHeld || !result.result.TreesRetired || !entry.Owner.MaintenanceRetired() {
					t.Fatalf("hold: %+v %v", result.result, result.err)
				}
				if !completes && time.Now().Before(result.result.DrainDeadline) {
					t.Fatal("unfinished work was cut off before accepted deadline")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("hold did not retire delivered work")
			}
			if _, _, _, err := d.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("same context respawned: %v", err)
			}
			if _, err := d.HandleRestartOwner(control.Request{ServerID: sid}); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("managed restart bypassed hold: %v", err)
			}
			if _, err := d.HandleShutdownWithError(0); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("shutdown bypassed hold: %v", err)
			}
			if _, _, err := d.HandleGracefulRestart(0); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("handoff bypassed hold: %v", err)
			}
			d.Shutdown()
			select {
			case <-d.Done():
				t.Fatal("direct shutdown bypassed hold")
			default:
			}
			outside := req
			outside.Cwd = t.TempDir()
			if _, _, _, err := d.Spawn(outside); err != nil {
				t.Fatalf("finite context fence widened to command wildcard: %v", err)
			}
		})
	}
}

func TestMaintenanceFiniteContextSurvivesNonceAndRetryGenerations(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			d := maintenanceDaemon(t)
			req, _, _, _ := maintenanceHelperRequest(t)
			req.ProtocolEra, _ = protocol.Wire()
			_, first, _, err := d.Spawn(req)
			if err != nil {
				t.Fatal(err)
			}
			_, second, _, err := d.Spawn(req)
			if err != nil {
				t.Fatal(err)
			}
			if first == second {
				t.Fatal("isolated generation identities collapsed")
			}
			a, b := d.Entry(first), d.Entry(second)
			result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: first, HoldTTLMS: maintenanceTTL(5000)})
			if err != nil || result.State != control.MaintenanceHeld || !a.Owner.MaintenanceRetired() || !b.Owner.MaintenanceRetired() {
				t.Fatalf("finite generation retirement: %+v %v", result, err)
			}
			if _, _, _, err := d.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("new nonce/retry crossed persisted context: %v", err)
			}
			other := req
			other.ProtocolEra, _ = era.EraLegacy.Wire()
			if protocol == era.EraLegacy {
				other.ProtocolEra, _ = era.EraModern20260728.Wire()
			}
			if _, _, _, err := d.Spawn(other); err != nil {
				t.Fatalf("fence crossed protocol era: %v", err)
			}
		})
	}
}

func TestMaintenancePlaceholderRaceCannotAcknowledgeBeforeSettlement(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	d.beforeColdOwnerPromotion = func(*owner.Owner) { close(entered); <-release }
	spawned := make(chan error, 1)
	go func() { _, _, _, err := d.Spawn(req); spawned <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("creator did not reach promotion barrier")
	}
	d.mu.RLock()
	var sid string
	for key := range d.owners {
		sid = key
	}
	d.mu.RUnlock()
	type held struct {
		result control.MaintenanceResult
		err    error
	}
	done := make(chan held, 1)
	go func() {
		result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, DrainTimeoutMs: 3000, HoldTTLMS: maintenanceTTL(5000)})
		done <- held{result, err}
	}()
	waitMaintenanceState(t, d, control.MaintenanceHolding)
	select {
	case result := <-done:
		t.Fatalf("placeholder acknowledged while creator owns it: %+v", result)
	default:
	}
	close(release)
	select {
	case err := <-spawned:
		if !errors.Is(err, control.ErrMaintenanceHeld) {
			t.Fatalf("late promotion: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("promotion did not settle")
	}
	select {
	case result := <-done:
		if result.err != nil || result.result.State != control.MaintenanceHeld {
			t.Fatalf("settled placeholder: %+v %v", result.result, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not observe creator settlement")
	}
}

func TestMaintenanceLedgerRenewReleaseRecoveryAndReadOnlyStatus(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(5000)})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(d.maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	d.HandleStatus()
	if err := CheckMaintenanceForActivation(d.namespace, d.ctlSrv.SocketPath()); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("persisted activation fence: %v", err)
	}
	after, err := os.ReadFile(d.maintenancePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("status/activation check mutated durable authority")
	}
	renewed, err := d.HandleMaintenance(control.Request{Cmd: "renew", HoldID: result.HoldID, HoldTTLMS: maintenanceTTL(10000)})
	if err != nil || !renewed.ExpiresAt.After(result.ExpiresAt) {
		t.Fatalf("exact renewal: %+v %v", renewed, err)
	}
	// Simulate unplanned daemon loss after actual tree death, retaining only the
	// ledger. SkipSnapshot must not bypass the startup fence.
	endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
	d.shutdown(nil)
	recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovered.shutdown(nil) })
	if _, _, _, err := recovered.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("aware startup bypassed ledger: %v", err)
	}
	states := recovered.maintenanceResults()
	if len(states) != 1 || states[0].HoldID != result.HoldID || states[0].ServerID != "" || states[0].State != control.MaintenanceHeld {
		t.Fatalf("recovered authority: %+v", states)
	}
	if _, err := recovered.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid}); !errors.Is(err, control.ErrMaintenanceNotFound) {
		t.Fatalf("recovered private context invented stale display-target authority: %v", err)
	}
	if _, err := recovered.HandleMaintenance(control.Request{Cmd: "resume", HoldID: "stale"}); !errors.Is(err, control.ErrMaintenanceConflict) {
		t.Fatalf("stale release: %v", err)
	}
	released, err := recovered.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID})
	if err != nil || released.State != control.MaintenanceReleased {
		t.Fatalf("durable release: %+v %v", released, err)
	}
	if err := CheckMaintenanceForActivation(namespace, endpoint); err != nil {
		t.Fatalf("released activation: %v", err)
	}
	if _, err := recovered.HandleMaintenance(control.Request{Cmd: "renew", HoldID: result.HoldID}); !errors.Is(err, control.ErrMaintenanceNotFound) {
		t.Fatalf("released lease renewed: %v", err)
	}
	if _, _, _, err := recovered.Spawn(req); err != nil {
		t.Fatalf("fresh same-context launch after durable release: %v", err)
	}
}

func TestMaintenanceBlockedTTLDoesNotReleaseUnprovenTree(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	original := finalizeOwnerForRemoval
	var allow atomic.Bool
	finalizeOwnerForRemoval = func(o *owner.Owner, soft bool) (int, bool, error) {
		if !allow.Load() {
			return 0, false, errors.New("tree retirement proof unavailable")
		}
		return original(o, soft)
	}
	t.Cleanup(func() { allow.Store(true); finalizeOwnerForRemoval = original })
	// Advance durable fixture time below instead of racing renew against an expiry
	// timer that acquires the same nonblocking namespace lock.
	result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid})
	if !errors.Is(err, control.ErrMaintenanceRetirementBlocked) || result.State != control.MaintenanceRetirementBlocked || result.TreesRetired {
		t.Fatalf("unproven tree hold: %+v %v", result, err)
	}
	renewed, renewErr := d.HandleMaintenance(control.Request{Cmd: "renew", HoldID: result.HoldID, HoldTTLMS: maintenanceTTL(600000)})
	if renewErr != nil || renewed.State != control.MaintenanceRetirementBlocked || !renewed.ExpiresAt.After(result.ExpiresAt) {
		t.Fatalf("exact blocked renewal: %+v %v", renewed, renewErr)
	}
	result = renewed
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		t.Fatal(err)
	}
	d.maintenanceGate.Lock()
	lease := d.maintenanceLeases[result.HoldID]
	updated := *lease
	updated.result.ExpiresAt = time.Now().Add(-time.Second)
	next := copyMaintenanceLeases(d.maintenanceLeases)
	next[result.HoldID] = &updated
	err = d.persistMaintenanceLocked(next)
	if err == nil {
		// Keep the exact pins and fence, without scheduling an immediate competing
		// timer; blocked expiry is released only after the existing retirement retry.
		d.maintenanceLeases = next
		result = updated.result
		for _, pin := range updated.pins {
			if pin.entry.Owner != nil {
				pin.entry.Owner.SetMaintenance(&updated.result)
			}
		}
		err = d.expireMaintenanceLocked(time.Now())
	}
	d.maintenanceGate.Unlock()
	_ = lock.Close()
	if err != nil || len(d.maintenanceResults()) != 1 {
		t.Fatalf("TTL released an unproven tree: %v", err)
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID}); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
		t.Fatalf("resume unproven tree: %v", err)
	}
	if _, _, _, err := d.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("TTL bypassed admission: %v", err)
	}
	if time.Now().Before(result.ExpiresAt) {
		t.Fatal("fixture lease did not reach TTL")
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "renew", HoldID: result.HoldID}); !errors.Is(err, control.ErrMaintenanceConflict) {
		t.Fatalf("expired blocked lease renewed: %v", err)
	}
	if competing, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(600000)}); !errors.Is(err, control.ErrMaintenanceConflict) || competing.HoldID != result.HoldID || competing.State != control.MaintenanceRetirementBlocked || !competing.ExpiresAt.Equal(result.ExpiresAt) {
		t.Fatalf("expired incomplete lease was replaced/extended by competing hold: %+v %v", competing, err)
	}
	allow.Store(true)
	waitForDaemonCondition(t, 5*time.Second, func() bool { return len(d.maintenanceResults()) == 0 }, "exact retirement retry did not durably release expired fence")
	if _, _, _, err := d.Spawn(req); err != nil {
		t.Fatalf("proved expiry did not reopen admission: %v", err)
	}
}

func TestMaintenancePersistenceFailureRemainsFencedUntilDurableRetry(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	d.maintenanceCommit = func(data []byte) error {
		var ledger maintenanceLedger
		_ = json.Unmarshal(data, &ledger)
		if len(ledger.Leases) > 0 && ledger.Leases[0].State == control.MaintenanceHeld {
			return errors.New("durability unavailable")
		}
		return writeMaintenanceLedger(d.maintenancePath, data)
	}
	result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(5000)})
	if !errors.Is(err, control.ErrMaintenancePersistenceFailed) || result.State == control.MaintenanceHeld {
		t.Fatalf("uncommitted HELD acknowledged: %+v %v", result, err)
	}
	if competing, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(600000)}); !errors.Is(err, control.ErrMaintenanceConflict) || competing.HoldID != result.HoldID || competing.State != result.State || !competing.ExpiresAt.Equal(result.ExpiresAt) {
		t.Fatalf("uncommitted retirement lease lost its original target conflict: %+v %v", competing, err)
	}
	if _, _, _, err := d.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("failed commit opened admission: %v", err)
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID}); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
		t.Fatalf("uncommitted retirement released: %v", err)
	}
	d.maintenanceGate.Lock()
	d.maintenanceCommit = nil
	d.maintenanceGate.Unlock()
	d.reconcileMaintenance()
	waitMaintenanceState(t, d, control.MaintenanceHeld)
	if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID}); err != nil {
		t.Fatalf("durable retry recovery: %v", err)
	}
}

func TestMaintenanceInitialPersistenceFailureClosesAllAdmission(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	d.maintenanceCommit = func([]byte) error { return errors.New("fence durability unavailable") }
	_, err = d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid})
	if !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("fence commit: %v", err)
	}
	other := req
	other.Cwd = t.TempDir()
	if _, _, _, err := d.Spawn(other); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("uncommitted fence permitted admission: %v", err)
	}
	if d.HandleStatus()["maintenance_error_code"] != control.ErrMaintenancePersistenceFailed.Code {
		t.Fatal("read-only status hid in-memory failclosed state")
	}
}

func TestMaintenanceIncompleteLedgerRecoveryCannotInventTreeDeath(t *testing.T) {
	for _, state := range []control.MaintenanceState{control.MaintenanceHolding, control.MaintenanceRetirementBlocked} {
		t.Run(string(state), func(t *testing.T) {
			d := maintenanceDaemon(t)
			req, _, _, _ := maintenanceHelperRequest(t)
			key := d.maintenanceContext(era.EraLegacy, req.Command, req.Args, req.Cwd, mergeEnv(req.Env))
			result := control.MaintenanceResult{HoldID: "incomplete", State: state, ExpiresAt: time.Now().Add(-time.Minute), DrainDeadline: time.Now().Add(-2 * time.Minute)}
			lease := &maintenanceLease{record: maintenanceRecord{HoldID: result.HoldID, Keys: []string{key}}, result: result}
			if err := d.persistMaintenanceLocked(map[string]*maintenanceLease{result.HoldID: lease}); err != nil {
				t.Fatal(err)
			}
			endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
			d.shutdown(nil)
			recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { recovered.shutdown(nil) })
			recovered.reconcileMaintenance()
			if _, _, _, err := recovered.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("incomplete recovery admitted launch: %v", err)
			}
			if _, err := recovered.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID}); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
				t.Fatalf("lost tree authority released: %v", err)
			}
			if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
				t.Fatalf("incomplete activation: %v", err)
			}
		})
	}
}

func TestMaintenanceContextIdentitySeparatesCredentialsCWDArgumentsEraAndNamespace(t *testing.T) {
	d := maintenanceDaemon(t)
	cwd := t.TempDir()
	args := []string{"a", "b"}
	env := map[string]string{"GITHUB_TOKEN": "non-production-test-a"}
	key := d.maintenanceContext(era.EraLegacy, "command", args, cwd, env)
	if key != d.maintenanceContext(era.EraLegacy, "command", args, filepath.Join(cwd, "."), env) {
		t.Fatal("canonical CWD changed finite identity")
	}
	variants := []string{
		d.maintenanceContext(era.EraLegacy, "command", []string{"a b"}, cwd, env),
		d.maintenanceContext(era.EraLegacy, "command", args, t.TempDir(), env),
		d.maintenanceContext(era.EraLegacy, "command", args, cwd, map[string]string{"GITHUB_TOKEN": "non-production-test-b"}),
		d.maintenanceContext(era.EraModern20260728, "command", args, cwd, env),
	}
	for _, other := range variants {
		if other == key {
			t.Fatal("distinct launch identity collided")
		}
	}
	other := &Daemon{maintenanceScope: maintenanceDigest("another namespace")}
	if other.maintenanceContext(era.EraLegacy, "command", args, cwd, env) == key {
		t.Fatal("engine namespace identity collided")
	}
}

func TestMaintenanceAwareShimRejectsHeldWorkWithoutReplayAndUsesFreshGeneration(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, effects := maintenanceHelperRequest(t)
	path, sid, token, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	hostIn, writer := io.Pipe()
	reader, hostOut := io.Pipe()
	frames := make(chan []byte, 32)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			frames <- append([]byte(nil), scanner.Bytes()...)
		}
		close(frames)
	}()
	done := make(chan error, 1)
	go func() {
		done <- owner.RunResilientClient(owner.ResilientClientConfig{Stdin: hostIn, Stdout: hostOut, InitialIPCPath: path, Token: token, ProbeGracePeriod: time.Nanosecond, ReconnectTimeout: time.Minute, RefreshToken: func() (string, string, error) {
			fresh, err := d.HandleRefreshSessionToken(token)
			if err == nil {
				token = fresh
			}
			return path, fresh, err
		}, Reconnect: func() (string, string, error) {
			freshPath, _, fresh, err := d.Spawn(req)
			if err == nil {
				path, token = freshPath, fresh
			}
			return freshPath, fresh, err
		}, Logger: testLogger(t)})
	}()
	t.Cleanup(func() { _ = writer.Close(); _ = hostIn.Close(); _ = reader.Close(); _ = hostOut.Close() })
	read := func(id string) []byte {
		t.Helper()
		for {
			select {
			case frame, ok := <-frames:
				if !ok {
					t.Fatal("aware host transport closed")
				}
				var obj map[string]json.RawMessage
				_ = json.Unmarshal(frame, &obj)
				if string(obj["id"]) == id {
					return frame
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("host response %s missing", id)
			}
		}
	}
	fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`)
	if daemonRespawnGeneration(t, read("1")) != 1 {
		t.Fatal("initial helper generation missing")
	}
	result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(5000)})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":2,"method":"maintenance/write","params":{"marker":"held-numeric"}}`)
	maintenanceResponseCode(t, read("2"), "2", -32005)
	fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":"held-string","method":"maintenance/write","params":{"marker":"held-string"}}`)
	maintenanceResponseCode(t, read(`"held-string"`), `"held-string"`, -32005)
	select {
	case err := <-done:
		t.Fatalf("shim exited during hold: %v", err)
	default:
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID}); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":3,"method":"maintenance/write","params":{"marker":"fresh"}}`)
	if daemonRespawnGeneration(t, read("3")) != 2 {
		t.Fatal("fresh request did not reach one replacement generation")
	}
	data, err := os.ReadFile(effects)
	if err != nil || string(data) != "fresh\n" {
		t.Fatalf("held side effects replayed: %q %v", data, err)
	}
	_ = writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shim did not exit on host EOF")
	}
}

func TestMaintenanceSafeTTLReleasesDurablyWithoutIdleExitBypass(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	// Start the short expiry interval after actual helper retirement; a race-built
	// helper can outlive it while process-exit finalization is still running.
	result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid})
	if err != nil || result.State != control.MaintenanceHeld || !result.TreesRetired {
		t.Fatalf("acquire retired fixture lease: %+v %v", result, err)
	}
	result, err = d.HandleMaintenance(control.Request{Cmd: "renew", HoldID: result.HoldID, HoldTTLMS: maintenanceTTL(500)})
	if err != nil {
		t.Fatal(err)
	}
	d.idleTimeout = 20 * time.Millisecond
	reaper := NewReaper(d, 5*time.Millisecond)
	defer reaper.Stop()
	select {
	case <-d.Done():
		t.Fatal("empty daemon idle exit bypassed active fence")
	case <-time.After(80 * time.Millisecond):
	}
	waitForDaemonCondition(t, 3*time.Second, func() bool { return len(d.maintenanceResults()) == 0 }, "safe HELD TTL did not release durably")
	if err := CheckMaintenanceForActivation(d.namespace, d.ctlSrv.SocketPath()); err != nil {
		t.Fatalf("expired durable fence remains: %v", err)
	}
	if time.Now().Before(result.ExpiresAt) {
		t.Fatal("safe expiry released before TTL")
	}
	select {
	case <-d.Done():
	case <-time.After(time.Second):
		t.Fatal("reaper stopped running while idle exit was refused")
	}
}

func TestMaintenanceStartupReadsExpiredHeldUnderNamespaceLock(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
	d.shutdown(nil)
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	d.maintenanceGate.Lock()
	lease := d.maintenanceLeases[result.HoldID]
	updated := *lease
	updated.result.ExpiresAt = time.Now().Add(-time.Second)
	err = d.commitMaintenanceLeaseLocked(lease, &updated)
	d.maintenanceGate.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(d.maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovered.shutdown(nil) })
	recovered.HandleStatus()
	if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("expired persisted activation grant: %v", err)
	}
	after, err := os.ReadFile(d.maintenancePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("startup/status mutated ledger while starter owns namespace lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	recovered.reconcileMaintenance()
	if len(recovered.maintenanceResults()) != 0 {
		t.Fatal("expired HELD did not release after startup lock left")
	}
}

func TestMaintenanceCorruptAuthorityRefusesBeforeListenerBind(t *testing.T) {
	d := maintenanceDaemon(t)
	endpoint, namespace, path := d.ctlSrv.SocketPath(), d.namespace, d.maintenancePath
	d.shutdown(nil)
	if err := writeMaintenanceLedger(path, []byte(`{"version":999}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true}); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("corrupt startup: %v", err)
	}
	if ipc.IsAvailable(endpoint) {
		t.Fatal("corrupt authority bound control admission")
	}
	if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("corrupt authority permitted activation: %v", err)
	}
}

func TestMaintenanceOverlappingContextSupersetRefusesBeforeFence(t *testing.T) {
	d := maintenanceDaemon(t)
	command := "maintenance-cache-only-scope"
	snap := daemonMaterializationSnapshot(false)
	d.updateTemplate(command, nil, snap)
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	_, first, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: a})
	if err != nil {
		t.Fatal(err)
	}
	_, reused, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: b})
	if err != nil || reused != first {
		t.Fatalf("shared admitted contexts: %s %s %v", first, reused, err)
	}
	_, second, _, err := d.Spawn(control.Request{Command: command, Mode: "isolated", Cwd: b})
	if err != nil {
		t.Fatal(err)
	}
	token, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := d.admitOwner(d.Entry(second), token, c, mergeEnv(nil), false)
	if err != nil || !admitted {
		t.Fatalf("superset admission: %t %v", admitted, err)
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: first}); !errors.Is(err, control.ErrMaintenanceInvalid) {
		t.Fatalf("overlapping context set silently widened: %v", err)
	}
	if len(d.maintenanceResults()) != 0 || d.OwnerCount() != 2 {
		t.Fatal("ambiguous hold mutated owners or ledger")
	}
}

func TestMaintenanceCompetingHoldPreservesRetiredTargetLease(t *testing.T) {
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	entry := d.Entry(sid)
	held, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(600000)})
	if err != nil || held.State != control.MaintenanceHeld || !entry.Owner.MaintenanceRetired() || d.Entry(sid) != nil {
		t.Fatalf("original target was not retired under its lease: %+v %v", held, err)
	}
	before, err := os.ReadFile(d.maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []int64{5000, 600000, 3600000} {
		result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, DrainTimeoutMs: 1000, HoldTTLMS: maintenanceTTL(ttl)})
		var failure *control.MaintenanceError
		if !errors.Is(err, control.ErrMaintenanceConflict) || !errors.As(err, &failure) || failure.Result == nil || result.HoldID != held.HoldID || result.ServerID != sid || result.State != held.State || !result.ExpiresAt.Equal(held.ExpiresAt) || !result.DrainDeadline.Equal(held.DrainDeadline) || result.TreesRetired != held.TreesRetired || failure.Result.HoldID != held.HoldID {
			t.Fatalf("competing hold replaced/renewed the original authority: %+v %v", result, err)
		}
	}
	after, err := os.ReadFile(d.maintenancePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("competing hold changed durable lease authority: %v", err)
	}
	transactionAfter, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if err != nil || !bytes.Equal(transactionBefore, transactionAfter) {
		t.Fatalf("competing hold changed durable transaction authority: %v", err)
	}
	states := d.maintenanceResults()
	if len(states) != 1 || states[0].HoldID != held.HoldID || !states[0].ExpiresAt.Equal(held.ExpiresAt) {
		t.Fatalf("competing hold changed live lease authority: %+v", states)
	}
	if _, _, _, err := d.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("competing hold reopened the original launch context: %v", err)
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid + "-other"}); !errors.Is(err, control.ErrMaintenanceNotFound) {
		t.Fatalf("exact display-target conflict widened to a different selector: %v", err)
	}

	other, _, _, effects := maintenanceHelperRequest(t)
	path, otherSID, token, err := d.Spawn(other)
	if err != nil || otherSID == sid {
		t.Fatalf("unrelated context could not obtain its own owner: %q %v", otherSID, err)
	}
	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"maintenance-competitor","version":"1"}}}`)
	readDaemonResponseID(t, scanner, "1")
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":2,"method":"maintenance/write","params":{"marker":"outside-active-lease"}}`)
	readDaemonResponseID(t, scanner, "2")
	data, err := os.ReadFile(effects)
	if err != nil || string(data) != "outside-active-lease\n" {
		t.Fatalf("unrelated context was not runnable during the lease: %q %v", data, err)
	}
	otherHeld, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: otherSID, HoldTTLMS: maintenanceTTL(600000)})
	if err != nil || otherHeld.State != control.MaintenanceHeld || otherHeld.HoldID == held.HoldID {
		t.Fatalf("different selector could not acquire its unrelated lease: %+v %v", otherHeld, err)
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: otherHeld.HoldID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: held.HoldID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid}); !errors.Is(err, control.ErrMaintenanceNotFound) {
		t.Fatalf("released target retained stale conflict authority: %v", err)
	}
}

func TestMaintenanceSnapshotPendingCompatibleEnvironmentScopeFailsClosed(t *testing.T) {
	t.Setenv("SERVICE_CONFIG_PATH", "")
	if err := os.Unsetenv("SERVICE_CONFIG_PATH"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(snapshotRestartEnv, "")
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", "")
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "")
	d := maintenanceDaemon(t)
	req, _, _, _ := maintenanceHelperRequest(t)
	req.Mode = "global"
	template := daemonMaterializationSnapshot(false)
	template.Env = mergeEnv(req.Env)
	d.updateTemplate(req.Command, req.Args, template)
	_, sid, firstToken, err := d.Spawn(req)
	if err != nil {
		t.Fatal(err)
	}
	optional := req
	optional.Env = cloneSnapshotStringMap(req.Env)
	optional.Env["SERVICE_CONFIG_PATH"] = filepath.Join(req.Cwd, "service.json")
	_, sharedSID, secondToken, err := d.Spawn(optional)
	if err != nil || sharedSID != sid || firstToken == secondToken {
		t.Fatalf("actual optional environment did not share the same-CWD owner: %q %q %v", sid, sharedSID, err)
	}
	original := d.Entry(sid)
	if !original.Owner.SessionMgr().IsPreRegistered(firstToken) || !original.Owner.SessionMgr().IsPreRegistered(secondToken) {
		t.Fatal("fixture did not retain both admitted, unconsumed tokens")
	}
	// Pending admission already records both maintenance contexts, but neither
	// token has reconnect history. No clock or synthetic snapshot mutation is
	// needed to reach the same payload loss as expired disconnected history.
	path, err := d.SerializeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot DaemonSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Owners) != 1 || len(snapshot.Sessions) != 0 {
		t.Fatal("fixture did not serialize exactly one owner before token consumption")
	}
	observed := snapshot.Owners[0]
	_, carriesOptionalEnv := observed.Env["SERVICE_CONFIG_PATH"]
	if observed.ServerID != sid || len(observed.CwdSet) != 1 || len(observed.BoundTokens) != 0 || carriesOptionalEnv {
		t.Fatal("actual snapshot unexpectedly preserved the optional admitted environment")
	}
	removed, err := d.removeOwnerIfCurrent(sid, original, ownerRemovalReasonRestoreFailed, false)
	if err != nil || !removed.Removed {
		t.Fatalf("predecessor retirement was not proven: %+v %v", removed, err)
	}
	if count := d.loadSnapshot(); count != 1 {
		t.Fatalf("ordinary snapshot restore without an active lease was refused: %d", count)
	}
	restored := d.Entry(sid)
	if restored == nil || restored == original || restored.Owner == nil {
		t.Fatal("snapshot did not register a successor owner")
	}
	identity := captureOwnerEntryIdentity(restored)
	pid := 0
	waitForDaemonCondition(t, 5*time.Second, func() bool {
		pid, _ = restored.Owner.Status()["upstream_pid"].(int)
		return daemonTestProcessAlive(pid)
	}, "ordinary restored upstream did not materialize")
	result, holdErr := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(600000)})
	// Exercise forgotten-context demand even on the unsafe original: accepting
	// the hold retires the owner and this distinct environment escapes its fence.
	_, demandedSID, freshToken, spawnErr := d.Spawn(optional)
	if !errors.Is(holdErr, control.ErrMaintenanceInvalid) || result.State == control.MaintenanceHeld || result.TreesRetired {
		t.Fatalf("incomplete restored scope granted replacement: hold=%+v err=%v forgotten-context spawn=%v predecessor_retired=%t", result, holdErr, spawnErr, restored.Owner.MaintenanceRetired())
	}
	if spawnErr != nil || demandedSID != sid || freshToken == "" || d.Entry(sid) != restored || !identity.matches(restored) {
		t.Fatalf("refused hold changed exact successor admission authority: %q %v", demandedSID, spawnErr)
	}
	if len(d.maintenanceResults()) != 0 || restored.Owner.MaintenanceRetired() || !daemonTestProcessAlive(pid) {
		t.Fatal("refused hold published a fence or retired the restored process authority")
	}
}
