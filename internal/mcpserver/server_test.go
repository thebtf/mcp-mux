package mcpserver

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

	"github.com/thebtf/mcp-mux/internal/muxcore/control"
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

func TestToolsCallUnknownTool(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"nonexistent_tool","arguments":{}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 10)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)

	if !result.IsError {
		t.Fatal("expected tool error payload with isError=true")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected non-empty content in tool error payload")
	}
	if !strings.Contains(strings.ToLower(result.Content[0].Text), "unknown tool") {
		t.Fatalf("unexpected tool error text: %q", result.Content[0].Text)
	}
}

func TestToolsCallInvalidParams(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":"not-json"}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 11)

	errField, ok := resp["error"]
	if !ok {
		t.Fatal("expected RPC error for invalid tools/call params")
	}

	var rpcErr struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(errField, &rpcErr); err != nil {
		t.Fatalf("unmarshal error field: %v", err)
	}
	if rpcErr.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", rpcErr.Code)
	}
}

func TestPromptsGetInvalidParams(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":12,"method":"prompts/get","params":"not-json"}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 12)

	errField, ok := resp["error"]
	if !ok {
		t.Fatal("expected RPC error for invalid prompts/get params")
	}

	var rpcErr struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(errField, &rpcErr); err != nil {
		t.Fatalf("unmarshal error field: %v", err)
	}
	if rpcErr.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", rpcErr.Code)
	}
}

func TestOwnerHasCwd(t *testing.T) {
	s := &Server{logger: log.New(os.Stderr, "", 0)}
	cwd := strings.ToLower(filepath.Clean(os.TempDir()))
	otherCwd := strings.ToLower(filepath.Clean(filepath.Join(os.TempDir(), "..", "mcp-mux-owner-has-cwd-other")))

	tests := []struct {
		name string
		data map[string]any
		want bool
	}{
		{
			name: "primary cwd matches case-insensitively",
			data: map[string]any{"cwd": strings.ToUpper(cwd)},
			want: true,
		},
		{
			name: "cwd_set contains match",
			data: map[string]any{"cwd_set": []any{otherCwd, strings.ToUpper(cwd)}},
			want: true,
		},
		{
			name: "missing cwd and cwd_set",
			data: map[string]any{},
			want: false,
		},
		{
			name: "primary cwd does not match",
			data: map[string]any{"cwd": otherCwd},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.ownerHasCwd(tc.data, cwd)
			if got != tc.want {
				t.Fatalf("ownerHasCwd() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseError(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `not valid json{{{`)
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":13,"method":"ping","params":{}}`)

	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 13)
	assertNoError(t, resp)
}

func TestEmptyLine(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, "")
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":14,"method":"ping","params":{}}`)

	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 14)
	assertNoError(t, resp)
}

func TestMultipleRequests(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// Interleave send/receive to avoid blocking the pipe.
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":21,"method":"initialize","params":{}}`)
	line1 := readLine(t, clientR)
	resp1 := parseResponse(t, line1)
	assertID(t, resp1, 21)
	assertNoError(t, resp1)

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":22,"method":"tools/list","params":{}}`)
	line2 := readLine(t, clientR)
	resp2 := parseResponse(t, line2)
	assertID(t, resp2, 22)
	assertNoError(t, resp2)

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":23,"method":"ping","params":{}}`)
	line3 := readLine(t, clientR)
	resp3 := parseResponse(t, line3)
	assertID(t, resp3, 23)
	assertNoError(t, resp3)
}

func TestMuxStopInvalidArgs(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// Pass malformed JSON arguments to mux_stop — triggers json.Unmarshal error inside toolMuxStop.
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"mux_stop","arguments":"not-json"}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 30)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true for invalid mux_stop args")
	}
}

func TestMuxStopNoIDOrName(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// Call mux_stop with empty server_id and name — resolveServerID returns an error.
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"mux_stop","arguments":{}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 31)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true when neither server_id nor name provided")
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "server_id") {
		t.Fatalf("expected error mentioning server_id, got: %v", result.Content)
	}
}

func TestMuxRestartInvalidArgs(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// Pass malformed JSON arguments to mux_restart — triggers json.Unmarshal error inside toolMuxRestart.
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":32,"method":"tools/call","params":{"name":"mux_restart","arguments":"not-json"}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 32)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true for invalid mux_restart args")
	}
}

func TestMuxRestartNoIDOrName(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// Call mux_restart with empty arguments — resolveServerID returns an error.
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"mux_restart","arguments":{}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 33)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true when neither server_id nor name provided for mux_restart")
	}
}

