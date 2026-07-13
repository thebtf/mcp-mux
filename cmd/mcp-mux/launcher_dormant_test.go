package main

import (
	"bufio"
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

type dormantSupervisorHarness struct {
	stdinW         *io.PipeWriter
	lines          <-chan string
	codeCh         <-chan int
	stderr         *strings.Builder
	generationFile string
	eventFile      string

	mu         sync.Mutex
	activePath string
	startPaths []string
}

func newDormantSupervisorHarness(t *testing.T, mode string, replyDelay time.Duration) *dormantSupervisorHarness {
	t.Helper()

	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(dir, "engine-v1", engineFileName())
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &strings.Builder{}
	lines := make(chan string, 32)
	go scanTestLines(stdoutR, lines)

	h := &dormantSupervisorHarness{
		stdinW:         stdinW,
		lines:          lines,
		stderr:         stderr,
		generationFile: filepath.Join(dir, "generation.txt"),
		eventFile:      filepath.Join(dir, "events.log"),
		activePath:     enginePath,
	}
	codeCh := make(chan int, 1)
	h.codeCh = codeCh

	go func() {
		codeCh <- runLauncherStdioSupervisor(launcherSupervisorConfig{
			LauncherPath:      launcherPath,
			InitialEnginePath: enginePath,
			Args:              []string{"fixture-mcp"},
			Stdin:             stdinR,
			Stdout:            stdoutW,
			Stderr:            stderr,
			ResolveActiveEngine: func(string) (string, bool) {
				h.mu.Lock()
				defer h.mu.Unlock()
				return h.activePath, true
			},
			StartChild: func(_, selectedEngine string, _ []string, childStderr io.Writer) (*supervisedEngineChild, error) {
				h.mu.Lock()
				h.startPaths = append(h.startPaths, selectedEngine)
				h.mu.Unlock()

				cmd := exec.Command(os.Args[0], "-test.run=TestLauncherSupervisorChildHelper", "--")
				cmd.Env = append(os.Environ(),
					"MCP_MUX_TEST_SUPERVISOR_CHILD=1",
					"MCP_MUX_TEST_SUPERVISOR_GEN_FILE="+h.generationFile,
					"MCP_MUX_TEST_SUPERVISOR_EVENT_FILE="+h.eventFile,
					"MCP_MUX_TEST_SUPERVISOR_DORMANT_MODE="+mode,
					"MCP_MUX_TEST_SUPERVISOR_DORMANT_REPLY_DELAY="+replyDelay.String(),
				)
				cmd.Stderr = childStderr
				return startSupervisedChildCommand(cmd)
			},
			RespawnDelay:       20 * time.Millisecond,
			ReplayTimeout:      2 * time.Second,
			EnginePollInterval: -1,
		})
	}()

	return h
}

func (h *dormantSupervisorHarness) initialize(t *testing.T) {
	t.Helper()
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	line := readTestLine(t, h.lines)
	if !strings.Contains(line, `"id":1`) || !strings.Contains(line, "generation-1") {
		t.Fatalf("initialize response = %s, want generation-1", line)
	}
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func (h *dormantSupervisorHarness) closeAndWait(t *testing.T) {
	t.Helper()
	_ = h.stdinW.Close()
	select {
	case code := <-h.codeCh:
		if code != 0 {
			t.Fatalf("supervisor exit code = %d, stderr:\n%s", code, h.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("supervisor did not exit after stdin close; stderr:\n%s", h.stderr.String())
	}
}

func (h *dormantSupervisorHarness) setActivePath(path string) {
	h.mu.Lock()
	h.activePath = path
	h.mu.Unlock()
}

func (h *dormantSupervisorHarness) starts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.startPaths...)
}

func TestLauncherSupervisorDormantChildDoesNotRespawnUntilDemand(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack", 0)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=send method="+launcherDormantAckMethod)
	assertNoTestLine(t, h.lines, 150*time.Millisecond)

	if got := readSupervisorGeneration(t, h.generationFile); got != 1 {
		t.Fatalf("generation after dormant ACK = %d, want 1 (no eager respawn)", got)
	}

	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("wake response = %s, want generation-2", line)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorDormantWakeReplaysHandshakeAndForwardsTriggerOnce(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack", 0)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=send method="+launcherDormantAckMethod)

	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("wake response = %s, want generation-2", line)
	}

	events := waitSupervisorEvent(t, h.eventFile, "generation=2 phase=recv method=tools/call id=2")
	assertEventCount(t, events, "generation=2 phase=recv method=initialize id=1", 1)
	assertEventCount(t, events, "generation=2 phase=recv method=notifications/initialized id=", 1)
	assertEventCount(t, events, "generation=2 phase=recv method=tools/call id=2", 1)
	h.closeAndWait(t)
}

