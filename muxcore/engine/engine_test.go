package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/registry"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// noopHandler is a Handler that does nothing and returns immediately.
var noopHandler Handler = func(_ context.Context, _ io.Reader, _ io.Writer) error {
	return nil
}

type pingOnlyControlHandler struct{}

func (pingOnlyControlHandler) HandleShutdown(int) string {
	return "shutdown ignored"
}

func (pingOnlyControlHandler) HandleStatus() map[string]interface{} {
	return map[string]interface{}{"daemon": true}
}

type runClientControlHandler struct {
	mu            sync.Mutex
	spawnRequests []control.Request
	refreshTokens []string
}

func (h *runClientControlHandler) HandleShutdown(int) string {
	return "shutdown ignored"
}

func (h *runClientControlHandler) HandleStatus() map[string]interface{} {
	return map[string]interface{}{"daemon": true}
}

func (h *runClientControlHandler) HandleSpawn(req control.Request) (string, string, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spawnRequests = append(h.spawnRequests, req)
	if req.ReconnectReason == "fallback_spawn" {
		return "ipc-fallback", "sid-fallback", "token-fallback", nil
	}
	return "ipc-initial", "sid-initial", "token-initial", nil
}

func (h *runClientControlHandler) HandleRemove(string) error {
	return nil
}

func (h *runClientControlHandler) HandleGracefulRestart(int) (string, func(), error) {
	return "", nil, errors.New("not implemented")
}

func (h *runClientControlHandler) HandleRefreshSessionToken(prevToken string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refreshTokens = append(h.refreshTokens, prevToken)
	if prevToken == "token-fallback" {
		return "token-after-fallback", nil
	}
	return "token-refreshed", nil
}

func (h *runClientControlHandler) HandleReconnectGiveUp(string) error {
	return nil
}

func (h *runClientControlHandler) HandleListOwners(control.Request) (control.ListOwnersResponse, error) {
	return control.ListOwnersResponse{}, nil
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
	if err != nil {
		t.Fatalf("New() unexpected error for missing Name: %v", err)
	}
	if e == nil {
		t.Fatal("New() returned nil engine")
	}
	if e.cfg.Name == "" {
		t.Fatal("New() did not derive an engine name")
	}
	if e.cfg.Namespace == "" {
		t.Fatal("New() did not derive an engine namespace")
	}
	if e.cfg.Name == e.cfg.Namespace {
		t.Fatalf("derived namespace = name %q, want collision-resistant namespace", e.cfg.Name)
	}
}

func TestNewDerivesDistinctNamespacesForSameNameDifferentProducts(t *testing.T) {
	origReadBuildInfo := engineReadBuildInfo
	origExecutable := engineExecutable
	t.Cleanup(func() {
		engineReadBuildInfo = origReadBuildInfo
		engineExecutable = origExecutable
	})
	engineExecutable = func() (string, error) { return "ignored.exe", nil }

	engineReadBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Path: "example.com/products/alpha/cmd/server"}, true
	}
	alpha, err := New(Config{Name: "mcp-mux", Command: "echo"})
	if err != nil {
		t.Fatalf("New(alpha): %v", err)
	}

	engineReadBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Path: "example.com/products/beta/cmd/server"}, true
	}
	beta, err := New(Config{Name: "mcp-mux", Command: "echo"})
	if err != nil {
		t.Fatalf("New(beta): %v", err)
	}

	if alpha.cfg.Name != beta.cfg.Name {
		t.Fatalf("display names differ: alpha=%q beta=%q", alpha.cfg.Name, beta.cfg.Name)
	}
	if alpha.cfg.Namespace == beta.cfg.Namespace {
		t.Fatalf("same display Name and different product identity produced same namespace %q", alpha.cfg.Namespace)
	}
	if alpha.cfg.Namespace == "mcp-mux" || beta.cfg.Namespace == "mcp-mux" {
		t.Fatalf("auto namespace fell back to raw display name: alpha=%q beta=%q", alpha.cfg.Namespace, beta.cfg.Namespace)
	}
}

func TestNewDerivesDistinctNamespacesForSameModuleDifferentCommands(t *testing.T) {
	origReadBuildInfo := engineReadBuildInfo
	origExecutable := engineExecutable
	t.Cleanup(func() {
		engineReadBuildInfo = origReadBuildInfo
		engineExecutable = origExecutable
	})
	engineExecutable = func() (string, error) { return "ignored.exe", nil }

	engineReadBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Path: "example.com/suite/cmd/alpha",
			Main: debug.Module{Path: "example.com/suite"},
		}, true
	}
	alpha, err := New(Config{Name: "suite", Command: "echo"})
	if err != nil {
		t.Fatalf("New(alpha): %v", err)
	}

	engineReadBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Path: "example.com/suite/cmd/beta",
			Main: debug.Module{Path: "example.com/suite"},
		}, true
	}
	beta, err := New(Config{Name: "suite", Command: "echo"})
	if err != nil {
		t.Fatalf("New(beta): %v", err)
	}

	if alpha.cfg.Namespace == beta.cfg.Namespace {
		t.Fatalf("same module and display Name for different commands produced same namespace %q", alpha.cfg.Namespace)
	}
}