func TestMuxStopNoMatchingServer(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// Call mux_stop with a name that matches no running server.
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":34,"method":"tools/call","params":{"name":"mux_stop","arguments":{"name":"nonexistent-server-xyzzy-12345"}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 34)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true for non-existent server name")
	}
}

func TestMuxRestartNoMatchingServer(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":35,"method":"tools/call","params":{"name":"mux_restart","arguments":{"name":"nonexistent-server-xyzzy-12345"}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 35)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true for non-existent server name in mux_restart")
	}
}

func TestToolsCallMuxListNoServers(t *testing.T) {
	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"mux_list","arguments":{}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 24)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)

	if result.IsError {
		t.Fatal("did not expect isError=true for mux_list")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected mux_list result content")
	}

	text := strings.TrimSpace(result.Content[0].Text)
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("mux_list content is not valid JSON: %v (raw: %q)", err, text)
	}
	if decoded != nil {
		if _, ok := decoded.([]any); !ok {
			t.Fatalf("mux_list JSON should be null or array, got %T", decoded)
		}
	}
}

// --- Tests with fake control server ---

// fakeHandler implements control.CommandHandler for testing.
type fakeHandler struct {
	status      map[string]any
	shutdownMsg string
}

func (h *fakeHandler) HandleShutdown(drainTimeoutMs int) string {
	return h.shutdownMsg
}

func (h *fakeHandler) HandleStatus() map[string]interface{} {
	return h.status
}

// startFakeControlServer creates a .ctl.sock in tmpDir with the given server ID
// and a real control.Server that responds to status/shutdown commands.
func startFakeControlServer(t *testing.T, serverID string, status map[string]any) *control.Server {
	t.Helper()
	ctlPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.ctl.sock", serverID))
	t.Cleanup(func() { os.Remove(ctlPath) })

	handler := &fakeHandler{
		status:      status,
		shutdownMsg: "draining (timeout 30000ms)",
	}

	srv, err := control.NewServer(ctlPath, handler, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("startFakeControlServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestMuxListWithFakeServer(t *testing.T) {
	sid := "aabbccdd11223344"
	_ = startFakeControlServer(t, sid, map[string]any{
		"command":             "uvx",
		"args":               []string{"test-server"},
		"session_count":      2,
		"pending_requests":   0,
		"auto_classification": "shared",
		"mux_version":        "test",
		"cwd":                os.TempDir(), // won't match our cwd
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// all=true to see servers from any cwd
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"mux_list","arguments":{"all":true}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 40)
	assertNoError(t, resp)

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if len(result.Content) == 0 {
		t.Fatal("expected mux_list content")
	}

	// Verify our fake server appears
	text := result.Content[0].Text
	if !strings.Contains(text, sid) {
		t.Errorf("mux_list output should contain server ID %s, got: %s", sid, text)
	}
	if !strings.Contains(text, "test-server") {
		t.Errorf("mux_list output should contain args, got: %s", text)
	}
}

func TestMuxListVerbose(t *testing.T) {
	sid := "eeff001122334455"
	_ = startFakeControlServer(t, sid, map[string]any{
		"command":             "node",
		"args":               []string{"verbose-test.js"},
		"session_count":      3,
		"pending_requests":   1,
		"auto_classification": "session-aware",
		"mux_version":        "v0.5.1",
		"cwd":                os.TempDir(),
		"upstream_pid":       12345,
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"mux_list","arguments":{"all":true,"verbose":true}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 41)
	assertNoError(t, resp)

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)

	text := result.Content[0].Text
	// Verbose mode should include upstream_pid
	if !strings.Contains(text, "12345") {
		t.Errorf("verbose mux_list should contain upstream_pid, got: %s", text)
	}
}

func TestMuxStopWithFakeServer(t *testing.T) {
	sid := "ddee112233445566"
	_ = startFakeControlServer(t, sid, map[string]any{
		"command":        "uvx",
		"args":           []string{"stop-test"},
		"session_count":  1,
		"mux_version":    "test",
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"mux_stop","arguments":{"server_id":"%s"}}}`, sid))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 42)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if result.IsError {
		t.Fatalf("mux_stop should succeed, got error: %v", result.Content)
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "draining") {
		t.Errorf("expected draining message, got: %v", result.Content)
	}
}

func TestMuxStopByName(t *testing.T) {
	sid := "aabb112233445566"
	_ = startFakeControlServer(t, sid, map[string]any{
		"command":        "uvx",
		"args":           []string{"unique-name-for-stop-test"},
		"session_count":  1,
		"mux_version":    "test",
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"mux_stop","arguments":{"name":"unique-name-for-stop-test"}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 43)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if result.IsError {
		t.Fatalf("mux_stop by name should succeed, got error: %v", result.Content)
	}
}

func TestMuxRestartWithFakeServer(t *testing.T) {
	sid := "ffaa112233445566"
	_ = startFakeControlServer(t, sid, map[string]any{
		"command":        "uvx",
		"args":           []string{"restart-test"},
		"session_count":  1,
		"mux_version":    "test",
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// force=true to skip drain wait
	sendLine(t, clientW, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":45,"method":"tools/call","params":{"name":"mux_restart","arguments":{"server_id":"%s","force":true}}}`, sid))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 45)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)

	// Restart may succeed (spawn new daemon) or fail at exec.Command
	// Either way, it should have gotten past status+shutdown successfully
	if result.IsError {
		// Accept failure at exec.Command stage — still covers status+shutdown paths
		text := result.Content[0].Text
		if strings.Contains(text, "unreachable") || strings.Contains(text, "no command info") {
			t.Fatalf("restart failed too early (status/shutdown stage): %s", text)
		}
	} else {
		// Success — includes "restarted:" prefix
		if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "restarted") {
			t.Errorf("expected 'restarted' in success message, got: %v", result.Content)
		}
	}
}