func TestLauncherSupervisorQuiescingBuffersNewFrames(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack", 250*time.Millisecond)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=commit method="+launcherCommitDormantMethod)

	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("quiesced request response = %s, want generation-2", line)
	}

	events := waitSupervisorEvent(t, h.eventFile, "generation=2 phase=recv method=tools/call id=2")
	assertEventCount(t, events, "generation=1 phase=recv method=tools/call id=2", 0)
	assertEventCount(t, events, "generation=2 phase=recv method=tools/call id=2", 1)
	h.closeAndWait(t)
}

func TestLauncherSupervisorDormantNackResumesExistingChild(t *testing.T) {
	h := newDormantSupervisorHarness(t, "nack", 100*time.Millisecond)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=commit method="+launcherCommitDormantMethod)
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)

	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-1") {
		t.Fatalf("NACK response = %s, want existing generation-1", line)
	}
	if got := readSupervisorGeneration(t, h.generationFile); got != 1 {
		t.Fatalf("generation after NACK = %d, want 1", got)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorCrashBeforeDormantAckRestartsAndPreservesBufferedFrames(t *testing.T) {
	h := newDormantSupervisorHarness(t, "crash-before-ack", 150*time.Millisecond)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=commit method="+launcherCommitDormantMethod)
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)

	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("post-crash response = %s, want generation-2", line)
	}
	events := waitSupervisorEvent(t, h.eventFile, "generation=2 phase=recv method=tools/call id=2")
	assertEventCount(t, events, "generation=2 phase=recv method=tools/call id=2", 1)
	h.closeAndWait(t)
}

