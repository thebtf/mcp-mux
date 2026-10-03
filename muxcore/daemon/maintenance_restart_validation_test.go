package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func sendMaintenanceRestartValidation(t *testing.T, socketPath, sid string, drain int, raw bool) *control.Response {
	t.Helper()
	if !raw {
		// A positive explicit budget bypasses the client's operational-default
		// range check, so the real daemon must validate the drain itself.
		resp, err := control.SendWithTimeout(socketPath, control.Request{Cmd: "restart_owner", ServerID: sid, DrainTimeoutMs: drain}, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	conn, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "{\"cmd\":\"restart_owner\",\"server_id\":%q,\"drain_timeout_ms\":%d}\n", sid, drain); err != nil {
		t.Fatal(err)
	}
	var resp control.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

func TestMaintenanceRestartDrainValidationBoundaries(t *testing.T) {
	maxSafe := int64((1<<63 - 1) / time.Millisecond)
	maxInt := int64(^uint(0) >> 1)
	type boundaryCase struct {
		name  string
		drain int
		want  *control.MaintenanceError
	}
	cases := []boundaryCase{
		{"negative", -1, control.ErrMaintenanceInvalid},
		{"zero", 0, control.ErrMaintenanceNotFound},
	}
	if maxSafe <= maxInt {
		cases = append(cases, boundaryCase{"maximum_safe", int(maxSafe), control.ErrMaintenanceNotFound})
	} else {
		cases = append(cases, boundaryCase{"maximum_native_int", int(maxInt), control.ErrMaintenanceNotFound})
	}
	if maxSafe < maxInt {
		cases = append(cases, boundaryCase{"maximum_safe_plus_one", int(maxSafe + 1), control.ErrMaintenanceInvalid})
	}
	for _, raw := range []bool{true, false} {
		transport := "raw_json"
		if !raw {
			transport = "explicit_positive_timeout"
		}
		t.Run(transport, func(t *testing.T) {
			d := maintenanceDaemon(t)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					// An absent exact target distinguishes valid shape from invalid
					// duration without ever starting a centuries-long drain.
					resp := sendMaintenanceRestartValidation(t, d.ctlSrv.SocketPath(), "absent-restart-validation-owner", tc.drain, raw)
					if resp.OK || resp.ErrorCode != tc.want.Code || !errors.Is(resp.Err(), tc.want) || resp.IPCPath != "" || resp.Token != "" {
						t.Fatalf("drain=%d response=%+v, want typed %s without admission", tc.drain, resp, tc.want.Code)
					}
				})
			}
			if d.OwnerCount() != 0 || len(d.maintenanceResults()) != 0 {
				t.Fatal("validation changed owner or maintenance state")
			}
			assertOwnerRemovalStatus(t, d.HandleStatus(), 0, "operator_hard", 0)
		})
	}
}

