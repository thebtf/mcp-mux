package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestRunStopDoesNotRemoveLiveDataSocketWhenControlIsStale(t *testing.T) {
	tmp, err := os.MkdirTemp("", "ms-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMP", tmp)

	id := "stop-race-owner"
	ctlPath := filepath.Join(tmp, ownSocketPrefix+id+".ctl.sock")
	dataPath := filepath.Join(tmp, ownSocketPrefix+id+".sock")

	if err := os.WriteFile(ctlPath, []byte("stale control socket placeholder"), 0o600); err != nil {
		t.Fatalf("write stale control placeholder: %v", err)
	}

	ln, err := ipc.Listen(dataPath)
	if err != nil {
		t.Fatalf("listen data socket: %v", err)
	}
	defer ln.Close()

	messages := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				buf := make([]byte, 512)
				n, _ := c.Read(buf)
				if n > 0 {
					select {
					case messages <- string(buf[:n]):
					default:
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})

	if !ipc.IsAvailable(dataPath) {
		t.Fatal("precondition: data socket should be available before stop")
	}

	runStop(0, false)

	if !ipc.IsAvailable(dataPath) {
		t.Fatal("live data socket was removed after stale control cleanup")
	}
	select {
	case msg := <-messages:
		t.Fatalf("unexpected legacy shutdown after live data socket was detected: %q", msg)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestRunStopDoesNotTreatDaemonControlSocketAsOwner(t *testing.T) {
	tmp, err := os.MkdirTemp("", "ms-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMP", tmp)

	daemonCtlPath := filepath.Join(tmp, ownSocketPrefix+"muxd.ctl.sock")
	if err := os.WriteFile(daemonCtlPath, []byte("new daemon control placeholder"), 0o600); err != nil {
		t.Fatalf("write daemon control placeholder: %v", err)
	}

	runStop(0, true)

	if _, err := os.Stat(daemonCtlPath); err != nil {
		t.Fatalf("daemon control socket should be ignored by owner cleanup: %v", err)
	}
}

const stopCommandHelperEnv = "MCPMUX_STOP_COMMAND_HELPER"

func TestRunStopCommandHelper(t *testing.T) {
	if os.Getenv(stopCommandHelperEnv) != "1" {
		return
	}
	os.Args = modernAdmissionMainArgs(t)
	main()
}

type stopOutcomeHandler struct {
	refreshTestHandler
	calls   chan int
	release <-chan struct{}
	err     error
}

func (h *stopOutcomeHandler) HandleShutdownWithError(drainMs int) (string, error) {
	h.calls <- drainMs
	if h.release != nil {
		<-h.release
	}
	return "fixture shutdown accepted", h.err
}

func startStopPingEndpoint(t *testing.T, dir, response string, release <-chan struct{}) <-chan string {
	t.Helper()
	endpoint, err := ipc.Listen(serverid.DaemonControlPath(dir, engineName))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := endpoint.Accept()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			request, _ := bufio.NewReader(conn).ReadString('\n')
			if request != "" {
				requests <- request
				if release != nil {
					<-release
				}
				_, _ = io.WriteString(conn, response)
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		endpoint.Close()
		<-done
	})
	return requests
}

func TestRunStopDaemonOutcome(t *testing.T) {
	for _, tc := range []struct {
		name          string
		delayed       bool
		force         bool
		absent        bool
		stale         bool
		pingFailure   bool
		pingDelayed   bool
		pingResponse  string
		refusal       error
		wantErrorText string
	}{
		{name: "deadline", delayed: true, wantErrorText: "control: read response:"},
		{name: "force-deadline", delayed: true, force: true, wantErrorText: "control: read response:"},
		{name: "unsuccessful-response", refusal: errors.New("outcome unavailable"), wantErrorText: "shutdown: outcome unavailable"},
		{name: "held", force: true, refusal: control.ErrMaintenanceHeld, wantErrorText: "maintenance_held: upstream held for update"},
		{name: "retirement-blocked", refusal: control.ErrMaintenanceRetirementBlocked, wantErrorText: "maintenance_retirement_blocked: maintenance retirement blocked"},
		{name: "invalid-response", refusal: control.ErrMaintenanceInvalid, wantErrorText: "maintenance_invalid: invalid maintenance request or response"},
		{name: "ping-nonOK", pingFailure: true, pingResponse: "{\"ok\":false,\"message\":\"ping outcome unavailable\"}\n", wantErrorText: "ping outcome unavailable"},
		{name: "ping-held", pingFailure: true, force: true, pingResponse: "{\"ok\":false,\"error_code\":\"maintenance_held\"}\n", wantErrorText: "maintenance_held: upstream held for update"},
		{name: "ping-retirement-blocked", pingFailure: true, pingResponse: "{\"ok\":false,\"error_code\":\"maintenance_retirement_blocked\"}\n", wantErrorText: "maintenance_retirement_blocked: maintenance retirement blocked"},
		{name: "ping-malformed", pingFailure: true, pingResponse: "not-json\n", wantErrorText: "control: read response:"},
		{name: "ping-empty-response", pingFailure: true, pingResponse: "{}\n", wantErrorText: "control request failed"},
		{name: "ping-invalid-typed-response", pingFailure: true, pingResponse: "{\"ok\":true,\"error_code\":\"maintenance_held\"}\n", wantErrorText: "maintenance_invalid: invalid maintenance request or response"},
		{name: "ping-read-deadline", pingFailure: true, pingDelayed: true, wantErrorText: "control: read response:"},
		{name: "ping-force-read-deadline", pingFailure: true, pingDelayed: true, force: true, wantErrorText: "control: read response:"},
		{name: "ping-eof", pingFailure: true, wantErrorText: "control: read response: EOF"},
		{name: "success"},
		{name: "absent-daemon", absent: true},
		{name: "stale-daemon-endpoint", absent: true, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortTempDir(t, "stop-outcome")
			t.Setenv("TMPDIR", dir)
			t.Setenv("TEMP", dir)
			t.Setenv("TMP", dir)
			daemon := &stopOutcomeHandler{calls: make(chan int, 2), err: tc.refusal}
			if tc.delayed {
				release := make(chan struct{})
				daemon.release = release
				// Keep dispatch unresolved past the production client's deadline.
				// Release only after observing the command's original outcome.
				defer close(release)
			}
			var pingRequests <-chan string
			if tc.pingFailure {
				var release chan struct{}
				if tc.pingDelayed {
					release = make(chan struct{})
					defer close(release)
				}
				pingRequests = startStopPingEndpoint(t, dir, tc.pingResponse, release)
			} else if !tc.absent {
				startFakeDaemon(t, dir, daemon)
			} else if tc.stale {
				if err := os.WriteFile(serverid.DaemonControlPath(dir, engineName), []byte("private stale daemon endpoint"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			owner := &stopOutcomeHandler{calls: make(chan int, 2)}
			ownerPath := serverid.ControlPath(dir, engineName, "private-owner")
			ownerEndpoint, err := control.NewServer(ownerPath, owner, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(ownerEndpoint.Close)

			legacyPath := serverid.IPCPath(dir, engineName, "private-legacy")
			legacyEndpoint, err := ipc.Listen(legacyPath)
			if err != nil {
				t.Fatal(err)
			}
			legacyMessages := make(chan string, 2)
			legacyDone := make(chan struct{})
			go func() {
				defer close(legacyDone)
				for {
					conn, err := legacyEndpoint.Accept()
					if err != nil {
						return
					}
					_ = conn.SetReadDeadline(time.Now().Add(time.Second))
					message, _ := bufio.NewReader(conn).ReadString('\n')
					conn.Close()
					if message != "" {
						legacyMessages <- message
					}
				}
			}()
			t.Cleanup(func() {
				legacyEndpoint.Close()
				<-legacyDone
			})

			if runtime.GOOS == "windows" {
				// Named pipes do not create the filesystem entries runStop scans.
				// These private markers make the real endpoints discoverable.
				for _, path := range []string{ownerPath, legacyPath} {
					if err := os.WriteFile(path, []byte("private named pipe discovery marker"), 0o600); err != nil {
						t.Fatalf("write endpoint discovery marker: %v", err)
					}
				}
			}

			args := []string{"-test.run=^TestRunStopCommandHelper$", "--", "stop", "--drain-timeout=17ms"}
			wantDrainMs := 17
			if tc.force {
				args = append(args, "--force")
				wantDrainMs = 0
			}
			command := exec.Command(os.Args[0], args...)
			command.Dir = dir
			command.Env = os.Environ()
			for key, value := range map[string]string{
				stopCommandHelperEnv: "1", "MCP_MUX_TEST_MAIN": "0",
				"MCPMUX_ENGINE": "1", "MCPMUX_DISABLE_LAUNCHER": "1",
			} {
				command.Env = setEnv(command.Env, key, value)
			}
			output, commandErr := command.CombinedOutput()
			text := string(output)
			if tc.pingFailure {
				if len(pingRequests) != 1 {
					t.Errorf("daemon preflight calls = %d, want exactly one; output=%q", len(pingRequests), text)
				} else if request := <-pingRequests; request != "{\"cmd\":\"ping\"}\n" {
					t.Errorf("daemon preflight frame = %q, want only ping", request)
				}
				if len(daemon.calls) != 0 || strings.Contains(text, "Stopping daemon...\n") {
					t.Errorf("failed preflight reached daemon shutdown: calls=%d output=%q", len(daemon.calls), text)
				}
			} else if !tc.absent {
				if len(daemon.calls) != 1 {
					t.Errorf("daemon shutdown calls = %d, want exactly one; output=%q", len(daemon.calls), text)
				} else if got := <-daemon.calls; got != wantDrainMs {
					t.Errorf("daemon drain = %d, want %d", got, wantDrainMs)
				}
				if !strings.HasPrefix(text, "Stopping daemon...\n") {
					t.Errorf("command did not follow successful daemon liveness: %q", text)
				}
			}

			if tc.wantErrorText != "" {
				if calls := len(owner.calls); calls != 0 {
					t.Errorf("contacted-daemon failure bypassed authority: owner received %d shutdown calls", calls)
				}
				if !ipc.IsAvailable(legacyPath) {
					t.Error("contacted-daemon failure removed the serving legacy endpoint")
				}
				select {
				case message := <-legacyMessages:
					t.Errorf("contacted-daemon failure reached legacy data shutdown: %q", message)
				case <-time.After(50 * time.Millisecond):
				}
				response, err := control.Send(ownerPath, control.Request{Cmd: "ping"})
				if err != nil || response.Err() != nil || response.Message != "pong" {
					t.Errorf("owner stopped serving after daemon failure: response=%+v error=%v", response, err)
				}
				var exitError *exec.ExitError
				if !errors.As(commandErr, &exitError) || exitError.ExitCode() != 1 {
					t.Errorf("command exit = %v, want status 1; output=%q", commandErr, text)
				}
				if strings.Count(text, "  daemon: error: ") != 1 || !strings.Contains(text, tc.wantErrorText) || strings.Contains(text, "Stopping all mcp-mux instances...") {
					t.Errorf("daemon failure did not terminate with its single original error: %q", text)
				}
				return
			}

			if commandErr != nil {
				t.Fatalf("compatible shutdown failed: %v; output=%q", commandErr, text)
			}
			if len(owner.calls) != 1 {
				t.Fatalf("compatible owner shutdown calls = %d, want one; output=%q", len(owner.calls), text)
			}
			if got := <-owner.calls; got != wantDrainMs {
				t.Errorf("owner drain = %d, want %d", got, wantDrainMs)
			}
			select {
			case message := <-legacyMessages:
				if message != "{\"jsonrpc\":\"2.0\",\"method\":\"mux/shutdown\"}\n" {
					t.Errorf("legacy shutdown frame changed: %q", message)
				}
			case <-time.After(time.Second):
				t.Fatal("compatible shutdown did not reach the data-only legacy endpoint")
			}
			if !strings.Contains(text, "  [private-] fixture shutdown accepted\n") || (!tc.absent && !strings.Contains(text, "  daemon: fixture shutdown accepted\n")) {
				t.Errorf("successful control response message changed: %q", text)
			}
		})
	}
}
