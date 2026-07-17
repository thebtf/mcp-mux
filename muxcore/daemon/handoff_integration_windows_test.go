//go:build windows

package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
	"golang.org/x/sys/windows"
)

type failFinalAckCaptureConn struct {
	fdConn
	received  []uintptr
	observers []windows.Handle
}

func (c *failFinalAckCaptureConn) RecvFDs() ([]uintptr, []byte, error) {
	fds, header, err := c.fdConn.RecvFDs()
	if err != nil {
		return nil, nil, err
	}
	for _, fd := range fds {
		var observer windows.Handle
		if err := windows.DuplicateHandle(
			windows.CurrentProcess(), windows.Handle(fd),
			windows.CurrentProcess(), &observer,
			0, false, windows.DUPLICATE_SAME_ACCESS,
		); err != nil {
			for _, handle := range c.observers {
				_ = windows.CloseHandle(handle)
			}
			c.observers = nil
			return nil, nil, err
		}
		c.observers = append(c.observers, observer)
	}
	c.received = append(c.received, fds...)
	return fds, header, err
}

func (c *failFinalAckCaptureConn) WriteJSON(v any) error {
	switch v.(type) {
	case HandoffAckMsg, *HandoffAckMsg:
		_ = c.fdConn.Close()
		return io.ErrClosedPipe
	default:
		return c.fdConn.WriteJSON(v)
	}
}

var compareObjectHandlesProc = windows.NewLazySystemDLL("kernelbase.dll").NewProc("CompareObjectHandles")

func sameKernelObject(a, b windows.Handle) bool {
	equal, _, _ := compareObjectHandlesProc.Call(uintptr(a), uintptr(b))
	return equal != 0
}

