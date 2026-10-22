package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestModernSharingDaemon_RejectsInvalidBindingBeforeOwnerCreation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, map[string]any)
	}{
		{"missing binding", func(_ *testing.T, request map[string]any) { delete(request, "sharing_binding") }},
		{"empty binding", func(_ *testing.T, request map[string]any) { request["sharing_binding"] = "" }},
		{"malformed binding", func(_ *testing.T, request map[string]any) { request["sharing_binding"] = "not-a-binding" }},
		{"tampered binding", func(_ *testing.T, request map[string]any) {
			binding := []byte(request["sharing_binding"].(string))
			index := len(binding) / 2
			if binding[index] == 'a' {
				binding[index] = 'b'
			} else {
				binding[index] = 'a'
			}
			request["sharing_binding"] = string(binding)
		}},
		{"missing nonce", func(_ *testing.T, request map[string]any) { delete(request, "spawn_nonce") }},
		{"changed nonce", func(_ *testing.T, request map[string]any) {
			nonce := []byte(request["spawn_nonce"].(string))
			if nonce[0] == '0' {
				nonce[0] = '1'
			} else {
				nonce[0] = '0'
			}
			request["spawn_nonce"] = string(nonce)
		}},
		{"changed command", func(t *testing.T, request map[string]any) {
			command, err := exec.LookPath("go")
			if err != nil {
				t.Fatal(err)
			}
			request["command"] = command
		}},
		{"reordered args", func(_ *testing.T, request map[string]any) {
			args := append([]string(nil), request["args"].([]string)...)
			args[0], args[1] = args[1], args[0]
			request["args"] = args
		}},
		{"changed cwd", func(t *testing.T, request map[string]any) { request["cwd"] = t.TempDir() }},
		{"changed authorization", func(_ *testing.T, request map[string]any) {
			request["env"].(map[string]string)["AUTHORIZATION"] = "changed-authorization-fixture"
		}},
		{"changed transient env", func(_ *testing.T, request map[string]any) {
			request["env"].(map[string]string)["CLAUDE_CODE_ENTRYPOINT"] = "changed-entrypoint-fixture"
		}},
		{"changed arbitrary env", func(_ *testing.T, request map[string]any) {
			request["env"].(map[string]string)["R2_BOUNDARY_CUSTOM_ENV"] = "changed-env-fixture"
		}},
		{"removed env", func(_ *testing.T, request map[string]any) {
			delete(request["env"].(map[string]string), "R2_BOUNDARY_CUSTOM_ENV")
		}},
		{"changed partition", func(_ *testing.T, request map[string]any) {
			request["partition"] = map[string]any{"known": true, "key": "other-partition-fixture"}
		}},
		{"changed era", func(_ *testing.T, request map[string]any) { request["protocol_era"] = "" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			launch := modernSharingLaunch(t)
			launch["env"].(map[string]string)["R2_BOUNDARY_CUSTOM_ENV"] = "initial-env-fixture"
			launch["env"].(map[string]string)["AUTHORIZATION"] = "initial-authorization-fixture"
			launch["env"].(map[string]string)["CLAUDE_CODE_ENTRYPOINT"] = "initial-entrypoint-fixture"
			d := testDaemon(t)
			request := prepareModernSharingBoundary(t, d, launch)
			if got := d.OwnerCount(); got != 0 {
				t.Fatalf("prepare created %d owners before spawn", got)
			}
			testCase.mutate(t, request)
			assertModernSharingBoundaryRejected(t, d, request)
		})
	}
}

func TestModernSharingDaemon_BindingCannotCrossDaemonInstances(t *testing.T) {
	launch := modernSharingLaunch(t)
	issuingDaemon := testDaemon(t)
	otherDaemon := testDaemon(t)
	request := prepareModernSharingBoundary(t, issuingDaemon, launch)
	assertModernSharingBoundaryRejected(t, otherDaemon, request)
	if got := issuingDaemon.OwnerCount(); got != 0 {
		t.Fatalf("prepare created %d owners on the issuing daemon", got)
	}
}

