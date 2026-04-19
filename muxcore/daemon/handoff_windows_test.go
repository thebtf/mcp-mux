//go:build windows

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"testing"
	"time"
)

func randomPipeName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "test-" + hex.EncodeToString(b[:])
}

// TestHandoffPipe_ListenDialRoundtrip verifies that a pipe listener and
// dialer created by listenHandoffPipe / dialHandoffPipe can exchange a
// newline-delimited JSON message in both directions.
func TestHandoffPipe_ListenDialRoundtrip(t *testing.T) {
	name := randomPipeName(t)
	ln, err := listenHandoffPipe(name)
	if err != nil {
		t.Fatalf("listenHandoffPipe: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		defer conn.Close()
		server := newWindowsFDConn(conn)
		// Read request.
		var req map[string]string
		if err := server.ReadJSON(&req); err != nil {
			t.Errorf("server ReadJSON: %v", err)
			return
		}
		if req["ping"] != "yes" {
			t.Errorf("unexpected req: %v", req)
			return
		}
		// Send response.
		if err := server.WriteJSON(map[string]string{"pong": "ok"}); err != nil {
			t.Errorf("server WriteJSON: %v", err)
		}
	}()

	conn, err := dialHandoffPipe(name, 3*time.Second)
	if err != nil {
		t.Fatalf("dialHandoffPipe: %v", err)
	}
	defer conn.Close()
	client := newWindowsFDConn(conn)
	if err := client.WriteJSON(map[string]string{"ping": "yes"}); err != nil {
		t.Fatalf("client WriteJSON: %v", err)
	}
	var resp map[string]string
	if err := client.ReadJSON(&resp); err != nil {
		t.Fatalf("client ReadJSON: %v", err)
	}
	if resp["pong"] != "ok" {
		t.Errorf("unexpected resp: %v", resp)
	}
	wg.Wait()
}

// TestHandoffPipe_DialMissingListener verifies that dial fails cleanly when
// no listener exists for the given name (not a hang).
func TestHandoffPipe_DialMissingListener(t *testing.T) {
	name := randomPipeName(t)
	_, err := dialHandoffPipe(name, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when no listener exists")
	}
}
