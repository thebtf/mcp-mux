package daemon

import (
	"encoding/json"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func TestModernSharingReview_LauncherAdvertisementsNeverReachSharedUpstream(t *testing.T) {
	for _, testCase := range []struct {
		name                   string
		clientAdvertisements   bool
		differentAuthorization bool
	}{
		{name: "client advertisements", clientAdvertisements: true},
		{name: "daemon fallback advertisements"},
		{name: "client advertisements with different authorization", clientAdvertisements: true, differentAuthorization: true},
		{name: "daemon fallback with different authorization", differentAuthorization: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("MCPMUX_LAUNCHER_ATTESTATION", "fixture-daemon-attestation")
			t.Setenv("MCPMUX_LAUNCHER_PROTOCOL", "1:41001")
			d, launchA := modernSharingDaemon(t)
			envA := launchA["env"].(map[string]string)
			delete(envA, "MCPMUX_LAUNCHER_ATTESTATION")
			delete(envA, "MCPMUX_LAUNCHER_PROTOCOL")
			envA["AUTHORIZATION"] = "fixture-authorization-a"
			envB := maps.Clone(envA)
			if testCase.clientAdvertisements {
				envA["MCPMUX_LAUNCHER_ATTESTATION"] = "fixture-client-attestation-a"
				envB["MCPMUX_LAUNCHER_ATTESTATION"] = "fixture-client-attestation-b"
				envA["MCPMUX_LAUNCHER_PROTOCOL"] = "1:41002"
				envB["MCPMUX_LAUNCHER_PROTOCOL"] = "1:41003"
			}
			if testCase.differentAuthorization {
				envB["AUTHORIZATION"] = "fixture-authorization-b"
			}
			launchB := maps.Clone(launchA)
			launchB["env"] = envB
			a := spawnModernSharing(t, d, launchA, true)
			b := spawnModernSharing(t, d, launchB, true)
			hostA := dialIPC(t, a.IPCPath, a.Token)
			t.Cleanup(func() { _ = hostA.conn.Close() })
			hostB := dialIPC(t, b.IPCPath, b.Token)
			t.Cleanup(func() { _ = hostB.conn.Close() })
			pidA := modernSharingReviewHostEnvironment(t, hostA)
			pidB := modernSharingReviewHostEnvironment(t, hostB)
			if testCase.differentAuthorization {
				if a.ServerID == b.ServerID || a.IPCPath == b.IPCPath || pidA == pidB || d.OwnerCount() != 2 {
					t.Errorf("different genuine authorization shared an upstream: owners=%d same_id=%t same_path=%t pids=%d/%d", d.OwnerCount(), a.ServerID == b.ServerID, a.IPCPath == b.IPCPath, pidA, pidB)
				}
			} else if a.ServerID != b.ServerID || a.IPCPath != b.IPCPath || pidA != pidB || d.OwnerCount() != 1 {
				t.Errorf("launcher-only advertisements fragmented identical upstreams: owners=%d same_id=%t same_path=%t pids=%d/%d", d.OwnerCount(), a.ServerID == b.ServerID, a.IPCPath == b.IPCPath, pidA, pidB)
			}
		})
	}
}

func modernSharingReviewHostEnvironment(t *testing.T, host *ipcConn) int {
	t.Helper()
	daemonSendReq(t, host, 1, "ping", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`)
	response := daemonReadResp(t, host)
	daemonAssertID(t, response, 1)
	var decoded struct {
		Result struct {
			PID                        int   `json:"pid"`
			LauncherAttestationPresent *bool `json:"launcher_attestation_present"`
			LauncherProtocolPresent    *bool `json:"launcher_protocol_present"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Result.PID <= 0 || decoded.Result.PID == os.Getpid() {
		t.Fatal("environment observation did not come from a real upstream child")
	}
	if decoded.Result.LauncherAttestationPresent == nil || decoded.Result.LauncherProtocolPresent == nil {
		t.Fatal("real upstream omitted its reserved-advertisement presence observation")
	}
	if *decoded.Result.LauncherAttestationPresent || *decoded.Result.LauncherProtocolPresent {
		t.Errorf("launcher controller advertisements reached actual upstream PID %d: attestation_present=%t protocol_present=%t", decoded.Result.PID, *decoded.Result.LauncherAttestationPresent, *decoded.Result.LauncherProtocolPresent)
	}
	return decoded.Result.PID
}

func TestModernSharingReview_RestartOwnerPreservesSharedAdmission(t *testing.T) {
	d, launch := modernSharingDaemon(t)
	original := spawnModernSharing(t, d, launch, true)
	originalHost := dialIPC(t, original.IPCPath, original.Token)
	t.Cleanup(func() { _ = originalHost.conn.Close() })
	originalPID := modernSharingHostPID(t, originalHost)

	restarted, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{
		Cmd: "restart_owner", ServerID: original.ServerID,
	}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Err(); err != nil {
		t.Fatal(err)
	}
	if restarted.ProtocolEra != "2026-07-28" || restarted.ServerID == "" || restarted.IPCPath == "" || restarted.Token == "" {
		t.Fatal("restart omitted the modern replacement's host connection identity")
	}
	replacementHost := dialIPC(t, restarted.IPCPath, restarted.Token)
	t.Cleanup(func() { _ = replacementHost.conn.Close() })
	replacementPID := modernSharingHostPID(t, replacementHost)
	if replacementPID == originalPID || daemonTestProcessAlive(originalPID) {
		t.Errorf("restart did not retire the original real upstream: original=%d replacement=%d original_live=%t", originalPID, replacementPID, daemonTestProcessAlive(originalPID))
	}

	response, err := control.SendWithTimeout(d.ctlSrv.SocketPath(), control.Request{Cmd: "list_owners"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Err(); err != nil {
		t.Fatal(err)
	}
	var listing control.ListOwnersResponse
	if err := json.Unmarshal(response.Data, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Owners) != 1 || listing.Owners[0].ServerID != restarted.ServerID {
		t.Fatal("restart did not leave exactly its returned replacement owner")
	}
	if listing.Owners[0].ProtocolEra != "2026-07-28" || listing.Owners[0].SharingPolicy != "shared-exact-context" {
		t.Errorf("restart changed the admitted sharing policy: era=%q policy=%q", listing.Owners[0].ProtocolEra, listing.Owners[0].SharingPolicy)
	}

	fresh := spawnModernSharing(t, d, launch, true)
	freshHost := dialIPC(t, fresh.IPCPath, fresh.Token)
	t.Cleanup(func() { _ = freshHost.conn.Close() })
	freshPID := modernSharingHostPID(t, freshHost)
	if fresh.ServerID != restarted.ServerID || fresh.IPCPath != restarted.IPCPath || freshPID != replacementPID || d.OwnerCount() != 1 {
		t.Errorf("fresh bound admission did not reuse the restarted shared upstream: owners=%d same_id=%t same_path=%t pids=%d/%d", d.OwnerCount(), fresh.ServerID == restarted.ServerID, fresh.IPCPath == restarted.IPCPath, replacementPID, freshPID)
	}
}
