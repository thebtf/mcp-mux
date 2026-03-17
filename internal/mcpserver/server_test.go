package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestServer creates a Server wired to an io.Pipe pair and returns
// the client-side reader/writer and a function that closes the input pipe
// (simulating EOF so Run() returns).
func newTestServer(t *testing.T) (clientW io.WriteCloser, clientR io.Reader, done chan error) {
	t.Helper()

	// clientW -> serverR  (test writes requests, server reads them)
	serverR, clientW := io.Pipe()
	// serverW -> clientR  (server writes responses, test reads them)
	clientR, serverW := io.Pipe()

	logger := log.New(os.Stderr, "[mcpserver-test] ", 0)
	srv := NewServer(serverR, serverW, logger)

	done = make(chan error, 1)
	go func() {
		err := srv.Run()
		serverW.Close()
		done <- err
	}()

	return clientW, clientR, done
}

// sendLine writes a single JSON-RPC line to the server.
func sendLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(w, line); err != nil {
		t.Fatalf("sendLine: %v", err)
	}
}

// readLine reads one newline-terminated line from the server with a timeout.
func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	scanner := bufio.NewScanner(r)
	result := make(chan string, 1)
	go func() {
		if scanner.Scan() {
			result <- scanner.Text()
		} else {
			result <- ""
		}
	}()
	select {
	case line := <-result:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("readLine: timeout waiting for server response")
		return ""
	}
}

// parseResponse unmarshals a JSON-RPC response line into a generic map.
func parseResponse(t *testing.T, line string) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("parseResponse: %v (raw: %s)", err, line)
	}
	return obj
}

// assertNoError verifies there is no "error" field in the response.
func assertNoError(t *testing.T, resp map[string]json.RawMessage) {
	t.Helper()
	if errField, ok := resp["error"]; ok {
		t.Fatalf("unexpected error in response: %s", string(errField))
	}
}

// assertID verifies the "id" field matches the expected integer.
func assertID(t *testing.T, resp map[string]json.RawMessage, expectedID int) {
	t.Helper()
	idRaw, ok := resp["id"]
	if !ok {
		t.Fatal("response missing id field")
	}
	var id int
	if err := json.Unmarshal(idRaw, &id); err != nil {
		t.Fatalf("unmarshal id: %v", err)
	}
	if id != expectedID {
		t.Errorf("id = %d, want %d", id, expectedID)
	}
}

// unmarshalResult unmarshals the "result" field into dst.
func unmarshalResult(t *testing.T, resp map[string]json.RawMessage, dst any) {
	t.Helper()
	resultRaw, ok := resp["result"]
	if !ok {
		t.Fatal("response missing result field")
	}
	if err := json.Unmarshal(resultRaw, dst); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
}

// --- Tests ---

func TestInitialize(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 1)
	assertNoError(t, resp)

	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools   map[string]any `json:"tools"`
			Prompts map[string]any `json:"prompts"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	unmarshalResult(t, resp, &result)

	if result.ProtocolVersion == "" {
		t.Error("protocolVersion is empty")
	}
	if result.Capabilities.Tools == nil {
		t.Error("capabilities.tools is nil")
	}
	if result.Capabilities.Prompts == nil {
		t.Error("capabilities.prompts is nil")
	}
	if result.ServerInfo.Name != "mcp-mux" {
		t.Errorf("serverInfo.name = %q, want %q", result.ServerInfo.Name, "mcp-mux")
	}
	if result.ServerInfo.Version == "" {
		t.Error("serverInfo.version is empty")
	}
	if result.Instructions == "" {
		t.Error("instructions field is empty")
	}
	if !strings.Contains(result.Instructions, "mcp-mux") {
		t.Errorf("instructions does not mention mcp-mux: %s", result.Instructions)
	}
}

func TestToolsList(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 2)
	assertNoError(t, resp)

	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	unmarshalResult(t, resp, &result)

	if len(result.Tools) != 3 {
		t.Fatalf("tools count = %d, want 3", len(result.Tools))
	}

	names := make(map[string]bool)
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}

	for _, expected := range []string{"mux_list", "mux_stop", "mux_restart"} {
		if !names[expected] {
			t.Errorf("tool %q not found in list", expected)
		}
	}
}

func TestPromptsList(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":3,"method":"prompts/list","params":{}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 3)
	assertNoError(t, resp)

	var result struct {
		Prompts []struct {
			Name string `json:"name"`
		} `json:"prompts"`
	}
	unmarshalResult(t, resp, &result)

	if len(result.Prompts) != 2 {
		t.Fatalf("prompts count = %d, want 2", len(result.Prompts))
	}

	names := make(map[string]bool)
	for _, p := range result.Prompts {
		names[p.Name] = true
	}

	for _, expected := range []string{"mux-guide", "mux-status-summary"} {
		if !names[expected] {
			t.Errorf("prompt %q not found in list", expected)
		}
	}
}

func TestPromptsGetMuxGuide(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	params := `{"name":"mux-guide"}`
	sendLine(t, clientW, fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"prompts/get","params":%s}`, params))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 4)
	assertNoError(t, resp)

	var result struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	unmarshalResult(t, resp, &result)

	if len(result.Messages) == 0 {
		t.Fatal("mux-guide returned no messages")
	}
	if result.Messages[0].Content.Text == "" {
		t.Error("mux-guide message content text is empty")
	}
}

func TestPromptsGetMuxStatusSummary(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	params := `{"name":"mux-status-summary"}`
	sendLine(t, clientW, fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":%s}`, params))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 5)
	assertNoError(t, resp)

	var result struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	unmarshalResult(t, resp, &result)

	if len(result.Messages) == 0 {
		t.Fatal("mux-status-summary returned no messages")
	}
}

func TestPromptsGetUnknownPrompt(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	params := `{"name":"nonexistent-prompt"}`
	sendLine(t, clientW, fmt.Sprintf(`{"jsonrpc":"2.0","id":6,"method":"prompts/get","params":%s}`, params))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 6)

	errField, ok := resp["error"]
	if !ok {
		t.Fatal("expected error for unknown prompt, got none")
	}

	var rpcErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(errField, &rpcErr); err != nil {
		t.Fatalf("unmarshal error field: %v", err)
	}
	if rpcErr.Code != -32602 {
		t.Errorf("error code = %d, want -32602", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "nonexistent-prompt") {
		t.Errorf("error message does not contain prompt name: %s", rpcErr.Message)
	}
}

func TestPing(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":7,"method":"ping","params":{}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 7)
	assertNoError(t, resp)

	// ping must return an empty object result
	resultRaw, ok := resp["result"]
	if !ok {
		t.Fatal("response missing result field")
	}
	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatalf("unmarshal ping result: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("ping result = %v, want empty object", result)
	}
}

func TestUnknownMethod(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":8,"method":"no/such/method","params":{}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 8)

	errField, ok := resp["error"]
	if !ok {
		t.Fatal("expected error for unknown method, got none")
	}

	var rpcErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(errField, &rpcErr); err != nil {
		t.Fatalf("unmarshal error field: %v", err)
	}
	if rpcErr.Code != -32601 {
		t.Errorf("error code = %d, want -32601", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "no/such/method") {
		t.Errorf("error message does not contain method name: %s", rpcErr.Message)
	}
}

func TestNotificationIgnored(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// A notification has no "id" field — the server must silently ignore it.
	sendLine(t, clientW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// Verify server is still alive and responding to a real request after the notification.
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":9,"method":"ping","params":{}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 9)
	assertNoError(t, resp)
}
