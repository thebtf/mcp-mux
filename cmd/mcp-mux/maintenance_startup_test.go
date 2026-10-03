package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const (
	maintenanceStartupHelperEnv  = "MCPMUX_MAINTENANCE_STARTUP_HELPER"
	maintenanceStartupEffectsEnv = "MCPMUX_MAINTENANCE_STARTUP_EFFECTS"
)

func TestMaintenanceStartupMainHelper(t *testing.T) {
	if os.Getenv(maintenanceStartupHelperEnv) != "1" {
		return
	}
	os.Args = modernAdmissionMainArgs(t)
	main()
	os.Exit(0)
}

// This native upstream commits each accepted write to private durable storage.
// Rejected openings must never appear there, including after owner replacement.
func TestMaintenanceStartupUpstreamHelper(t *testing.T) {
	if os.Getenv(maintenanceStartupHelperEnv) != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta      map[string]json.RawMessage `json:"_meta"`
				Name      string                     `json:"name"`
				Arguments struct {
					Marker string `json:"marker"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		if request.Method != "tools/call" || request.Params.Name != "write" || string(request.Params.Meta["io.modelcontextprotocol/protocolVersion"]) != `"2026-07-28"` {
			os.Exit(3)
		}
		file, err := os.OpenFile(os.Getenv(maintenanceStartupEffectsEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(4)
		}
		_, writeErr := fmt.Fprintln(file, request.Params.Arguments.Marker)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			os.Exit(5)
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"pid":%d}}`+"\n", request.ID, os.Getpid())
	}
	os.Exit(0)
}

type maintenanceStartupResponse struct {
	ID     json.RawMessage `json:"id"`
	Result struct {
		PID int `json:"pid"`
	} `json:"result"`
	Error *struct {
		Code int `json:"code"`
		Data struct {
			ErrorCode string `json:"error_code"`
		} `json:"data"`
	} `json:"error"`
}

type maintenanceStartupHost struct {
	stdin     io.WriteCloser
	responses <-chan maintenanceStartupResponse
	done      <-chan error
}

