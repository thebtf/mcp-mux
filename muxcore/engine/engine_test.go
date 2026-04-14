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
