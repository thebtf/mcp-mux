package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

func reattachFixturePID(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		return os.Getpid()
	}
	proc, err := upstream.Start("sleep", []string{"30"}, nil, "", nil)
	if err != nil {
		t.Fatalf("start handoff fixture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })
	return proc.PID()
}

// syncBuffer is a thread-safe bytes.Buffer — the daemon writes logs from
// multiple goroutines concurrently (supervisor, reaper, owner callbacks).
// Tests that inspect log content need serialized reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// testDaemonWithLog constructs a daemon whose logger writes into a
// syncBuffer in addition to stderr, so log-based assertions can inspect
// daemon output without racing with concurrent writers.
func testDaemonWithLog(t *testing.T) (*Daemon, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	ctlPath := shortSocketPath(t, "daemon.ctl.sock")
	logger := log.New(buf, "[daemon-test] ", log.LstdFlags)
	d, err := New(Config{
		Name:         "test-daemon",
		ControlPath:  ctlPath,
		GracePeriod:  1 * time.Second,
		IdleTimeout:  5 * time.Second,
		SkipSnapshot: true,
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d, buf
}

func TestLoadSnapshotMetadataOnlyConsumesWithoutRestoringOwners(t *testing.T) {
	os.Remove(SnapshotPath())
	t.Cleanup(func() { os.Remove(SnapshotPath()) })

	sid := "metadata-only-sessionhandler-owner"
	snapshot := DaemonSnapshot{
		Version:          mcpsnapshot.SnapshotVersion,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		DaemonGeneration: "daemon_previous",
		PredecessorPID:   4242,
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "",
				Args:           nil,
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeSessionAware,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}

	d, logBuf := testDaemonWithLog(t)
	deferred := d.loadSnapshotMetadataOnly("test")
	if deferred != 1 {
		t.Fatalf("loadSnapshotMetadataOnly() = %d, want 1", deferred)
	}
	if _, err := os.Stat(SnapshotPath()); !os.IsNotExist(err) {
		t.Fatalf("snapshot file still exists after metadata-only load: %v", err)
	}
	if entry := d.Entry(sid); entry != nil {
		t.Fatalf("metadata-only load restored owner entry: %+v", entry)
	}
	if d.predecessorPID != 4242 {
		t.Fatalf("predecessorPID = %d, want 4242", d.predecessorPID)
	}
	if d.predecessorDaemonGeneration != "daemon_previous" {
		t.Fatalf("predecessorDaemonGeneration = %q, want daemon_previous", d.predecessorDaemonGeneration)
	}
	if _, ok := d.getTemplate("", nil); !ok {
		t.Fatal("ordinary metadata-only load did not retain coherent template cache")
	}
	if !strings.Contains(logBuf.String(), "snapshot: deferred restore of 1 owners (test)") {
		t.Fatalf("missing deferred restore log:\n%s", logBuf.String())
	}
}

func TestRestartMetadataOnlyLoadSuppressesStaleTemplateCache(t *testing.T) {
	os.Remove(SnapshotPath())
	t.Cleanup(func() { os.Remove(SnapshotPath()) })
	t.Setenv(snapshotRestartEnv, "1")

	const command = "restart-metadata-cache"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{{
			ServerID:       "restart-metadata-cache-owner",
			Command:        command,
			Cwd:            t.TempDir(),
			Mode:           "global",
			Classification: classify.ModeShared,
			CachedInit:     "e30=",
			CachedTools:    "e30=",
		}},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}

	d, _ := testDaemonWithLog(t)
	if deferred := d.loadSnapshotMetadataOnly("restart-test"); deferred != 1 {
		t.Fatalf("loadSnapshotMetadataOnly() = %d, want 1", deferred)
	}
	if _, ok := d.getTemplate(command, nil); ok {
		t.Fatal("restart metadata-only load published stale template cache")
	}
}

func TestSnapshotRestartModeRestoresSessionHandlerOwner(t *testing.T) {
	os.Remove(SnapshotPath())
	t.Cleanup(func() { os.Remove(SnapshotPath()) })

	origDelay := snapshotRestartControlBindDelay
	snapshotRestartControlBindDelay = 0
	t.Cleanup(func() { snapshotRestartControlBindDelay = origDelay })
	t.Setenv(snapshotRestartEnv, "1")
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", "")
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "")

	sid := "snapshot-restart-sessionhandler-owner"
	now := time.Now()
	snapshot := DaemonSnapshot{
		Version:          mcpsnapshot.SnapshotVersion,
		Timestamp:        now.UTC().Format(time.RFC3339),
		DaemonGeneration: "daemon_previous",
		PredecessorPID:   4242,
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "",
				Args:           nil,
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeSessionAware,
				BoundTokens: []mcpsnapshot.BoundTokenSnapshot{
					{
						Token:    "prev-token",
						OwnerKey: sid,
						Cwd:      "/project/restart",
						Env:      map[string]string{"A": "B"},
						BoundAt:  now,
						LastUsed: now,
					},
				},
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}

	ctlPath := shortSocketPath(t, "snapshot-restart-sessionhandler.ctl.sock")
	d, err := New(Config{
		Name:           "test-daemon",
		ControlPath:    ctlPath,
		GracePeriod:    1 * time.Second,
		IdleTimeout:    5 * time.Second,
		SessionHandler: noopSessionHandler{},
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	waitOwnerAccepting(t, d, sid)
	if entry := d.Entry(sid); entry == nil || entry.Owner == nil {
		t.Fatal("snapshot restart mode did not restore SessionHandler owner")
	}
	newToken, err := d.HandleRefreshSessionToken("prev-token")
	if err != nil {
		t.Fatalf("HandleRefreshSessionToken() error = %v, want nil", err)
	}
	if newToken == "" || newToken == "prev-token" {
		t.Fatalf("newToken = %q, want distinct non-empty token", newToken)
	}
	if got := d.HandleStatus()["shim_reconnect_fallback_spawned"]; got != uint64(0) {
		t.Fatalf("shim_reconnect_fallback_spawned = %v, want 0", got)
	}
}

func TestSnapshotRestartDefersFreshGenerationUntilControlTakeover(t *testing.T) {
	const roleEnv = "MCPMUX_TEST_SNAPSHOT_RESTART_BARRIER_ROLE"
	if os.Getenv(roleEnv) == "upstream" {
		pidFile, err := os.OpenFile(os.Getenv("MCPMUX_TEST_SNAPSHOT_RESTART_BARRIER_PIDS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open pid file: %v", err)
		}
		_, _ = fmt.Fprintln(pidFile, os.Getpid())
		_ = pidFile.Close()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				t.Fatalf("decode upstream request: %v", err)
			}
			switch req.Method {
			case "initialize":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"snapshot-barrier","version":"1"}}}`+"\n", req.ID)
			case "tools/list":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`+"\n", req.ID)
			}
			if err != nil {
				t.Fatalf("write upstream response: %v", err)
			}
		}
		return
	}

	os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
	pidPath := filepath.Join(t.TempDir(), "upstream-pids.txt")
	ctlPath := shortSocketPath(t, "snapshot-restart-barrier.ctl.sock")

	predecessor, err := New(Config{
		Name:        "snapshot-restart-barrier",
		ControlPath: ctlPath,
		GracePeriod: time.Second,
		IdleTimeout: 5 * time.Second,
		Logger:      testLogger(t),
	})
	if err != nil {
		t.Fatalf("New(predecessor): %v", err)
	}
	predecessorStopped := false
	t.Cleanup(func() {
		if !predecessorStopped {
			predecessor.Shutdown()
		}
	})

	_, sid, _, err := predecessor.Spawn(control.Request{
		Cmd:     "spawn",
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestSnapshotRestartDefersFreshGenerationUntilControlTakeover$"},
		Env: map[string]string{
			roleEnv: "upstream",
			"MCPMUX_TEST_SNAPSHOT_RESTART_BARRIER_PIDS": pidPath,
		},
		Cwd:  t.TempDir(),
		Mode: "global",
	})
	if err != nil {
		t.Fatalf("Spawn(predecessor): %v", err)
	}
	waitForDaemonCondition(t, 3*time.Second, func() bool {
		return len(uniquePIDsFromFile(pidPath)) == 1
	}, "predecessor upstream did not start")
	if _, err := predecessor.SerializeSnapshot(); err != nil {
		t.Fatalf("SerializeSnapshot(): %v", err)
	}

	origDelay := snapshotRestartControlBindDelay
	origInterval := controlBindRetryInterval
	origAttempts := controlBindMaxAttempts
	snapshotRestartControlBindDelay = 0
	controlBindRetryInterval = 20 * time.Millisecond
	controlBindMaxAttempts = 200
	t.Cleanup(func() {
		snapshotRestartControlBindDelay = origDelay
		controlBindRetryInterval = origInterval
		controlBindMaxAttempts = origAttempts
	})
	t.Setenv(snapshotRestartEnv, "1")
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", "")
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "")

	logBuf := &syncBuffer{}
	type newResult struct {
		d   *Daemon
		err error
	}
	successorDone := make(chan newResult, 1)
	go func() {
		d, newErr := New(Config{
			Name:        "snapshot-restart-barrier",
			ControlPath: ctlPath,
			GracePeriod: time.Second,
			IdleTimeout: 5 * time.Second,
			Logger:      log.New(logBuf, "[successor] ", log.LstdFlags),
		})
		successorDone <- newResult{d: d, err: newErr}
	}()

	waitForDaemonCondition(t, 3*time.Second, func() bool {
		return strings.Contains(logBuf.String(), "handoff.control_bind_wait")
	}, "successor did not reach control-takeover barrier")
	time.Sleep(100 * time.Millisecond)
	if pids := uniquePIDsFromFile(pidPath); len(pids) != 1 {
		t.Fatalf("successor started process before control takeover: pids=%v logs:\n%s", pids, logBuf.String())
	}

	predecessor.Shutdown()
	predecessorStopped = true
	result := <-successorDone
	if result.err != nil {
		t.Fatalf("New(successor): %v; logs:\n%s", result.err, logBuf.String())
	}
	successor := result.d
	t.Cleanup(func() { successor.Shutdown() })

	waitForDaemonCondition(t, 3*time.Second, func() bool {
		return len(uniquePIDsFromFile(pidPath)) == 2
	}, "successor did not start exactly one post-barrier generation")
	time.Sleep(150 * time.Millisecond)
	if pids := uniquePIDsFromFile(pidPath); len(pids) != 2 {
		t.Fatalf("snapshot restart created duplicate generations: pids=%v logs:\n%s", pids, logBuf.String())
	}
	entry := successor.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatalf("successor owner %s missing; logs:\n%s", sid, logBuf.String())
	}
	if entry.RestoreSource != "snapshot_fallback" {
		t.Fatalf("restore_source=%q, want snapshot_fallback", entry.RestoreSource)
	}
	if state := entry.Owner.MaterializationState(); state == owner.MaterializationCacheOnly {
		t.Fatalf("post-barrier owner state=%s, want eager generation", state)
	}
}