func TestModernSharingDaemon_ExpiredBindingCreatesNoOwner(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	originalNow := modernSharingNow
	modernSharingNow = func() time.Time { return time.Unix(0, clock.Load()) }
	t.Cleanup(func() { modernSharingNow = originalNow })

	launch := modernSharingLaunch(t)
	d := testDaemon(t)
	request := prepareModernSharingBoundary(t, d, launch)
	clock.Add(int64(modernSharingBindingTTL + time.Second))
	assertModernSharingBoundaryRejected(t, d, request)
}

func TestModernSharingDaemon_UnknownPartitionWithKeyIsInvalid(t *testing.T) {
	for _, command := range []string{"prepare_modern_sharing", "spawn"} {
		t.Run(command, func(t *testing.T) {
			request := modernSharingLaunch(t)
			request["cmd"] = command
			request["spawn_nonce"] = strings.Repeat("1", 32)
			request["partition"] = map[string]any{"known": false, "key": "inconsistent-partition-fixture"}
			d := testDaemon(t)
			assertModernSharingBoundaryRejected(t, d, request)
		})
	}
}

func TestModernSharingDaemon_ConcurrentFirstAdmissionsUseOneOwnerAndUpstream(t *testing.T) {
	const count = 8
	launch := modernSharingLaunch(t)
	d := testDaemon(t)
	requests := make([]map[string]any, count)
	for i := range count {
		requests[i] = prepareModernSharingBoundary(t, d, launch)
	}
	if got := d.OwnerCount(); got != 0 {
		t.Fatalf("prepare created %d owners before the admission race", got)
	}

	responses := make([]control.Response, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range count {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			responses[i], errs[i] = roundTripModernSharingBoundary(d, requests[i])
		}(i)
	}
	close(start)
	workers.Wait()

	tokens := make(map[string]bool, count)
	for i, response := range responses {
		if errs[i] != nil || !response.OK {
			t.Fatalf("concurrent admission %d failed: transport=%v accepted=%t", i, errs[i], response.OK)
		}
		if response.ServerID == "" || response.IPCPath == "" || response.ProtocolEra != "2026-07-28" {
			t.Fatalf("concurrent admission %d omitted its modern connection identity", i)
		}
		if response.ServerID != responses[0].ServerID || response.IPCPath != responses[0].IPCPath {
			t.Fatalf("concurrent admission %d created a competing owner", i)
		}
		if response.Token == "" || tokens[response.Token] {
			t.Fatalf("concurrent admission %d reused a host handshake token", i)
		}
		tokens[response.Token] = true
	}
	if got := d.OwnerCount(); got != 1 {
		t.Fatalf("concurrent admissions created %d owners, want 1", got)
	}

	var upstreamPID int
	for i, response := range responses {
		host := dialIPC(t, response.IPCPath, response.Token)
		t.Cleanup(func() { _ = host.conn.Close() })
		pid := modernSharingHostPID(t, host)
		if i == 0 {
			upstreamPID = pid
		} else if pid != upstreamPID {
			t.Fatalf("concurrent admission %d reached a competing upstream process", i)
		}
	}
	entry := d.Entry(responses[0].ServerID)
	if entry == nil || entry.Owner == nil {
		t.Fatal("shared owner disappeared after host attachment")
	}
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == count }, "not every concurrent host was admitted to the shared owner")
}

func TestModernSharingDaemon_PartitionAndCwdChangesNeedDistinctOwners(t *testing.T) {
	for _, boundary := range []string{"partition", "cwd"} {
		t.Run(boundary, func(t *testing.T) {
			launchA := modernSharingLaunch(t)
			launchB := cloneModernSharingBoundaryLaunch(launchA)
			if boundary == "partition" {
				launchB["partition"] = map[string]any{"known": true, "key": "other-partition-fixture"}
			} else {
				launchB["cwd"] = t.TempDir()
			}
			d := testDaemon(t)
			a := spawnModernSharing(t, d, launchA, true)
			b := spawnModernSharing(t, d, launchB, true)
			if a.ServerID == b.ServerID || a.IPCPath == b.IPCPath || d.OwnerCount() != 2 {
				t.Fatalf("different %s crossed the owner boundary: owners=%d", boundary, d.OwnerCount())
			}
			hostA := dialIPC(t, a.IPCPath, a.Token)
			t.Cleanup(func() { _ = hostA.conn.Close() })
			hostB := dialIPC(t, b.IPCPath, b.Token)
			t.Cleanup(func() { _ = hostB.conn.Close() })
			if modernSharingHostPID(t, hostA) == modernSharingHostPID(t, hostB) {
				t.Fatalf("different %s reached one upstream process", boundary)
			}
		})
	}
}

