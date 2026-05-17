package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestMain(m *testing.M) {
	if os.Getenv("MCP_MUX_TEST_MAIN") == "1" {
		os.Args = []string{"mcp-mux", "definitely-not-a-real-mcp-server-command"}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix+"*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type refreshTestHandler struct {
	refreshToken string
	refreshErr   error
	prevToken    string
	spawnErr     error
}

func (h *refreshTestHandler) HandleShutdown(int) string { return "ok" }

func (h *refreshTestHandler) HandleStatus() map[string]interface{} {
	return map[string]interface{}{"daemon": true}
}

func (h *refreshTestHandler) HandleSpawn(control.Request) (string, string, string, error) {
	if h.spawnErr != nil {
		return "", "", "", h.spawnErr
	}
	return "", "", "", nil
}

func (h *refreshTestHandler) HandleRemove(string) error { return nil }

func (h *refreshTestHandler) HandleGracefulRestart(int) (string, func(), error) { return "", nil, nil }

func (h *refreshTestHandler) HandleRefreshSessionToken(prevToken string) (string, error) {
	h.prevToken = prevToken
	if h.refreshErr != nil {
		return "", h.refreshErr
	}
	return h.refreshToken, nil
}

func (h *refreshTestHandler) HandleReconnectGiveUp(string) error { return nil }

func (h *refreshTestHandler) HandleListOwners(control.Request) (control.ListOwnersResponse, error) {
	return control.ListOwnersResponse{}, nil
}

func TestRefreshTokenViaDaemon(t *testing.T) {
	tempDir := shortTempDir(t, "rt")
	handler := &refreshTestHandler{refreshToken: "new-token"}
	startFakeDaemon(t, tempDir, handler)

	token, err := refreshTokenViaDaemon("prev-token", log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if err != nil {
		t.Fatalf("refreshTokenViaDaemon() error = %v", err)
	}
	if token != "new-token" {
		t.Fatalf("token = %q, want %q", token, "new-token")
	}
	if handler.prevToken != "prev-token" {
		t.Fatalf("prevToken = %q, want %q", handler.prevToken, "prev-token")
	}
}

func TestRefreshTokenViaDaemonOwnerGone(t *testing.T) {
	tempDir := shortTempDir(t, "ro")
	handler := &refreshTestHandler{refreshErr: daemon.ErrOwnerGone}
	startFakeDaemon(t, tempDir, handler)

	_, err := refreshTokenViaDaemon("prev-token", log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if !errors.Is(err, daemon.ErrOwnerGone) {
		t.Fatalf("refreshTokenViaDaemon() error = %v, want %v", err, daemon.ErrOwnerGone)
	}
}

func TestRefreshTokenViaDaemonUnknownToken(t *testing.T) {
	tempDir := shortTempDir(t, "ru")
	handler := &refreshTestHandler{refreshErr: daemon.ErrUnknownToken}
	startFakeDaemon(t, tempDir, handler)

	_, err := refreshTokenViaDaemon("prev-token", log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if !errors.Is(err, daemon.ErrUnknownToken) {
		t.Fatalf("refreshTokenViaDaemon() error = %v, want %v", err, daemon.ErrUnknownToken)
	}
}

func startFakeDaemon(t *testing.T, tempDir string, handler control.DaemonHandler) {
	t.Helper()
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)
	ctlPath := serverid.DaemonControlPath("", engineName)
	_ = os.Remove(ctlPath)
	t.Cleanup(func() { _ = os.Remove(ctlPath) })

	srv, err := control.NewServer(ctlPath, handler, log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
}

func runHelperMain(t *testing.T, tempDir string, extraEnv ...string) (string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"MCP_MUX_TEST_MAIN=1",
		"MCP_MUX_NO_DAEMON=0",
		"MCP_MUX_ISOLATED=0",
		"MCP_MUX_STATELESS=0",
		"MCP_MUX_DAEMON=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Dir = tempDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}

	err = cmd.Run()
	return stderr.String(), err
}

func TestMainFailsFastOnDaemonSpawnFailure(t *testing.T) {
	tempDir := shortTempDir(t, "ff")
	startFakeDaemon(t, tempDir, &refreshTestHandler{spawnErr: errors.New("forced daemon spawn failure")})

	output, err := runHelperMain(t, tempDir)
	if err == nil {
		t.Fatalf("helper main exited successfully; stderr:\n%s", output)
	}
	if !strings.Contains(output, "shim startup step=daemon_spawn status=error") {
		t.Fatalf("stderr missing daemon spawn failure; stderr:\n%s", output)
	}
	if strings.Contains(output, "fallback=legacy_owner") {
		t.Fatalf("daemon spawn failure fell back to legacy owner; stderr:\n%s", output)
	}
	if strings.Contains(output, "becoming owner") {
		t.Fatalf("daemon spawn failure started a legacy owner; stderr:\n%s", output)
	}
}

func TestMainIsolatedModeStillUsesDaemon(t *testing.T) {
	tempDir := shortTempDir(t, "iso")
	startFakeDaemon(t, tempDir, &refreshTestHandler{spawnErr: errors.New("forced isolated daemon spawn failure")})

	output, err := runHelperMain(t, tempDir, "MCP_MUX_ISOLATED=1")
	if err == nil {
		t.Fatalf("helper main exited successfully; stderr:\n%s", output)
	}
	if !strings.Contains(output, "shim startup step=daemon_spawn status=error") {
		t.Fatalf("isolated mode did not attempt daemon spawn; stderr:\n%s", output)
	}
	if strings.Contains(output, "isolated mode: starting dedicated upstream") {
		t.Fatalf("isolated mode bypassed daemon and started a direct owner; stderr:\n%s", output)
	}
	if strings.Contains(output, "becoming owner") {
		t.Fatalf("isolated daemon failure started a legacy owner; stderr:\n%s", output)
	}
}