func uniquePIDsFromFile(path string) []int {
	data, _ := os.ReadFile(path)
	seen := make(map[int]struct{})
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err == nil && pid > 0 {
			seen[pid] = struct{}{}
		}
	}
	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	return pids
}

func TestMixedReadyCacheOnlyRestartHandoffKeepsOneAuthorityPerOwner(t *testing.T) {
	const roleEnv = "MCPMUX_TEST_MIXED_RESTART_HANDOFF_ROLE"
	const pidPathEnv = "MCPMUX_TEST_MIXED_RESTART_HANDOFF_PIDS"
	if os.Getenv(roleEnv) == "upstream" {
		pidFile, err := os.OpenFile(os.Getenv(pidPathEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open mixed restart pid file: %v", err)
		}
		_, _ = fmt.Fprintln(pidFile, os.Getpid())
		_ = pidFile.Close()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				t.Fatalf("decode mixed restart upstream request: %v", err)
			}
			switch req.Method {
			case "initialize":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"mixed-restart","version":"1"}}}`+"\n", req.ID)
			case "tools/list":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`+"\n", req.ID)
			default:
				continue
			}
			if err != nil {
				t.Fatalf("write mixed restart upstream response: %v", err)
			}
		}
		return
	}

	pidPath := filepath.Join(t.TempDir(), "mixed-restart-pids.txt")
	command := os.Args[0]
	args := []string{"-test.run=^TestMixedReadyCacheOnlyRestartHandoffKeepsOneAuthorityPerOwner$"}
	launchEnv := mergeEnv(map[string]string{roleEnv: "upstream", pidPathEnv: pidPath})
	proc, err := upstream.Start(command, args, launchEnv, t.TempDir(), testLogger(t))
	if err != nil {
		t.Fatalf("start adopted handoff process: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	waitForDaemonCondition(t, 3*time.Second, func() bool { return len(uniquePIDsFromFile(pidPath)) == 1 }, "adopted handoff process did not start")

	pid, stdinFD, stdoutFD, stderrFD, authorityFD, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}
	stdinDup := dupRawHandoffHandle(t, stdinFD)
	stdoutDup := dupRawHandoffHandle(t, stdoutFD)
	stderrDup := dupRawHandoffHandle(t, stderrFD)
	authorityDup := dupRawHandoffHandle(t, authorityFD)
	transferred := false
	t.Cleanup(func() {
		if transferred {
			return
		}
		for _, handle := range []uintptr{stdinDup, stdoutDup, stderrDup, authorityDup} {
			closeRawHandoffHandle(handle)
		}
	})

	d := testDaemon(t)
	adoptedSnapshot := daemonMaterializationSnapshot(false)
	adoptedSnapshot.ServerID = "mixed-ready-adopted-owner"
	adoptedSnapshot.Command = command
	adoptedSnapshot.Args = args
	adoptedSnapshot.Cwd = t.TempDir()
	adoptedSnapshot.Env = map[string]string{roleEnv: "upstream", pidPathEnv: pidPath}
	adoptedSnapshot.Mode = "global"
	adoptedPlan := d.makeSnapshotRestorePlan(adoptedSnapshot)
	adoptedEntry, reattached, err := d.restoreSnapshotPlan(adoptedPlan, &HandoffUpstream{
		ServerID:    adoptedSnapshot.ServerID,
		Command:     command,
		PID:         pid,
		StdinFD:     stdinDup,
		StdoutFD:    stdoutDup,
		StderrFD:    stderrDup,
		AuthorityFD: authorityDup,
	}, "snapshot_handoff", false, false)
	if err != nil {
		t.Fatalf("restore adopted plan: %v", err)
	}
	if !reattached {
		t.Fatal("process-backed owner was not reattached")
	}
	transferred = true
	if err := proc.CommitDetach(); err != nil {
		t.Fatalf("CommitDetach: %v", err)
	}
	if state := adoptedEntry.Owner.MaterializationState(); state != owner.MaterializationReady {
		t.Fatalf("adopted owner state=%s, want READY", state)
	}

	cacheSnapshot := daemonMaterializationSnapshot(false)
	cacheSnapshot.ServerID = "mixed-cache-only-owner"
	cacheSnapshot.Command = command
	cacheSnapshot.Args = args
	cacheSnapshot.Cwd = t.TempDir()
	cacheSnapshot.Env = map[string]string{roleEnv: "upstream", pidPathEnv: pidPath}
	cacheSnapshot.Mode = "global"
	cacheEntry, reattached, err := d.restoreSnapshotPlan(d.makeSnapshotRestorePlan(cacheSnapshot), nil, "snapshot_fallback", false, true)
	if err != nil {
		t.Fatalf("restore cache-only plan: %v", err)
	}
	if reattached {
		t.Fatal("no-handle owner unexpectedly reported reattached")
	}
	if state := cacheEntry.Owner.MaterializationState(); state != owner.MaterializationCacheOnly {
		t.Fatalf("no-handle owner state=%s, want CACHE_ONLY", state)
	}
	if pids := uniquePIDsFromFile(pidPath); len(pids) != 1 {
		t.Fatalf("mixed READY/CACHE_ONLY state has pids=%v, want sole adopted authority", pids)
	}
	if pid, _ := cacheEntry.Owner.Status()["upstream_pid"].(int); pid != 0 {
		t.Fatalf("cache-only owner upstream pid=%d, want 0 before activation", pid)
	}

	cacheEntry.Owner.SpawnUpstreamBackground()
	waitForDaemonCondition(t, 3*time.Second, func() bool {
		return cacheEntry.Owner.MaterializationState() == owner.MaterializationReady && len(uniquePIDsFromFile(pidPath)) == 2
	}, "cache-only owner did not activate exactly one eager generation")
	adoptedPID, _ := adoptedEntry.Owner.Status()["upstream_pid"].(int)
	cachePID, _ := cacheEntry.Owner.Status()["upstream_pid"].(int)
	if adoptedPID <= 0 || cachePID <= 0 || adoptedPID == cachePID {
		t.Fatalf("final owner authorities adopted=%d cache=%d, want two distinct positive pids", adoptedPID, cachePID)
	}
	if pids := uniquePIDsFromFile(pidPath); len(pids) != 2 {
		t.Fatalf("mixed restart created duplicate process authorities: %v", pids)
	}
}

