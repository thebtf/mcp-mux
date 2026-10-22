package daemon

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestModernSharingDaemon_TwoHostPipesUseOneUpstreamPID(t *testing.T) {
	d := testDaemon(t)
	launch := modernSharingLaunch(t)
	a := spawnModernSharing(t, d, launch, true)
	b := spawnModernSharing(t, d, launch, true)
	if a.ServerID != b.ServerID || a.IPCPath != b.IPCPath || d.OwnerCount() != 1 {
		t.Fatalf("identical admitted launches did not share one owner: owners=%d ids=%q/%q paths=%q/%q", d.OwnerCount(), a.ServerID, b.ServerID, a.IPCPath, b.IPCPath)
	}
	if a.Token == "" || b.Token == "" || a.Token == b.Token {
		t.Fatal("shared admission did not issue independent host handshake tokens")
	}
	hostA := dialIPC(t, a.IPCPath, a.Token)
	t.Cleanup(func() { _ = hostA.conn.Close() })
	hostB := dialIPC(t, b.IPCPath, b.Token)
	t.Cleanup(func() { _ = hostB.conn.Close() })
	entry := d.Entry(a.ServerID)
	if entry == nil || entry.Owner == nil {
		t.Fatal("shared owner is absent after admission")
	}
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == 2 }, "both real host pipes were not admitted to one owner")
	pidA := modernSharingHostPID(t, hostA)
	pidB := modernSharingHostPID(t, hostB)
	if pidA != pidB {
		t.Fatalf("two host pipes reached different upstream processes: %d / %d", pidA, pidB)
	}
}

func TestModernSharingDaemon_UnknownPartitionAndDifferentEffectiveEnvStayIsolated(t *testing.T) {
	for _, boundary := range []string{"unknown partition", "AUTHORIZATION", "CLAUDE_CODE_ENTRYPOINT"} {
		t.Run(boundary, func(t *testing.T) {
			d := testDaemon(t)
			launchA := modernSharingLaunch(t)
			launchB := make(map[string]any, len(launchA))
			for key, value := range launchA {
				launchB[key] = value
			}
			prepare := boundary != "unknown partition"
			if !prepare {
				launchA["partition"] = map[string]any{"known": false}
				launchB["partition"] = map[string]any{"known": false}
			} else {
				envA := launchA["env"].(map[string]string)
				envB := make(map[string]string, len(envA))
				for key, value := range envA {
					envB[key] = value
				}
				// These keys are ignored by legacy identity filtering; R2 must
				// compare all daemon-normalized effective environment entries.
				envA[boundary] = "fixture-a"
				envB[boundary] = "fixture-b"
				launchB["env"] = envB
			}
			a := spawnModernSharing(t, d, launchA, prepare)
			b := spawnModernSharing(t, d, launchB, prepare)
			if a.ServerID == b.ServerID || a.IPCPath == b.IPCPath || d.OwnerCount() != 2 {
				t.Fatalf("%s crossed modern isolation boundary: owners=%d ids=%q/%q", boundary, d.OwnerCount(), a.ServerID, b.ServerID)
			}
			hostA := dialIPC(t, a.IPCPath, a.Token)
			t.Cleanup(func() { _ = hostA.conn.Close() })
			hostB := dialIPC(t, b.IPCPath, b.Token)
			t.Cleanup(func() { _ = hostB.conn.Close() })
			if modernSharingHostPID(t, hostA) == modernSharingHostPID(t, hostB) {
				t.Fatalf("%s unexpectedly shared one real upstream PID", boundary)
			}
		})
	}
}

func modernSharingLaunch(t *testing.T) map[string]any {
	t.Helper()
	command, args, env := daemonRespawnHelperCommand(filepath.Join(t.TempDir(), "generation"))
	return map[string]any{
		"cmd":            "spawn",
		"command":        command,
		"args":           args,
		"cwd":            t.TempDir(),
		"env":            env,
		"mode":           "global",
		"protocol_era":   "2026-07-28",
		"modern_sharing": true,
		"partition":      map[string]any{"known": true, "key": "r2-fixture"},
	}
}

// Raw control fields keep the baseline executable before the public R2 API exists.
// The prepare response binds the assertion to this daemon and this exact spawn.
func spawnModernSharing(t *testing.T, d *Daemon, launch map[string]any, prepare bool) control.Response {
	t.Helper()
	request := make(map[string]any, len(launch)+2)
	for key, value := range launch {
		request[key] = value
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	request["spawn_nonce"] = fmt.Sprintf("%x", nonce)
	if prepare {
		request["cmd"] = "prepare_modern_sharing"
		prepared := sendModernSharingControl(t, d, request)
		var binding string
		if err := json.Unmarshal(prepared["sharing_binding"], &binding); err != nil || binding == "" {
			t.Fatal("prepare_modern_sharing did not return an opaque sharing_binding")
		}
		request["sharing_binding"] = binding
	}
	request["cmd"] = "spawn"
	response := sendModernSharingControl(t, d, request)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var spawned control.Response
	if err := json.Unmarshal(encoded, &spawned); err != nil {
		t.Fatal(err)
	}
	if spawned.ProtocolEra != "2026-07-28" || spawned.Token == "" || spawned.ServerID == "" || spawned.IPCPath == "" {
		t.Fatal("modern spawn omitted its era or host connection identity")
	}
	return spawned
}

func sendModernSharingControl(t *testing.T, d *Daemon, request map[string]any) map[string]json.RawMessage {
	t.Helper()
	conn, err := ipc.Dial(d.ctlSrv.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err := json.Unmarshal(response["ok"], &ok); err != nil || !ok {
		t.Fatalf("%s rejected before modern shared admission: %s", request["cmd"], response["message"])
	}
	return response
}

func modernSharingHostPID(t *testing.T, host *ipcConn) int {
	t.Helper()
	daemonSendReq(t, host, 1, "ping", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`)
	response := daemonReadResp(t, host)
	daemonAssertID(t, response, 1)
	var decoded struct {
		Result struct {
			PID int `json:"pid"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Result.PID <= 0 || decoded.Result.PID == os.Getpid() {
		t.Fatalf("host did not reach a real upstream child: %s", response)
	}
	return decoded.Result.PID
}
