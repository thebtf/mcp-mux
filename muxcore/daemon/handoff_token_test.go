package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// jsonFDConn implements fdConn using pre-queued JSON messages.
// Suitable for testing verifyHandoffToken where only ReadJSON is needed.
type jsonFDConn struct {
	readCh chan []byte
}

func newJSONFDConn(msgs ...any) *jsonFDConn {
	ch := make(chan []byte, len(msgs))
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		ch <- b
	}
	return &jsonFDConn{readCh: ch}
}

func (c *jsonFDConn) WriteJSON(v any) error                      { return nil }
func (c *jsonFDConn) ReadJSON(v any) error                       { b := <-c.readCh; return json.Unmarshal(b, v) }
func (c *jsonFDConn) SendFDs(fds []uintptr, header []byte) error { return nil }
func (c *jsonFDConn) RecvFDs() ([]uintptr, []byte, error)        { return nil, nil, nil }
func (c *jsonFDConn) Close() error                               { return nil }

// TestWriteReadHandoffToken verifies that writeHandoffToken creates a file with
// the correct name, contents, and permissions, and that readHandoffToken returns
// the same token.
func TestWriteReadHandoffToken(t *testing.T) {
	dir := t.TempDir()

	token, path, err := writeHandoffToken(dir)
	if err != nil {
		t.Fatalf("writeHandoffToken: %v", err)
	}

	// Path must end with the canonical filename.
	if filepath.Base(path) != "mcp-mux-handoff.tok" {
		t.Errorf("unexpected filename: %s", filepath.Base(path))
	}

	// Token must be at least 32 hex chars (generateToken returns 32 for 128-bit).
	if len(token) < 32 {
		t.Errorf("token too short: %q (len=%d)", token, len(token))
	}

	// readHandoffToken must return the same token.
	got, err := readHandoffToken(path)
	if err != nil {
		t.Fatalf("readHandoffToken: %v", err)
	}
	if got != token {
		t.Errorf("token mismatch: wrote %q, read %q", token, got)
	}

	// On Unix: verify 0600 file permissions.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("os.Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("expected 0600 perms, got %04o", perm)
		}
	}
}

// TestDeleteHandoffTokenIdempotent verifies that deleteHandoffToken succeeds
// both when the file exists and when it has already been deleted.
func TestDeleteHandoffTokenIdempotent(t *testing.T) {
	dir := t.TempDir()

	_, path, err := writeHandoffToken(dir)
	if err != nil {
		t.Fatalf("writeHandoffToken: %v", err)
	}

	// First delete: file exists, should succeed.
	if err := deleteHandoffToken(path); err != nil {
		t.Fatalf("first deleteHandoffToken: %v", err)
	}

	// Second delete: file already gone, must still succeed (idempotent).
	if err := deleteHandoffToken(path); err != nil {
		t.Fatalf("second deleteHandoffToken (idempotent): %v", err)
	}

	// File must be gone.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist after delete, got: %v", err)
	}
}

// TestVerifyHandoffToken_Happy verifies that verifyHandoffToken returns nil
// when the HelloMsg carries the correct protocol version and matching token.
func TestVerifyHandoffToken_Happy(t *testing.T) {
	conn := newJSONFDConn(HelloMsg{
		Type:            MsgHello,
		ProtocolVersion: HandoffProtocolVersion,
		Token:           "valid-token",
	})
	if err := verifyHandoffToken(conn, "valid-token"); err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
}

// TestVerifyHandoffToken_WrongToken verifies that verifyHandoffToken returns
// ErrTokenMismatch when the presented token does not match the expected token.
func TestVerifyHandoffToken_WrongToken(t *testing.T) {
	conn := newJSONFDConn(HelloMsg{
		Type:            MsgHello,
		ProtocolVersion: HandoffProtocolVersion,
		Token:           "wrong-token",
	})
	err := verifyHandoffToken(conn, "different-token")
	if err == nil {
		t.Fatal("expected ErrTokenMismatch, got nil")
	}
	if !errors.Is(err, ErrTokenMismatch) {
		t.Errorf("expected ErrTokenMismatch, got: %v", err)
	}
}

// TestVerifyHandoffToken_WrongVersion verifies that verifyHandoffToken returns
// ErrProtocolVersionMismatch when the HelloMsg carries an unknown version.
func TestVerifyHandoffToken_WrongVersion(t *testing.T) {
	conn := newJSONFDConn(HelloMsg{
		Type:            MsgHello,
		ProtocolVersion: 2,
		Token:           "any-token",
	})
	err := verifyHandoffToken(conn, "any-token")
	if err == nil {
		t.Fatal("expected ErrProtocolVersionMismatch, got nil")
	}
	if !errors.Is(err, ErrProtocolVersionMismatch) {
		t.Errorf("expected ErrProtocolVersionMismatch, got: %v", err)
	}
}