func TestNewDerivesDistinctNamespacesForSameNameDifferentExecutables(t *testing.T) {
	origReadBuildInfo := engineReadBuildInfo
	origExecutable := engineExecutable
	origGOOS := engineGOOS
	t.Cleanup(func() {
		engineReadBuildInfo = origReadBuildInfo
		engineExecutable = origExecutable
		engineGOOS = origGOOS
	})
	engineReadBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	engineGOOS = "windows"

	engineExecutable = func() (string, error) { return `C:\Products\Alpha\server.exe`, nil }
	alpha, err := New(Config{Name: "server", Command: "echo"})
	if err != nil {
		t.Fatalf("New(alpha): %v", err)
	}

	engineExecutable = func() (string, error) { return `C:\Products\Beta\server.exe`, nil }
	beta, err := New(Config{Name: "server", Command: "echo"})
	if err != nil {
		t.Fatalf("New(beta): %v", err)
	}

	if alpha.cfg.Namespace == beta.cfg.Namespace {
		t.Fatalf("same display Name and executable basename produced same namespace %q", alpha.cfg.Namespace)
	}
}

func TestNewNormalizesWindowsExecutableCaseForNamespaceIdentity(t *testing.T) {
	origReadBuildInfo := engineReadBuildInfo
	origExecutable := engineExecutable
	origGOOS := engineGOOS
	t.Cleanup(func() {
		engineReadBuildInfo = origReadBuildInfo
		engineExecutable = origExecutable
		engineGOOS = origGOOS
	})
	engineReadBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	engineGOOS = "windows"

	engineExecutable = func() (string, error) { return `C:\Products\Alpha\Server.exe`, nil }
	upper, err := New(Config{Name: "server", Command: "echo"})
	if err != nil {
		t.Fatalf("New(upper): %v", err)
	}

	engineExecutable = func() (string, error) { return `c:\products\alpha\server.exe`, nil }
	lower, err := New(Config{Name: "server", Command: "echo"})
	if err != nil {
		t.Fatalf("New(lower): %v", err)
	}

	if upper.cfg.Namespace != lower.cfg.Namespace {
		t.Fatalf("Windows executable path case changed namespace: upper=%q lower=%q", upper.cfg.Namespace, lower.cfg.Namespace)
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

func TestRunDaemonDoesNotStealActiveControlSocket(t *testing.T) {
	baseDir, err := os.MkdirTemp("", "ea*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })

	name := "ea"
	ctlPath := serverid.DaemonControlPath(baseDir, name)

	active, err := control.NewServer(ctlPath, pingOnlyControlHandler{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("start active control server: %v", err)
	}
	defer active.Close()

	if resp, err := control.Send(ctlPath, control.Request{Cmd: "ping"}); err != nil || !resp.OK {
		t.Fatalf("active control server should respond before runDaemon: resp=%+v err=%v", resp, err)
	}

	e, err := New(Config{
		Name:         name,
		Namespace:    name,
		Handler:      noopHandler,
		BaseDir:      baseDir,
		SkipSnapshot: true,
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- e.runDaemon(ctx)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runDaemon() expected active-socket error, got nil")
		}
		if !strings.Contains(err.Error(), "already running") {
			t.Fatalf("runDaemon() error = %v, want already running", err)
		}
	case <-time.After(300 * time.Millisecond):
		cancel()
		err := <-errCh
		t.Fatalf("runDaemon() stole active control socket and kept running; returned after cancel with %v", err)
	}

	if resp, err := control.Send(ctlPath, control.Request{Cmd: "ping"}); err != nil || !resp.OK {
		t.Fatalf("original control server should remain reachable: resp=%+v err=%v", resp, err)
	}
}

func TestRunClientConfiguresRefreshToken(t *testing.T) {
	baseDir, err := os.MkdirTemp("", "er*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })

	name := "er"
	ctlPath := serverid.DaemonControlPath(baseDir, name)

	handler := &runClientControlHandler{}
	srv, err := control.NewServer(ctlPath, handler, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("start control server: %v", err)
	}
	defer srv.Close()

	e, err := New(Config{
		Name:      name,
		Namespace: name,
		Command:   "test-command",
		Args:      []string{"--flag"},
		BaseDir:   baseDir,
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	stopErr := errors.New("stop after capture")
	called := false
	origRunResilientClient := runResilientClient
	runResilientClient = func(cfg owner.ResilientClientConfig) error {
		called = true
		if cfg.RefreshToken == nil {
			t.Fatal("RefreshToken is nil; embedded engine clients cannot refresh consumed reconnect tokens")
		}
		if cfg.Reconnect == nil {
			t.Fatal("Reconnect is nil")
		}
		if cfg.InitialIPCPath != "ipc-initial" {
			t.Fatalf("InitialIPCPath = %q, want ipc-initial", cfg.InitialIPCPath)
		}
		if cfg.Token != "token-initial" {
			t.Fatalf("Token = %q, want token-initial", cfg.Token)
		}

		path, token, err := cfg.RefreshToken()
		if err != nil {
			t.Fatalf("RefreshToken() error: %v", err)
		}
		if path != "ipc-initial" || token != "token-refreshed" {
			t.Fatalf("RefreshToken() = (%q, %q), want (ipc-initial, token-refreshed)", path, token)
		}

		path, token, err = cfg.Reconnect()
		if err != nil {
			t.Fatalf("Reconnect() error: %v", err)
		}
		if path != "ipc-fallback" || token != "token-fallback" {
			t.Fatalf("Reconnect() = (%q, %q), want (ipc-fallback, token-fallback)", path, token)
		}

		path, token, err = cfg.RefreshToken()
		if err != nil {
			t.Fatalf("RefreshToken() after fallback spawn error: %v", err)
		}
		if path != "ipc-fallback" || token != "token-after-fallback" {
			t.Fatalf("RefreshToken() after fallback spawn = (%q, %q), want (ipc-fallback, token-after-fallback)", path, token)
		}
		return stopErr
	}
	defer func() { runResilientClient = origRunResilientClient }()

	if err := e.runClient(context.Background()); !errors.Is(err, stopErr) {
		t.Fatalf("runClient() error = %v, want %v", err, stopErr)
	}
	if !called {
		t.Fatal("runClient did not call runResilientClient")
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()
	wantRefreshTokens := []string{"token-initial", "token-fallback"}
	if strings.Join(handler.refreshTokens, ",") != strings.Join(wantRefreshTokens, ",") {
		t.Fatalf("refreshTokens = %v, want %v", handler.refreshTokens, wantRefreshTokens)
	}
	if len(handler.spawnRequests) != 2 {
		t.Fatalf("spawnRequests = %d, want 2", len(handler.spawnRequests))
	}
	if handler.spawnRequests[0].ReconnectReason != "" {
		t.Fatalf("initial spawn ReconnectReason = %q, want empty", handler.spawnRequests[0].ReconnectReason)
	}
	if handler.spawnRequests[1].ReconnectReason != "fallback_spawn" {
		t.Fatalf("fallback spawn ReconnectReason = %q, want fallback_spawn", handler.spawnRequests[1].ReconnectReason)
	}
}

func TestRunClientReconnectWaitsForSuccessorBeforeSelfStart(t *testing.T) {
	baseDir, err := os.MkdirTemp("", "er*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })

	name := "er-wait"
	ctlPath := serverid.DaemonControlPath(baseDir, name)
	handler := &runClientControlHandler{}
	srv, err := control.NewServer(ctlPath, handler, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("start control server: %v", err)
	}
	defer srv.Close()

	e, err := New(Config{
		Name:      name,
		Namespace: name,
		Command:   "test-command",
		BaseDir:   baseDir,
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	origRunResilientClient := runResilientClient
	origStartDaemon := engineClientStartDaemon
	startDaemonCalls := 0
	engineClientStartDaemon = func(*MuxEngine) error {
		startDaemonCalls++
		return errors.New("self-start should not run while successor is binding")
	}
	t.Cleanup(func() {
		runResilientClient = origRunResilientClient
		engineClientStartDaemon = origStartDaemon
	})

	stopErr := errors.New("stop after refresh")
	runResilientClient = func(cfg owner.ResilientClientConfig) error {
		srv.Close()

		successorReady := make(chan struct{})
		go func() {
			time.Sleep(50 * time.Millisecond)
			successor, err := control.NewServer(ctlPath, handler, log.New(io.Discard, "", 0))
			if err != nil {
				t.Errorf("start successor control server: %v", err)
				close(successorReady)
				return
			}
			t.Cleanup(func() { successor.Close() })
			close(successorReady)
		}()

		path, token, err := cfg.RefreshToken()
		if err != nil {
			t.Fatalf("RefreshToken() error: %v", err)
		}
		if path != "ipc-initial" || token != "token-refreshed" {
			t.Fatalf("RefreshToken() = (%q, %q), want (ipc-initial, token-refreshed)", path, token)
		}
		select {
		case <-successorReady:
		case <-time.After(time.Second):
			t.Fatal("successor server did not start")
		}
		return stopErr
	}

	if err := e.runClient(context.Background()); !errors.Is(err, stopErr) {
		t.Fatalf("runClient() error = %v, want %v", err, stopErr)
	}
	if startDaemonCalls != 0 {
		t.Fatalf("startDaemon calls = %d, want 0", startDaemonCalls)
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

	// ControlSocketPath must be non-empty and match the resolved namespace,
	// not the display Name.
	want := serverid.DaemonControlPath(cfg.BaseDir, eng.cfg.Namespace)
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

func TestEngineRegistryConfigPropagatesToDaemon(t *testing.T) {
	const testFlag = "--test-daemon-registry-propagation"

	baseDir, err := os.MkdirTemp("", "er*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })

	eng, err := New(Config{
		Name:           "er",
		SessionHandler: noopSessionHandler{},
		BaseDir:        baseDir,
		DaemonFlag:     testFlag,
		IdleTimeout:    2 * time.Second,
		SkipSnapshot:   true,
		Registry: &registry.Config{
			ProductName:    "Engine Registry Test",
			MuxcoreVersion: "test-version",
			Capabilities:   registry.Capabilities{ListOwners: true},
		},
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	origArgs := os.Args
	os.Args = append(os.Args, testFlag)
	defer func() { os.Args = origArgs }()

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- eng.Run(ctx) }()

	select {
	case <-eng.Ready():
	case err := <-runErr:
		t.Fatalf("Run() returned before Ready(): %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Ready() did not fire within 5s")
	}

	records, err := registry.ListDescriptors(baseDir)
	if err != nil {
		cancel()
		t.Fatalf("ListDescriptors: %v", err)
	}
	if len(records) != 1 {
		cancel()
		t.Fatalf("registry descriptors = %d, want 1: %+v", len(records), records)
	}
	rec := records[0]
	if rec.Err != nil {
		cancel()
		t.Fatalf("descriptor parse error: %v", rec.Err)
	}
	if rec.Descriptor.EngineName != "er" {
		cancel()
		t.Fatalf("engine_name = %q, want er", rec.Descriptor.EngineName)
	}
	if rec.Descriptor.DaemonControlPath != serverid.DaemonControlPath(baseDir, eng.cfg.Namespace) {
		cancel()
		t.Fatalf("control path = %q, want %q", rec.Descriptor.DaemonControlPath, serverid.DaemonControlPath(baseDir, eng.cfg.Namespace))
	}

	cancel()
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within 5s after cancel")
	}
}

// TestNewRejectsEmptyName verifies that New returns an error with the exact
// required message when cfg.Name is empty.
func TestNewDerivesNameAndNamespaceWhenNameEmpty(t *testing.T) {
	cfg := Config{
		Command: "echo",
		// Name intentionally omitted (empty string)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error for empty Name: %v", err)
	}
	if e == nil {
		t.Fatal("New() returned nil engine")
	}
	if e.cfg.Name == "" {
		t.Fatal("New() did not derive Name")
	}
	if e.cfg.Namespace == "" {
		t.Fatal("New() did not derive Namespace")
	}
	if e.cfg.Namespace == e.cfg.Name {
		t.Fatalf("Namespace = Name = %q, want collision-resistant derived namespace", e.cfg.Name)
	}
}

func TestNewPreservesExplicitNamespaceForLegacyCompatibility(t *testing.T) {
	e, err := New(Config{
		Name:      "aimux",
		Namespace: "Legacy.Namespace_1",
		Command:   "echo",
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if e.cfg.Name != "aimux" {
		t.Fatalf("Name = %q, want aimux", e.cfg.Name)
	}
	if e.cfg.Namespace != "Legacy.Namespace_1" {
		t.Fatalf("Namespace = %q, want Legacy.Namespace_1", e.cfg.Namespace)
	}
}

// TestPersistentPropagatesToDaemonConfig verifies that engine.New preserves
// cfg.Persistent in the engine config, which runDaemon then passes through to
// daemon.Config{Persistent: true}.
func TestPersistentPropagatesToDaemonConfig(t *testing.T) {
	cfg := Config{
		Name:           "test-persistent",
		SessionHandler: noopSessionHandler{},
		Persistent:     true,
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if !e.cfg.Persistent {
		t.Error("engine.cfg.Persistent = false, want true — Persistent not preserved by New()")
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