func TestModernSharingDaemon_PublicStatusOmitsAdmissionSecrets(t *testing.T) {
	const partitionKey = "private-r2-partition-fixture"
	const envKey = "R2_PRIVATE_ENV_FIXTURE"
	const envValue = "private-r2-env-value-fixture"
	launch := modernSharingLaunch(t)
	launch["partition"] = map[string]any{"known": true, "key": partitionKey}
	launch["env"].(map[string]string)[envKey] = envValue
	d := testDaemon(t)
	request := prepareModernSharingBoundary(t, d, launch)
	spawned, err := roundTripModernSharingBoundary(d, request)
	if err != nil || !spawned.OK || spawned.ServerID == "" {
		t.Fatalf("shared status fixture admission failed: transport=%v accepted=%t", err, spawned.OK)
	}
	host := dialIPC(t, spawned.IPCPath, spawned.Token)
	t.Cleanup(func() { _ = host.conn.Close() })
	_ = modernSharingHostPID(t, host)

	for _, command := range []string{"status", "list_owners"} {
		t.Run(command, func(t *testing.T) {
			response := sendModernSharingControl(t, d, map[string]any{"cmd": command})
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			text := string(encoded)
			if !strings.Contains(text, spawned.ServerID) {
				t.Fatal("public status did not include the admitted owner")
			}
			for _, secret := range []string{partitionKey, envKey, envValue, request["sharing_binding"].(string), request["spawn_nonce"].(string), hex.EncodeToString(d.modernSharingKey[:])} {
				if strings.Contains(text, secret) {
					t.Fatal("public status disclosed private admission material")
				}
			}
			for _, field := range []string{"partition", "partition_key", "partition_digest", "sharing_binding", "spawn_nonce", "compatibility", "compatibility_digest", "env", "env_digest"} {
				if strings.Contains(text, `"`+field+`":`) {
					t.Fatalf("public status exposed private admission field %q", field)
				}
			}
		})
	}
}

func TestModernSharingDaemon_InvalidBindingCannotReuseExistingOwner(t *testing.T) {
	for _, boundary := range []string{"missing binding", "changed partition"} {
		t.Run(boundary, func(t *testing.T) {
			launch := modernSharingLaunch(t)
			d := testDaemon(t)
			admitted := spawnModernSharing(t, d, launch, true)
			host := dialIPC(t, admitted.IPCPath, admitted.Token)
			t.Cleanup(func() { _ = host.conn.Close() })
			_ = modernSharingHostPID(t, host)
			request := prepareModernSharingBoundary(t, d, launch)
			if boundary == "missing binding" {
				delete(request, "sharing_binding")
			} else {
				otherLaunch := cloneModernSharingBoundaryLaunch(launch)
				otherLaunch["partition"] = map[string]any{"known": true, "key": "other-partition-fixture"}
				request = prepareModernSharingBoundary(t, d, otherLaunch)
				request["partition"] = launch["partition"]
			}
			response, err := roundTripModernSharingBoundary(d, request)
			if err != nil {
				t.Fatal(err)
			}
			if response.OK || response.Message == "" || response.Token != "" || response.IPCPath != "" || response.ServerID != "" {
				t.Fatal("invalid binding admitted a host to an existing shared owner")
			}
			fresh := spawnModernSharing(t, d, launch, true)
			if d.OwnerCount() != 1 || fresh.ServerID != admitted.ServerID || fresh.IPCPath != admitted.IPCPath {
				t.Fatal("rejected admission disturbed the existing compatible owner")
			}
		})
	}
}