// TestHandoffIntegration_FullRoundtripWindows runs the full handoff
// protocol between two goroutines over a named pipe with DuplicateHandle
// FD transfer. Drives the protocol manually to bypass the non-wired
// performHandoff/receiveHandoff (T020 does the wiring).
func TestHandoffIntegration_FullRoundtripWindows(t *testing.T) {
	var nameBuf [8]byte
	if _, err := rand.Read(nameBuf[:]); err != nil {
		t.Fatal(err)
	}
	pipeName := "int-win-" + hex.EncodeToString(nameBuf[:])
	token := "windows-integration-test-token"

	tmp, err := os.CreateTemp("", "handoff-win-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmp.Name()); _ = tmp.Close() }()
	content := []byte("handoff-roundtrip-payload")
	if _, err := tmp.Write(content); err != nil {
		t.Fatal(err)
	}

	upstreams := []UpstreamRef{
		{ServerID: "win-s1", Command: "test.exe", PID: os.Getpid()},
	}

	ln, err := listenHandoffPipe(pipeName)
	if err != nil {
		t.Fatalf("listenHandoffPipe: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	var serverErr error
	var transferredCount int

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer conn.Close()
		fdc := newWindowsFDConn(conn)

		// Read Hello (with SourcePID).
		var hello HelloMsg
		if err := fdc.ReadJSON(&hello); err != nil {
			serverErr = err
			return
		}
		if err := validateProtocolVersion(hello.ProtocolVersion); err != nil {
			serverErr = err
			return
		}
		if hello.Token != token {
			serverErr = ErrTokenMismatch
			return
		}
		if hello.SourcePID == 0 {
			serverErr = ErrTokenMismatch // reuse sentinel as generic protocol violation
			return
		}
		fdc.SetTargetPID(hello.SourcePID)

		// Send Ready.
		if err := fdc.WriteJSON(NewReadyMsg(upstreams)); err != nil {
			serverErr = err
			return
		}

		// Per upstream: SendFDs with tmpFile handle, then read AckTransfer.
		for _, u := range upstreams {
			hdr := []byte(u.ServerID)
			if err := fdc.SendFDs([]uintptr{tmp.Fd()}, hdr); err != nil {
				serverErr = err
				return
			}
			var ack AckTransferMsg
			if err := fdc.ReadJSON(&ack); err != nil {
				serverErr = err
				return
			}
			if !ack.OK {
				serverErr = ErrTokenMismatch
				return
			}
			transferredCount++
		}

		// Send Done.
		if err := fdc.WriteJSON(NewDoneMsg([]string{upstreams[0].ServerID}, nil)); err != nil {
			serverErr = err
			return
		}

		// Read HandoffAck.
		var finalAck HandoffAckMsg
		serverErr = fdc.ReadJSON(&finalAck)
	}()

	// listenHandoffPipe already returned a bound listener above, so the pipe is
	// ready to accept before the goroutine even starts. No sleep needed.
	conn, err := dialHandoffPipe(pipeName, 2*time.Second)
	if err != nil {
		t.Fatalf("dialHandoffPipe: %v", err)
	}
	defer conn.Close()
	client := newWindowsFDConn(conn)

	// Send Hello with our PID (so server can DuplicateHandle into us).
	if err := client.WriteJSON(NewHelloMsgWithPID(token, os.Getpid())); err != nil {
		t.Fatalf("client Hello: %v", err)
	}

	// Read Ready.
	var ready ReadyMsg
	if err := client.ReadJSON(&ready); err != nil {
		t.Fatalf("client Ready: %v", err)
	}
	if len(ready.Upstreams) != 1 {
		t.Fatalf("Ready.Upstreams: %v", ready.Upstreams)
	}

	// Per upstream: RecvFDs, send AckTransfer{ok: true}.
	for _, u := range ready.Upstreams {
		fds, hdr, err := client.RecvFDs()
		if err != nil {
			t.Fatalf("RecvFDs: %v", err)
		}
		if len(fds) != 1 {
			t.Fatalf("expected 1 fd, got %d", len(fds))
		}
		if string(hdr) != u.ServerID {
			t.Errorf("header %q != ServerID %q", hdr, u.ServerID)
		}

		// Verify the DuplicateHandle'd FD is actually usable -- read the content.
		dup := os.NewFile(fds[0], "dup")
		if dup == nil {
			t.Fatal("os.NewFile returned nil for duplicated handle")
		}
		if _, err := dup.Seek(0, 0); err != nil {
			t.Fatalf("seek dup: %v", err)
		}
		got := make([]byte, len(content))
		if _, err := dup.Read(got); err != nil {
			t.Fatalf("read via duplicated handle: %v", err)
		}
		if string(got) != string(content) {
			t.Errorf("duplicated handle content %q != source %q", got, content)
		}
		_ = dup.Close()

		if err := client.WriteJSON(NewAckTransferMsg(u.ServerID, true, nil)); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	}

	// Read Done.
	var done DoneMsg
	if err := client.ReadJSON(&done); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if len(done.Transferred) != 1 {
		t.Errorf("Done.Transferred: %v", done.Transferred)
	}

	// Send final HandoffAck.
	if err := client.WriteJSON(NewHandoffAckMsg("accepted")); err != nil {
		t.Fatalf("HandoffAck: %v", err)
	}

	wg.Wait()
	if serverErr != nil {
		t.Fatalf("server: %v", serverErr)
	}
	if transferredCount != 1 {
		t.Errorf("transferredCount: got %d, want 1", transferredCount)
	}
}

func TestHandoffIntegration_FourHandleAuthoritySurvivesPredecessorRelease(t *testing.T) {
	if os.Getenv("MCPMUX_TEST_HANDOFF_SUCCESSOR") == "1" {
		conn, err := dialHandoffWindows(os.Getenv("MCPMUX_TEST_HANDOFF_PIPE"), 5*time.Second)
		if err != nil {
			t.Fatalf("successor dial: %v", err)
		}
		receipt, err := prepareHandoffReceive(context.Background(), conn, "authority-token")
		if err != nil {
			t.Fatalf("successor receive: %v", err)
		}
		received, ok := receipt.take("authority-roundtrip")
		if !ok {
			t.Fatal("successor did not receipt four-handle transfer")
		}
		adopted, err := upstream.AttachFromFDsWithAuthority(
			received.PID, received.StdinFD, received.StdoutFD, received.StderrFD, received.AuthorityFD, received.Command, nil,
		)
		if err != nil {
			_ = receipt.finalize(nil)
			t.Fatalf("successor adoption: %v", err)
		}
		if err := receipt.finalize([]string{"authority-roundtrip"}); err != nil {
			t.Fatalf("successor finalize: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv("MCPMUX_TEST_HANDOFF_COMMITTED")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("predecessor did not publish commit marker")
			}
			time.Sleep(10 * time.Millisecond)
		}
		processHandle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(received.PID))
		if err != nil {
			t.Fatalf("OpenProcess adopted upstream: %v", err)
		}
		defer windows.CloseHandle(processHandle)
		if status, err := windows.WaitForSingleObject(processHandle, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("upstream did not survive predecessor release: WaitForSingleObject = (%d, %v)", status, err)
		}
		if err := windows.TerminateJobObject(windows.Handle(received.AuthorityFD), 1); err != nil {
			t.Fatalf("transferred Job authority is unusable after predecessor release: %v", err)
		}
		if status, err := windows.WaitForSingleObject(processHandle, 5000); err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatalf("WaitForSingleObject adopted upstream = (%d, %v), want process exit", status, err)
		}
		if err := adopted.Close(); err != nil {
			t.Fatalf("adopted Close: %v", err)
		}
		return
	}

	proc, err := upstream.Start("ping", []string{"-n", "30", "127.0.0.1"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })

	pid, stdinFD, stdoutFD, stderrFD, authorityFD, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}
	if stdinFD == 0 || stdoutFD == 0 || stderrFD == 0 || authorityFD == 0 {
		t.Fatalf("detached handles = [%d %d %d %d], want four non-zero handles", stdinFD, stdoutFD, stderrFD, authorityFD)
	}
	name := randomPipeName(t)
	connCh := make(chan fdConn, 1)
	go func() {
		conn, _ := listenHandoffWindows(name, 5*time.Second)
		connCh <- conn
	}()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	commitMarker := filepath.Join(t.TempDir(), "committed")
	cmd := exec.Command(exe, "-test.run=^TestHandoffIntegration_FourHandleAuthoritySurvivesPredecessorRelease$")
	cmd.Env = append(os.Environ(),
		"MCPMUX_TEST_HANDOFF_SUCCESSOR=1",
		"MCPMUX_TEST_HANDOFF_PIPE="+name,
		"MCPMUX_TEST_HANDOFF_COMMITTED="+commitMarker,
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
	conn := <-connCh
	if conn == nil {
		t.Fatal("successor did not connect to handoff pipe")
	}
	defer conn.Close()
	_, handoffErr := performHandoff(context.Background(), conn, "authority-token", []HandoffUpstream{{
		ServerID:    "authority-roundtrip",
		Command:     "ping",
		PID:         pid,
		StdinFD:     stdinFD,
		StdoutFD:    stdoutFD,
		StderrFD:    stderrFD,
		AuthorityFD: authorityFD,
		commit:      proc.CommitDetach,
		abort:       proc.AbortDetach,
	}})
	if err := os.WriteFile(commitMarker, nil, 0o600); err != nil {
		t.Fatalf("write commit marker: %v", err)
	}
	if handoffErr != nil {
		t.Fatalf("performHandoff: %v", handoffErr)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("successor failed: %v", err)
	}
}