func TestLauncherSupervisorExit75WithoutAckStillRespawns(t *testing.T) {
	h := newDormantSupervisorHarness(t, "exit75-without-ack", 0)
	h.initialize(t)
	waitSupervisorGeneration(t, h.generationFile, 2)

	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("response after unacknowledged exit 75 = %s, want generation-2", line)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorDormantWakeUsesLatestActiveEngine(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack", 0)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=send method="+launcherDormantAckMethod)

	enginePath2 := filepath.Join(filepath.Dir(filepath.Dir(h.generationFile)), "engine-v2", engineFileName())
	h.setActivePath(enginePath2)
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
	_ = readTestLineContaining(t, h.lines, `"id":2`)

	starts := h.starts()
	if len(starts) != 2 {
		t.Fatalf("start paths = %v, want exactly two generations", starts)
	}
	if !samePath(starts[1], enginePath2) {
		t.Fatalf("wake engine path = %q, want latest %q", starts[1], enginePath2)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorEOFWhileDormantExitsWithoutWake(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack", 0)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=send method="+launcherDormantAckMethod)
	h.closeAndWait(t)

	if got := readSupervisorGeneration(t, h.generationFile); got != 1 {
		t.Fatalf("generation after dormant EOF = %d, want 1", got)
	}
}

func TestLauncherSupervisorDrainsTrailingStdoutBeforeDormantExit(t *testing.T) {
	h := newDormantSupervisorHarness(t, "trailing", 0)
	h.initialize(t)
	line := readTestLineContaining(t, h.lines, "test/trailing")
	if strings.Contains(line, launcherDormantAckMethod) {
		t.Fatalf("private ACK leaked to host: %s", line)
	}
	waitSupervisorEvent(t, h.eventFile, "phase=send method="+launcherDormantAckMethod)
	assertNoTestLine(t, h.lines, 100*time.Millisecond)
	if got := readSupervisorGeneration(t, h.generationFile); got != 1 {
		t.Fatalf("generation after trailing stdout drain = %d, want 1", got)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorSilentBeforeAckTimesOutAndFlushes(t *testing.T) {
	h := newDormantSupervisorHarness(t, "silent-before-ack", 0)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=commit method="+launcherCommitDormantMethod)
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`)
	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("response after silent commit = %s, want generation-2", line)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorAckThenStdoutEOFLiveTimesOutAndFlushes(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack-close-stdout-live", 0)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=send method="+launcherDormantAckMethod)
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`)
	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("response after ACK + stdout EOF = %s, want generation-2", line)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorAckWrongExitRestarts(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack-wrong-exit", 0)
	h.initialize(t)
	waitSupervisorGeneration(t, h.generationFile, 2)
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`)
	line := readTestLineContaining(t, h.lines, `"id":2`)
	if !strings.Contains(line, "generation-2") {
		t.Fatalf("response after wrong dormant exit = %s, want generation-2", line)
	}
	h.closeAndWait(t)
}

func TestLauncherSupervisorDrainsMoreThanChannelCapacityAfterAck(t *testing.T) {
	h := newDormantSupervisorHarness(t, "post-ack-many", 0)
	h.initialize(t)
	for i := 0; i < 32; i++ {
		line := readTestLine(t, h.lines)
		want := fmt.Sprintf("test/post-ack-%d", i)
		if !strings.Contains(line, want) {
			t.Fatalf("post-ACK line %d = %s, want %s", i, line, want)
		}
	}
	waitSupervisorGeneration(t, h.generationFile, 2)
	writeTestLine(t, h.stdinW, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`)
	_ = readTestLineContaining(t, h.lines, `"id":2`)
	h.closeAndWait(t)
}

func TestLauncherSupervisorFlushesBufferedFramesFIFO(t *testing.T) {
	h := newDormantSupervisorHarness(t, "ack", 250*time.Millisecond)
	h.initialize(t)
	waitSupervisorEvent(t, h.eventFile, "phase=commit method="+launcherCommitDormantMethod)
	for id := 2; id <= 4; id++ {
		writeTestLine(t, h.stdinW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call"}`, id))
	}
	for id := 2; id <= 4; id++ {
		line := readTestLine(t, h.lines)
		if !strings.Contains(line, fmt.Sprintf(`"id":%d`, id)) || !strings.Contains(line, "generation-2") {
			t.Fatalf("buffered response %d = %s", id, line)
		}
	}
	h.closeAndWait(t)
}

func TestReplayLauncherSupervisorHandshakeWaitsForMatchingID(t *testing.T) {
	childStdinR, childStdinW := io.Pipe()
	childStdoutR, childStdoutW := io.Pipe()
	child := &supervisedEngineChild{
		stdin:  childStdinW,
		stdout: bufio.NewReader(childStdoutR),
	}
	initialized := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(childStdinR)
		if !scanner.Scan() {
			return
		}
		_, _ = fmt.Fprintln(childStdoutW, `{"jsonrpc":"2.0","method":"notifications/test"}`)
		_, _ = fmt.Fprintln(childStdoutW, `{"jsonrpc":"2.0","id":2,"result":{"wrong":true}}`)
		_, _ = fmt.Fprintln(childStdoutW, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
		if scanner.Scan() && strings.Contains(scanner.Text(), "notifications/initialized") {
			close(initialized)
		}
	}()

	response, preserved, err := replayLauncherSupervisorHandshake(
		child,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
		time.Second,
	)
	if err != nil {
		t.Fatalf("replay handshake: %v", err)
	}
	if !strings.Contains(string(response), `"id":1`) {
		t.Fatalf("matching response = %s", response)
	}
	if len(preserved) != 2 || !strings.Contains(string(preserved[0]), "notifications/test") || !strings.Contains(string(preserved[1]), `"id":2`) {
		t.Fatalf("preserved prefix = %q", preserved)
	}
	select {
	case <-initialized:
	case <-time.After(time.Second):
		t.Fatal("initialized notification was not replayed")
	}
	_ = childStdinW.Close()
	_ = childStdoutW.Close()
}

func waitSupervisorEvent(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("timeout waiting for event %q; events:\n%s", want, data)
	return ""
}

func assertEventCount(t *testing.T, events, want string, expected int) {
	t.Helper()
	if got := strings.Count(events, want); got != expected {
		t.Fatalf("event %q count = %d, want %d; events:\n%s", want, got, expected, events)
	}
}

func assertNoTestLine(t *testing.T, lines <-chan string, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case line, ok := <-lines:
		if ok {
			t.Fatalf("unexpected host stdout line: %s", line)
		}
	case <-timer.C:
	}
}

func readSupervisorGeneration(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}
	var generation int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &generation); err != nil {
		t.Fatalf("parse generation %q: %v", data, err)
	}
	return generation
}

func waitSupervisorGeneration(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			var got int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &got); scanErr == nil && got >= want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for generation %d", want)
}
