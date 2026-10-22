package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const modernSharingRefreshHelperEnv = "MCP_MUX_ENGINE_REFRESH_HELPER"

type modernSharingRefreshAppState struct {
	PID     int `json:"pid"`
	Counter int `json:"counter"`
}

// This follows the existing daemon self-executable fixture, with process-local state.
func TestModernSharingRefreshUpstreamHelper(t *testing.T) {
	if os.Getenv(modernSharingRefreshHelperEnv) != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	counter := 0
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		if request.Method != "refresh/state" || len(request.ID) == 0 {
			t.Fatal("unexpected request to refresh upstream fixture")
		}
		counter++
		if err := encoder.Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": modernSharingRefreshAppState{PID: os.Getpid(), Counter: counter},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestModernSharingEngine_RefreshUnknownPartitionPreservesIsolatedOwnerAndState(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		resolver func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error)
	}{
		{name: "absent resolver"},
		{name: "unknown resolver", resolver: func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error) {
			return control.PartitionAssertion{}, nil
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			d, bridge, _ := captureModernSharingRefreshClient(t, testCase.resolver)
			host := dialModernSharingRefreshHost(t, bridge.InitialIPCPath, bridge.Token)
			initial := readModernSharingRefreshState(t, host, 1)
			owners := listModernSharingRefreshOwners(t, d)
			if len(owners) != 1 || owners[0].SharingPolicy != "forced-isolated" || owners[0].UpstreamPID != initial.PID {
				t.Fatal("unknown partition did not establish one real isolated upstream")
			}
			initialOwnerID := owners[0].ServerID
			previousToken := bridge.Token
			for round := 1; round <= 2; round++ {
				path, token, err := bridge.RefreshToken()
				if err != nil {
					t.Fatalf("refresh of the admitted isolated owner failed: %v", err)
				}
				if token == "" || token == previousToken {
					t.Fatal("refresh did not mint a fresh host token")
				}
				refreshedHost := dialModernSharingRefreshHost(t, path, token)
				state := readModernSharingRefreshState(t, refreshedHost, round+1)
				owners = listModernSharingRefreshOwners(t, d)
				if d.OwnerCount() != 1 || len(owners) != 1 || owners[0].ServerID != initialOwnerID ||
					path != bridge.InitialIPCPath || state.PID != initial.PID || state.Counter != initial.Counter+round {
					t.Fatalf("isolated refresh replaced application authority: round=%d owners=%d old_pid=%d new_pid=%d counter=%d want=%d", round, d.OwnerCount(), initial.PID, state.PID, state.Counter, initial.Counter+round)
				}
				if err := host.conn.Close(); err != nil {
					t.Fatal(err)
				}
				host = refreshedHost
				previousToken = token
			}
		})
	}
}

func TestModernSharingEngine_RefreshLostKnownPartitionCannotReuseSharedAuthority(t *testing.T) {
	const secret = "private-refresh-resolver-fixture"
	for _, transition := range []string{"unknown", "error", "panic"} {
		t.Run(transition, func(t *testing.T) {
			resolverCalls := 0
			resolver := func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error) {
				resolverCalls++
				if resolverCalls == 1 {
					return control.PartitionAssertion{Known: true, Key: secret}, nil
				}
				switch transition {
				case "error":
					return control.PartitionAssertion{}, errors.New(secret)
				case "panic":
					panic(secret)
				default:
					return control.PartitionAssertion{}, nil
				}
			}
			d, bridge, engineLog := captureModernSharingRefreshClient(t, resolver)
			host := dialModernSharingRefreshHost(t, bridge.InitialIPCPath, bridge.Token)
			initial := readModernSharingRefreshState(t, host, 1)
			owners := listModernSharingRefreshOwners(t, d)
			if len(owners) != 1 || owners[0].SharingPolicy != "shared-exact-context" || owners[0].UpstreamPID != initial.PID {
				t.Fatal("known partition did not establish the real shared upstream fixture")
			}
			initialOwnerID := owners[0].ServerID
			path, token, refreshErr := bridge.RefreshToken()
			if resolverCalls != 2 {
				t.Fatalf("refresh must resolve the current partition exactly once: resolver calls=%d, want 2 including initial admission", resolverCalls)
			}
			if strings.Contains(fmt.Sprint(refreshErr)+engineLog.String(), secret) {
				t.Fatal("refresh error or engine log disclosed private resolver material")
			}
			if transition != "unknown" && refreshErr == nil {
				t.Fatal("refresh accepted a failed partition resolver")
			}
			if refreshErr != nil {
				if path != "" || token != "" || d.OwnerCount() != 1 {
					t.Fatal("failed refresh exposed attachment authority or created another owner")
				}
			} else {
				// Unknown may fail closed or freshly isolate, but cannot inherit known shared authority.
				if path == "" || path == bridge.InitialIPCPath || token == "" || token == bridge.Token {
					t.Fatal("unknown partition reused the previous known shared owner's attachment authority")
				}
				isolatedHost := dialModernSharingRefreshHost(t, path, token)
				isolated := readModernSharingRefreshState(t, isolatedHost, 2)
				owners = listModernSharingRefreshOwners(t, d)
				if d.OwnerCount() != 2 || len(owners) != 2 || isolated.PID == initial.PID || isolated.Counter != 1 {
					t.Fatal("unknown partition did not receive a separate cold isolated upstream")
				}
				sharedCount, isolatedCount := 0, 0
				for _, info := range owners {
					if info.ServerID == initialOwnerID && info.SharingPolicy == "shared-exact-context" {
						sharedCount++
					}
					if info.ServerID != initialOwnerID && info.SharingPolicy == "forced-isolated" && info.UpstreamPID == isolated.PID {
						isolatedCount++
					}
				}
				if sharedCount != 1 || isolatedCount != 1 {
					t.Fatal("unknown refresh crossed or relabeled the existing shared partition boundary")
				}
			}
			retained := readModernSharingRefreshState(t, host, 3)
			if retained.PID != initial.PID || retained.Counter != initial.Counter+1 {
				t.Fatal("lost partition assertion disturbed the original shared upstream's application state")
			}
		})
	}
}

