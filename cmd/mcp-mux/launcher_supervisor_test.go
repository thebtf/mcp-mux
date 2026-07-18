package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/supervisor"
	"github.com/thebtf/mcp-mux/muxcore/supervisor/attest"
)

func TestPrepareSupervisedEngineStartOwnsDaemonInLauncher(t *testing.T) {
	t.Setenv("MCP_MUX_NO_DAEMON", "")
	launcherPath := filepath.Join(t.TempDir(), "mcp-mux.exe")
	enginePath := writeContentAddressedTestEngine(t, launcherPath, "active engine")
	if err := writeActiveEngine(launcherPath, enginePath); err != nil {
		t.Fatal(err)
	}
	oldEnsure := launcherSupervisorEnsureDaemon
	called := false
	launcherSupervisorEnsureDaemon = func(_ *log.Logger, gotLauncher, gotEngine string) error {
		called = true
		if gotLauncher != launcherPath || gotEngine != enginePath {
			t.Fatalf("daemon preparation paths = (%q, %q), want (%q, %q)", gotLauncher, gotEngine, launcherPath, enginePath)
		}
		return nil
	}
	t.Cleanup(func() { launcherSupervisorEnsureDaemon = oldEnsure })

	if err := prepareSupervisedEngineStart(context.Background(), launcherPath, enginePath, []string{"fixture-mcp"}, io.Discard); err != nil {
		t.Fatalf("prepareSupervisedEngineStart: %v", err)
	}
	if !called {
		t.Fatal("launcher did not prepare the shared daemon from the active engine before child start")
	}

	env := launcherSupervisorEnv("launcher.exe", []string{"fixture-mcp"})
	want := envLauncherOwnsDaemon + "=1"
	found := false
	for _, entry := range env {
		if entry == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("supervised child env missing %q", want)
	}
}

func TestPrepareSupervisedEngineStartRejectsNonActiveEngineBeforeDaemon(t *testing.T) {
	t.Setenv("MCP_MUX_NO_DAEMON", "")
	launcherPath := filepath.Join(t.TempDir(), "mcp-mux.exe")
	activeEnginePath := writeContentAddressedTestEngine(t, launcherPath, "active engine")
	staleEnginePath := writeContentAddressedTestEngine(t, launcherPath, "stale engine")
	if err := writeActiveEngine(launcherPath, activeEnginePath); err != nil {
		t.Fatal(err)
	}

	oldEnsure := launcherSupervisorEnsureDaemon
	called := false
	launcherSupervisorEnsureDaemon = func(*log.Logger, string, string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { launcherSupervisorEnsureDaemon = oldEnsure })

	err := prepareSupervisedEngineStart(context.Background(), launcherPath, staleEnginePath, []string{"fixture-mcp"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "active engine path is not authorized") {
		t.Fatalf("prepare stale engine error = %v", err)
	}
	if called {
		t.Fatal("stale engine prepared the shared daemon")
	}
}

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
		codeCh <- runTestLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:        launcherPath,
			InitialEnginePath:   enginePath,
			Args:                []string{"fixture-mcp"},
			Stdin:               stdinR,
			Stdout:              stdoutW,
			Stderr:              stderr,
			ResolveActiveEngine: func(string) (string, bool) { return enginePath, true },
			StartChild: func(ctx context.Context, _, selected string, _ []string, stderr io.Writer) (supervisor.StartResult, error) {
				cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
				cmd.Env = append(os.Environ(),
					"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
					"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationFile,
					"MCP_MUX_TEST_SUPERVISOR_CRASH_ON=tools",
				)
				cmd.Stderr = stderr
				child, err := startSupervisedChildCommand(ctx, cmd)
				return supervisor.StartResult{Child: child, Actual: supervisor.EngineRef{ID: canonicalLauncherEnginePath(selected)}}, err
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

func TestStableLauncherFallbackDoesNotCreatePrivateAdmission(t *testing.T) {
	original := launcherAttestationStart
	starts := 0
	launcherAttestationStart = func() (*attest.Parent, error) {
		starts++
		return nil, errors.New("unexpected attestation")
	}
	t.Cleanup(func() { launcherAttestationStart = original })

	missing := filepath.Join(t.TempDir(), launcherFileName())
	_, admission, err := startAttestedSupervisorCommand(context.Background(), missing, missing, nil, io.Discard)
	if err == nil {
		t.Fatal("missing fallback launcher unexpectedly started")
	}
	if admission != nil || starts != 0 {
		t.Fatalf("fallback admission=%v attestation starts=%d", admission != nil, starts)
	}
}

func TestUnauthorizedActiveEngineDoesNotCreatePrivateAdmission(t *testing.T) {
	original := launcherAttestationStart
	starts := 0
	launcherAttestationStart = func() (*attest.Parent, error) {
		starts++
		return nil, errors.New("unexpected attestation")
	}
	t.Cleanup(func() { launcherAttestationStart = original })

	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherFileName())
	outside := filepath.Join(dir, "outside", engineFileName())
	writeTestFile(t, outside, "outside engine")
	_, admission, err := startAttestedSupervisorCommand(context.Background(), launcherPath, outside, nil, io.Discard)
	if err == nil {
		t.Fatal("unauthorized active engine unexpectedly started")
	}
	if admission != nil || starts != 0 {
		t.Fatalf("unauthorized admission=%v attestation starts=%d", admission != nil, starts)
	}
}

func TestDefaultSupervisorStartFailureRedactsPaths(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "secret-stable-launcher", launcherFileName())
	enginePath := filepath.Join(dir, "secret-active-engine", engineFileName())
	var stderr strings.Builder
	_, err := startDefaultSupervisedEngineChild(context.Background(), launcherPath, enginePath, []string{"serve", "secret-argument"}, &stderr)
	if err == nil {
		t.Fatal("missing active and fallback executables unexpectedly started")
	}
	combined := stderr.String() + err.Error()
	for _, forbidden := range []string{launcherPath, enginePath, "secret-stable-launcher", "secret-active-engine", "secret-argument"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("supervisor start failure leaked %q: %s", forbidden, combined)
		}
	}
}

