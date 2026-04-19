//go:build windows

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"
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

	time.Sleep(100 * time.Millisecond)

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
