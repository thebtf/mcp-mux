package mux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitswan-space/mcp-mux/internal/ipc"
)

// testLogger returns a logger that writes to test output.
func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(os.Stderr, "[test] ", log.LstdFlags)
}

// ipcPath returns a unique IPC socket path for the test.
func testIPCPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test-mux.sock")
}

func TestOwnerSingleSession(t *testing.T) {
	ipcPath := testIPCPath(t)

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Send ping request
	sendReq(t, clientW, 1, "ping", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 1)
}

func TestOwnerWithMockServer(t *testing.T) {
	ipcPath := testIPCPath(t)

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	session := NewSession(serverR, serverW)
	owner.AddSession(session)

	// Send initialize request
	sendReq(t, clientW, 1, "initialize", `{}`)
	resp := readResp(t, clientR)
	assertResponseID(t, resp, 1)
	if !strings.Contains(string(resp), "mock-server") {
		t.Errorf("initialize response missing mock-server: %s", string(resp))
	}

	// Send tools/list request
	sendReq(t, clientW, 2, "tools/list", `{}`)
	resp = readResp(t, clientR)
	assertResponseID(t, resp, 2)
	if !strings.Contains(string(resp), "echo") {
		t.Errorf("tools/list response missing 'echo' tool: %s", string(resp))
	}

	// Send tools/call request
	sendReq(t, clientW, 3, "tools/call", `{"name":"echo","arguments":{"message":"hello-mux"}}`)
	resp = readResp(t, clientR)
	assertResponseID(t, resp, 3)
	if !strings.Contains(string(resp), "hello-mux") {
		t.Errorf("tools/call response missing 'hello-mux': %s", string(resp))
	}
}

func TestOwnerMultipleSessions(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Create two sessions
	c1R, s1W := io.Pipe()
	s1R, c1W := io.Pipe()
	session1 := NewSession(s1R, s1W)
	owner.AddSession(session1)

	c2R, s2W := io.Pipe()
	s2R, c2W := io.Pipe()
	session2 := NewSession(s2R, s2W)
	owner.AddSession(session2)

	// Warm up: ensure mock_server is running by sending init from session 1
	sendReq(t, c1W, 99, "initialize", `{}`)
	warmup := readResp(t, c1R)
	if !strings.Contains(string(warmup), "mock-server") {
		t.Fatalf("warmup failed: %s", string(warmup))
	}

	// Both sessions send requests with the SAME id=1 — sequentially to avoid pipe race
	sendReq(t, c1W, 1, "tools/call", `{"name":"echo","arguments":{"message":"from-session1"}}`)
	sendReq(t, c2W, 1, "tools/call", `{"name":"echo","arguments":{"message":"from-session2"}}`)

	// Each session should get the correct response with id=1
	resp1 := readResp(t, c1R)
	resp2 := readResp(t, c2R)

	assertResponseID(t, resp1, 1)
	assertResponseID(t, resp2, 1)

	// Verify each got their own response content
	s1Got := string(resp1)
	s2Got := string(resp2)

	if !strings.Contains(s1Got, "from-session1") {
		t.Errorf("session 1 got wrong response: %s", s1Got)
	}
	if !strings.Contains(s2Got, "from-session2") {
		t.Errorf("session 2 got wrong response: %s", s2Got)
	}
}

func TestOwnerIPCClient(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Add owner's own session
	ownerR, ownerSW := io.Pipe()
	ownerSR, ownerW := io.Pipe()
	ownerSession := NewSession(ownerSR, ownerSW)
	owner.AddSession(ownerSession)

	// Connect a remote client via IPC
	conn, err := ipc.Dial(ipcPath)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer conn.Close()

	// Wait for IPC session to be registered
	time.Sleep(100 * time.Millisecond)

	if owner.SessionCount() < 2 {
		t.Errorf("SessionCount = %d, want >= 2", owner.SessionCount())
	}

	// Send request through IPC client
	ipcReq := `{"jsonrpc":"2.0","id":42,"method":"ping","params":{}}` + "\n"
	_, err = conn.Write([]byte(ipcReq))
	if err != nil {
		t.Fatalf("IPC write error: %v", err)
	}

	// Read response from IPC
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no IPC response received")
	}
	ipcResp := scanner.Text()
	if !strings.Contains(ipcResp, `"id":42`) {
		t.Errorf("IPC response wrong id: %s", ipcResp)
	}

	// Verify owner's session still works independently
	sendReq(t, ownerW, 99, "ping", `{}`)
	ownerResp := readResp(t, ownerR)
	assertResponseID(t, ownerResp, 99)

	_ = ownerW
	_ = ownerR
}

func TestSessionDisconnectDoesNotCrashOwner(t *testing.T) {
	ipcPath := testIPCPath(t)

	owner, err := NewOwner(OwnerConfig{
		Command: "go",
		Args:    []string{"run", "../../testdata/mock_server.go"},
		IPCPath: ipcPath,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner() error: %v", err)
	}
	defer owner.Shutdown()

	// Add two sessions
	_, s1W := io.Pipe()
	s1R, c1W := io.Pipe()
	session1 := NewSession(s1R, s1W)
	owner.AddSession(session1)

	c2R, s2W := io.Pipe()
	s2R, c2W := io.Pipe()
	session2 := NewSession(s2R, s2W)
	owner.AddSession(session2)

	// Disconnect session 1 by closing its write pipe
	c1W.Close()

	// Wait for cleanup
	time.Sleep(100 * time.Millisecond)

	// Session 2 should still work
	sendReq(t, c2W, 1, "ping", `{}`)
	resp := readResp(t, c2R)
	assertResponseID(t, resp, 1)
}

// --- Helpers ---

func sendReq(t *testing.T, w io.Writer, id int, method, params string) {
	t.Helper()
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"%s","params":%s}`, id, method, params)
	_, err := w.Write([]byte(req + "\n"))
	if err != nil {
		t.Fatalf("sendReq error: %v", err)
	}
}

func readResp(t *testing.T, r io.Reader) []byte {
	t.Helper()
	scanner := bufio.NewScanner(r)
	done := make(chan bool, 1)
	var line string
	go func() {
		if scanner.Scan() {
			line = scanner.Text()
		}
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("readResp timeout")
	}

	return []byte(line)
}

func assertResponseID(t *testing.T, resp []byte, expectedID int) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(resp, &obj); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, string(resp))
	}

	idRaw, ok := obj["id"]
	if !ok {
		t.Fatalf("response has no id field: %s", string(resp))
	}

	var id int
	if err := json.Unmarshal(idRaw, &id); err != nil {
		t.Fatalf("unmarshal id: %v (raw: %s)", err, string(idRaw))
	}

	if id != expectedID {
		t.Errorf("response id = %d, want %d (full: %s)", id, expectedID, string(resp))
	}
}
