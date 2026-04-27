package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

// shortBaseDir returns a fresh temp directory whose path is short enough to host
// AF_UNIX socket files. On macOS the default os.TempDir() lands under
// /var/folders/.../T/<TestName>NNNN/, easily breaking the 104-byte limit on
// `bind(2)` for AF_UNIX paths once the test appends a socket name. We force
// /tmp on darwin so paths fit. On other platforms the default is fine.
//
// Cleanup runs via t.Cleanup. Caller treats the return as an opaque base dir.
func shortBaseDir(t *testing.T, prefix string) string {
	t.Helper()
	parent := ""
	if runtime.GOOS == "darwin" {
		parent = "/tmp"
	}
	dir, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		t.Fatalf("shortBaseDir(%q): %v", prefix, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newTestServer creates a Server wired to an io.Pipe pair and returns
// the client-side reader/writer, a done channel, and an isolated base directory
// for socket files. BaseDir is a short temp dir so Unix socket paths stay
// within the ~108-byte limit on Windows; tests never touch real daemon sockets.
func newTestServer(t *testing.T) (clientW io.WriteCloser, clientR io.Reader, done chan error, baseDir string) {
	t.Helper()

	baseDir = shortBaseDir(t, "mcpmux-")

	// clientW -> serverR  (test writes requests, server reads them)
	serverR, clientW := io.Pipe()
	// serverW -> clientR  (server writes responses, test reads them)
	clientR, serverW := io.Pipe()

	logger := log.New(os.Stderr, "[mcpserver-test] ", 0)
	srv := NewServer(serverR, serverW, logger)
	srv.BaseDir = baseDir // isolate from real daemon sockets

	done = make(chan error, 1)
	go func() {
		err := srv.Run()
		serverW.Close()
		done <- err
	}()

	return clientW, clientR, done, baseDir
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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

func TestOwnerInfoHasCwd(t *testing.T) {
	s := &Server{logger: log.New(os.Stderr, "", 0)}
	cwd := strings.ToLower(filepath.Clean(os.TempDir()))
	otherCwd := strings.ToLower(filepath.Clean(filepath.Join(os.TempDir(), "..", "mcp-mux-owner-has-cwd-other")))

	tests := []struct {
		name string
		info control.OwnerInfo
		want bool
	}{
		{
			name: "primary cwd matches case-insensitively",
			info: control.OwnerInfo{Cwd: strings.ToUpper(cwd)},
			want: true,
		},
		{
			name: "cwd_set contains match",
			info: control.OwnerInfo{Cwd: otherCwd, CwdSet: []string{otherCwd, strings.ToUpper(cwd)}},
			want: true,
		},
		{
			name: "missing cwd and cwd_set",
			info: control.OwnerInfo{},
			want: false,
		},
		{
			name: "primary cwd does not match",
			info: control.OwnerInfo{Cwd: otherCwd},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.ownerInfoHasCwd(tc.info, cwd)
			if got != tc.want {
				t.Fatalf("ownerInfoHasCwd() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseError(t *testing.T) {
	clientW, clientR, _, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, `not valid json{{{`)
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":13,"method":"ping","params":{}}`)

	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 13)
	assertNoError(t, resp)
}

func TestEmptyLine(t *testing.T) {
	clientW, clientR, _, _ := newTestServer(t)
	defer clientW.Close()

	sendLine(t, clientW, "")
	sendLine(t, clientW, `{"jsonrpc":"2.0","id":14,"method":"ping","params":{}}`)

	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 14)
	assertNoError(t, resp)
}

func TestMultipleRequests(t *testing.T) {
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
	defer clientW.Close()

	// Call mux_stop with empty server_id and name — resolveOwner returns an error.
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
	clientW, clientR, _, _ := newTestServer(t)
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
	clientW, clientR, _, _ := newTestServer(t)
	defer clientW.Close()

	// Call mux_restart with empty arguments — resolveOwner returns an error.
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
	baseDir := shortBaseDir(t, "mcpmux-stopnomatch-")
	t.Cleanup(func() { os.RemoveAll(baseDir) })

	daemonCtlPath := filepath.Join(baseDir, "stopnomatch-muxd.ctl.sock")
	startFakeDaemonControlServer(t, daemonCtlPath, control.ListOwnersResponse{})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
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
	if !strings.Contains(result.Content[0].Text, "no server matching 'nonexistent-server-xyzzy-12345' found") {
		t.Fatalf("expected no-match error, got: %s", result.Content[0].Text)
	}
}

func TestMuxRestartNoMatchingServer(t *testing.T) {
	baseDir := shortBaseDir(t, "mcpmux-restartnomatch-")
	t.Cleanup(func() { os.RemoveAll(baseDir) })

	daemonCtlPath := filepath.Join(baseDir, "restartnomatch-muxd.ctl.sock")
	startFakeDaemonControlServer(t, daemonCtlPath, control.ListOwnersResponse{})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
	defer clientW.Close()

	sendLine(t, clientW, `{"jsonrpc":"2.0","id":35,"method":"tools/call","params":{"name":"mux_restart","arguments":{"name":"nonexistent-server-xyzzy-12345"}}}`)
	line := readLine(t, clientR)
	resp := parseResponse(t, line)

	assertID(t, resp, 35)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true for non-existent server name in mux_restart")
	}
	if !strings.Contains(result.Content[0].Text, "no server matching 'nonexistent-server-xyzzy-12345' found") {
		t.Fatalf("expected no-match error, got: %s", result.Content[0].Text)
	}
}

func TestToolsCallMuxListNoServers(t *testing.T) {
	// Default DaemonCtlPath won't exist in test environment → graceful note returned
	clientW, clientR, _, _ := newTestServer(t)
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
		t.Fatal("did not expect isError=true for mux_list when daemon not running")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected mux_list result content")
	}

	text := strings.TrimSpace(result.Content[0].Text)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("mux_list content is not valid JSON: %v (raw: %q)", err, text)
	}
	if _, ok := decoded["note"]; !ok {
		t.Errorf("expected 'note' field in graceful-degradation response, got: %s", text)
	}
	if servers, ok := decoded["servers"].([]any); !ok || len(servers) != 0 {
		t.Errorf("expected empty 'servers' array, got: %s", text)
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

// startFakeControlServer creates a .ctl.sock in dir with the given server ID
// and a real control.Server that responds to status/shutdown commands.
// Pass the same dir that was returned by newTestServer so the Server finds
// the socket without touching os.TempDir().
func startFakeControlServer(t *testing.T, dir, serverID string, status map[string]any) *control.Server {
	t.Helper()
	ctlPath := filepath.Join(dir, fmt.Sprintf("mcp-mux-%s.ctl.sock", serverID))
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

// fakeDaemonHandler implements control.DaemonHandler for testing the list_owners path.
type fakeDaemonHandler struct {
	fakeHandler
	listOwnersResp control.ListOwnersResponse
	listOwnersErr  error
}

func (h *fakeDaemonHandler) HandleSpawn(_ control.Request) (string, string, string, error) {
	return "", "", "", fmt.Errorf("not implemented")
}
func (h *fakeDaemonHandler) HandleRemove(_ string) error {
	return fmt.Errorf("not implemented")
}
func (h *fakeDaemonHandler) HandleGracefulRestart(_ int) (string, func(), error) {
	return "", nil, fmt.Errorf("not implemented")
}
func (h *fakeDaemonHandler) HandleRefreshSessionToken(_ string) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (h *fakeDaemonHandler) HandleReconnectGiveUp(_ string) error {
	return fmt.Errorf("not implemented")
}
func (h *fakeDaemonHandler) HandleListOwners(_ control.Request) (control.ListOwnersResponse, error) {
	return h.listOwnersResp, h.listOwnersErr
}

// startFakeDaemonControlServer creates a daemon control server at socketPath responding to list_owners.
func startFakeDaemonControlServer(t *testing.T, socketPath string, resp control.ListOwnersResponse) *control.Server {
	t.Helper()
	t.Cleanup(func() { os.Remove(socketPath) })
	handler := &fakeDaemonHandler{
		fakeHandler:    fakeHandler{status: map[string]any{}, shutdownMsg: "ok"},
		listOwnersResp: resp,
	}
	srv, err := control.NewServer(socketPath, handler, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("startFakeDaemonControlServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// newTestServerFull creates a Server with both DaemonCtlPath and BaseDir set.
func newTestServerFull(t *testing.T, daemonCtlPath, baseDir string) (clientW io.WriteCloser, clientR io.Reader, done chan error) {
	t.Helper()
	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()
	logger := log.New(os.Stderr, "[mcpserver-test] ", 0)
	srv := NewServer(serverR, serverW, logger)
	srv.BaseDir = baseDir
	srv.DaemonCtlPath = daemonCtlPath
	done = make(chan error, 1)
	go func() {
		err := srv.Run()
		serverW.Close()
		done <- err
	}()
	return clientW, clientR, done
}

// newTestServerWithDaemonCtl creates a Server with a specific DaemonCtlPath for testing.
func newTestServerWithDaemonCtl(t *testing.T, daemonCtlPath string) (clientW io.WriteCloser, clientR io.Reader, done chan error, baseDir string) {
	t.Helper()
	baseDir = shortBaseDir(t, "mcpmux-")

	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()

	logger := log.New(os.Stderr, "[mcpserver-test] ", 0)
	srv := NewServer(serverR, serverW, logger)
	srv.BaseDir = baseDir
	srv.DaemonCtlPath = daemonCtlPath

	done = make(chan error, 1)
	go func() {
		err := srv.Run()
		serverW.Close()
		done <- err
	}()

	return clientW, clientR, done, baseDir
}

func TestMuxListWithFakeServer(t *testing.T) {
	sid := "aabbccdd11223344"
	baseDir := shortBaseDir(t, "mcpmux-daemon-")

	daemonCtlPath := filepath.Join(baseDir, "test-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{
				ServerID:       sid,
				Command:        "uvx",
				Args:           []string{"test-server"},
				Sessions:       2,
				Classification: "shared",
				MuxVersion:     "test",
				Cwd:            baseDir,
			},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)

	clientW, clientR, _, _ := newTestServerWithDaemonCtl(t, daemonCtlPath)
	defer clientW.Close()

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
	baseDir := shortBaseDir(t, "mcpmux-daemon-")

	daemonCtlPath := filepath.Join(baseDir, "test-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{
				ServerID:       sid,
				Command:        "node",
				Args:           []string{"verbose-test.js"},
				Sessions:       3,
				Pending:        1,
				Classification: "session-aware",
				MuxVersion:     "v0.5.1",
				Cwd:            baseDir,
			},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)

	clientW, clientR, _, _ := newTestServerWithDaemonCtl(t, daemonCtlPath)
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
	if !strings.Contains(text, "session-aware") {
		t.Errorf("verbose mux_list should contain classification, got: %s", text)
	}
}

func TestMuxStopWithFakeServer(t *testing.T) {
	sid := "ddee112233445566"
	baseDir := shortBaseDir(t, "mcpmux-stop-")

	daemonCtlPath := filepath.Join(baseDir, "test-stop-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{ServerID: sid, Command: "uvx", Args: []string{"stop-test"}, Sessions: 1},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)
	startFakeControlServer(t, baseDir, sid, map[string]any{
		"command": "uvx", "args": []string{"stop-test"},
	})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
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
	baseDir := shortBaseDir(t, "mcpmux-stopname-")

	daemonCtlPath := filepath.Join(baseDir, "test-stopname-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{ServerID: sid, Command: "uvx", Args: []string{"unique-name-for-stop-test"}, Sessions: 1},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)
	startFakeControlServer(t, baseDir, sid, map[string]any{
		"command": "uvx", "args": []string{"unique-name-for-stop-test"},
	})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
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
	baseDir := shortBaseDir(t, "mcpmux-restart-")

	daemonCtlPath := filepath.Join(baseDir, "test-restart-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{ServerID: sid, Command: "uvx", Args: []string{"restart-test"}, Sessions: 1},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)
	startFakeControlServer(t, baseDir, sid, map[string]any{
		"command": "uvx", "args": []string{"restart-test"},
	})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
	defer clientW.Close()
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

	if result.IsError {
		text := result.Content[0].Text
		if strings.Contains(text, "unreachable") || strings.Contains(text, "no command info") || strings.Contains(text, "not managed") {
			t.Fatalf("restart failed at resolve stage: %s", text)
		}
		// exec.Command spawn failure is acceptable in test environment
	} else {
		if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "restarted") {
			t.Errorf("expected 'restarted' in success message, got: %v", result.Content)
		}
	}
}

func TestMuxRestartByName(t *testing.T) {
	sid := "ffbb112233445566"
	baseDir := shortBaseDir(t, "mcpmux-restartname-")

	daemonCtlPath := filepath.Join(baseDir, "test-restartname-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{ServerID: sid, Command: "node", Args: []string{"unique-restart-by-name-test.js"}, Sessions: 1},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)
	startFakeControlServer(t, baseDir, sid, map[string]any{
		"command": "node", "args": []string{"unique-restart-by-name-test.js"},
	})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
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

	if result.IsError {
		text := result.Content[0].Text
		if strings.Contains(text, "no server matching") || strings.Contains(text, "not managed") {
			t.Fatalf("restart by name failed to resolve: %s", text)
		}
	}
}

func TestMuxRestartNoCommandInfo(t *testing.T) {
	sid := "ffcc112233445566"
	baseDir := shortBaseDir(t, "mcpmux-nocmd-")

	daemonCtlPath := filepath.Join(baseDir, "test-nocmd-muxd.ctl.sock")
	// Daemon returns owner with empty Command
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{ServerID: sid, Command: "", Args: nil, Sessions: 1},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
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
	baseDir := shortBaseDir(t, "mcpmux-stopforce-")

	daemonCtlPath := filepath.Join(baseDir, "test-stopforce-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{ServerID: sid, Command: "uvx", Args: []string{"force-stop-test"}, Sessions: 1},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)
	startFakeControlServer(t, baseDir, sid, map[string]any{
		"command": "uvx", "args": []string{"force-stop-test"},
	})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
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

	baseDir := shortBaseDir(t, "mcpmux-daemon-")

	daemonCtlPath := filepath.Join(baseDir, "test-muxd.ctl.sock")
	owners := control.ListOwnersResponse{
		Owners: []control.OwnerInfo{
			{
				ServerID: "ccdd001122334455",
				Command:  "uvx",
				Args:     []string{"my-server"},
				Sessions: 1,
				Cwd:      myCwd,
			},
			{
				ServerID: "ccdd998877665544",
				Command:  "uvx",
				Args:     []string{"other-server"},
				Sessions: 1,
				Cwd:      filepath.Join(baseDir, "nonexistent-project"),
			},
		},
	}
	startFakeDaemonControlServer(t, daemonCtlPath, owners)

	clientW, clientR, _, _ := newTestServerWithDaemonCtl(t, daemonCtlPath)
	defer clientW.Close()

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

func TestMuxRestartRefusesForeignID(t *testing.T) {
	baseDir := shortBaseDir(t, "mcpmux-foreign-")

	daemonCtlPath := filepath.Join(baseDir, "foreign-test-muxd.ctl.sock")
	// Empty owners list — any server_id is foreign
	startFakeDaemonControlServer(t, daemonCtlPath, control.ListOwnersResponse{})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
	defer clientW.Close()

	foreignID := "deadbeef12345678"
	sendLine(t, clientW, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":50,"method":"tools/call","params":{"name":"mux_restart","arguments":{"server_id":"%s"}}}`, foreignID))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 50)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true for foreign server_id")
	}
	if !strings.Contains(result.Content[0].Text, "not managed by this mcp-mux daemon") {
		t.Errorf("expected 'not managed by this mcp-mux daemon', got: %s", result.Content[0].Text)
	}
}

func TestMuxStopRefusesForeignID(t *testing.T) {
	baseDir := shortBaseDir(t, "mcpmux-stopforeign-")

	daemonCtlPath := filepath.Join(baseDir, "stopforeign-muxd.ctl.sock")
	startFakeDaemonControlServer(t, daemonCtlPath, control.ListOwnersResponse{})

	clientW, clientR, _ := newTestServerFull(t, daemonCtlPath, baseDir)
	defer clientW.Close()

	foreignID := "deadbeef87654321"
	sendLine(t, clientW, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":51,"method":"tools/call","params":{"name":"mux_stop","arguments":{"server_id":"%s"}}}`, foreignID))
	line := readLine(t, clientR)
	resp := parseResponse(t, line)
	assertID(t, resp, 51)
	assertNoError(t, resp)

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if !result.IsError {
		t.Fatal("expected isError=true for foreign server_id in mux_stop")
	}
	if !strings.Contains(result.Content[0].Text, "not managed by this mcp-mux daemon") {
		t.Errorf("expected 'not managed by this mcp-mux daemon', got: %s", result.Content[0].Text)
	}
}
