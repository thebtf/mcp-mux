package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLauncherSupervisorRespawnsChildWithoutClosingTransport(t *testing.T) {
	dir := t.TempDir()
	generationFile := filepath.Join(dir, "generation.txt")
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := os.Args[0]

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &strings.Builder{}
	lines := make(chan string, 8)
	go scanTestLines(stdoutR, lines)

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:        launcherPath,
			InitialEnginePath:   enginePath,
			Args:                []string{"fixture-mcp"},
			Stdin:               stdinR,
			Stdout:              stdoutW,
			Stderr:              stderr,
			ResolveActiveEngine: func(string) (string, bool) { return enginePath, true },
			StartChild: func(_, _ string, _ []string, stderr io.Writer) (*supervisedEngineChild, error) {
				cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
				cmd.Env = append(os.Environ(),
					"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
					"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationFile,
					"MCP_MUX_TEST_SUPERVISOR_CRASH_ON=tools",
				)
				cmd.Stderr = stderr
				return startSupervisedChildCommand(cmd)
			},
			RespawnDelay:  10 * time.Millisecond,
			ReplayTimeout: 2 * time.Second,
		})
	}()

	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if line := readTestLine(t, lines); !strings.Contains(line, `"version":"generation-1"`) {
		t.Fatalf("initialize response = %s, want generation-1", line)
	}
	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"crash"}}`)
	if line := readTestLine(t, lines); !strings.Contains(line, `"id":2`) || !strings.Contains(line, "engine restarted") {
		t.Fatalf("lost request response = %s, want id=2 engine-restarted error", line)
	}

	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"after-restart"}}`)
	if line := readTestLine(t, lines); !strings.Contains(line, `"id":3`) || !strings.Contains(line, "generation-2") {
		t.Fatalf("post-restart response = %s, want id=3 generation-2", line)
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("supervisor exit code = %d, stderr:\n%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("supervisor did not exit after stdin close; stderr:\n%s", stderr.String())
	}
}

func TestLauncherSupervisorRetriesInitializeWithoutClosingTransport(t *testing.T) {
	dir := t.TempDir()
	generationFile := filepath.Join(dir, "generation.txt")
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := os.Args[0]

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &strings.Builder{}
	lines := make(chan string, 8)
	go scanTestLines(stdoutR, lines)

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:      launcherPath,
			InitialEnginePath: enginePath,
			Args:              []string{"fixture-mcp"},
			Stdin:             stdinR,
			Stdout:            stdoutW,
			Stderr:            stderr,
			ResolveActiveEngine: func(string) (string, bool) {
				return enginePath, true
			},
			StartChild: func(_, _ string, _ []string, stderr io.Writer) (*supervisedEngineChild, error) {
				cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
				cmd.Env = append(os.Environ(),
					"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
					"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationFile,
					"MCP_MUX_TEST_SUPERVISOR_CRASH_ON=initialize",
				)
				cmd.Stderr = stderr
				return startSupervisedChildCommand(cmd)
			},
			RespawnDelay:  10 * time.Millisecond,
			ReplayTimeout: 2 * time.Second,
		})
	}()

	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if line := readTestLine(t, lines); !strings.Contains(line, `"id":1`) || !strings.Contains(line, `"version":"generation-2"`) {
		t.Fatalf("initialize response = %s, want replayed generation-2 response for original id", line)
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("supervisor exit code = %d, stderr:\n%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("supervisor did not exit after stdin close; stderr:\n%s", stderr.String())
	}
}

func TestLauncherSupervisorChildHelper(t *testing.T) {
	if os.Getenv("MCP_MUX_TEST_SUPERVISOR_CHILD") != "1" {
		return
	}
	generation := incrementSupervisorGeneration(os.Getenv("MCP_MUX_TEST_SUPERVISOR_GEN_FILE"))
	crashOn := os.Getenv("MCP_MUX_TEST_SUPERVISOR_CRASH_ON")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var msg struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			os.Exit(2)
		}
		switch msg.Method {
		case "initialize":
			if generation == 1 && crashOn == "initialize" {
				os.Exit(42)
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"generation-%d"}}}`+"\n", msg.ID, generation)
		case "notifications/initialized":
		case "tools/call":
			if generation == 1 && crashOn == "tools" {
				os.Exit(42)
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"generation-%d"}],"isError":false}}`+"\n", msg.ID, generation)
		default:
			if msg.ID != nil {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", msg.ID)
			}
		}
	}
	os.Exit(0)
}

func incrementSupervisorGeneration(path string) int {
	generation := 1
	if data, err := os.ReadFile(path); err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &generation)
		generation++
	}
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d\n", generation)), 0644)
	return generation
}

func scanTestLines(r io.Reader, out chan<- string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		out <- scanner.Text()
	}
	close(out)
}

func writeTestLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
		t.Fatalf("write line: %v", err)
	}
}

func readTestLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatal("stdout closed before expected line")
		}
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for stdout line")
	}
	return ""
}
