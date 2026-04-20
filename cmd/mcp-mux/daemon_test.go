package main

import (
	"log"
	"os"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

type refreshTestHandler struct {
	refreshToken string
	refreshErr   error
	prevToken    string
}

func (h *refreshTestHandler) HandleShutdown(int) string { return "ok" }

func (h *refreshTestHandler) HandleStatus() map[string]interface{} {
	return map[string]interface{}{"daemon": true}
}

func (h *refreshTestHandler) HandleSpawn(control.Request) (string, string, string, error) {
	return "", "", "", nil
}

func (h *refreshTestHandler) HandleRemove(string) error { return nil }

func (h *refreshTestHandler) HandleGracefulRestart(int) (string, error) { return "", nil }

func (h *refreshTestHandler) HandleRefreshSessionToken(prevToken string) (string, error) {
	h.prevToken = prevToken
	if h.refreshErr != nil {
		return "", h.refreshErr
	}
	return h.refreshToken, nil
}

func (h *refreshTestHandler) HandleReconnectGiveUp(string) error { return nil }

func TestRefreshTokenViaDaemon(t *testing.T) {
	ctlPath := serverid.DaemonControlPath("", "")
	_ = os.Remove(ctlPath)
	t.Cleanup(func() { _ = os.Remove(ctlPath) })

	handler := &refreshTestHandler{refreshToken: "new-token"}
	srv, err := control.NewServer(ctlPath, handler, log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	defer srv.Close()

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
	ctlPath := serverid.DaemonControlPath("", "")
	_ = os.Remove(ctlPath)
	t.Cleanup(func() { _ = os.Remove(ctlPath) })

	handler := &refreshTestHandler{refreshErr: daemon.ErrOwnerGone}
	srv, err := control.NewServer(ctlPath, handler, log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	defer srv.Close()

	_, err = refreshTokenViaDaemon("prev-token", log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if err != daemon.ErrOwnerGone {
		t.Fatalf("refreshTokenViaDaemon() error = %v, want %v", err, daemon.ErrOwnerGone)
	}
}

func TestRefreshTokenViaDaemonUnknownToken(t *testing.T) {
	ctlPath := serverid.DaemonControlPath("", "")
	_ = os.Remove(ctlPath)
	t.Cleanup(func() { _ = os.Remove(ctlPath) })

	handler := &refreshTestHandler{refreshErr: daemon.ErrUnknownToken}
	srv, err := control.NewServer(ctlPath, handler, log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	defer srv.Close()

	_, err = refreshTokenViaDaemon("prev-token", log.New(os.Stderr, "[cmd-test] ", log.LstdFlags))
	if err != daemon.ErrUnknownToken {
		t.Fatalf("refreshTokenViaDaemon() error = %v, want %v", err, daemon.ErrUnknownToken)
	}
}
