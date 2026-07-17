//go:build linux

package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
	"golang.org/x/sys/unix"
)

type failFinalAckUnixConn struct {
	fdConn
	received []uintptr
	objects  []unix.Stat_t
}

func (c *failFinalAckUnixConn) RecvFDs() ([]uintptr, []byte, error) {
	fds, header, err := c.fdConn.RecvFDs()
	if err != nil {
		return nil, nil, err
	}
	for _, fd := range fds {
		var stat unix.Stat_t
		if err := unix.Fstat(int(fd), &stat); err != nil {
			return nil, nil, err
		}
		c.received = append(c.received, fd)
		c.objects = append(c.objects, stat)
	}
	return fds, header, nil
}

func (c *failFinalAckUnixConn) WriteJSON(v any) error {
	switch v.(type) {
	case HandoffAckMsg, *HandoffAckMsg:
		_ = c.fdConn.Close()
		return io.ErrClosedPipe
	default:
		return c.fdConn.WriteJSON(v)
	}
}

func TestHandoffUnix_FinalAckDisconnectClosesTransferredFDsAndStartsOneFallback(t *testing.T) {
	const roleEnv = "MCPMUX_TEST_FINAL_ACK_ROLLBACK_UNIX_ROLE"
	const pidsEnv = "MCPMUX_TEST_FINAL_ACK_ROLLBACK_UNIX_PIDS"
	const snapshotEnv = "MCPMUX_TEST_FINAL_ACK_ROLLBACK_UNIX_SNAPSHOT"
	const oldPIDEnv = "MCPMUX_TEST_FINAL_ACK_ROLLBACK_UNIX_OLD_PID"
	switch os.Getenv(roleEnv) {
	case "upstream":
		pidFile, err := os.OpenFile(os.Getenv(pidsEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open pid file: %v", err)
		}
		_, _ = fmt.Fprintln(pidFile, os.Getpid())
		_ = pidFile.Close()
		go func() {
			for range time.NewTicker(10 * time.Millisecond).C {
				_, _ = fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"handoff"}}`)
			}
		}()
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	case "successor":
		snapshotData, err := base64.StdEncoding.DecodeString(os.Getenv(snapshotEnv))
		if err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		if err := os.WriteFile(SnapshotPath(), snapshotData, 0o600); err != nil {
			t.Fatalf("write successor snapshot: %v", err)
		}
		oldPID, err := strconv.Atoi(os.Getenv(oldPIDEnv))
		if err != nil {
			t.Fatalf("parse predecessor pid: %v", err)
		}
		origWindow := restoreHealthGateWindow
		restoreHealthGateWindow = time.Hour
		defer func() { restoreHealthGateWindow = origWindow }()

		var failedConn *failFinalAckUnixConn
		origHook := dialHandoffHook
		dialHandoffHook = func(name string, timeout time.Duration) (fdConn, error) {
			conn, err := dialHandoffUnix(name, timeout)
			if err != nil {
				return nil, err
			}
			failedConn = &failFinalAckUnixConn{fdConn: conn}
			return failedConn, nil
		}
		defer func() { dialHandoffHook = origHook }()

		d, logs := testDaemonWithLog(t)
		if restored := d.loadSnapshot(); restored != 1 {
			t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
		}
		if activated := d.activateRestartStaging(); activated != 1 {
			t.Fatalf("activateRestartStaging()=%d, want 1; logs:\n%s", activated, logs.String())
		}
		entry := d.Entry("aabbccdd-final-ack-rollback-unix")
		if entry == nil || entry.Owner == nil {
			t.Fatalf("fallback owner missing after final ACK failure; logs:\n%s", logs.String())
		}
		if entry.RestoreSource != "snapshot_fallback" {
			t.Fatalf("restore_source=%q, want snapshot_fallback after final ACK failure; logs:\n%s", entry.RestoreSource, logs.String())
		}
		if !entry.Owner.CacheReady() {
			t.Fatalf("fallback owner lost cached discovery state; logs:\n%s", logs.String())
		}
		if state := entry.Owner.MaterializationState(); state == owner.MaterializationCacheOnly {
			t.Fatalf("fallback materialization state=%s after predecessor barrier, want eager restore", state)
		}
		var newPIDs []int
		waitForDaemonCondition(t, 2*time.Second, func() bool {
			pidData, _ := os.ReadFile(os.Getenv(pidsEnv))
			seen := make(map[int]struct{})
			for _, field := range strings.Fields(string(pidData)) {
				pid, _ := strconv.Atoi(field)
				if pid > 0 && pid != oldPID {
					seen[pid] = struct{}{}
				}
			}
			newPIDs = newPIDs[:0]
			for pid := range seen {
				newPIDs = append(newPIDs, pid)
			}
			return len(newPIDs) == 1
		}, "rollback fallback did not start exactly one fresh upstream generation")
		if pid, _ := entry.Owner.Status()["upstream_pid"].(int); pid != newPIDs[0] {
			t.Fatalf("fallback upstream pid=%d, want sole fresh pid %d", pid, newPIDs[0])
		}
		if failedConn == nil || len(failedConn.received) != 3 {
			t.Fatalf("received fds=%v, want stdin/stdout/stderr", failedConn)
		}
		for i, fd := range failedConn.received {
			var current unix.Stat_t
			if err := unix.Fstat(int(fd), &current); err == nil && current.Dev == failedConn.objects[i].Dev && current.Ino == failedConn.objects[i].Ino {
				t.Fatalf("rolled-back fd %d still refers to transferred object", fd)
			}
		}
		return
	}

	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
	const sid = "aabbccdd-final-ack-rollback-unix"
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	args := []string{"-test.run=^TestHandoffUnix_FinalAckDisconnectClosesTransferredFDsAndStartsOneFallback$"}
	pidPath := filepath.Join(t.TempDir(), "pids")
	upstreamEnv := map[string]string{roleEnv: "upstream", pidsEnv: pidPath}
	proc, err := upstream.Start(exe, args, upstreamEnv, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("start predecessor upstream: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	oldPID, stdinFD, stdoutFD, stderrFD, _, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}

	snap := DaemonSnapshot{
		Version:   mcpsnapshot.SnapshotVersion,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Owners: []mcpsnapshot.OwnerSnapshot{{
			ServerID:       sid,
			Command:        exe,
			Args:           args,
			Cwd:            t.TempDir(),
			Mode:           "global",
			Classification: classify.ModeShared,
			CachedInit:     "e30=",
			CachedTools:    "e30=",
			Env:            upstreamEnv,
		}},
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(SnapshotPath(), data, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	tokenPath := filepath.Join(t.TempDir(), "handoff.token")
	const token = "final-ack-rollback-unix-token"
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	socketPath := filepath.Join(t.TempDir(), "handoff.sock")
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listenHandoffUnix(socketPath, 5*time.Second)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		_, err = performHandoff(context.Background(), conn, token, []HandoffUpstream{{
			ServerID: sid, Command: exe, PID: oldPID,
			StdinFD: stdinFD, StdoutFD: stdoutFD, StderrFD: stderrFD,
			commit: proc.CommitDetach, abort: proc.AbortDetach,
		}})
		serverErr <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handoff socket did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(),
		roleEnv+"=successor",
		oldPIDEnv+"="+strconv.Itoa(oldPID),
		pidsEnv+"="+pidPath,
		snapshotEnv+"="+base64.StdEncoding.EncodeToString(data),
		"MCPMUX_HANDOFF_TOKEN_PATH="+tokenPath,
		"MCPMUX_HANDOFF_SOCKET="+socketPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start successor: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	select {
	case err := <-serverErr:
		if err == nil {
			t.Fatal("predecessor handoff error=nil, want final ACK failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("predecessor did not observe final ACK failure")
	}
	select {
	case <-proc.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("predecessor did not abort the uncommitted upstream")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("successor rollback failed: %v", err)
	}
}

func TestHandoffIntegration_FinalAckWriteFailureAbortsPreparedTreeUnix(t *testing.T) {
	proc, err := upstream.Start("sleep", []string{"30"}, nil, "", nil)
	if err != nil {
		t.Fatalf("start upstream: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	pid, stdinFD, stdoutFD, stderrFD, _, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("detach upstream: %v", err)
	}
	const sid = "unix-final-ack-platform"
	const token = "unix-final-ack-platform-token"
	socketPath := filepath.Join(t.TempDir(), "handoff.sock")
	serverDone := make(chan error, 1)
	go func() {
		conn, listenErr := listenHandoffUnix(socketPath, 5*time.Second)
		if listenErr != nil {
			serverDone <- listenErr
			return
		}
		defer conn.Close()
		_, handoffErr := performHandoff(context.Background(), conn, token, []HandoffUpstream{{
			ServerID: sid, Command: "sleep", PID: pid,
			StdinFD: stdinFD, StdoutFD: stdoutFD, StderrFD: stderrFD,
			commit: proc.CommitDetach, abort: proc.AbortDetach,
		}})
		serverDone <- handoffErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handoff listener did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, err := dialHandoffUnix(socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial handoff: %v", err)
	}
	conn := &failFinalAckUnixConn{fdConn: raw}
	receipt, err := prepareHandoffReceive(context.Background(), conn, token)
	if err != nil {
		t.Fatalf("prepare receive: %v", err)
	}
	received, ok := receipt.take(sid)
	if !ok {
		t.Fatal("transferred process missing from receipt")
	}
	adopted, err := upstream.AttachFromFDsWithAuthority(received.PID, received.StdinFD, received.StdoutFD, received.StderrFD, received.AuthorityFD, received.Command, nil)
	if err != nil {
		_ = receipt.finalize(nil)
		t.Fatalf("attach transferred process: %v", err)
	}
	if err := receipt.finalize([]string{sid}); err == nil {
		_ = adopted.Close()
		t.Fatal("final ACK error=nil, want injected disconnect")
	}
	if err := adopted.Close(); err != nil {
		t.Fatalf("rollback adopted process: %v", err)
	}
	select {
	case err := <-serverDone:
		if err == nil {
			t.Fatal("predecessor committed despite final ACK disconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("predecessor did not abort after final ACK disconnect")
	}
	select {
	case <-proc.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared predecessor tree remained alive")
	}
}

func TestMixedRestartActivatesCacheOnlyOnlyAfterPredecessorBarrier(t *testing.T) {
	const roleEnv = "MCPMUX_TEST_MIXED_BARRIER_ROLE"
	const pidsEnv = "MCPMUX_TEST_MIXED_BARRIER_PIDS"
	const labelEnv = "MCPMUX_TEST_MIXED_BARRIER_LABEL"
	if os.Getenv(roleEnv) == "upstream" {
		pidFile, err := os.OpenFile(os.Getenv(pidsEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open pid file: %v", err)
		}
		_, _ = fmt.Fprintf(pidFile, "%s %d\n", os.Getenv(labelEnv), os.Getpid())
		_ = pidFile.Close()
		go func() {
			for range time.NewTicker(10 * time.Millisecond).C {
				_, _ = fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"barrier"}}`)
			}
		}()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			switch req.Method {
			case "initialize":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"mixed-barrier","version":"1"}}}`+"\n", req.ID)
			case "tools/list":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`+"\n", req.ID)
			}
			if err != nil {
				t.Fatalf("write response: %v", err)
			}
		}
		return
	}

	for _, failFinalAck := range []bool{false, true} {
		name := "success"
		if failFinalAck {
			name = "final-ack-failure"
		}
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(SnapshotPath())
			t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
			tmp := t.TempDir()
			pidPath := filepath.Join(tmp, "pids")
			controlPath := filepath.Join(tmp, "control.sock")
			predecessor, err := New(Config{
				Name:         "mixed-barrier-predecessor",
				ControlPath:  controlPath,
				GracePeriod:  time.Second,
				IdleTimeout:  time.Minute,
				SkipSnapshot: true,
			})
			if err != nil {
				t.Fatalf("New predecessor: %v", err)
			}
			predecessorStopped := false
			t.Cleanup(func() {
				if !predecessorStopped {
					predecessor.Shutdown()
				}
			})

			exe, err := os.Executable()
			if err != nil {
				t.Fatalf("os.Executable: %v", err)
			}
			args := []string{"-test.run=^TestMixedRestartActivatesCacheOnlyOnlyAfterPredecessorBarrier$"}
			envA := map[string]string{roleEnv: "upstream", pidsEnv: pidPath, labelEnv: "A"}
			envB := map[string]string{roleEnv: "upstream", pidsEnv: pidPath, labelEnv: "B"}
			proc, err := upstream.Start(exe, args, envA, tmp, nil)
			if err != nil {
				t.Fatalf("start adopted predecessor: %v", err)
			}
			t.Cleanup(func() { _ = proc.AbortDetach() })
			oldPID, stdinFD, stdoutFD, stderrFD, _, err := proc.DetachWithAuthority()
			if err != nil {
				t.Fatalf("detach adopted predecessor: %v", err)
			}
			const adoptedID = "aabbccdd-mixed-adopted"
			const cacheID = "eeff0011-mixed-cache"
			snapshot := DaemonSnapshot{
				Version:   mcpsnapshot.SnapshotVersion,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Owners: []mcpsnapshot.OwnerSnapshot{
					{ServerID: adoptedID, Command: exe, Args: args, Cwd: tmp, Env: envA, Mode: "global", Classification: classify.ModeShared, CachedInit: "e30=", CachedTools: "e30="},
					{ServerID: cacheID, Command: exe, Args: args, Cwd: tmp, Env: envB, Mode: "global", Classification: classify.ModeShared, CachedInit: "e30=", CachedTools: "e30="},
				},
			}
			data, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatalf("marshal snapshot: %v", err)
			}
			if err := os.WriteFile(SnapshotPath(), data, 0o600); err != nil {
				t.Fatalf("write snapshot: %v", err)
			}
			tokenPath := filepath.Join(tmp, "handoff.token")
			const token = "mixed-barrier-token"
			if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
				t.Fatalf("write token: %v", err)
			}
			handoffPath := filepath.Join(tmp, "handoff.sock")
			handoffDone := make(chan error, 1)
			go func() {
				conn, listenErr := listenHandoffUnix(handoffPath, 5*time.Second)
				if listenErr != nil {
					handoffDone <- listenErr
					return
				}
				defer conn.Close()
				_, handoffErr := performHandoff(context.Background(), conn, token, []HandoffUpstream{{
					ServerID: adoptedID, Command: exe, PID: oldPID,
					StdinFD: stdinFD, StdoutFD: stdoutFD, StderrFD: stderrFD,
					commit: proc.CommitDetach, abort: proc.AbortDetach,
				}})
				handoffDone <- handoffErr
			}()
			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(handoffPath); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("handoff listener did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}

			origDelay := snapshotRestartControlBindDelay
			origInterval := controlBindRetryInterval
			origAttempts := controlBindMaxAttempts
			snapshotRestartControlBindDelay = 0
			controlBindRetryInterval = 10 * time.Millisecond
			controlBindMaxAttempts = 500
			t.Cleanup(func() {
				snapshotRestartControlBindDelay = origDelay
				controlBindRetryInterval = origInterval
				controlBindMaxAttempts = origAttempts
			})
			origHook := dialHandoffHook
			if failFinalAck {
				dialHandoffHook = func(name string, timeout time.Duration) (fdConn, error) {
					conn, dialErr := dialHandoffUnix(name, timeout)
					if dialErr != nil {
						return nil, dialErr
					}
					return &failFinalAckUnixConn{fdConn: conn}, nil
				}
			}
			t.Cleanup(func() { dialHandoffHook = origHook })
			t.Setenv(snapshotRestartEnv, "1")
			t.Setenv("MCPMUX_HANDOFF_TOKEN_PATH", tokenPath)
			t.Setenv("MCPMUX_HANDOFF_SOCKET", handoffPath)
			type newResult struct {
				d   *Daemon
				err error
			}
			successorDone := make(chan newResult, 1)
			go func() {
				d, newErr := New(Config{Name: "mixed-barrier-successor", ControlPath: controlPath, GracePeriod: time.Second, IdleTimeout: time.Minute})
				successorDone <- newResult{d: d, err: newErr}
			}()
			select {
			case handoffErr := <-handoffDone:
				if failFinalAck && handoffErr == nil {
					t.Fatal("handoff succeeded despite injected final ACK failure")
				}
				if !failFinalAck && handoffErr != nil {
					t.Fatalf("handoff failed: %v", handoffErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("handoff did not reach terminal result")
			}
			time.Sleep(100 * time.Millisecond)
			if counts := mixedBarrierPIDCounts(pidPath); counts["A"] != 1 || counts["B"] != 0 {
				t.Fatalf("pre-barrier starts=%v, want only predecessor A", counts)
			}
			select {
			case early := <-successorDone:
				if early.d != nil {
					early.d.Shutdown()
				}
				t.Fatalf("successor escaped predecessor barrier early: %v", early.err)
			default:
			}

			predecessor.Shutdown()
			predecessorStopped = true
			var result newResult
			select {
			case result = <-successorDone:
				if result.err != nil {
					t.Fatalf("New successor: %v", result.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("successor did not cross predecessor barrier")
			}
			successor := result.d
			t.Cleanup(successor.Shutdown)
			wantA, wantB := 1, 1
			if failFinalAck {
				wantA, wantB = 2, 0
			}
			waitForDaemonCondition(t, 3*time.Second, func() bool {
				counts := mixedBarrierPIDCounts(pidPath)
				return counts["A"] == wantA && counts["B"] == wantB
			}, "post-barrier owners did not reach the committed restore outcome")
			time.Sleep(100 * time.Millisecond)
			if counts := mixedBarrierPIDCounts(pidPath); counts["A"] != wantA || counts["B"] != wantB {
				t.Fatalf("post-barrier duplicate starts=%v, want A=%d B=%d", counts, wantA, wantB)
			}
			adoptedEntry := successor.Entry(adoptedID)
			cacheEntry := successor.Entry(cacheID)
			if adoptedEntry == nil {
				t.Fatal("adopted/fallback entry missing")
			}
			if failFinalAck {
				if adoptedEntry.RestoreSource != "snapshot_fallback" || cacheEntry != nil {
					t.Fatalf("failed commit outcome adopted=%q cache=%v, want sole adopted fallback", adoptedEntry.RestoreSource, cacheEntry)
				}
			} else if adoptedEntry.RestoreSource != "snapshot_handoff" || cacheEntry == nil || cacheEntry.RestoreSource != "snapshot_fallback" {
				t.Fatalf("committed outcome adopted=%q cache=%v", adoptedEntry.RestoreSource, cacheEntry)
			}
		})
	}
}

func mixedBarrierPIDCounts(path string) map[string]int {
	data, _ := os.ReadFile(path)
	counts := make(map[string]int)
	fields := strings.Fields(string(data))
	for i := 0; i+1 < len(fields); i += 2 {
		counts[fields[i]]++
	}
	return counts
}