func TestLoadSnapshot_Reattach_HappyPath(t *testing.T) {
	os.Remove(SnapshotPath())

	sid := "aabbccdd-happy-reattach-test"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "echo",
				Args:           []string{"hello"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	// Write a handoff token file.
	tokenFile, err := os.CreateTemp("", "mcp-mux-handoff-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenFile.Close()
	const testToken = "happy-path-test-token"
	if err := os.WriteFile(tokenFile.Name(), []byte(testToken), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	defer os.Remove(tokenFile.Name())

	// Create real pipes — AttachFromFDs requires non-zero FDs.
	// stdinR/stdinW: upstream reads from stdinR; we write to stdinW (not used in test).
	// stdoutR/stdoutW: upstream writes to stdoutW; we read from stdoutR (not used in test).
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdin: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	defer stdoutR.Close()
	defer stdoutW.Close()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stderr: %v", err)
	}
	defer stderrR.Close()
	defer stderrW.Close()

	// Prepare mock conn pair: oldDaemonConn (sender) <--> successorConn (receiver).
	oldDaemonConn, successorConn := newMockFDConnPair()

	// Run the old-daemon (sender) side in a background goroutine.
	// Transfer stdinR.Fd() as StdinFD and stdoutW.Fd() as StdoutFD.
	// These are the pipe ends the upstream process would read/write.
	// Capture the performHandoff error on a buffered channel so we can
	// propagate sender-side failures to the test instead of relying on
	// receiveHandoff to time out on a half-done protocol.
	stdinFD := dupFDForHandoff(t, stdinR)
	stdoutFD := dupFDForHandoff(t, stdoutW)
	stderrFD := dupFDForHandoff(t, stderrR)
	senderErrCh := make(chan error, 1)
	go func() {
		upstreams := []HandoffUpstream{
			{
				ServerID: sid,
				Command:  "echo",
				PID:      reattachFixturePID(t),
				StdinFD:  stdinFD,
				StdoutFD: stdoutFD,
				StderrFD: stderrFD,
			},
		}
		ctx := context.Background()
		_, err := performHandoff(ctx, oldDaemonConn, testToken, upstreams)
		senderErrCh <- err
	}()
	t.Cleanup(func() {
		select {
		case err := <-senderErrCh:
			if err != nil {
				t.Errorf("performHandoff (sender): %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("performHandoff (sender) did not return within 500ms")
		}
	})

	// Install dialHandoffHook to return the successor side of the mock conn pair.
	origHook := dialHandoffHook
	dialHandoffHook = func(_ string, _ time.Duration) (fdConn, error) {
		return successorConn, nil
	}
	defer func() { dialHandoffHook = origHook }()

	// Set handoff env vars.
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenFile.Name())
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/mock/socket") // intercepted by hook

	d, logBuf := testDaemonWithLog(t)
	restored := d.loadSnapshot()
	if restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}
	// loadSnapshot keeps no-handle entries metadata-only until the caller
	// proves predecessor finalization. This direct unit test models that gate.
	wantActivated := 0
	if runtime.GOOS == "windows" {
		wantActivated = 1
	}
	activated, err := d.activateRestartStaging()
	if err != nil {
		t.Fatalf("activateRestartStaging() error: %v; logs:\n%s", err, logBuf.String())
	}
	if activated != wantActivated {
		t.Fatalf("activateRestartStaging()=%d, want %d; logs:\n%s", activated, wantActivated, logBuf.String())
	}

	// Assert the handoff result via structural log inspection rather
	// than polling d.owners. The supervisor goroutine started by
	// supervisor.Add(o) can race through o.Serve() → proactive-init write
	// failure → OnUpstreamExit → delete(d.owners, sid) fast enough on
	// Windows scheduling that the entry is never observable in any poll
	// iteration. The daemon's per-owner "reattached from handoff" marker,
	// emitted inside loadSnapshot itself (snapshot.go line 223), is
	// recorded unconditionally before the supervisor goroutine can remove
	// the entry, so log inspection is race-free.
	logs := logBuf.String()

	wantHandoff := "snapshot: reattached owner " + sid[:8] + " from handoff"
	if runtime.GOOS == "windows" {
		if strings.Contains(logs, wantHandoff) {
			t.Errorf("did not expect Windows fixture without Job authority to reattach the owner.\nLogs:\n%s", logs)
		}
		if !strings.Contains(logs, "Windows handoff missing Job authority") || !strings.Contains(logs, "staging eager fallback") {
			t.Errorf("missing safe Windows staged fallback marker.\nLogs:\n%s", logs)
		}
	} else if !strings.Contains(logs, wantHandoff) {
		t.Errorf("expected log %q (handoff path marker), not found.\nLogs:\n%s",
			wantHandoff, logs)
	}

	wantAccepted := 1
	if runtime.GOOS == "windows" {
		wantAccepted = 0
	}
	wantHandoffLog := fmt.Sprintf("handoff.receive.ok upstreams=%d", wantAccepted)
	if !strings.Contains(logs, wantHandoffLog) {
		t.Errorf("expected %q in logs, not found.\nLogs:\n%s",
			wantHandoffLog, logs)
	}
}

func TestLoadSnapshot_Reattach_SocketUnreachable(t *testing.T) {
	os.Remove(SnapshotPath())

	sid := "aabbccdd-fallback-reattach-test"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "echo",
				Args:           []string{"fallback"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	// Write handoff token pointing at a valid file.
	tokenFile, err := os.CreateTemp("", "mcp-mux-handoff-fallback-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenFile.Close()
	if err := os.WriteFile(tokenFile.Name(), []byte("some-token"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	defer os.Remove(tokenFile.Name())

	// Set env vars with an invalid socket path (guaranteed to fail dial).
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenFile.Name())
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/nonexistent/path/to/socket.sock")

	d, logBuf := testDaemonWithLog(t)
	restored := d.loadSnapshot()

	// FR-8 fallback: must restore owner via legacy path (SpawnUpstreamBackground).
	if restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners (want 1); FR-8 fallback failed", restored)
	}
	if _, err := d.activateRestartStaging(); err != nil {
		t.Fatalf("activateRestartStaging() error: %v", err)
	}

	// Structural log assertions replace the d.owners poll (which races with
	// OnUpstreamExit on fast schedulers — see F80-4 and the happy-path fix
	// above). On the FR-8 fallback path the daemon logs exactly the markers
	// we assert below; the ephemeral d.owners entry is irrelevant to the
	// contract this test verifies.
	logs := logBuf.String()

	// Positive: legacy spawn path.
	wantLegacy := "snapshot: restored owner " + sid[:8]
	if !strings.Contains(logs, wantLegacy) {
		t.Errorf("expected log %q (legacy restore marker), not found.\nLogs:\n%s",
			wantLegacy, logs)
	}

	// Positive: handoff receive failed, falling back.
	if !strings.Contains(logs, "handoff.receive.fail") {
		t.Errorf("expected %q in logs (FR-8 trigger), not found.\nLogs:\n%s",
			"handoff.receive.fail", logs)
	}

	// Negative: handoff reattach must NOT have happened.
	dontWantHandoff := "snapshot: reattached owner " + sid[:8] + " from handoff"
	if strings.Contains(logs, dontWantHandoff) {
		t.Errorf("did not expect %q (socket was unreachable, handoff must fail).\nLogs:\n%s",
			dontWantHandoff, logs)
	}
}

func TestLoadSnapshot_Reattach_LegacyV1FallsBackToFreshSpawn(t *testing.T) {
	if marker := os.Getenv("MCPMUX_TEST_V1_FALLBACK_SPAWN"); marker != "" {
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatalf("write spawn marker: %v", err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	os.Remove(SnapshotPath())
	t.Cleanup(func() { os.Remove(SnapshotPath()) })

	const sid = "aabbccdd-version-skew-fallback"
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	spawnMarker := filepath.Join(t.TempDir(), "spawned")
	t.Setenv("MCPMUX_TEST_V1_FALLBACK_SPAWN", spawnMarker)
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{{
			ServerID:       sid,
			Command:        exe,
			Args:           []string{"-test.run=^TestLoadSnapshot_Reattach_LegacyV1FallsBackToFreshSpawn$"},
			Cwd:            ".",
			Mode:           "global",
			Classification: classify.ModeShared,
			CachedInit:     "e30=",
			CachedTools:    "e30=",
		}},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}

	tokenFile, err := os.CreateTemp("", "mcp-mux-handoff-v1-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenPath := tokenFile.Name()
	if err := tokenFile.Close(); err != nil {
		t.Fatalf("Close token: %v", err)
	}
	t.Cleanup(func() { os.Remove(tokenPath) })
	if err := os.WriteFile(tokenPath, []byte("version-skew-token"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}

	legacy := &scriptedHandoffConn{reads: []scriptedHandoffRead{{
		msg: ReadyMsg{
			Type:            MsgReady,
			ProtocolVersion: HandoffProtocolVersion - 1,
			Upstreams: []UpstreamRef{{
				ServerID: sid,
				Command:  "go",
				PID:      os.Getpid(),
			}},
		},
	}}}
	origHook := dialHandoffHook
	dialHandoffHook = func(string, time.Duration) (fdConn, error) { return legacy, nil }
	t.Cleanup(func() { dialHandoffHook = origHook })
	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenPath)
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/mock/legacy-v1")

	d, logBuf := testDaemonWithLog(t)
	if restored := d.loadSnapshot(); restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}

	if _, err := os.Stat(spawnMarker); !os.IsNotExist(err) {
		t.Fatalf("snapshot stage started an upstream before predecessor barrier: err=%v logs:\n%s", err, logBuf.String())
	}
	if entry := d.Entry(sid); entry != nil {
		t.Fatalf("snapshot stage bound owner IPC before predecessor barrier: %#v", entry)
	}
	activated, err := d.activateRestartStaging()
	if err != nil {
		t.Fatalf("activateRestartStaging() error: %v", err)
	}
	if activated != 1 {
		t.Fatalf("activateRestartStaging()=%d, want 1", activated)
	}
	waitForDaemonCondition(t, 2*time.Second, func() bool {
		_, err := os.Stat(spawnMarker)
		return err == nil
	}, "snapshot fallback did not start one eager restore generation")
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatalf("eager fallback owner missing after activation; logs:\n%s", logBuf.String())
	}
	if entry.Owner.CacheReady() {
		t.Fatalf("eager fallback owner exposed stale cached discovery; logs:\n%s", logBuf.String())
	}
	if state := entry.Owner.MaterializationState(); state == owner.MaterializationCacheOnly {
		t.Fatalf("fallback state=%s after predecessor barrier, want eager materialization", state)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "handoff.receive.fail reason=handoff: protocol version mismatch") {
		t.Fatalf("missing v1 rejection marker; logs:\n%s", logs)
	}
	if strings.Contains(logs, "snapshot: reattached owner "+sid[:8]+" from handoff") {
		t.Fatalf("legacy v1 peer was reattached without v2 authority; logs:\n%s", logs)
	}
}

func TestLoadSnapshot_SessionHandlerRestoreDoesNotSpawnCommand(t *testing.T) {
	os.Remove(SnapshotPath())

	sid := "b86e71a0093a84a9"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "definitely-not-a-real-sessionhandler-snapshot-command",
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	buf := &syncBuffer{}
	ctlPath := shortSocketPath(t, "daemon.ctl.sock")
	logger := log.New(buf, "[daemon-test] ", log.LstdFlags)
	d, err := New(Config{
		Name:           "test-daemon",
		ControlPath:    ctlPath,
		GracePeriod:    1 * time.Second,
		IdleTimeout:    5 * time.Second,
		SkipSnapshot:   true,
		SessionHandler: noopSessionHandler{},
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	if restored := d.loadSnapshot(); restored != 1 {
		t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
	}

	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("restored SessionHandler owner missing")
	}
	if state := entry.Owner.MaterializationState(); state != owner.MaterializationReady {
		t.Fatalf("SessionHandler materialization state=%s, want ready", state)
	}
	if pid, _ := entry.Owner.Status()["upstream_pid"].(int); pid != 0 {
		t.Fatalf("SessionHandler restore spawned upstream pid %d", pid)
	}

	logs := buf.String()
	if strings.Contains(logs, "background upstream spawn failed") {
		t.Fatalf("snapshot restore attempted snapshotted command for SessionHandler owner:\n%s", logs)
	}
}

func TestLoadSnapshot_ShutsDownRestoredOwnerWhenGenerationFails(t *testing.T) {
	os.Remove(SnapshotPath())

	sid := "b86e71a0093a84aa"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid,
				Command:        "echo",
				Args:           []string{"restore-failed"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	d, logBuf := testDaemonWithLog(t)
	t.Cleanup(func() { d.Shutdown() })

	oldGenerateToken := generateTokenFunc
	generateTokenFunc = func() (string, error) {
		return "", errors.New("test generation failure")
	}
	t.Cleanup(func() { generateTokenFunc = oldGenerateToken })

	if restored := d.loadSnapshot(); restored != 0 {
		t.Fatalf("loadSnapshot() restored %d owners, want 0", restored)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "snapshot: failed to generate owner generation") {
		t.Fatalf("missing generation failure log:\n%s", logs)
	}
	if !strings.Contains(logs, "owner shut down") {
		t.Fatalf("restored owner was not shut down after generation failure:\n%s", logs)
	}
}

// TestLoadSnapshot_Reattach_PartialHandoff exercises FR-7: when the handoff
// delivers handles for a subset of the snapshot's owners, the remainder MUST
// fall through to the legacy SpawnUpstreamBackground path. On Windows this
// legacy two-handle fixture also lacks Job authority, so both owners safely
// fall back to snapshot respawn instead of adopting an unauthoritative tree.
func TestLoadSnapshot_Reattach_PartialHandoff(t *testing.T) {
	os.Remove(SnapshotPath())

	sid1 := "aabbccdd-partial-handoff-sid1"
	sid2 := "eeff0011-partial-handoff-sid2"
	snapshot := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{
			{
				ServerID:       sid1,
				Command:        "echo",
				Args:           []string{"one"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
			{
				ServerID:       sid2,
				Command:        "echo",
				Args:           []string{"two"},
				Cwd:            t.TempDir(),
				Mode:           "global",
				Classification: classify.ModeShared,
				CachedInit:     "e30=",
				CachedTools:    "e30=",
			},
		},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o644); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	defer os.Remove(SnapshotPath())

	tokenFile, err := os.CreateTemp("", "mcp-mux-handoff-partial-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenFile.Close()
	const testToken = "partial-handoff-test-token"
	if err := os.WriteFile(tokenFile.Name(), []byte(testToken), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	defer os.Remove(tokenFile.Name())

	// Real pipes for sid1 only — sid2 is intentionally omitted from the
	// handoff payload so its reattach fails and the legacy path kicks in.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdin: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	defer stdoutR.Close()
	defer stdoutW.Close()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stderr: %v", err)
	}
	defer stderrR.Close()
	defer stderrW.Close()

	oldDaemonConn, successorConn := newMockFDConnPair()

	stdinFD := dupFDForHandoff(t, stdinR)
	stdoutFD := dupFDForHandoff(t, stdoutW)
	stderrFD := dupFDForHandoff(t, stderrR)
	senderErrCh := make(chan error, 1)
	go func() {
		upstreams := []HandoffUpstream{
			{
				ServerID: sid1, // Only sid1 transferred — sid2 falls through.
				Command:  "echo",
				PID:      reattachFixturePID(t),
				StdinFD:  stdinFD,
				StdoutFD: stdoutFD,
				StderrFD: stderrFD,
			},
		}
		_, err := performHandoff(context.Background(), oldDaemonConn, testToken, upstreams)
		senderErrCh <- err
	}()
	t.Cleanup(func() {
		select {
		case err := <-senderErrCh:
			if err != nil {
				t.Errorf("performHandoff (sender): %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("performHandoff (sender) did not return within 500ms")
		}
	})

	origHook := dialHandoffHook
	dialHandoffHook = func(_ string, _ time.Duration) (fdConn, error) {
		return successorConn, nil
	}
	defer func() { dialHandoffHook = origHook }()

	t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenFile.Name())
	t.Setenv("MCPMUX_HANDOFF_SOCKET", "/mock/socket")

	d, logBuf := testDaemonWithLog(t)
	restored := d.loadSnapshot()
	if restored != 2 {
		t.Fatalf("loadSnapshot() restored %d owners, want 2", restored)
	}
	if _, err := d.activateRestartStaging(); err != nil {
		t.Fatalf("activateRestartStaging() error: %v", err)
	}

	// After the modeled predecessor barrier, structural logs show which
	// entries retained transferred authority and which started one fresh
	// fallback generation. The map itself is intentionally ephemeral here:
	// the test fixtures exit quickly after registration.
	logs := logBuf.String()

	wantSid1Handoff := "snapshot: reattached owner " + sid1[:8] + " from handoff"
	if runtime.GOOS == "windows" {
		if strings.Contains(logs, wantSid1Handoff) {
			t.Errorf("did not expect Windows fixture without Job authority to reattach sid1.\nLogs:\n%s", logs)
		}
		if !strings.Contains(logs, "Windows handoff missing Job authority") || !strings.Contains(logs, "staging eager fallback") {
			t.Errorf("missing safe Windows staged fallback marker.\nLogs:\n%s", logs)
		}
	} else if !strings.Contains(logs, wantSid1Handoff) {
		t.Errorf("expected log %q for sid1 (handoff path), not found.\nLogs:\n%s", wantSid1Handoff, logs)
	}

	// Negative: sid2 was NOT taken via handoff path.
	dontWantSid2Handoff := "snapshot: reattached owner " + sid2[:8] + " from handoff"
	if strings.Contains(logs, dontWantSid2Handoff) {
		t.Errorf("did not expect log %q for sid2 — sid2 was absent from handoff payload.\nLogs:\n%s", dontWantSid2Handoff, logs)
	}

	// The absent sid2 always takes the fresh-generation path. On Windows the
	// fixture intentionally lacks transferable Job authority, so sid1 does too.
	expectedFresh := []string{"snapshot: restored owner " + sid2[:8] + " for echo [two]"}
	if runtime.GOOS == "windows" {
		expectedFresh = append(expectedFresh, "snapshot: restored owner "+sid1[:8]+" for echo [one]")
	}
	for _, expected := range expectedFresh {
		if !strings.Contains(logs, expected) {
			t.Errorf("expected log %q, not found.\nLogs:\n%s", expected, logs)
		}
	}

	wantAccepted := 1
	if runtime.GOOS == "windows" {
		wantAccepted = 0
	}
	wantHandoffLog := fmt.Sprintf("handoff.receive.ok upstreams=%d", wantAccepted)
	if !strings.Contains(logs, wantHandoffLog) {
		t.Errorf("expected %q in logs, not found.\nLogs:\n%s", wantHandoffLog, logs)
	}
}
