package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const coldStartDaemonFlag = "-test.run=^TestEngineColdStartDaemonProcess$"

func TestEngineColdStartConcurrentClientsShareWinningDaemon(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern_%t", modern), func(t *testing.T) {
			root := coldStartFixture(t, modern)
			cfg := coldStartConfig(root)
			ctlPath := serverid.DaemonControlPath(root, cfg.Namespace)
			t.Cleanup(func() {
				coldStartWrite(t, filepath.Join(root, "release-winner"), nil)
				starts := coldStartDaemonStarts(t, root)
				if len(starts) == 0 {
					return
				}
				_, _ = control.SendWithTimeout(ctlPath, control.Request{Cmd: "shutdown"}, 5*time.Second)
				for _, name := range starts {
					coldStartWaitFile(t, filepath.Join(root, strings.TrimSuffix(name, ".started")+".done"), 45*time.Second)
				}
			})

			winner := coldStartLaunchClient(t, root, "winner", false)
			coldStartOpenClient(t, winner, "winner", modern)
			coldStartWaitFile(t, filepath.Join(root, "locked-winner"), 5*time.Second)
			loser := coldStartLaunchClient(t, root, "loser", false)
			coldStartOpenClient(t, loser, "loser", modern)
			coldStartWaitFile(t, filepath.Join(root, "contended-loser"), 5*time.Second)
			if starts := coldStartDaemonStarts(t, root); len(starts) != 0 {
				t.Fatalf("daemon started before the winning namespace lock was released to its startup path: %v", starts)
			}
			coldStartWrite(t, filepath.Join(root, "release-winner"), nil)

			clients := []*coldStartClient{winner, loser}
			roles := []string{"winner", "loser"}
			if !modern {
				for i, client := range clients {
					reply := client.read(t)
					if reply.Error != nil || string(reply.ID) != strconv.Quote("init-"+roles[i]) {
						t.Fatalf("legacy initialize lost its original ID: %+v", reply)
					}
					client.write(t, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`+"\n"+coldStartRequest(roles[i], false)+"\n")
				}
			}
			if err := waitForDaemon(ctlPath, daemonStartupTimeout); err != nil {
				t.Fatalf("winning daemon did not become control-ping ready: %v", err)
			}
			identity, err := daemonIdentityFromStatus(ctlPath)
			if err != nil || identity.pid <= 0 || identity.generation == "" {
				t.Fatalf("actual winning daemon identity: %+v %v", identity, err)
			}
			for i, client := range clients {
				reply := client.read(t)
				wantID, wantValue := "701", 5
				if roles[i] == "loser" {
					wantID, wantValue = `"loser-original"`, 24
				}
				if reply.JSONRPC != "2.0" || reply.Error != nil || string(reply.ID) != wantID || reply.Result.Value != wantValue || reply.Result.PID != identity.pid {
					t.Fatalf("%s arithmetic response did not come from the one winning daemon with its original ID: %+v; daemon=%+v", roles[i], reply, identity)
				}
				raw, err := os.ReadFile(filepath.Join(root, "request-"+roles[i]))
				if err != nil || !bytes.Equal(raw, []byte(coldStartRequest(roles[i], modern))) {
					t.Fatalf("%s original request changed before real handler dispatch: %s %v", roles[i], raw, err)
				}
			}
			ping, err := control.Send(ctlPath, control.Request{Cmd: "ping"})
			if err != nil || !ping.OK {
				t.Fatalf("winning control endpoint is not ping-ready: %+v %v", ping, err)
			}
			after, err := daemonIdentityFromStatus(ctlPath)
			if err != nil || !after.same(identity) {
				t.Fatalf("loser replaced/rebound the winning daemon: before=%+v after=%+v err=%v", identity, after, err)
			}
			status, err := control.Send(ctlPath, control.Request{Cmd: "status"})
			if err != nil || !status.OK {
				t.Fatalf("winning daemon status: %+v %v", status, err)
			}
			var data struct {
				Servers []struct {
					Sessions      []int  `json:"sessions"`
					SessionCount  int    `json:"session_count"`
					ProtocolEra   string `json:"protocol_era"`
					SharingPolicy string `json:"sharing_policy"`
				} `json:"servers"`
			}
			if err := json.Unmarshal(status.Data, &data); err != nil {
				t.Fatal(err)
			}
			wantOwners, sessions := 1, 0
			if modern {
				wantOwners = 2
			}
			if len(data.Servers) != wantOwners {
				t.Fatalf("owner count = %d, want %d: %s", len(data.Servers), wantOwners, status.Data)
			}
			for _, entry := range data.Servers {
				sessions += entry.SessionCount
				if !modern && len(entry.Sessions) != entry.SessionCount {
					t.Fatalf("legacy session IDs/count disagree: %s", status.Data)
				}
				if modern && (entry.ProtocolEra != "2026-07-28" || entry.SharingPolicy != "forced-isolated") {
					t.Fatalf("modern cold start lost exact-era isolated admission: %s", status.Data)
				}
				if !modern && entry.ProtocolEra != "" {
					t.Fatalf("zero-value legacy cold start selected another era: %s", status.Data)
				}
			}
			if sessions != 2 {
				t.Fatalf("attached sessions = %d, want both real clients", sessions)
			}
			starts := coldStartDaemonStarts(t, root)
			if len(starts) != 1 || starts[0] != fmt.Sprintf("daemon-%d.started", identity.pid) {
				t.Fatalf("actual daemon process generations = %v, want only PID %d", starts, identity.pid)
			}
			if _, err := os.Stat(filepath.Join(root, "spawn-winner")); err != nil {
				t.Fatalf("winner did not use the existing detached spawn: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "spawn-loser")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("loser attempted a competing detached spawn: %v", err)
			}
		})
	}
}

func TestEngineColdStartOccupiedUnresponsiveNamespaceFailsClosed(t *testing.T) {
	root := coldStartFixture(t, false)
	cfg := coldStartConfig(root)
	lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath(root, cfg.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	listener, err := ipc.Listen(serverid.DaemonControlPath(root, cfg.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var pings atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request control.Request
				if json.NewDecoder(conn).Decode(&request) == nil && request.Cmd == "ping" {
					pings.Add(1)
				}
				// A bound endpoint that cannot answer control ping is not ready.
			}()
		}
	}()
	client := coldStartLaunchClient(t, root, "loser", false)
	client.expectFailure = true
	started := time.Now()
	coldStartOpenClient(t, client, "loser", false)
	coldStartWaitFile(t, filepath.Join(root, "contended-loser"), 5*time.Second)
	client.wait(t, daemonStartupTimeout+5*time.Second)
	if client.err == nil {
		t.Fatal("occupied namespace without a ping-ready winner admitted the client")
	}
	failure, err := os.ReadFile(filepath.Join(root, "run-error-loser"))
	if err != nil || !strings.Contains(string(failure), "daemon did not start within 10s") {
		t.Fatalf("contention did not reach the existing bounded readiness error: %s %v", failure, err)
	}
	if elapsed := time.Since(started); elapsed < daemonStartupTimeout || elapsed > daemonStartupTimeout+5*time.Second {
		t.Fatalf("readiness wait = %s, want existing %s budget with process allowance", elapsed, daemonStartupTimeout)
	}
	if pings.Load() < 2 {
		t.Fatalf("bound endpoint bypassed actual control readiness rechecks: %d pings", pings.Load())
	}
	if _, err := os.Stat(filepath.Join(root, "spawn-loser")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unresponsive winner triggered a competing spawn: %v", err)
	}
	if replacement, err := ipc.AcquireFileLock(serverid.DaemonLockPath(root, cfg.Namespace)); !errors.Is(err, ipc.ErrFileLocked) {
		if replacement != nil {
			_ = replacement.Close()
		}
		t.Fatalf("loser released or bypassed the occupied namespace lock: %v", err)
	}
}

func TestEngineColdStartMaintenanceContentionPreservesHeldRefusal(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern_%t", modern), func(t *testing.T) {
			root := coldStartFixture(t, modern)
			cfg := coldStartConfig(root)
			ctlPath := serverid.DaemonControlPath(root, cfg.Namespace)
			d, err := daemon.New(daemon.Config{ControlPath: ctlPath, Namespace: cfg.Namespace, SkipSnapshot: true, SessionHandler: cfg.SessionHandler, Logger: cfg.Logger})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(d.Shutdown)
			protocol, _ := protocolEraForPolicy(cfg.ProtocolPolicy)
			wire, _ := protocol.Wire()
			request := control.Request{Command: cfg.Command, Cwd: root, Mode: "global", Env: collectEnv(), ProtocolEra: wire}
			_, sid, _, err := d.Spawn(request)
			if err != nil {
				t.Fatal(err)
			}
			hold, err := control.SendMaintenance(ctlPath, control.Request{Cmd: "hold", ServerID: sid}, 5*time.Second)
			if err != nil || hold.State != control.MaintenanceHeld || !hold.TreesRetired {
				t.Fatalf("real held fixture did not settle retirement: %+v %v", hold, err)
			}
			t.Cleanup(func() {
				if _, err := control.SendMaintenance(ctlPath, control.Request{Cmd: "resume", HoldID: hold.HoldID}, 5*time.Second); err != nil {
					t.Errorf("release exact private fixture hold: %v", err)
				}
			})
			lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath(root, cfg.Namespace))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lock.Close() })
			if err := daemon.CheckMaintenanceForActivation(cfg.Namespace, ctlPath); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("private fixture lacks its durable activation fence: %v", err)
			}
			client := coldStartLaunchClient(t, root, "loser", true)
			client.write(t, coldStartRequest("loser", modern)+"\n")
			coldStartWaitFile(t, filepath.Join(root, "contended-loser"), 5*time.Second)
			reply := client.read(t)
			if string(reply.ID) != `"loser-original"` || reply.Error == nil || reply.Error.Code != -32005 || reply.Error.Data.ErrorCode != "maintenance_held" {
				t.Fatalf("ready winner's real HELD refusal lost its type or original request ID: %+v", reply)
			}
			request.Cmd = "spawn"
			response, err := control.Send(ctlPath, request)
			if err != nil || !errors.Is(response.Err(), control.ErrMaintenanceHeld) {
				t.Fatalf("held control admission opened after contention wait: %+v %v", response, err)
			}
			if err := daemon.CheckMaintenanceForActivation(cfg.Namespace, ctlPath); !errors.Is(err, control.ErrMaintenanceHeld) {
				t.Fatalf("readiness wait bypassed or altered the durable activation fence: %v", err)
			}
			for _, path := range []string{"spawn-loser", "request-loser"} {
				if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("held loser crossed startup/handler authority at %s: %v", path, err)
				}
			}
		})
	}
}

func TestEngineColdStartNonContentionLockErrorIsTerminal(t *testing.T) {
	root := coldStartFixture(t, false)
	cfg := coldStartConfig(root)
	cfg.BaseDir = filepath.Join(root, "missing-lock-directory")
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.startDaemon(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-contention lock error was suppressed: %v", err)
	}
	if starts := coldStartDaemonStarts(t, root); len(starts) != 0 {
		t.Fatalf("lock I/O failure started a daemon: %v", starts)
	}
}

func TestEngineColdStartClientProcess(t *testing.T) {
	root, role := os.Getenv("MCP_MUX_COLDSTART_ROOT"), os.Getenv("MCP_MUX_COLDSTART_ROLE")
	if root == "" || (role != "winner" && role != "loser") {
		return
	}
	eng, err := New(coldStartConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	acquire := engineAcquireDaemonLock
	engineAcquireDaemonLock = func(path string) (daemonLock, error) {
		lock, err := acquire(path)
		if errors.Is(err, ipc.ErrFileLocked) {
			coldStartWrite(t, filepath.Join(root, "contended-"+role), []byte(err.Error()))
		}
		if err == nil && role == "winner" {
			coldStartWrite(t, filepath.Join(root, "locked-winner"), nil)
			coldStartWaitFile(t, filepath.Join(root, "release-winner"), 30*time.Second)
		}
		return lock, err
	}
	start := engineStartDaemonExecutable
	engineStartDaemonExecutable = func(exe, flag string) error {
		coldStartWrite(t, filepath.Join(root, "spawn-"+role), []byte(strconv.Itoa(os.Getpid())))
		return start(exe, flag)
	}
	closeInput := time.AfterFunc(30*time.Second, func() { _ = os.Stdin.Close() })
	defer closeInput.Stop()
	if os.Getenv("MCP_MUX_COLDSTART_FORCE_START") == "true" {
		err = eng.startDaemon()
	}
	if err == nil {
		err = eng.Run(context.Background())
	}
	if err != nil {
		coldStartWrite(t, filepath.Join(root, "run-error-"+role), []byte(err.Error()))
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestEngineColdStartDaemonProcess(t *testing.T) {
	root := os.Getenv("MCP_MUX_COLDSTART_ROOT")
	if root == "" {
		return
	}
	name := fmt.Sprintf("daemon-%d", os.Getpid())
	coldStartWrite(t, filepath.Join(root, name+".started"), nil)
	defer coldStartWrite(t, filepath.Join(root, name+".done"), nil)
	eng, err := New(coldStartConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := eng.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func coldStartFixture(t *testing.T, modern bool) string {
	t.Helper()
	scratch := os.Getenv("GOTMPDIR")
	canonical := strings.ToLower(filepath.ToSlash(filepath.Clean(scratch)))
	if !filepath.IsAbs(scratch) || !strings.Contains(canonical, "/.agent/") || strings.Contains(canonical, "/.agent/worktrees/") {
		t.Skip("requires parent-supplied absolute GOTMPDIR beneath primary .agent (not a linked worktree)")
	}
	root, err := os.MkdirTemp(scratch, "cs-")
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME", "USERPROFILE"} {
		t.Setenv(key, config)
	}
	for _, key := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(key, root)
	}
	for _, key := range []string{"MCP_MUX_SESSION_ID", "MCP_MUX_ISOLATED", "MCP_MUX_STATELESS"} {
		t.Setenv(key, "")
	}
	t.Setenv("MCP_MUX_DEFAULT_MODE", "global")
	t.Setenv("MCP_MUX_COLDSTART_ROOT", root)
	t.Setenv("MCP_MUX_COLDSTART_ROLE", "")
	t.Setenv("MCP_MUX_COLDSTART_FORCE_START", "")
	t.Setenv("MCP_MUX_COLDSTART_MODERN", strconv.FormatBool(modern))
	t.Logf("private cold-start evidence: %s; runtime.GOOS=%s", root, runtime.GOOS)
	return root
}

func coldStartConfig(root string) Config {
	cfg := Config{
		Name:                    "coldstart-regression",
		Namespace:               filepath.Base(root),
		Command:                 "coldstart-arithmetic-handler",
		SessionHandler:          coldStartArithmetic{root: root},
		BaseDir:                 root,
		DaemonFlag:              coldStartDaemonFlag,
		SkipSnapshot:            true,
		Persistent:              true,
		IdleTimeout:             time.Minute,
		ZeroSessionCleanupDelay: -1,
		Logger:                  log.New(os.Stderr, "[coldstart] ", 0),
	}
	if os.Getenv("MCP_MUX_COLDSTART_MODERN") == "true" {
		cfg.ProtocolPolicy = era.PolicyModern20260728
	}
	return cfg
}

func coldStartRequest(role string, modern bool) string {
	id, a, b := "701", 2, 3
	if role == "loser" {
		id, a, b = `"loser-original"`, 11, 13
	}
	meta := ""
	if modern {
		meta = `,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"add","arguments":{"a":%d,"b":%d,"probe":%q}%s}}`, id, a, b, role, meta)
}

type coldStartArithmetic struct{ root string }

func (h coldStartArithmetic) HandleRequest(_ context.Context, _ muxcore.ProjectContext, raw []byte) ([]byte, error) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				A     int    `json:"a"`
				B     int    `json:"b"`
				Probe string `json:"probe"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	var result any
	switch request.Method {
	case "initialize":
		result = json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"coldstart-arithmetic","version":"1"}}`)
	case "tools/call":
		args := request.Params.Arguments
		if request.Params.Name != "add" || (args.Probe != "winner" && args.Probe != "loser") {
			return nil, errors.New("invalid private arithmetic operation")
		}
		if err := os.WriteFile(filepath.Join(h.root, "request-"+args.Probe), raw, 0o600); err != nil {
			return nil, err
		}
		result = map[string]any{"value": args.A + args.B, "pid": os.Getpid(), "content": []map[string]string{{"type": "text", "text": strconv.Itoa(args.A + args.B)}}}
	default:
		return nil, fmt.Errorf("unsupported private arithmetic method %q", request.Method)
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", request.ID, result})
}

type coldStartReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  struct {
		Value int `json:"value"`
		PID   int `json:"pid"`
	} `json:"result"`
	Error *struct {
		Code int `json:"code"`
		Data struct {
			ErrorCode string `json:"error_code"`
		} `json:"data"`
	} `json:"error"`
}

type coldStartClient struct {
	root          string
	role          string
	input         io.WriteCloser
	output        *json.Decoder
	stderr        bytes.Buffer
	done          chan struct{}
	err           error
	expectFailure bool
}

func coldStartLaunchClient(t *testing.T, root, role string, forceStart bool) *coldStartClient {
	t.Helper()
	client := &coldStartClient{root: root, role: role, done: make(chan struct{})}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEngineColdStartClientProcess$")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "MCP_MUX_COLDSTART_ROLE="+role, "MCP_MUX_COLDSTART_FORCE_START="+strconv.FormatBool(forceStart))
	cmd.Stderr = &client.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	client.input, client.output = stdin, json.NewDecoder(stdout)
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		t.Fatal(err)
	}
	go func() {
		client.err = cmd.Wait()
		close(client.done)
	}()
	t.Cleanup(func() {
		_ = client.input.Close()
		client.wait(t, 45*time.Second)
	})
	return client
}

func coldStartOpenClient(t *testing.T, client *coldStartClient, role string, modern bool) {
	t.Helper()
	if modern {
		client.write(t, coldStartRequest(role, true)+"\n")
		return
	}
	client.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"coldstart-%s","version":"1"}}}`+"\n", "init-"+role, role))
}