func TestMaintenanceRestartDrainOverflowPreservesServingOwner(t *testing.T) {
	maxSafe := int64((1<<63 - 1) / time.Millisecond)
	if maxSafe >= int64(^uint(0)>>1) {
		t.Skip("native int cannot represent a duration-overflowing millisecond drain")
	}
	for _, raw := range []bool{true, false} {
		transport := "raw_json"
		if !raw {
			transport = "explicit_positive_timeout"
		}
		t.Run(transport, func(t *testing.T) {
			d := maintenanceDaemon(t)
			d.persistent = true
			req, started, release, effects := maintenanceHelperRequest(t)
			path, sid, token, err := d.Spawn(req)
			if err != nil {
				t.Fatal(err)
			}
			entry := d.Entry(sid)
			conn, scanner := connectSpawnedOwner(t, path, token)
			defer conn.Close()
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"restart-validation","version":"1"}}}`)
			readDaemonResponseID(t, scanner, "1")
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":2,"method":"maintenance/write","params":{"marker":"before"}}`)
			if daemonRespawnGeneration(t, readDaemonResponseID(t, scanner, "2")) != 1 {
				t.Fatal("original helper generation was not serving")
			}
			pid, _ := entry.Owner.Status()["upstream_pid"].(int)
			if !daemonTestProcessAlive(pid) {
				t.Fatal("fixture has no live upstream process")
			}
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":3,"method":"maintenance/wait","params":{}}`)
			waitForDaemonCondition(t, 5*time.Second, func() bool { _, err := os.Stat(started); return err == nil }, "original upstream did not receive pending work")
			if entry.Owner.PendingRequests() != 1 {
				t.Fatal("fixture must have one delivered request at the restart boundary")
			}

			resp := sendMaintenanceRestartValidation(t, d.ctlSrv.SocketPath(), sid, int(maxSafe+1), raw)
			if resp.OK || resp.ErrorCode != control.ErrMaintenanceInvalid.Code || !errors.Is(resp.Err(), control.ErrMaintenanceInvalid) || resp.IPCPath != "" || resp.Token != "" {
				t.Errorf("overflow response=%+v, want typed maintenance_invalid without admission", resp)
			}
			currentPID, _ := entry.Owner.Status()["upstream_pid"].(int)
			unchanged := d.Entry(sid) == entry && currentPID == pid && daemonTestProcessAlive(pid)
			if !unchanged || d.OwnerCount() != 1 {
				t.Errorf("overflow retired/replaced the original owner: same_entry=%t old_pid=%d current_pid=%d old_alive=%t owners=%d", d.Entry(sid) == entry, pid, currentPID, daemonTestProcessAlive(pid), d.OwnerCount())
			}
			generation, err := os.ReadFile(req.Env["MCPMUX_MAINTENANCE_GENERATION"])
			if err != nil || string(generation) != "1" {
				t.Errorf("overflow spawned another generation: generation=%q err=%v", generation, err)
			}
			if len(d.maintenanceResults()) != 0 {
				t.Error("invalid restart created maintenance authority")
			}
			assertOwnerRemovalStatus(t, d.HandleStatus(), 0, "operator_hard", 0)
			if !unchanged {
				return
			}
			if entry.Owner.PendingRequests() != 1 {
				t.Fatal("invalid restart settled the original pending request")
			}
			if err := os.WriteFile(release, []byte("complete"), 0o600); err != nil {
				t.Fatal(err)
			}
			if daemonRespawnGeneration(t, readDaemonResponseID(t, scanner, "3")) != 1 {
				t.Fatal("original delivered work did not complete in its original process")
			}
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":4,"method":"maintenance/write","params":{"marker":"after"}}`)
			if daemonRespawnGeneration(t, readDaemonResponseID(t, scanner, "4")) != 1 {
				t.Fatal("ordinary request after rejection did not reach the original process")
			}
			data, err := os.ReadFile(effects)
			if err != nil || string(data) != "before\nafter\n" {
				t.Fatalf("actual upstream effects=%q err=%v", data, err)
			}
		})
	}
}

func TestMaintenanceRestartZeroDrainReplacesActualOwner(t *testing.T) {
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
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"zero-restart-validation","version":"1"}}}`)
	readDaemonResponseID(t, scanner, "1")
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":2,"method":"maintenance/write","params":{"marker":"original"}}`)
	if daemonRespawnGeneration(t, readDaemonResponseID(t, scanner, "2")) != 1 {
		t.Fatal("original helper generation was not serving")
	}
	pid, _ := entry.Owner.Status()["upstream_pid"].(int)
	if !daemonTestProcessAlive(pid) {
		t.Fatal("fixture has no live upstream process")
	}
	resp := sendMaintenanceRestartValidation(t, d.ctlSrv.SocketPath(), sid, 0, false)
	if !resp.OK || resp.Err() != nil || resp.ServerID != sid || resp.IPCPath == "" || resp.Token == "" {
		t.Fatalf("zero-drain restart response=%+v", resp)
	}
	next := d.Entry(sid)
	if next == nil || next == entry || next.Owner == entry.Owner || daemonTestProcessAlive(pid) || d.OwnerCount() != 1 {
		t.Fatal("zero-drain restart did not retire and replace the exact original owner")
	}
	freshConn, freshScanner := connectSpawnedOwner(t, resp.IPCPath, resp.Token)
	defer freshConn.Close()
	fmt.Fprintln(freshConn, `{"jsonrpc":"2.0","id":3,"method":"maintenance/write","params":{"marker":"replacement"}}`)
	if daemonRespawnGeneration(t, readDaemonResponseID(t, freshScanner, "3")) != 2 {
		t.Fatal("zero-drain replacement was not serving in the next actual generation")
	}
	data, err := os.ReadFile(effects)
	if err != nil || string(data) != "original\nreplacement\n" {
		t.Fatalf("actual zero-drain restart effects=%q err=%v", data, err)
	}
	assertOwnerRemovalStatus(t, d.HandleStatus(), 1, "operator_hard", 1)
}
