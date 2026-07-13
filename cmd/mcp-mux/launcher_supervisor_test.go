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
	"sync"
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

func TestLauncherReplayBudgetExceedsDaemonSpawnBudget(t *testing.T) {
	if defaultLauncherReplayTimeout <= defaultDaemonSpawnTimeout {
		t.Fatalf("launcher replay timeout = %s, must exceed daemon spawn timeout %s", defaultLauncherReplayTimeout, defaultDaemonSpawnTimeout)
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

func TestLauncherSupervisorRestartsChildWhenActiveEnginePointerChanges(t *testing.T) {
	dir := t.TempDir()
	generationFile := filepath.Join(dir, "generation.txt")
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath1 := filepath.Join(dir, "engine-v1", engineFileName())
	enginePath2 := filepath.Join(dir, "engine-v2", engineFileName())

	var mu sync.Mutex
	activeEnginePath := enginePath1
	resolveActive := func(string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return activeEnginePath, true
	}
	setActive := func(path string) {
		mu.Lock()
		activeEnginePath = path
		mu.Unlock()
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &strings.Builder{}
	lines := make(chan string, 16)
	go scanTestLines(stdoutR, lines)

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:        launcherPath,
			InitialEnginePath:   enginePath1,
			Args:                []string{"serve"},
			Stdin:               stdinR,
			Stdout:              stdoutW,
			Stderr:              stderr,
			ResolveActiveEngine: resolveActive,
			StartChild: func(_, _ string, _ []string, stderr io.Writer) (*supervisedEngineChild, error) {
				cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
				cmd.Env = append(os.Environ(),
					"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
					"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationFile,
				)
				cmd.Stderr = stderr
				return startSupervisedChildCommand(cmd)
			},
			RespawnDelay:       10 * time.Millisecond,
			ReplayTimeout:      2 * time.Second,
			EnginePollInterval: 10 * time.Millisecond,
		})
	}()

	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if line := readTestLine(t, lines); !strings.Contains(line, `"version":"generation-1"`) {
		t.Fatalf("initialize response = %s, want generation-1", line)
	}
	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	setActive(enginePath2)
	readTestLineContaining(t, lines, "notifications/tools/list_changed")
	readTestLineContaining(t, lines, "notifications/prompts/list_changed")

	writeTestLine(t, stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"after-pointer-change"}}`)
	if line := readTestLineContaining(t, lines, `"id":2`); !strings.Contains(line, "generation-2") {
		t.Fatalf("post-pointer-change response = %s, want generation-2", line)
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
	dormantMode := os.Getenv("MCP_MUX_TEST_SUPERVISOR_DORMANT_MODE")
	dormantReplyDelay, _ := time.ParseDuration(os.Getenv("MCP_MUX_TEST_SUPERVISOR_DORMANT_REPLY_DELAY"))
	eventFile := os.Getenv("MCP_MUX_TEST_SUPERVISOR_EVENT_FILE")
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
		recordSupervisorEvent(eventFile, generation, "recv", msg.Method, string(msg.ID))
		switch msg.Method {
		case "initialize":
			if generation == 1 && crashOn == "initialize" {
				os.Exit(42)
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"generation-%d"}}}`+"\n", msg.ID, generation)
		case "notifications/initialized":
			if generation == 1 {
				switch dormantMode {
				case "ack", "nack", "crash-before-ack", "trailing", "silent-before-ack", "ack-close-stdout-live", "ack-wrong-exit", "post-ack-many":
					recordSupervisorEvent(eventFile, generation, "send", launcherDormantReadyMethod, "")
					fmt.Printf(`{"jsonrpc":"2.0","method":%q}`+"\n", launcherDormantReadyMethod)
				case "exit75-without-ack":
					os.Exit(launcherDormantExitCode)
				}
			}
		case launcherCommitDormantMethod:
			if generation != 1 {
				continue
			}
			recordSupervisorEvent(eventFile, generation, "commit", msg.Method, "")
			if dormantReplyDelay > 0 {
				time.Sleep(dormantReplyDelay)
			}
			switch dormantMode {
			case "ack":
				recordSupervisorEvent(eventFile, generation, "send", launcherDormantAckMethod, "")
				fmt.Printf(`{"jsonrpc":"2.0","method":%q}`+"\n", launcherDormantAckMethod)
				os.Exit(launcherDormantExitCode)
			case "nack":
				recordSupervisorEvent(eventFile, generation, "send", launcherDormantNackMethod, "")
				fmt.Printf(`{"jsonrpc":"2.0","method":%q}`+"\n", launcherDormantNackMethod)
			case "crash-before-ack":
				os.Exit(42)
			case "trailing":
				fmt.Printf(`{"jsonrpc":"2.0","method":"test/trailing"}` + "\n")
				recordSupervisorEvent(eventFile, generation, "send", launcherDormantAckMethod, "")
				fmt.Printf(`{"jsonrpc":"2.0","method":%q}`+"\n", launcherDormantAckMethod)
				os.Exit(launcherDormantExitCode)
			case "silent-before-ack":
				select {}
			case "ack-close-stdout-live":
				recordSupervisorEvent(eventFile, generation, "send", launcherDormantAckMethod, "")
				fmt.Printf(`{"jsonrpc":"2.0","method":%q}`+"\n", launcherDormantAckMethod)
				_ = os.Stdout.Close()
				time.Sleep(10 * time.Second)
				os.Exit(0)
			case "ack-wrong-exit":
				recordSupervisorEvent(eventFile, generation, "send", launcherDormantAckMethod, "")
				fmt.Printf(`{"jsonrpc":"2.0","method":%q}`+"\n", launcherDormantAckMethod)
				os.Exit(42)
			case "post-ack-many":
				recordSupervisorEvent(eventFile, generation, "send", launcherDormantAckMethod, "")
				fmt.Printf(`{"jsonrpc":"2.0","method":%q}`+"\n", launcherDormantAckMethod)
				for i := 0; i < 32; i++ {
					fmt.Printf(`{"jsonrpc":"2.0","method":"test/post-ack-%d"}`+"\n", i)
				}
				os.Exit(launcherDormantExitCode)
			}
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

func recordSupervisorEvent(path string, generation int, phase, method, id string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "generation=%d phase=%s method=%s id=%s\n", generation, phase, method, id)
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

func readTestLineContaining(t *testing.T, lines <-chan string, want string) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stdout closed before line containing %q", want)
			}
			if strings.Contains(line, want) {
				return line
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for stdout line containing %q", want)
		}
	}
}