func captureModernSharingRefreshClient(t *testing.T, resolver func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error)) (*daemon.Daemon, owner.ResilientClientConfig, *bytes.Buffer) {
	t.Helper()
	t.Setenv("MCP_MUX_DEFAULT_MODE", "global")
	t.Setenv("MCP_MUX_STATELESS", "0")
	t.Setenv("MCP_MUX_ISOLATED", "0")
	t.Setenv(modernSharingRefreshHelperEnv, "1")
	baseDir := shortEngineBaseDir(t)
	const name = "r2refresh"
	d, err := daemon.New(daemon.Config{
		Name: name, Namespace: name,
		ControlPath:  serverid.DaemonControlPath(baseDir, name),
		SkipSnapshot: true, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	engineLog := &bytes.Buffer{}
	eng, err := New(Config{
		Name: name, Namespace: name, BaseDir: baseDir,
		Command: os.Args[0], Args: []string{"-test.run=^TestModernSharingRefreshUpstreamHelper$", "--"},
		ProtocolPolicy: era.PolicyModern20260728, ModernSharing: true,
		PartitionResolver: resolver, Logger: log.New(engineLog, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	redirectEngineClientStdio(t, `{"jsonrpc":"2.0","id":1,"method":"refresh/state","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
	stopAfterCapture := errors.New("capture callbacks after real daemon admission")
	var captured owner.ResilientClientConfig
	originalBridge := runResilientClient
	runResilientClient = func(cfg owner.ResilientClientConfig) error {
		captured = cfg
		return stopAfterCapture
	}
	t.Cleanup(func() { runResilientClient = originalBridge })
	if err := eng.runClient(context.Background()); !errors.Is(err, stopAfterCapture) {
		t.Fatalf("real client admission failed before callback capture: %v", err)
	}
	if captured.InitialError != nil || captured.InitialIPCPath == "" || captured.Token == "" || captured.RefreshToken == nil {
		t.Fatal("real client admission did not provide a usable refresh callback")
	}
	return d, captured, engineLog
}

type modernSharingRefreshHost struct {
	conn    net.Conn
	decoder *json.Decoder
}

func dialModernSharingRefreshHost(t *testing.T, path, token string) *modernSharingRefreshHost {
	t.Helper()
	conn, err := ipc.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(conn, token); err != nil {
		t.Fatal(err)
	}
	return &modernSharingRefreshHost{conn: conn, decoder: json.NewDecoder(conn)}
}

func readModernSharingRefreshState(t *testing.T, host *modernSharingRefreshHost, id int) modernSharingRefreshAppState {
	t.Helper()
	if err := host.conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(host.conn, `{"jsonrpc":"2.0","id":%d,"method":"refresh/state","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n", id); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     json.RawMessage              `json:"id"`
		Result modernSharingRefreshAppState `json:"result"`
		Error  json.RawMessage              `json:"error"`
	}
	if err := host.decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if string(response.ID) != fmt.Sprint(id) || len(response.Error) != 0 || response.Result.PID <= 0 || response.Result.PID == os.Getpid() || response.Result.Counter <= 0 {
		t.Fatal("refresh state request did not reach the real upstream child")
	}
	return response.Result
}

func listModernSharingRefreshOwners(t *testing.T, d *daemon.Daemon) []control.OwnerInfo {
	t.Helper()
	listing, err := d.HandleListOwners(control.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return listing.Owners
}
