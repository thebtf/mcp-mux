package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestShimLifecycleDurationsDefaults(t *testing.T) {
	idle, grace := shimLifecycleDurations(func(string) string { return "" })
	if idle != 10*time.Minute {
		t.Fatalf("idle timeout = %s, want 10m", idle)
	}
	if grace != 30*time.Second {
		t.Fatalf("dormant grace = %s, want 30s", grace)
	}
}

func TestShimLifecycleDurationsNegativeDisables(t *testing.T) {
	values := map[string]string{
		envShimIdleTimeout:  "-1s",
		envShimDormantGrace: "-1s",
	}
	idle, grace := shimLifecycleDurations(func(key string) string { return values[key] })
	if idle >= 0 || grace >= 0 {
		t.Fatalf("negative env must disable: idle=%s grace=%s", idle, grace)
	}
}

func TestShimLifecycleDurationsInvalidFallsBackSafely(t *testing.T) {
	values := map[string]string{
		envShimIdleTimeout:  "garbage",
		envShimDormantGrace: "also-garbage",
	}
	idle, grace := shimLifecycleDurations(func(key string) string { return values[key] })
	if idle != 10*time.Minute || grace != 30*time.Second {
		t.Fatalf("invalid env fallback = (%s, %s), want (10m, 30s)", idle, grace)
	}
}

func TestResilientClientExitCodeMapsDormantSentinel(t *testing.T) {
	if got := resilientClientExitCode(owner.ErrIdleDormant); got != launcherDormantExitCode {
		t.Fatalf("dormant exit code = %d, want %d", got, launcherDormantExitCode)
	}
	if got := resilientClientExitCode(errors.New("boom")); got != 1 {
		t.Fatalf("ordinary error exit code = %d, want 1", got)
	}
}

func TestCanSuspendViaDaemonUnsupportedDisablesGate(t *testing.T) {
	tempDir := shortTempDir(t, "suspend-unsupported")
	startFakeDaemon(t, tempDir, &refreshTestHandler{})

	_, _, err := canSuspendViaDaemon("previous-token", "owner-1")
	if !errors.Is(err, owner.ErrIdleSuspendGateUnavailable) {
		t.Fatalf("canSuspendViaDaemon() error = %v, want ErrIdleSuspendGateUnavailable", err)
	}
}

func TestCanSuspendViaDaemonPermanentWireResponses(t *testing.T) {
	for name, response := range map[string]control.Response{
		// This is the exact default response from muxcore/v0.26.13's control server.
		"v02613_unknown_command": {OK: false, Message: "unknown command: can_suspend"},
		"unknown_token":          {OK: false, Message: "unknown token"},
		"owner_gone":             {OK: false, Message: "owner gone"},
		"unknown_protocol":       {OK: false, Message: "unexpected gate state"},
		"malformed_json":         {OK: true, Data: []byte(`[]`)},
		"missing_allowed":        {OK: true, Data: []byte(`{"reason":"busy"}`)},
		"persistent":             {OK: true, Data: []byte(`{"allowed":false,"reason":"persistent"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			calls := startCanSuspendWireServer(t, shortTempDir(t, name), response)
			_, _, err := canSuspendViaDaemon("previous-token", "owner-1")
			if !errors.Is(err, owner.ErrIdleSuspendGateUnavailable) {
				t.Fatalf("canSuspendViaDaemon() error = %v, want unavailable", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("can_suspend wire calls = %d, want 1", got)
			}
		})
	}
}

func TestCanSuspendViaDaemonDaemonShutdownRetries(t *testing.T) {
	startCanSuspendWireServer(t, shortTempDir(t, "suspend-shutdown"), control.Response{OK: false, Message: "daemon shutting down"})
	_, _, err := canSuspendViaDaemon("previous-token", "owner-1")
	if errors.Is(err, owner.ErrIdleSuspendGateUnavailable) || err == nil {
		t.Fatalf("canSuspendViaDaemon() error = %v, want retryable shutdown error", err)
	}
}

func startCanSuspendWireServer(t *testing.T, tempDir string, response control.Response) *atomic.Int32 {
	t.Helper()
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)
	path := serverid.DaemonControlPath("", engineName)
	_ = os.Remove(path)
	listener, err := ipc.Listen(path)
	if err != nil {
		t.Fatalf("listen legacy can_suspend wire: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(path) })
	var calls atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var req control.Request
				if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
					return
				}
				if req.Cmd == "can_suspend" {
					calls.Add(1)
				}
				_ = json.NewEncoder(conn).Encode(response)
			}()
		}
	}()
	return &calls
}

type suspendTestHandler struct {
	refreshTestHandler
	prevToken string
	serverID  string
}

func (h *suspendTestHandler) HandleCanSuspendForOwner(prevToken, serverID string) (control.SuspendCheckResponse, error) {
	h.prevToken = prevToken
	h.serverID = serverID
	return control.SuspendCheckResponse{Allowed: true}, nil
}

func TestCanSuspendViaDaemonBindsExactOwner(t *testing.T) {
	tempDir := shortTempDir(t, "suspend-owner")
	handler := &suspendTestHandler{}
	startFakeDaemon(t, tempDir, handler)

	allowed, reason, err := canSuspendViaDaemon("previous-token", "owner-1")
	if err != nil || !allowed || reason != "" {
		t.Fatalf("canSuspendViaDaemon() = (%v, %q, %v), want allowed", allowed, reason, err)
	}
	if handler.prevToken != "previous-token" || handler.serverID != "owner-1" {
		t.Fatalf("suspend request = token %q owner %q, want previous-token owner-1", handler.prevToken, handler.serverID)
	}
}
