package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/supervisor"
)

func TestLauncherModern_OnlyTopLevelProtocolOptionSelectsNativeSupervisor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		modern bool
	}{
		{"explicit modern", []string{"--mcp-protocol=2026-07-28", "--mcp-sharing=shared", "--mcp-partition=r2-fixture", "fixture-mcp"}, true},
		{"upstream argument is not selector", []string{"fixture-mcp", "--mcp-protocol=2026-07-28"}, false},
		{"sharing mode is not protocol era", []string{"--stateless", "fixture-mcp"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			eventsPath := filepath.Join(dir, "events")
			generationPath := filepath.Join(dir, "generation")
			enginePath := os.Args[0]
			input, host := io.Pipe()
			output, writer := io.Pipe()
			lines := make(chan string, 8)
			go scanTestLines(output, lines)
			var stderr bytes.Buffer
			done := make(chan int, 1)
			t.Cleanup(func() { _ = host.Close(); _ = output.Close() })
			go func() {
				code := runTestLauncherStdioSupervisor(launcherSupervisorConfig{
					LauncherPath:        filepath.Join(dir, launcherFileName()),
					InitialEnginePath:   enginePath,
					Args:                tc.args,
					Stdin:               input,
					Stdout:              writer,
					Stderr:              &stderr,
					ResolveActiveEngine: func(string) (string, bool) { return enginePath, true },
					StartChild: func(ctx context.Context, _, selected string, args []string, childStderr io.Writer) (supervisor.StartResult, error) {
						if !reflect.DeepEqual(args, tc.args) {
							return supervisor.StartResult{}, errors.New("launcher changed child CLI argument boundaries")
						}
						cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherSupervisorChildHelper$", "--")
						cmd.Env = append(os.Environ(),
							"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
							"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationPath,
							"MCP_MUX_TEST_SUPERVISOR_EVENT_FILE="+eventsPath,
							"MCP_MUX_TEST_SUPERVISOR_CRASH_ON=",
							"MCP_MUX_TEST_SUPERVISOR_DORMANT_MODE=",
						)
						cmd.Stderr = childStderr
						child, err := startSupervisedChildCommand(ctx, cmd)
						return supervisor.StartResult{Child: child, Actual: supervisor.EngineRef{ID: canonicalLauncherEnginePath(selected)}}, err
					},
				})
				_ = writer.Close()
				done <- code
			}()
			opening := `{"jsonrpc":"2.0","id":"native","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"--mcp-protocol=2026-07-28","version":"1"}}}}`
			writeTestLine(t, host, opening)
			raw := readTestLine(t, lines)
			var response struct {
				ID     string          `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatal(err)
			}
			if response.ID != "native" {
				t.Fatalf("launcher response lost native opening ID: %s", raw)
			}
			if tc.modern {
				if response.Error != nil || len(response.Result) == 0 {
					t.Fatalf("explicit modern CLI opening hit legacy initialization gate: %s", raw)
				}
			} else if response.Error == nil || response.Error.Code != -32600 {
				t.Fatalf("upstream flags, sharing mode, or clientInfo selected modern era without its own CLI option: %s", raw)
			}
			if err := host.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("launcher exit=%d stderr=%s", code, stderr.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("launcher did not settle after host EOF")
			}
			events, err := os.ReadFile(eventsPath)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.Contains(string(events), "method=initialize ") || strings.Contains(string(events), "method=notifications/initialized ") {
				t.Fatalf("launcher injected legacy bootstrap for native opening: %s", events)
			}
			if got := strings.Count(string(events), "phase=recv method=tools/list "); (tc.modern && got != 1) || (!tc.modern && got != 0) {
				t.Fatalf("native first request upstream count=%d modern=%v events=%s", got, tc.modern, events)
			}
		})
	}
}

func TestLauncherModern_InvalidOwnOptionsRejectBeforePreparationOrChildStart(t *testing.T) {
	t.Setenv("MCP_MUX_DEFAULT_MODE", "global")
	t.Setenv("MCP_MUX_ISOLATED", "0")
	t.Setenv("MCP_MUX_STATELESS", "0")
	for _, args := range [][]string{
		{"--mcp-protocol=2030-01-01", "fixture-mcp"},
		{"--mcp-sharing=shared", "--mcp-partition=r2-fixture", "fixture-mcp"},
		{"--mcp-protocol=2026-07-28", "--mcp-sharing=shared", "fixture-mcp"},
		{"--mcp-protocol=2026-07-28", "--mcp-sharing=unknown", "--mcp-partition=r2-fixture", "fixture-mcp"},
		{"--mcp-protocol=2026-07-28", "--mcp-partition=r2-fixture", "fixture-mcp"},
	} {
		t.Run(strings.Join(args[:len(args)-1], " "), func(t *testing.T) {
			input, host := io.Pipe()
			t.Cleanup(func() { _ = host.Close(); _ = input.Close() })
			effects := make(chan string, 4)
			done := make(chan error, 1)
			go func() {
				done <- runLauncherStdioSupervisorErr(launcherSupervisorConfig{
					LauncherPath:        filepath.Join(t.TempDir(), launcherFileName()),
					InitialEnginePath:   os.Args[0],
					Args:                args,
					Stdin:               input,
					Stdout:              io.Discard,
					Stderr:              io.Discard,
					ResolveActiveEngine: func(string) (string, bool) { return os.Args[0], true },
					PrepareStart: func(context.Context, string, string, []string, io.Writer) error {
						effects <- "prepare"
						return errors.New("invalid CLI reached launch preparation")
					},
					StartChild: func(context.Context, string, string, []string, io.Writer) (supervisor.StartResult, error) {
						effects <- "start"
						return supervisor.StartResult{}, errors.New("invalid CLI reached child start")
					},
				})
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("invalid own launcher options were accepted")
				}
				select {
				case effect := <-effects:
					t.Fatalf("invalid own options caused %s before refusal", effect)
				default:
				}
			case effect := <-effects:
				_ = host.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("invalid-option launch loop did not settle after EOF")
				}
				t.Fatalf("invalid own options caused %s instead of local refusal", effect)
			case <-time.After(3 * time.Second):
				_ = host.Close()
				t.Fatal("invalid own options waited for host traffic instead of failing before launch")
			}
		})
	}
}
