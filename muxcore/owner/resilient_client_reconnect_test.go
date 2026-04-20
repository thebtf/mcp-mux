package owner

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func newReconnectClient(t *testing.T, stdout io.Writer, logger *log.Logger) *resilientClient {
	t.Helper()
	if stdout == nil {
		stdout = io.Discard
	}
	if logger == nil {
		logger = resilientTestLogger(t)
	}
	return &resilientClient{
		cfg: ResilientClientConfig{
			Stdout:             stdout,
			ReconnectTimeout:   time.Second,
			MaxRefreshAttempts: 3,
		},
		msgFromCC:  make(chan []byte, msgFromCCBufferSize),
		msgFromIPC: make(chan []byte, msgFromIPCBufferSize),
		ipcEOF:     make(chan struct{}),
		stdoutDead: make(chan struct{}),
		startTime:  time.Now(),
		log:        logger,
	}
}

func tempReconnectSocketPath(t *testing.T, sid string) string {
	t.Helper()
	f, err := os.CreateTemp("", "mcp-mux-"+sid+"-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp() error: %v", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func closeReconnectConn(t *testing.T, conn interface {
	io.Reader
	io.Writer
	io.Closer
}) {
	t.Helper()
	if conn == nil {
		return
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() error: %v", err)
	}
}

func TestReconnect_RefreshesAndSucceeds(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	stdout := &bytes.Buffer{}
	rc := newReconnectClient(t, stdout, logger)
	rc.token = "prev-token"

	refreshPath := tempReconnectSocketPath(t, "abcdef123456")
	srv, _ := startEchoIPCServer(t, refreshPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		return refreshPath, "fresh-token", nil
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return "", "", errors.New("spawn should not be called")
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
	if reconnectCalls != 0 {
		t.Fatalf("reconnectCalls = %d, want 0", reconnectCalls)
	}
	if rc.token != "fresh-token" {
		t.Fatalf("rc.token = %q, want %q", rc.token, "fresh-token")
	}
	logText := logs.String()
	if !strings.Contains(logText, "shim.reconnect.refresh_ok owner=abcdef12") {
		t.Fatalf("missing refresh_ok log, got %q", logText)
	}
	if strings.Contains(logText, "shim.reconnect.fallback_spawn") {
		t.Fatalf("unexpected fallback log, got %q", logText)
	}
}

func TestReconnect_FallsBackToSpawnAfterNRejects(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.token = "prev-token"

	spawnPath := tempReconnectSocketPath(t, "fedcba987654")
	srv, _ := startEchoIPCServer(t, spawnPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		return "", "", errors.New("unknown token")
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return spawnPath, "spawn-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 3 {
		t.Fatalf("refreshCalls = %d, want 3", refreshCalls)
	}
	if reconnectCalls != 1 {
		t.Fatalf("reconnectCalls = %d, want 1", reconnectCalls)
	}
	if rc.token != "spawn-token" {
		t.Fatalf("rc.token = %q, want %q", rc.token, "spawn-token")
	}
	logText := logs.String()
	if strings.Count(logText, "shim.reconnect.refresh_fail reason=unknown_token") != 3 {
		t.Fatalf("refresh_fail logs = %d, want 3; logs=%q", strings.Count(logText, "shim.reconnect.refresh_fail reason=unknown_token"), logText)
	}
	if !strings.Contains(logText, "shim.reconnect.fallback_spawn reason=N_refresh_fail") {
		t.Fatalf("missing fallback_spawn log, got %q", logText)
	}
}

func TestReconnect_ImmediateSpawnOnOwnerGone(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.token = "prev-token"

	spawnPath := tempReconnectSocketPath(t, "112233445566")
	srv, _ := startEchoIPCServer(t, spawnPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	refreshCalls := 0
	reconnectCalls := 0
	rc.cfg.RefreshToken = func() (string, string, error) {
		refreshCalls++
		return "", "", errors.New("owner gone")
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return spawnPath, "spawn-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
	if reconnectCalls != 1 {
		t.Fatalf("reconnectCalls = %d, want 1", reconnectCalls)
	}
	logText := logs.String()
	if !strings.Contains(logText, "shim.reconnect.refresh_fail reason=owner_gone") {
		t.Fatalf("missing owner_gone refresh_fail log, got %q", logText)
	}
	if !strings.Contains(logText, "shim.reconnect.fallback_spawn reason=owner_gone") {
		t.Fatalf("missing owner_gone fallback log, got %q", logText)
	}
}

func TestReconnect_RefreshNilFallsBackImmediately(t *testing.T) {
	rc := newReconnectClient(t, io.Discard, resilientTestLogger(t))
	rc.token = "prev-token"

	spawnPath := tempReconnectSocketPath(t, "77889900aabb")
	srv, _ := startEchoIPCServer(t, spawnPath)
	defer srv.closeAll()

	stdinDone := make(chan error)
	reconnectCalls := 0
	rc.cfg.Reconnect = func() (string, string, error) {
		reconnectCalls++
		return spawnPath, "spawn-token", nil
	}

	conn, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	defer closeReconnectConn(t, conn)

	if reconnectCalls != 1 {
		t.Fatalf("reconnectCalls = %d, want 1", reconnectCalls)
	}
	if rc.token != "spawn-token" {
		t.Fatalf("rc.token = %q, want %q", rc.token, "spawn-token")
	}
}

func TestReconnect_NoGoroutineLeakOnRefreshTimeout(t *testing.T) {
	baseline := runtime.NumGoroutine()

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	rc := newReconnectClient(t, io.Discard, logger)
	rc.cfg.ReconnectTimeout = 100 * time.Millisecond
	rc.cfg.MaxRefreshAttempts = 1

	started := make(chan struct{})
	release := make(chan struct{})
	rc.cfg.RefreshToken = func() (string, string, error) {
		close(started)
		<-release
		return "", "", errors.New("unknown token")
	}
	rc.cfg.Reconnect = func() (string, string, error) {
		return "", "", errors.New("spawn should not be called after timeout")
	}

	stdinDone := make(chan error)
	_, err := rc.reconnect(&sync.Mutex{}, stdinDone)
	if err == nil || !strings.Contains(err.Error(), "reconnect timeout") {
		t.Fatalf("reconnect() error = %v, want timeout", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh callback never started")
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("goroutine count did not settle: baseline=%d current=%d logs=%q", baseline, runtime.NumGoroutine(), logs.String())
}

func TestOwnerPrefixFromIPCPath(t *testing.T) {
	path := filepath.Join(os.TempDir(), "mcp-mux-1234567890abcdef.sock")
	if got := ownerPrefixFromIPCPath(path); got != "12345678" {
		t.Fatalf("ownerPrefixFromIPCPath() = %q, want %q", got, "12345678")
	}
}

func closeNetConn(t *testing.T, conn net.Conn) {
	t.Helper()
	if conn != nil {
		if err := conn.Close(); err != nil {
			t.Fatalf("conn.Close() error: %v", err)
		}
	}
}