func TestDefaultSupervisorDoesNotFallbackAfterUnprovenRollback(t *testing.T) {
	original := launcherSupervisorCommandStart
	defer func() { launcherSupervisorCommandStart = original }()

	calls := 0
	launcherSupervisorCommandStart = func(context.Context, string, string, []string, io.Writer) (*supervisor.CommandChild, *attest.Parent, error) {
		calls++
		return nil, nil, supervisor.ErrStartRollbackUnproven
	}

	result, err := startDefaultSupervisedEngineChild(
		context.Background(),
		filepath.Join(t.TempDir(), launcherFileName()),
		filepath.Join(t.TempDir(), engineFileName()),
		[]string{"serve"},
		io.Discard,
	)
	if !errors.Is(err, supervisor.ErrStartRollbackUnproven) {
		t.Fatalf("start error = %v, want rollback-unproven sentinel", err)
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, fallback started before rollback proof", calls)
	}
	if result.Child != nil || result.Admission != nil {
		t.Fatalf("unexpected partial authority = %#v", result)
	}
}

func TestLauncherStartBudgetCoversDaemonSpawnBudget(t *testing.T) {
	if defaultLauncherStartTimeout < defaultDaemonSpawnTimeout {
		t.Fatalf("launcher start timeout = %s, must cover daemon spawn timeout %s", defaultLauncherStartTimeout, defaultDaemonSpawnTimeout)
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
		codeCh <- runTestLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:      launcherPath,
			InitialEnginePath: enginePath,
			Args:              []string{"fixture-mcp"},
			Stdin:             stdinR,
			Stdout:            stdoutW,
			Stderr:            stderr,
			ResolveActiveEngine: func(string) (string, bool) {
				return enginePath, true
			},
			StartChild: func(ctx context.Context, _, selected string, _ []string, stderr io.Writer) (supervisor.StartResult, error) {
				cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
				cmd.Env = append(os.Environ(),
					"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
					"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationFile,
					"MCP_MUX_TEST_SUPERVISOR_CRASH_ON=initialize",
				)
				cmd.Stderr = stderr
				child, err := startSupervisedChildCommand(ctx, cmd)
				return supervisor.StartResult{Child: child, Actual: supervisor.EngineRef{ID: canonicalLauncherEnginePath(selected)}}, err
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
		codeCh <- runTestLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:        launcherPath,
			InitialEnginePath:   enginePath1,
			Args:                []string{"serve"},
			Stdin:               stdinR,
			Stdout:              stdoutW,
			Stderr:              stderr,
			ResolveActiveEngine: resolveActive,
			StartChild: func(ctx context.Context, _, selected string, _ []string, stderr io.Writer) (supervisor.StartResult, error) {
				cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
				cmd.Env = append(os.Environ(),
					"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
					"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationFile,
				)
				cmd.Stderr = stderr
				child, err := startSupervisedChildCommand(ctx, cmd)
				return supervisor.StartResult{Child: child, Actual: supervisor.EngineRef{ID: canonicalLauncherEnginePath(selected)}}, err
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

func TestLauncherSupervisorEightHostsSwitchEngineOnSamePipes(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherFileName())
	enginePath1 := filepath.Join(dir, "engine-v1", engineFileName())
	enginePath2 := filepath.Join(dir, "engine-v2", engineFileName())

	var activeMu sync.RWMutex
	activeEnginePath := enginePath1
	resolveActive := func(string) (string, bool) {
		activeMu.RLock()
		defer activeMu.RUnlock()
		return activeEnginePath, true
	}

	type openHost struct {
		stdin  *io.PipeWriter
		lines  <-chan string
		code   <-chan int
		stderr *strings.Builder
		starts *atomic.Int32
	}
	hosts := make([]openHost, 0, 8)
	for index := 0; index < 8; index++ {
		generationFile := filepath.Join(dir, fmt.Sprintf("host-%d-generation.txt", index))
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		stderr := &strings.Builder{}
		lines := make(chan string, 32)
		go scanTestLines(stdoutR, lines)
		codeCh := make(chan int, 1)
		starts := &atomic.Int32{}
		go func() {
			codeCh <- runTestLauncherStdioSupervisor(launcherSupervisorConfig{
				LauncherPath:        launcherPath,
				InitialEnginePath:   enginePath1,
				Args:                []string{"serve"},
				Stdin:               stdinR,
				Stdout:              stdoutW,
				Stderr:              stderr,
				ResolveActiveEngine: resolveActive,
				StartChild: func(ctx context.Context, _, selected string, _ []string, childStderr io.Writer) (supervisor.StartResult, error) {
					starts.Add(1)
					cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
					cmd.Env = append(os.Environ(),
						"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
						"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+generationFile,
					)
					cmd.Stderr = childStderr
					child, err := startSupervisedChildCommand(ctx, cmd)
					return supervisor.StartResult{Child: child, Actual: supervisor.EngineRef{ID: canonicalLauncherEnginePath(selected)}}, err
				},
				RespawnDelay:       10 * time.Millisecond,
				ReplayTimeout:      10 * time.Second,
				EnginePollInterval: 10 * time.Millisecond,
			})
		}()
		hosts = append(hosts, openHost{stdin: stdinW, lines: lines, code: codeCh, stderr: stderr, starts: starts})
	}

	for index, host := range hosts {
		writeTestLine(t, host.stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":"init-%d","method":"initialize","params":{}}`, index))
		if line := readTestLine(t, host.lines); !strings.Contains(line, `"version":"generation-1"`) {
			t.Fatalf("host %d initialize response = %s", index, line)
		}
		writeTestLine(t, host.stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	}

	activeMu.Lock()
	activeEnginePath = enginePath2
	activeMu.Unlock()
	for index, host := range hosts {
		readTestLineContaining(t, host.lines, "notifications/tools/list_changed")
		readTestLineContaining(t, host.lines, "notifications/prompts/list_changed")
		requestID := fmt.Sprintf("switch-%d", index)
		writeTestLine(t, host.stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"after-switch"}}`, requestID))
		line := readTestLineContaining(t, host.lines, `"id":"`+requestID+`"`)
		if !strings.Contains(line, "generation-2") {
			t.Fatalf("host %d switched response = %s", index, line)
		}
	}

	for index, host := range hosts {
		if got := host.starts.Load(); got != 2 {
			t.Fatalf("host %d child starts = %d, want exactly 2", index, got)
		}
		if err := host.stdin.Close(); err != nil {
			t.Fatalf("host %d close stdin: %v", index, err)
		}
		select {
		case code := <-host.code:
			if code != 0 {
				t.Fatalf("host %d supervisor exit = %d stderr=%s", index, code, host.stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("host %d supervisor did not stop stderr=%s", index, host.stderr.String())
		}
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
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{"listChanged":true},"prompts":{"listChanged":true}},"serverInfo":{"name":"fixture","version":"generation-%d"}}}`+"\n", msg.ID, generation)
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
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
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d\n", generation)), 0o644)
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
	timer := time.NewTimer(15 * time.Second)
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