func startMaintenanceStartupHost(t *testing.T, executable, dir, effects string) maintenanceStartupHost {
	t.Helper()
	cmd := exec.Command(executable, "-test.run=^TestMaintenanceStartupMainHelper$", "--", "--mcp-protocol=2026-07-28", executable, "-test.run=^TestMaintenanceStartupUpstreamHelper$")
	cmd.Dir = dir
	env := os.Environ()
	for key, value := range map[string]string{
		maintenanceStartupHelperEnv: "1", maintenanceStartupEffectsEnv: effects,
		"MCPMUX_ENGINE": "1", "MCPMUX_DISABLE_LAUNCHER": "1", "MCP_MUX_TEST_MAIN": "0",
		"MCP_MUX_NO_DAEMON": "0", "MCP_MUX_DAEMON": "0", "MCP_MUX_ISOLATED": "0",
		"MCP_MUX_STATELESS": "0", "MCP_MUX_DEFAULT_MODE": "global", "MCP_MUX_SHIM_LOG": "",
		envShimIdleTimeout: "0", "TEMP": dir, "TMP": dir, "TMPDIR": dir,
	} {
		env = setEnv(env, key, value)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	responses := make(chan maintenanceStartupResponse, 16)
	go func() {
		defer close(responses)
		decoder := json.NewDecoder(stdout)
		for {
			var response maintenanceStartupResponse
			if decoder.Decode(&response) != nil {
				return
			}
			responses <- response
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return maintenanceStartupHost{stdin: stdin, responses: responses, done: done}
}

func (h maintenanceStartupHost) write(t *testing.T, id, marker string) {
	t.Helper()
	_, err := fmt.Fprintf(h.stdin, `{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"write","arguments":{"marker":%q},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n", id, marker)
	if err != nil {
		t.Fatalf("host input closed: %v", err)
	}
}

func (h maintenanceStartupHost) read(t *testing.T, id string, held bool) maintenanceStartupResponse {
	t.Helper()
	select {
	case response, open := <-h.responses:
		if !open {
			t.Fatal("product closed host output")
		}
		if string(response.ID) != id {
			t.Fatalf("response ID=%s, want %s (lost or replayed request)", response.ID, id)
		}
		if held {
			if response.Error == nil || response.Error.Code != -32005 || response.Error.Data.ErrorCode != "maintenance_held" {
				t.Fatalf("request did not receive original-ID held failure: %+v", response)
			}
		} else if response.Error != nil || response.Result.PID <= 0 {
			t.Fatalf("native write failed: %+v", response)
		}
		return response
	case <-time.After(10 * time.Second):
		t.Fatal("product did not answer on the existing host output")
		return maintenanceStartupResponse{}
	}
}

// Run with: go test ./cmd/mcp-mux -run '^TestMaintenanceStartupHeldOpeningKeepsPipesAndResumesFresh$' -count=1 -timeout=90s
func TestMaintenanceStartupHeldOpeningKeepsPipesAndResumesFresh(t *testing.T) {
	for _, id := range []string{"41", `"opening-string"`} {
		t.Run(id, func(t *testing.T) {
			dir := shortTempDir(t, "mh")
			for _, key := range []string{"TEMP", "TMP", "TMPDIR", "APPDATA", "XDG_CONFIG_HOME"} {
				t.Setenv(key, dir)
			}
			d, err := daemon.New(daemon.Config{
				ControlPath: serverid.DaemonControlPath(dir, engineName), Name: engineName,
				Namespace: "startup-" + filepath.Base(dir), SkipSnapshot: true,
				ZeroSessionCleanupDelay: -1, Logger: log.New(io.Discard, "", 0),
			})
			if err != nil {
				t.Fatal(err)
			}
			var holdID string
			t.Cleanup(func() {
				if holdID != "" {
					_, _ = d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: holdID})
				}
				d.Shutdown()
			})
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			effects := filepath.Join(dir, "effects")
			first := startMaintenanceStartupHost(t, executable, dir, effects)
			first.write(t, "1", "first")
			initial := first.read(t, "1", false)
			owners, err := d.HandleListOwners(control.Request{})
			if err != nil || len(owners.Owners) != 1 {
				t.Fatalf("initial native owner: %+v %v", owners, err)
			}

			// Pipes exist before fencing; the second native opening is withheld.
			second := startMaintenanceStartupHost(t, executable, dir, effects)
			ttl := int64(60000)
			hold, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: owners.Owners[0].ServerID, HoldTTLMS: &ttl})
			holdID = hold.HoldID
			if err != nil || hold.State != control.MaintenanceHeld || !hold.TreesRetired {
				t.Fatalf("hold: %+v %v", hold, err)
			}
			second.write(t, id, "rejected-opening")
			second.read(t, id, true)
			second.write(t, `"held-next"`, "rejected-later")
			second.read(t, `"held-next"`, true)
			select {
			case err := <-second.done:
				t.Fatalf("aware product exited while held: %v", err)
			default:
			}

			if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: holdID}); err != nil {
				t.Fatal(err)
			}
			holdID = ""
			second.write(t, `"fresh"`, "fresh")
			fresh := second.read(t, `"fresh"`, false)
			if fresh.Result.PID == initial.Result.PID {
				t.Fatal("resume reused the retired upstream instead of fresh native admission")
			}
			contents, err := os.ReadFile(effects)
			if err != nil || string(contents) != "first\nfresh\n" {
				t.Fatalf("rejected request was replayed or fresh write lost/duplicated: effects=%q error=%v", contents, err)
			}
			_ = second.stdin.Close()
			select {
			case err := <-second.done:
				if err != nil {
					t.Fatalf("product failed after successful resume: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("product did not finish after intentional host EOF")
			}
		})
	}
}