func TestHandoffIntegration_UncommittedFourHandlesAreClosed(t *testing.T) {
	proc, err := upstream.Start("ping", []string{"-n", "30", "127.0.0.1"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	pid, stdinFD, stdoutFD, stderrFD, authorityFD, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}

	name := randomPipeName(t)
	ln, err := listenHandoffPipe(name)
	if err != nil {
		t.Fatalf("listenHandoffPipe: %v", err)
	}
	defer ln.Close()
	senderResult := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err == nil {
			_, err = performHandoff(context.Background(), newWindowsFDConn(raw), "abort-token", []HandoffUpstream{{
				ServerID: "uncommitted", Command: "ping", PID: pid,
				StdinFD: stdinFD, StdoutFD: stdoutFD, StderrFD: stderrFD, AuthorityFD: authorityFD,
				commit: proc.CommitDetach, abort: proc.AbortDetach,
			}})
		}
		senderResult <- err
	}()

	raw, err := dialHandoffPipe(name, 2*time.Second)
	if err != nil {
		t.Fatalf("dialHandoffPipe: %v", err)
	}
	receipt, err := prepareHandoffReceive(context.Background(), newWindowsFDConn(raw), "abort-token")
	if err != nil {
		t.Fatalf("prepareHandoffReceive: %v", err)
	}
	received := receipt.received["uncommitted"]
	if err := receipt.finalize(nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := <-senderResult; err != nil {
		t.Fatalf("performHandoff: %v", err)
	}
	select {
	case <-proc.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("predecessor did not abort the uncommitted upstream")
	}
	for _, handle := range []uintptr{received.StdinFD, received.StdoutFD, received.StderrFD} {
		if _, err := windows.GetFileType(windows.Handle(handle)); err == nil {
			t.Fatalf("uncommitted stdio handle %d remained open", handle)
		}
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(
		windows.Handle(received.AuthorityFD), windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil,
	); err == nil {
		t.Fatalf("uncommitted Job handle %d remained open", received.AuthorityFD)
	}
}

func TestLoadSnapshot_FinalAckWriteFailureRollsBackAdoptedOwner(t *testing.T) {
	const roleEnv = "MCPMUX_TEST_FINAL_ACK_ROLLBACK_ROLE"
	switch os.Getenv(roleEnv) {
	case "upstream":
		pidFile, err := os.OpenFile(os.Getenv("MCPMUX_TEST_FINAL_ACK_ROLLBACK_PIDS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
		snapshotData, err := base64.StdEncoding.DecodeString(os.Getenv("MCPMUX_TEST_FINAL_ACK_ROLLBACK_SNAPSHOT"))
		if err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		if err := os.WriteFile(SnapshotPath(), snapshotData, 0o600); err != nil {
			t.Fatalf("write successor snapshot: %v", err)
		}
		oldPID, err := strconv.Atoi(os.Getenv("MCPMUX_TEST_FINAL_ACK_ROLLBACK_OLD_PID"))
		if err != nil {
			t.Fatalf("parse predecessor pid: %v", err)
		}
		origWindow := restoreHealthGateWindow
		restoreHealthGateWindow = time.Hour
		defer func() { restoreHealthGateWindow = origWindow }()

		var failedConn *failFinalAckCaptureConn
		origHook := dialHandoffHook
		dialHandoffHook = func(name string, timeout time.Duration) (fdConn, error) {
			conn, err := dialHandoffWindows(name, timeout)
			if err != nil {
				return nil, err
			}
			failedConn = &failFinalAckCaptureConn{fdConn: conn}
			return failedConn, nil
		}
		defer func() { dialHandoffHook = origHook }()

		d, logs := testDaemonWithLog(t)
		if restored := d.loadSnapshot(); restored != 1 {
			t.Fatalf("loadSnapshot() restored %d owners, want 1", restored)
		}
		activated, err := d.activateRestartStaging()
		if err != nil {
			t.Fatalf("activateRestartStaging() error: %v; logs:\n%s", err, logs.String())
		}
		if activated != 1 {
			t.Fatalf("activateRestartStaging()=%d, want 1; logs:\n%s", activated, logs.String())
		}
		entry := d.Entry("aabbccdd-final-ack-rollback")
		if entry == nil || entry.Owner == nil {
			t.Fatalf("fallback owner missing after final ACK failure; logs:\n%s", logs.String())
		}
		if entry.RestoreSource != "snapshot_fallback" {
			t.Fatalf("restore_source=%q, want snapshot_fallback after final ACK failure; logs:\n%s", entry.RestoreSource, logs.String())
		}

		if entry.Owner.CacheReady() {
			t.Fatalf("fallback owner exposed stale cached discovery after final ACK failure; logs:\n%s", logs.String())
		}
		if state := entry.Owner.MaterializationState(); state == owner.MaterializationCacheOnly {
			t.Fatalf("fallback materialization state=%s after predecessor barrier, want eager restore", state)
		}
		var newPIDs []int
		waitForDaemonCondition(t, 2*time.Second, func() bool {
			pidData, _ := os.ReadFile(os.Getenv("MCPMUX_TEST_FINAL_ACK_ROLLBACK_PIDS"))
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

		if failedConn == nil || len(failedConn.received) != 4 {
			t.Fatalf("received handles=%v, want stdin/stdout/stderr/authority", failedConn)
		}
		defer func() {
			for _, handle := range failedConn.observers {
				_ = windows.CloseHandle(handle)
			}
		}()
		handleKinds := []string{"stdin", "stdout", "stderr", "authority"}
		for i, handle := range failedConn.received {
			if sameKernelObject(windows.Handle(handle), failedConn.observers[i]) {
				t.Fatalf("rolled-back %s handle %d still refers to transferred kernel object", handleKinds[i], handle)
			}
		}
		return
	}
	os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })

	const sid = "aabbccdd-final-ack-rollback"
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	args := []string{"-test.run=^TestLoadSnapshot_FinalAckWriteFailureRollsBackAdoptedOwner$"}
	pidPath := filepath.Join(t.TempDir(), "pids")
	upstreamEnv := map[string]string{
		roleEnv:                               "upstream",
		"MCPMUX_TEST_FINAL_ACK_ROLLBACK_PIDS": pidPath,
	}

	proc, err := upstream.Start(exe, args, upstreamEnv, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("start predecessor upstream: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	oldPID, stdinFD, stdoutFD, stderrFD, authorityFD, err := proc.DetachWithAuthority()
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

	tokenFile, err := os.CreateTemp("", "mcp-mux-final-ack-rollback-*.tok")
	if err != nil {
		t.Fatalf("CreateTemp token: %v", err)
	}
	tokenPath := tokenFile.Name()
	_ = tokenFile.Close()
	t.Cleanup(func() { _ = os.Remove(tokenPath) })
	const token = "final-ack-rollback-token"
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	name := randomPipeName(t)
	ln, err := listenHandoffPipe(name)
	if err != nil {
		t.Fatalf("listenHandoffPipe: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(),
		roleEnv+"=successor",
		"MCPMUX_TEST_FINAL_ACK_ROLLBACK_OLD_PID="+strconv.Itoa(oldPID),
		"MCPMUX_TEST_FINAL_ACK_ROLLBACK_PIDS="+pidPath,
		"MCPMUX_TEST_FINAL_ACK_ROLLBACK_SNAPSHOT="+base64.StdEncoding.EncodeToString(data),
		"MCPMUX_HANDOFF_TOKEN_PATH="+tokenPath,
		"MCPMUX_HANDOFF_SOCKET="+name,
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

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- acceptResult{conn: conn, err: err}
	}()
	var raw net.Conn
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatalf("accept successor: %v", result.err)
		}
		raw = result.conn
	case <-time.After(5 * time.Second):
		_ = ln.Close()
		_ = cmd.Process.Kill()
		t.Fatal("successor did not connect to handoff pipe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, handoffErr := performHandoff(ctx, newWindowsFDConn(raw), token, []HandoffUpstream{{
		ServerID: sid, Command: exe, PID: oldPID,
		StdinFD: stdinFD, StdoutFD: stdoutFD, StderrFD: stderrFD, AuthorityFD: authorityFD,
		commit: proc.CommitDetach, abort: proc.AbortDetach,
	}})
	if handoffErr == nil {
		t.Fatal("predecessor handoff error=nil, want final ACK failure")
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

func TestHandoffIntegration_MaterializingOwnerTransfersSingleReadyGeneration(t *testing.T) {
	const roleEnv = "MCPMUX_TEST_MATERIALIZING_HANDOFF_ROLE"
	const pipeEnv = "MCPMUX_TEST_MATERIALIZING_HANDOFF_PIPE"
	const snapshotEnv = "MCPMUX_TEST_MATERIALIZING_HANDOFF_SNAPSHOT"
	const ipcEnv = "MCPMUX_TEST_MATERIALIZING_HANDOFF_IPC"
	const committedEnv = "MCPMUX_TEST_MATERIALIZING_HANDOFF_COMMITTED"
	const pidPathEnv = "MCPMUX_TEST_MATERIALIZING_HANDOFF_PIDS"
	switch os.Getenv(roleEnv) {
	case "upstream":
		pidFile, err := os.OpenFile(os.Getenv(pidPathEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"materializing-handoff","version":"1"}}}`+"\n", req.ID)
			case "tools/list":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"handoff-tool"}]}}`+"\n", req.ID)
			case "ping":
				_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"pid":%d}}`+"\n", req.ID, os.Getpid())
			}
			if err != nil {
				t.Fatalf("write upstream response: %v", err)
			}
		}
		return
	case "successor":
		rawSnapshot, err := base64.StdEncoding.DecodeString(os.Getenv(snapshotEnv))
		if err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		var snapshot mcpsnapshot.OwnerSnapshot
		if err := json.Unmarshal(rawSnapshot, &snapshot); err != nil {
			t.Fatalf("unmarshal snapshot: %v", err)
		}
		conn, err := dialHandoffWindows(os.Getenv(pipeEnv), 5*time.Second)
		if err != nil {
			t.Fatalf("successor dial: %v", err)
		}
		receipt, err := prepareHandoffReceive(context.Background(), conn, "materializing-token")
		if err != nil {
			t.Fatalf("successor receive: %v", err)
		}
		received, ok := receipt.take(snapshot.ServerID)
		if !ok {
			t.Fatal("successor did not receive materializing owner")
		}
		successor, err := owner.NewOwnerFromHandoff(owner.OwnerConfig{
			Command:              snapshot.Command,
			Args:                 snapshot.Args,
			Cwd:                  snapshot.Cwd,
			Env:                  snapshot.Env,
			IPCPath:              os.Getenv(ipcEnv),
			ServerID:             snapshot.ServerID,
			CachedClassification: snapshot.Classification,
			AdoptedSnapshot:      &snapshot,
		}, owner.HandoffPayload{
			ServerID:    snapshot.ServerID,
			PID:         received.PID,
			StdinFD:     received.StdinFD,
			StdoutFD:    received.StdoutFD,
			StderrFD:    received.StderrFD,
			AuthorityFD: received.AuthorityFD,
			Command:     snapshot.Command,
			Args:        snapshot.Args,
			Cwd:         snapshot.Cwd,
		})
		if err != nil {
			_ = receipt.finalize(nil)
			t.Fatalf("successor adoption: %v", err)
		}
		defer successor.Shutdown()
		if err := receipt.finalize([]string{snapshot.ServerID}); err != nil {
			t.Fatalf("successor finalize: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv(committedEnv)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("predecessor did not publish commit marker")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if successor.MaterializationState() != owner.MaterializationReady || successor.Status()["upstream_pid"] != received.PID {
			t.Fatalf("successor status=%+v", successor.Status())
		}
		ownerConn, err := ipc.Dial(os.Getenv(ipcEnv))
		if err != nil {
			t.Fatalf("dial adopted owner: %v", err)
		}
		defer ownerConn.Close()
		if _, err := fmt.Fprintln(ownerConn, `{"jsonrpc":"2.0","id":91,"method":"ping","params":{}}`); err != nil {
			t.Fatalf("write adopted ping: %v", err)
		}
		responses := bufio.NewScanner(ownerConn)
		for responses.Scan() {
			if strings.Contains(responses.Text(), `"id":91`) {
				if !strings.Contains(responses.Text(), `"pid":`) {
					t.Fatalf("adopted ping response=%s", responses.Text())
				}
				return
			}
		}
		t.Fatalf("adopted response stream ended: %v", responses.Err())
	}

	tmp := t.TempDir()
	pidPath := filepath.Join(tmp, "pids")
	committedPath := filepath.Join(tmp, "committed")
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	predecessor, err := owner.NewOwner(owner.OwnerConfig{
		Command:  exe,
		Args:     []string{"-test.run=^TestHandoffIntegration_MaterializingOwnerTransfersSingleReadyGeneration$"},
		Env:      map[string]string{roleEnv: "upstream", pidPathEnv: pidPath},
		Cwd:      t.TempDir(),
		IPCPath:  shortSocketPath(t, "materializing-predecessor.sock"),
		ServerID: "materializing-handoff-owner",
		OnCacheReady: func(*owner.Owner, owner.OwnerSnapshot) bool {
			close(publishEntered)
			<-releasePublish
			return true
		},
	})
	if err != nil {
		t.Fatalf("NewOwner predecessor: %v", err)
	}
	predecessorClosed := false
	t.Cleanup(func() {
		if !predecessorClosed {
			predecessor.Shutdown()
		}
	})
	select {
	case <-publishEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("predecessor did not reach materializing publish barrier")
	}
	type handoffResult struct {
		payload owner.HandoffPayload
		err     error
	}
	handoffDone := make(chan handoffResult, 1)
	go func() {
		payload, handoffErr := predecessor.ShutdownForHandoff()
		handoffDone <- handoffResult{payload: payload, err: handoffErr}
	}()
	select {
	case result := <-handoffDone:
		t.Fatalf("handoff detached before cache/template commit: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePublish)
	var result handoffResult
	select {
	case result = <-handoffDone:
		if result.err != nil {
			t.Fatalf("ShutdownForHandoff: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handoff did not complete after coherent materialization")
	}
	predecessorClosed = true
	snapshot := predecessor.ExportSnapshot()
	snapshot.ServerID = result.payload.ServerID
	snapshotData, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	name := randomPipeName(t)
	connCh := make(chan fdConn, 1)
	go func() {
		conn, _ := listenHandoffWindows(name, 5*time.Second)
		connCh <- conn
	}()
	successorIPC := shortSocketPath(t, "materializing-successor.sock")
	cmd := exec.Command(exe, "-test.run=^TestHandoffIntegration_MaterializingOwnerTransfersSingleReadyGeneration$")
	cmd.Env = append(os.Environ(),
		roleEnv+"=successor",
		pipeEnv+"="+name,
		snapshotEnv+"="+base64.StdEncoding.EncodeToString(snapshotData),
		ipcEnv+"="+successorIPC,
		committedEnv+"="+committedPath,
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
	conn := <-connCh
	if conn == nil {
		t.Fatal("successor did not connect to handoff pipe")
	}
	defer conn.Close()
	_, handoffErr := performHandoff(context.Background(), conn, "materializing-token", []HandoffUpstream{{
		ServerID:    result.payload.ServerID,
		Command:     result.payload.Command,
		PID:         result.payload.PID,
		StdinFD:     result.payload.StdinFD,
		StdoutFD:    result.payload.StdoutFD,
		StderrFD:    result.payload.StderrFD,
		AuthorityFD: result.payload.AuthorityFD,
		commit:      result.payload.Commit,
		abort:       result.payload.Abort,
	}})
	if handoffErr != nil {
		t.Fatalf("performHandoff: %v", handoffErr)
	}
	if err := os.WriteFile(committedPath, nil, 0o600); err != nil {
		t.Fatalf("write commit marker: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("successor failed: %v", err)
	}
	pidData, _ := os.ReadFile(pidPath)
	if got := len(strings.Fields(string(pidData))); got != 1 {
		t.Fatalf("upstream starts=%d, want one transferred generation; data=%q", got, pidData)
	}
}
