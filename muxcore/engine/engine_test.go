package engine

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// noopHandler is a Handler that does nothing and returns immediately.
var noopHandler Handler = func(_ context.Context, _ io.Reader, _ io.Writer) error {
	return nil
}

// TestNew_ValidConfig verifies that New returns a non-nil engine when given a
// minimal valid configuration (Name + Command).
func TestNew_ValidConfig(t *testing.T) {
	cfg := Config{
		Name:    "test-server",
		Command: "echo",
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if e == nil {
		t.Fatal("New() returned nil engine")
	}
}

// TestNew_MissingName verifies that New returns an error when Name is empty.
func TestNew_MissingName(t *testing.T) {
	cfg := Config{
		Command: "echo",
	}
	e, err := New(cfg)
	if err == nil {
		t.Fatal("New() expected error for missing Name, got nil")
	}
	if e != nil {
		t.Fatal("New() expected nil engine on error, got non-nil")
	}
}

// TestNew_MissingCommandAndHandler verifies that New returns an error when
// neither Command nor Handler is provided.
func TestNew_MissingCommandAndHandler(t *testing.T) {
	cfg := Config{
		Name: "test-server",
		// Command and Handler both omitted
	}
	e, err := New(cfg)
	if err == nil {
		t.Fatal("New() expected error for missing Command/Handler, got nil")
	}
	if e != nil {
		t.Fatal("New() expected nil engine on error, got non-nil")
	}
}

// TestNew_Defaults verifies that New applies the documented default values for
// IdleTimeout, ProgressInterval, and DaemonFlag when they are not set in Config.
func TestNew_Defaults(t *testing.T) {
	cfg := Config{
		Name:    "test-server",
		Command: "echo",
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	if e.cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("IdleTimeout default: got %v, want %v", e.cfg.IdleTimeout, 5*time.Minute)
	}
	if e.cfg.ProgressInterval != 5*time.Second {
		t.Errorf("ProgressInterval default: got %v, want %v", e.cfg.ProgressInterval, 5*time.Second)
	}
	if e.cfg.DaemonFlag != "--muxcore-daemon" {
		t.Errorf("DaemonFlag default: got %q, want %q", e.cfg.DaemonFlag, "--muxcore-daemon")
	}
}

// TestNew_ExplicitDefaults verifies that explicitly provided non-zero values
// for IdleTimeout, ProgressInterval, and DaemonFlag are preserved by New.
func TestNew_ExplicitDefaults(t *testing.T) {
	cfg := Config{
		Name:             "test-server",
		Command:          "echo",
		IdleTimeout:      2 * time.Minute,
		ProgressInterval: 10 * time.Second,
		DaemonFlag:       "--custom-daemon",
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	if e.cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout: got %v, want %v", e.cfg.IdleTimeout, 2*time.Minute)
	}
	if e.cfg.ProgressInterval != 10*time.Second {
		t.Errorf("ProgressInterval: got %v, want %v", e.cfg.ProgressInterval, 10*time.Second)
	}
	if e.cfg.DaemonFlag != "--custom-daemon" {
		t.Errorf("DaemonFlag: got %q, want %q", e.cfg.DaemonFlag, "--custom-daemon")
	}
}

// TestIsProxyMode verifies that isProxyMode returns true when MCP_MUX_SESSION_ID
// is set, and false when it is absent.
func TestIsProxyMode(t *testing.T) {
	e, err := New(Config{Name: "test-server", Command: "echo"})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	// Ensure env var is absent first.
	os.Unsetenv("MCP_MUX_SESSION_ID")
	if e.isProxyMode() {
		t.Error("isProxyMode() = true when MCP_MUX_SESSION_ID is unset, want false")
	}

	// Set the env var and re-check.
	t.Setenv("MCP_MUX_SESSION_ID", "test-session-123")
	if !e.isProxyMode() {
		t.Error("isProxyMode() = false when MCP_MUX_SESSION_ID is set, want true")
	}
}

// TestRunProxy_WithHandler verifies that runProxy calls the provided Handler
// with the supplied reader/writer, and that the handler receives the data written
// to its stdin pipe.
func TestRunProxy_WithHandler(t *testing.T) {
	called := false
	var receivedReader io.Reader
	var receivedWriter io.Writer

	handler := Handler(func(_ context.Context, r io.Reader, w io.Writer) error {
		called = true
		receivedReader = r
		receivedWriter = w
		return nil
	})

	e, err := New(Config{
		Name:    "test-server",
		Handler: handler,
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	// Set the env var so isProxyMode() returns true.
	t.Setenv("MCP_MUX_SESSION_ID", "test-session-456")

	// Provide custom pipes so the handler does not read from real os.Stdin.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	// Temporarily redirect os.Stdin / os.Stdout with pipes for runProxy.
	// runProxy uses os.Stdin and os.Stdout directly, so we restore them after.
	origStdin := os.Stdin
	origStdout := os.Stdout
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	// Create an os.File-backed pipe for stdin.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() for stdin: %v", err)
	}
	defer stdinR.Close()
	stdinW.Close() // Close write end immediately — handler gets EOF.

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() for stdout: %v", err)
	}
	defer stdoutR.Close()
	defer stdoutW.Close()

	os.Stdin = stdinR
	os.Stdout = stdoutW

	ctx := context.Background()
	if err := e.runProxy(ctx); err != nil {
		t.Fatalf("runProxy() unexpected error: %v", err)
	}

	if !called {
		t.Fatal("runProxy() did not call the Handler")
	}
	if receivedReader == nil {
		t.Error("Handler received nil reader")
	}
	if receivedWriter == nil {
		t.Error("Handler received nil writer")
	}
}

// TestRunProxy_NoHandler verifies that runProxy returns an error when Handler
// is nil, even when MCP_MUX_SESSION_ID is set.
func TestRunProxy_NoHandler(t *testing.T) {
	e, err := New(Config{
		Name:    "test-server",
		Command: "echo", // satisfies the Command-or-Handler requirement
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Setenv("MCP_MUX_SESSION_ID", "test-session-789")

	ctx := context.Background()
	if err := e.runProxy(ctx); err == nil {
		t.Fatal("runProxy() expected error for nil Handler, got nil")
	}
}

// TestNew_HandlerOnly verifies that New succeeds when only Handler is set
// (no Command), which is the in-process mode configuration.
func TestNew_HandlerOnly(t *testing.T) {
	e, err := New(Config{
		Name:    "test-handler-server",
		Handler: noopHandler,
	})
	if err != nil {
		t.Fatalf("New() unexpected error with Handler-only config: %v", err)
	}
	if e == nil {
		t.Fatal("New() returned nil engine")
	}
	if e.cfg.Handler == nil {
		t.Error("Handler field not preserved in engine config")
	}
	if e.cfg.Command != "" {
		t.Errorf("Command should be empty for Handler-only config, got %q", e.cfg.Command)
	}
}

// noopSessionHandler implements muxcore.SessionHandler for testing.
type noopSessionHandler struct{}

func (noopSessionHandler) HandleRequest(_ context.Context, _ muxcore.ProjectContext, _ []byte) ([]byte, error) {
	return []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), nil
}

// TestEngineConfig_BothHandlerAndSessionHandler verifies that when both Handler
// and SessionHandler are set, New() succeeds and logs a warning that SessionHandler
// takes priority.
func TestEngineConfig_BothHandlerAndSessionHandler(t *testing.T) {
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	cfg := Config{
		Name:           "test-server",
		Handler:        noopHandler,
		SessionHandler: noopSessionHandler{},
		Logger:         logger,
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error when both Handler and SessionHandler set: %v", err)
	}
	if e == nil {
		t.Fatal("New() returned nil engine")
	}

	// Verify the warning was logged.
	logged := logBuf.String()
	if !strings.Contains(logged, "SessionHandler takes priority") {
		t.Errorf("expected warning about SessionHandler priority, got log: %q", logged)
	}

	// Verify both fields are preserved in the config.
	if e.cfg.Handler == nil {
		t.Error("Handler field should be preserved in engine config")
	}
	if e.cfg.SessionHandler == nil {
		t.Error("SessionHandler field should be preserved in engine config")
	}
}

// TestEngineConfig_SessionHandlerOnly verifies that New() succeeds when only
// SessionHandler is set (no Command, no Handler).
func TestEngineConfig_SessionHandlerOnly(t *testing.T) {
	cfg := Config{
		Name:           "test-server",
		SessionHandler: noopSessionHandler{},
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error with SessionHandler-only config: %v", err)
	}
	if e == nil {
		t.Fatal("New() returned nil engine")
	}
	if e.cfg.SessionHandler == nil {
		t.Error("SessionHandler field not preserved in engine config")
	}
}

// TestEngineConfig_MissingAll verifies that New() returns an error when none of
// Command, Handler, or SessionHandler are provided.
func TestEngineConfig_MissingAll(t *testing.T) {
	cfg := Config{
		Name: "test-server",
		// Command, Handler, and SessionHandler all omitted
	}
	e, err := New(cfg)
	if err == nil {
		t.Fatal("New() expected error when Command/Handler/SessionHandler all missing, got nil")
	}
	if e != nil {
		t.Fatal("New() expected nil engine on error, got non-nil")
	}
	if !strings.Contains(err.Error(), "SessionHandler") {
		t.Errorf("error message should mention SessionHandler, got: %v", err)
	}
}

// TestRunProxy_HandlerReceivesData verifies that when runProxy is called with a
// Handler, the handler can both read from its stdin and write to its stdout.
// This exercises the full in-process pipe path.
func TestRunProxy_HandlerReceivesData(t *testing.T) {
	const testPayload = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	receivedLine := make(chan string, 1)
	handler := Handler(func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		buf := make([]byte, 256)
		n, _ := stdin.Read(buf)
		receivedLine <- string(buf[:n])
		_, err := io.WriteString(stdout, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		return err
	})

	e, err := New(Config{
		Name:    "test-server",
		Handler: handler,
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Setenv("MCP_MUX_SESSION_ID", "test-session-data")

	// Build pipe-backed os.Stdin / os.Stdout for runProxy.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() stdin: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() stdout: %v", err)
	}
	defer stdoutR.Close()

	// Write test payload then close write end so handler sees EOF after one read.
	if _, err := io.WriteString(stdinW, testPayload); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	stdinW.Close()

	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinR, stdoutW
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	ctx := context.Background()
	if err := e.runProxy(ctx); err != nil {
		t.Fatalf("runProxy() unexpected error: %v", err)
	}
	stdoutW.Close()
	stdinR.Close()

	select {
	case line := <-receivedLine:
		if line != testPayload {
			t.Errorf("handler received %q, want %q", line, testPayload)
		}
	default:
		t.Error("handler did not receive stdin data")
	}
}

// TestRunProxy_BothHandlersSet_FallsThroughToHandler is a regression test for
// the muxcore engine proxy-mode fallthrough bug. v0.18.0–v0.19.3 had a branch
// in runProxy that returned nil immediately when both Handler and SessionHandler
// were set — the comment claimed "SessionHandler is handled by the daemon", but
// in proxy mode the consumer IS the subprocess being wrapped by an external
// parent shim (e.g. mcp-mux wrapping aimux), and there is no daemon at our
// level. The early return caused the subprocess to exit in ~138ms, which the
// parent shim observed as a dead upstream, triggering restart → circuit
// breaker → permanent FAILED state.
//
// The fix: when both handlers are set, fall through to the legacy Handler
// callback for stdio I/O. SessionHandler is irrelevant in proxy mode because
// we cannot provide Owner-backed session routing from a subprocess.
//
// This test asserts the fix: with both handlers set, runProxy MUST call
// Handler and must NOT return nil without reading stdin.
func TestRunProxy_BothHandlersSet_FallsThroughToHandler(t *testing.T) {
	const testPayload = `{"jsonrpc":"2.0","id":1,"method":"initialize"}`

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	handlerCalled := false
	handlerReadBytes := 0
	handler := Handler(func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		handlerCalled = true
		buf := make([]byte, 256)
		n, _ := stdin.Read(buf)
		handlerReadBytes = n
		_, err := io.WriteString(stdout, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		return err
	})

	// Both handlers set — the condition that triggered the old bug.
	e, err := New(Config{
		Name:           "test-both-handlers",
		Handler:        handler,
		SessionHandler: noopSessionHandler{},
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Setenv("MCP_MUX_SESSION_ID", "sess_regression")

	// Real os.Pipe-backed stdin/stdout so runProxy's direct use of os.Stdin/
	// os.Stdout actually flows through to the Handler.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() stdin: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() stdout: %v", err)
	}
	defer stdoutR.Close()

	if _, err := io.WriteString(stdinW, testPayload); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	stdinW.Close()

	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinR, stdoutW
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	ctx := context.Background()
	if err := e.runProxy(ctx); err != nil {
		t.Fatalf("runProxy() unexpected error: %v", err)
	}
	stdoutW.Close()
	stdinR.Close()

	if !handlerCalled {
		t.Fatal("Handler was NOT called in proxy mode with both handlers set — " +
			"this is the regression bug: runProxy returned early without using Handler, " +
			"which would cause the subprocess to exit and crash-loop under a parent shim")
	}
	if handlerReadBytes == 0 {
		t.Error("Handler was called but did not read any stdin bytes — stdio passthrough broken")
	}

	// The new log line must NOT match the old "skipping Handler" string — that
	// phrasing was the fingerprint of the broken code path.
	logOut := logBuf.String()
	if strings.Contains(logOut, "skipping Handler for proxy mode") {
		t.Errorf("log contains the old 'skipping Handler' marker — bug regressed:\n%s", logOut)
	}
	if !strings.Contains(logOut, "both handlers set, using Handler for stdio passthrough") {
		t.Errorf("log missing the new fallthrough marker:\n%s", logOut)
	}
}

// TestRunProxy_SessionHandlerOnly_ReturnsError verifies that when only
// SessionHandler is set (no Handler), runProxy returns a clear error
// explaining that raw stdio proxy requires Handler. This guards the
// negative case of the fallthrough fix: we only fall through when
// Handler is actually present.
func TestRunProxy_SessionHandlerOnly_ReturnsError(t *testing.T) {
	e, err := New(Config{
		Name:           "test-session-only",
		SessionHandler: noopSessionHandler{},
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Setenv("MCP_MUX_SESSION_ID", "sess_session_only")

	ctx := context.Background()
	err = e.runProxy(ctx)
	if err == nil {
		t.Fatal("runProxy() expected error for SessionHandler-only config, got nil")
	}
	if !strings.Contains(err.Error(), "proxy mode requires Handler") {
		t.Errorf("error should explain that Handler is required, got: %v", err)
	}
	if !strings.Contains(err.Error(), "proxy-mode compatibility") {
		t.Errorf("error should hint that consumers keep Handler for proxy-mode compatibility, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Accessor tests
// ---------------------------------------------------------------------------

// TestEngine_AccessorsBeforeRun verifies the zero-value invariants on a freshly
// created engine before Run() is called.
func TestEngine_AccessorsBeforeRun(t *testing.T) {
	cfg := Config{
		Name:         "test-accessors",
		Command:      "echo",
		BaseDir:      t.TempDir(),
		DaemonFlag:   "--test-daemon-flag-pre",
		IdleTimeout:  2 * time.Second,
		SkipSnapshot: true,
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	// Mode must be ModeUnset before Run().
	if got := eng.Mode(); got != ModeUnset {
		t.Errorf("Mode() before Run() = %v, want ModeUnset", got)
	}

	// Daemon must be nil before Run().
	if d := eng.Daemon(); d != nil {
		t.Errorf("Daemon() before Run() = non-nil, want nil")
	}

	// Ready() must NOT be closed before Run().
	select {
	case <-eng.Ready():
		t.Fatal("Ready() was already closed before Run() — should block")
	default:
		// expected: channel is open
	}

	// ControlSocketPath must be non-empty and match serverid.DaemonControlPath.
	want := serverid.DaemonControlPath(cfg.BaseDir, cfg.Name)
	got := eng.ControlSocketPath()
	if got == "" {
		t.Error("ControlSocketPath() returned empty string")
	}
	if got != want {
		t.Errorf("ControlSocketPath() = %q, want %q", got, want)
	}
}

// TestEngine_DaemonModeExposesDaemon runs an engine in daemon mode and asserts:
//   - Ready() fires within 2 seconds.
//   - Mode() == ModeDaemon after Ready.
//   - Daemon() != nil after Ready, with OwnerCount() == 0.
//   - After ctx cancel and Run() return, Daemon() == nil (deferred reset works).
func TestEngine_DaemonModeExposesDaemon(t *testing.T) {
	// Use a unique flag so this test's daemon-mode trigger doesn't collide with
	// other tests or the default "--muxcore-daemon" flag.
	const testFlag = "--test-daemon-accessor"

	// Windows Unix domain socket paths are limited to ~108 characters.
	// t.TempDir() embeds the full test name, making the socket path too long on
	// Windows when the test name is long. Use a short prefix under os.TempDir().
	baseDir, err := os.MkdirTemp("", "td*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })

	cfg := Config{
		Name:           "td",
		SessionHandler: noopSessionHandler{},
		BaseDir:        baseDir,
		DaemonFlag:     testFlag,
		IdleTimeout:    2 * time.Second,
		SkipSnapshot:   true,
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	// Inject the daemon flag into os.Args for the duration of this test so that
	// isDaemonMode() returns true when Run() is called.
	origArgs := os.Args
	os.Args = append(os.Args, testFlag)
	defer func() { os.Args = origArgs }()

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- eng.Run(ctx) }()

	// Wait for Ready() to fire (daemon bound and accepting).
	select {
	case <-eng.Ready():
		// good
	case err := <-runErr:
		t.Fatalf("Run() returned before Ready() fired: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Ready() did not fire within 5s in daemon mode")
	}

	// Mode must be ModeDaemon.
	if got := eng.Mode(); got != ModeDaemon {
		t.Errorf("Mode() after Ready = %v, want ModeDaemon", got)
	}

	// Daemon() must be non-nil and have zero owners.
	d := eng.Daemon()
	if d == nil {
		t.Fatal("Daemon() == nil after Ready() in daemon mode")
	}
	if n := d.OwnerCount(); n != 0 {
		t.Errorf("OwnerCount() = %d, want 0", n)
	}

	// Shutdown: cancel ctx and wait for Run to return.
	cancel()
	select {
	case <-runErr:
		// Run() returned (context.Canceled or nil from d.Done)
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within 5s after ctx cancel")
	}

	// After shutdown, Daemon() must be nil (deferred reset in runDaemon).
	if d := eng.Daemon(); d != nil {
		t.Error("Daemon() != nil after Run() returned — deferred reset failed")
	}
}

// TestEngine_ProxyModeReady verifies that runProxy calls markReady(ModeProxy)
// before invoking the handler, so Ready() fires and Mode() == ModeProxy.
func TestEngine_ProxyModeReady(t *testing.T) {
	readyFiredBeforeReturn := make(chan struct{}, 1)

	handler := Handler(func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		// At the time the handler is called, markReady has already been called.
		// Signal the test that we can sample Mode/Ready from inside the handler.
		readyFiredBeforeReturn <- struct{}{}
		// Drain stdin so runProxy returns cleanly.
		io.Copy(io.Discard, stdin) //nolint:errcheck
		return nil
	})

	eng, err := New(Config{
		Name:         "test-proxy-accessor",
		Handler:      handler,
		BaseDir:      t.TempDir(),
		IdleTimeout:  2 * time.Second,
		SkipSnapshot: true,
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Setenv("MCP_MUX_SESSION_ID", "sess_proxy_accessor")

	// Pipe-backed stdin (closes immediately so handler drains and returns).
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() stdin: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() stdout: %v", err)
	}
	defer stdoutR.Close()

	stdinW.Close() // EOF immediately

	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinR, stdoutW
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	runDone := make(chan error, 1)
	go func() { runDone <- eng.runProxy(context.Background()) }()

	// Wait for handler to signal (markReady has fired at this point).
	select {
	case <-readyFiredBeforeReturn:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called within 2s")
	}

	// Ready() must be closed.
	select {
	case <-eng.Ready():
		// good
	default:
		t.Error("Ready() not closed after runProxy called markReady")
	}

	// Mode must be ModeProxy.
	if got := eng.Mode(); got != ModeProxy {
		t.Errorf("Mode() = %v, want ModeProxy", got)
	}

	// Daemon must be nil in proxy mode.
	if d := eng.Daemon(); d != nil {
		t.Error("Daemon() != nil in proxy mode — should always be nil")
	}

	stdoutW.Close()
	stdinR.Close()
	<-runDone
}
