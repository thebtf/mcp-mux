package owner_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func TestMaintenanceAuthorizationHoldRetainsAuthorityAllOwnerModes(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []string{"", "2026-07-28"} {
			for _, completes := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/era_%s/completes_%t", mode, protocol, completes), func(t *testing.T) {
					config := t.TempDir()
					for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
						t.Setenv(key, config)
					}
					file, err := os.CreateTemp("", "m-auth-*.sock")
					if err != nil {
						t.Fatal(err)
					}
					path := file.Name()
					_ = file.Close()
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { ipc.Cleanup(path) })
					entered := make(chan context.Context, 1)
					release := make(chan struct{})
					var releaseOnce sync.Once
					cfg := daemon.Config{
						ControlPath: path, Namespace: filepath.Base(path), SkipSnapshot: true, Logger: log.New(io.Discard, "", 0),
						AuthorizeSession: func(ctx context.Context, _ muxcore.ConnInfo, _ muxcore.ProjectContext) muxcore.SessionAuth {
							entered <- ctx
							<-release
							return muxcore.SessionAuth{Decision: muxcore.AuthAllow}
						},
					}
					if mode == "handler_func" {
						cfg.HandlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
							scanner := bufio.NewScanner(stdin)
							for scanner.Scan() {
								var request struct {
									ID     json.RawMessage `json:"id"`
									Method string          `json:"method"`
								}
								if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
									return err
								}
								if len(request.ID) == 0 {
									continue
								}
								result := `{"tools":[]}`
								if request.Method == "initialize" {
									result = `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"authorization-hold","version":"1"}}`
								}
								if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", request.ID, result); err != nil {
									return err
								}
							}
							return scanner.Err()
						}
					}
					d, err := daemon.New(cfg)
					if err != nil {
						t.Fatal(err)
					}
					var held *control.MaintenanceResult
					t.Cleanup(func() {
						releaseOnce.Do(func() { close(release) })
						if held != nil {
							deadline := time.Now().Add(5 * time.Second)
							for time.Now().Before(deadline) {
								_, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: held.HoldID})
								if err == nil {
									break
								}
								time.Sleep(5 * time.Millisecond)
							}
						}
						d.Shutdown()
					})
					request := control.Request{
						Command: os.Args[0], Args: []string{"-test.run=^TestMaintenanceAuthorizationHelperProcess$"}, Mode: "isolated", Cwd: config,
						Env: map[string]string{"MCPMUX_MAINTENANCE_AUTHORIZATION_HELPER": "1"}, ProtocolEra: protocol,
					}
					path, sid, token, err := d.Spawn(request)
					if err != nil {
						t.Fatal(err)
					}
					entry := d.Entry(sid)
					if entry == nil || entry.Owner == nil {
						t.Fatal("spawn did not retain an exact owner entry")
					}
					deadline := time.Now().Add(5 * time.Second)
					for entry.Owner.MaterializationState() != owner.MaterializationReady || entry.Owner.PendingRequests() != 0 {
						if time.Now().After(deadline) {
							t.Fatal("real subprocess/pipe owner did not become ready")
						}
						time.Sleep(5 * time.Millisecond)
					}
					conn, err := ipc.Dial(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
					if _, err := fmt.Fprintln(conn, token); err != nil {
						t.Fatal(err)
					}
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("real IPC session did not reach authorization")
					}
					ttl := int64(60000)
					request = control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: &ttl}
					if completes {
						request.DrainTimeoutMs = 3000
					}
					type response struct {
						result *control.MaintenanceResult
						err    error
					}
					done := make(chan response, 1)
					go func() {
						result, err := control.SendMaintenance(cfg.ControlPath, request, 10*time.Second)
						done <- response{result, err}
					}()
					if completes {
						deadline = time.Now().Add(5 * time.Second)
						for {
							results := d.HandleStatus()["maintenance"].([]control.MaintenanceResult)
							if len(results) == 1 {
								held = &results[0]
								if held.State != control.MaintenanceHolding || held.TreesRetired {
									t.Fatalf("hold advanced before actual authorization return: %+v", held)
								}
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("public hold did not publish its fence")
							}
							time.Sleep(5 * time.Millisecond)
						}
						select {
						case early := <-done:
							t.Fatalf("positive-grace hold completed before authorization return: %+v %v", early.result, early.err)
						default:
						}
						releaseOnce.Do(func() { close(release) })
					}
					var result response
					select {
					case result = <-done:
					case <-time.After(10 * time.Second):
						t.Fatal("public hold did not return a bounded verdict")
					}
					held = result.result
					if completes {
						if result.err != nil || held == nil || held.State != control.MaintenanceHeld || !held.TreesRetired {
							t.Fatalf("actual authorization return did not complete positive-grace hold: %+v %v", held, result.err)
						}
					} else {
						if !errors.Is(result.err, control.ErrMaintenanceRetirementBlocked) || held == nil || held.State != control.MaintenanceRetirementBlocked || held.TreesRetired {
							t.Fatalf("blocked authorization falsely produced HELD: %+v %v", held, result.err)
						}
						if d.Entry(sid) != entry || entry.Owner.MaintenanceRetired() || entry.Owner.PendingRequests() != 0 || entry.Owner.SessionCount() != 0 {
							t.Fatal("blocked authorization lost exact authority or polluted request metrics")
						}
						select {
						case <-entry.Owner.Done():
							t.Fatal("blocked authorization closed owner Done")
						default:
						}
						releaseOnce.Do(func() { close(release) })
					}
					deadline = time.Now().Add(5 * time.Second)
					for !entry.Owner.MaintenanceRetired() || d.Entry(sid) != nil {
						if time.Now().After(deadline) {
							t.Fatal("actual authorization return did not settle through existing exact-entry retry")
						}
						time.Sleep(5 * time.Millisecond)
					}
					if protocol != "" && entry.ProtocolEra != era.EraModern20260728 {
						t.Fatal("authorization changed modern owner era")
					}
				})
			}
		}
	}
}
