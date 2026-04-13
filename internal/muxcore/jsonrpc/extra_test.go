package jsonrpc

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// errReader returns a fixed error after an optional prefix of bytes.
type errReader struct {
	data []byte
	pos  int
	err  error
}

func (r *errReader) Read(p []byte) (n int, err error) {
	if r.pos < len(r.data) {
		n = copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

// TestScannerReaderError verifies that a read error from the underlying reader
// is surfaced as a non-EOF error from Scan().
func TestScannerReaderError(t *testing.T) {
	// Provide one valid line followed by an I/O error (no trailing newline so
	// bufio.Scanner will try to flush on the next Read call and get the error).
	readErr := errors.New("simulated read error")
	r := &errReader{
		data: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"),
		err:  readErr,
	}

	scanner := NewScanner(r)

	// First Scan should succeed.
	msg, err := scanner.Scan()
	if err != nil {
		t.Fatalf("Scan 1 error: %v", err)
	}
	if msg.Method != "ping" {
		t.Errorf("Method = %q, want ping", msg.Method)
	}

	// Second Scan should propagate the reader error (wrapped).
	_, err = scanner.Scan()
	if err == nil {
		t.Fatal("Scan 2: expected error, got nil")
	}
	if err == io.EOF {
		t.Fatal("Scan 2: got io.EOF, expected wrapped reader error")
	}
}

// TestScannerInvalidJSONLine verifies that a malformed JSON line causes Scan
// to return an error (not panic or silently skip).
func TestScannerInvalidJSONLine(t *testing.T) {
	input := "not valid json\n"
	scanner := NewScanner(strings.NewReader(input))
	_, err := scanner.Scan()
	if err == nil {
		t.Fatal("Scan: expected error for invalid JSON line, got nil")
	}
}

// TestScannerEmptyInput verifies EOF on an empty reader.
func TestScannerEmptyInput(t *testing.T) {
	scanner := NewScanner(strings.NewReader(""))
	_, err := scanner.Scan()
	if err != io.EOF {
		t.Errorf("Scan on empty reader = %v, want io.EOF", err)
	}
}

// TestScannerOnlyEmptyLines verifies that a reader containing only newlines
// returns io.EOF without error.
func TestScannerOnlyEmptyLines(t *testing.T) {
	scanner := NewScanner(strings.NewReader("\n\n\n"))
	_, err := scanner.Scan()
	if err != io.EOF {
		t.Errorf("Scan on empty-lines-only reader = %v, want io.EOF", err)
	}
}

// TestReplaceIDWithStringID replaces a numeric id with a string id.
func TestReplaceIDWithStringID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":99,"method":"ping"}`)
	newID := json.RawMessage(`"new-string-id"`)

	result, err := ReplaceID(raw, newID)
	if err != nil {
		t.Fatalf("ReplaceID() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if string(obj["id"]) != `"new-string-id"` {
		t.Errorf("id = %s, want \"new-string-id\"", string(obj["id"]))
	}
}

// TestReplaceIDWithNullID replaces an existing id with null.
func TestReplaceIDWithNullID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	newID := json.RawMessage(`null`)

	result, err := ReplaceID(raw, newID)
	if err != nil {
		t.Fatalf("ReplaceID() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if string(obj["id"]) != "null" {
		t.Errorf("id = %s, want null", string(obj["id"]))
	}
}

// TestReplaceIDWithNumericID replaces a string id with a numeric id.
func TestReplaceIDWithNumericID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"old","method":"ping"}`)
	newID := json.RawMessage(`42`)

	result, err := ReplaceID(raw, newID)
	if err != nil {
		t.Fatalf("ReplaceID() error: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if string(obj["id"]) != "42" {
		t.Errorf("id = %s, want 42", string(obj["id"]))
	}
}

// TestReplaceIDInvalidJSON verifies that ReplaceID returns an error for
// malformed input rather than panicking.
func TestReplaceIDInvalidJSON(t *testing.T) {
	_, err := ReplaceID([]byte("not json"), json.RawMessage(`1`))
	if err == nil {
		t.Error("ReplaceID() expected error for invalid JSON, got nil")
	}
}

// TestMessageTypeStringUnknown exercises the default branch of String().
func TestMessageTypeStringUnknown(t *testing.T) {
	unknown := MessageType(99)
	if got := unknown.String(); got != "unknown" {
		t.Errorf("MessageType(99).String() = %q, want \"unknown\"", got)
	}
}

// TestParseRequestWithNullID verifies that a request with id:null is treated
// as a request (id present, even though null).
func TestParseRequestWithNullID(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`)
	msg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if msg.Type != TypeRequest {
		t.Errorf("Type = %v, want TypeRequest", msg.Type)
	}
	if string(msg.ID) != "null" {
		t.Errorf("ID = %s, want null", string(msg.ID))
	}
}

// TestParseRawIsIndependentCopy verifies that mutating the original slice
// does not affect msg.Raw.
func TestParseRawIsIndependentCopy(t *testing.T) {
	original := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	data := make([]byte, len(original))
	copy(data, original)

	msg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// Overwrite original slice
	for i := range data {
		data[i] = 0
	}

	if string(msg.Raw) != string(original) {
		t.Errorf("msg.Raw was mutated: got %q, want %q", string(msg.Raw), string(original))
	}
}