func (client *coldStartClient) write(t *testing.T, input string) {
	t.Helper()
	if _, err := io.WriteString(client.input, input); err != nil {
		t.Fatalf("%s client stdin: %v", client.role, err)
	}
}

func (client *coldStartClient) read(t *testing.T) coldStartReply {
	t.Helper()
	var reply coldStartReply
	done := make(chan error, 1)
	go func() { done <- client.output.Decode(&reply) }()
	select {
	case err := <-done:
		if err != nil {
			failure, _ := os.ReadFile(filepath.Join(client.root, "run-error-"+client.role))
			t.Fatalf("%s real New/Run response: %v; startup error: %s", client.role, err, failure)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s real New/Run produced no response", client.role)
	}
	return reply
}

func (client *coldStartClient) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-client.done:
		if client.err != nil {
			t.Logf("%s helper exited: %v; stderr: %s", client.role, client.err, client.stderr.String())
			if !client.expectFailure {
				t.Errorf("%s real New/Run failed: %v", client.role, client.err)
			}
		}
	case <-time.After(timeout):
		t.Fatalf("%s private helper did not exit naturally within %s", client.role, timeout)
	}
}

func coldStartWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func coldStartWaitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("private startup marker did not appear within %s: %s", timeout, path)
}

func coldStartDaemonStarts(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var starts []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "daemon-") && strings.HasSuffix(entry.Name(), ".started") {
			starts = append(starts, entry.Name())
		}
	}
	return starts
}
