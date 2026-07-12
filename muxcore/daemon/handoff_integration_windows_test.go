//go:build windows

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/thebtf/mcp-mux/muxcore/upstream"
	"golang.org/x/sys/windows"
)

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

func TestHandoffIntegration_ThreeHandleAuthoritySurvivesPredecessorRelease(t *testing.T) {
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
			t.Fatal("successor did not receipt three-handle transfer")
		}
		adopted, err := upstream.AttachFromFDsWithAuthority(
			received.PID, received.StdinFD, received.StdoutFD, received.AuthorityFD, received.Command,
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

	pid, stdinFD, stdoutFD, authorityFD, err := proc.DetachWithAuthority()
	if err != nil {
		t.Fatalf("DetachWithAuthority: %v", err)
	}
	if stdinFD == 0 || stdoutFD == 0 || authorityFD == 0 {
		t.Fatalf("detached handles = [%d %d %d], want three non-zero handles", stdinFD, stdoutFD, authorityFD)
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
	cmd := exec.Command(exe, "-test.run=^TestHandoffIntegration_ThreeHandleAuthoritySurvivesPredecessorRelease$")
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

func TestHandoffIntegration_UncommittedThreeHandlesAreClosed(t *testing.T) {
	proc, err := upstream.Start("ping", []string{"-n", "30", "127.0.0.1"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = proc.AbortDetach() })
	pid, stdinFD, stdoutFD, authorityFD, err := proc.DetachWithAuthority()
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
				StdinFD: stdinFD, StdoutFD: stdoutFD, AuthorityFD: authorityFD,
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
	for _, handle := range []uintptr{received.StdinFD, received.StdoutFD} {
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