func TestModernSharingDaemon_SharedRefreshRequiresFreshAdmission(t *testing.T) {
	launch := modernSharingLaunch(t)
	d := testDaemon(t)
	admitted := spawnModernSharing(t, d, launch, true)
	host := dialIPC(t, admitted.IPCPath, admitted.Token)
	t.Cleanup(func() { _ = host.conn.Close() })
	_ = modernSharingHostPID(t, host)
	for _, protocolEra := range []string{"", "2026-07-28"} {
		response, err := roundTripModernSharingBoundary(d, map[string]any{
			"cmd": "refresh-token", "prev_token": admitted.Token, "protocol_era": protocolEra,
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.OK || response.Message == "" || response.Token != "" {
			t.Fatal("raw token refresh bypassed fresh shared-owner admission")
		}
	}
	fresh := spawnModernSharing(t, d, launch, true)
	if d.OwnerCount() != 1 || fresh.ServerID != admitted.ServerID || fresh.Token == admitted.Token {
		t.Fatal("fresh bound admission did not safely reuse the shared owner after refresh refusal")
	}
}

func TestModernSharingDaemon_LegacyCwdDedupCannotReuseModernSharedOwner(t *testing.T) {
	launch := modernSharingLaunch(t)
	d := testDaemon(t)
	modern := spawnModernSharing(t, d, launch, true)
	modernHost := dialIPC(t, modern.IPCPath, modern.Token)
	t.Cleanup(func() { _ = modernHost.conn.Close() })
	modernPID := modernSharingHostPID(t, modernHost)
	legacyRequest := cloneModernSharingBoundaryLaunch(launch)
	legacyRequest["mode"] = "cwd"
	delete(legacyRequest, "protocol_era")
	delete(legacyRequest, "modern_sharing")
	delete(legacyRequest, "partition")
	legacy, err := roundTripModernSharingBoundary(d, legacyRequest)
	if err != nil || !legacy.OK || legacy.Token == "" || legacy.IPCPath == "" || legacy.ProtocolEra != "" {
		t.Fatalf("legacy control admission failed: transport=%v accepted=%t", err, legacy.OK)
	}
	if legacy.ServerID == modern.ServerID || legacy.IPCPath == modern.IPCPath || d.OwnerCount() != 2 {
		t.Fatal("legacy cwd dedup attached to the admitted modern shared owner")
	}
	legacyHost := dialIPC(t, legacy.IPCPath, legacy.Token)
	t.Cleanup(func() { _ = legacyHost.conn.Close() })
	if modernSharingHostPID(t, legacyHost) == modernPID {
		t.Fatal("legacy cwd admission reached the modern upstream process")
	}
}

func cloneModernSharingBoundaryLaunch(launch map[string]any) map[string]any {
	request := make(map[string]any, len(launch)+2)
	for key, value := range launch {
		request[key] = value
	}
	return request
}

func prepareModernSharingBoundary(t *testing.T, d *Daemon, launch map[string]any) map[string]any {
	t.Helper()
	request := cloneModernSharingBoundaryLaunch(launch)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	request["spawn_nonce"] = hex.EncodeToString(nonce[:])
	request["cmd"] = "prepare_modern_sharing"
	response := sendModernSharingControl(t, d, request)
	var binding string
	if err := json.Unmarshal(response["sharing_binding"], &binding); err != nil || binding == "" {
		t.Fatal("prepare_modern_sharing did not return an opaque sharing_binding")
	}
	request["sharing_binding"] = binding
	request["cmd"] = "spawn"
	return request
}

func roundTripModernSharingBoundary(d *Daemon, request map[string]any) (control.Response, error) {
	conn, err := ipc.Dial(d.ctlSrv.SocketPath())
	if err != nil {
		return control.Response{}, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return control.Response{}, err
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return control.Response{}, err
	}
	var response control.Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return control.Response{}, fmt.Errorf("decode control response: %w", err)
	}
	return response, nil
}

func assertModernSharingBoundaryRejected(t *testing.T, d *Daemon, request map[string]any) {
	t.Helper()
	response, err := roundTripModernSharingBoundary(d, request)
	if err != nil {
		t.Fatalf("control transport failed instead of returning an admission refusal: %v", err)
	}
	if response.OK || response.Message == "" || response.ServerID != "" || response.IPCPath != "" || response.Token != "" {
		t.Fatalf("invalid admission did not fail closed: accepted=%t connection_identity=%t", response.OK, response.ServerID != "" || response.IPCPath != "" || response.Token != "")
	}
	if got := d.OwnerCount(); got != 0 {
		t.Fatalf("rejected admission created %d owners", got)
	}
}