func TestMuxRestartByName(t *testing.T) {
	sid := "ffbb112233445566"
	_ = startFakeControlServer(t, sid, map[string]any{
		"command":        "node",
		"args":           []string{"unique-restart-by-name-test.js"},
		"session_count":  1,
		"mux_version":    "test",
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":46,"method":"tools/call","params":{"name":"mux_restart","arguments":{"name":"unique-restart-by-name-test","force":true}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 46)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)

	// Accept both success and exec.Command failure — covers resolveServerID+status+shutdown paths
	if result.IsError {
		text := result.Content[0].Text
		if strings.Contains(text, "no server matching") {
			t.Fatalf("restart by name failed to resolve: %s", text)
		}
	}
}

func TestMuxRestartNoCommandInfo(t *testing.T) {
	sid := "ffcc112233445566"
	// Status returns no command field — should fail with "has no command info"
	_ = startFakeControlServer(t, sid, map[string]any{
		"session_count": 1,
		"mux_version":   "test",
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":47,"method":"tools/call","params":{"name":"mux_restart","arguments":{"server_id":"%s"}}}`, sid))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 47)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected error when server has no command info")
	}
	if !strings.Contains(result.Content[0].Text, "no command info") {
		t.Errorf("expected 'no command info' error, got: %s", result.Content[0].Text)
	}
}

func TestMuxStopForce(t *testing.T) {
	sid := "ffdd112233445566"
	_ = startFakeControlServer(t, sid, map[string]any{
		"command":        "uvx",
		"args":           []string{"force-stop-test"},
		"session_count":  1,
		"mux_version":    "test",
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":48,"method":"tools/call","params":{"name":"mux_stop","arguments":{"server_id":"%s","force":true}}}`, sid))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 48)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if result.IsError {
		t.Fatalf("force stop should succeed, got: %v", result.Content)
	}
}

func TestMuxListFilterByCwd(t *testing.T) {
	myCwd, _ := os.Getwd()

	// Server WITH our cwd
	sid1 := "ccdd001122334455"
	_ = startFakeControlServer(t, sid1, map[string]any{
		"command":        "uvx",
		"args":           []string{"my-server"},
		"session_count":  1,
		"cwd":            myCwd,
		"mux_version":    "test",
	})

	// Server with DIFFERENT cwd
	sid2 := "ccdd998877665544"
	_ = startFakeControlServer(t, sid2, map[string]any{
		"command":        "uvx",
		"args":           []string{"other-server"},
		"session_count":  1,
		"cwd":            filepath.Join(os.TempDir(), "nonexistent-project"),
		"mux_version":    "test",
	})

	clientW, clientR, _ := newTestServer(t)
	defer clientW.Close()

	// Default (no all=true) — should filter by cwd
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"mux_list","arguments":{}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 44)

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)

	text := result.Content[0].Text
	if !strings.Contains(text, "my-server") {
		t.Errorf("filtered mux_list should contain my-server, got: %s", text)
	}
	if strings.Contains(text, "other-server") {
		t.Errorf("filtered mux_list should NOT contain other-server, got: %s", text)
	}
}
