package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	modernProtocolVersion = "2026-07-28"
	modernCaptureEnv      = "MCP_MUX_MODERN_CAPTURE_FILE"
	modernModeEnv         = "MCP_MUX_MODERN_MODE"
)

type modernCorpusFrame struct {
	raw    string
	id     json.RawMessage
	method string
}

type modernResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func TestModernOpeningCorpusAndFixtureContract(t *testing.T) {
	fixtureDir := modernFixtureDir(t)
	frames := loadModernCorpus(t, filepath.Join(fixtureDir, "modern_opening_corpus.ndjson"))
	validateModernCorpus(t, frames)

	binary := buildModernFixture(t, fixtureDir)
	capturePath := filepath.Join(t.TempDir(), "modern-capture.ndjson")
	stdout, stderr, err := runModernFixture(binary, rawFrames(frames), map[string]string{
		modernCaptureEnv: capturePath,
	})
	if err != nil {
		t.Fatalf("run modern fixture: %v\nstderr:\n%s", err, stderr)
	}

	captured, err := readNDJSON(capturePath)
	if err != nil {
		t.Fatalf("read fixture capture: %v", err)
	}
	if len(captured) != len(frames) {
		t.Fatalf("captured %d frame(s), want %d", len(captured), len(frames))
	}
	for i, frame := range frames {
		if captured[i] != frame.raw {
			t.Fatalf("capture[%d] changed opening bytes\n got: %q\nwant: %q", i, captured[i], frame.raw)
		}
	}

	responses := decodeNDJSON(t, stdout)
	if len(responses) != len(frames) {
		t.Fatalf("fixture wrote %d frame(s), want one native result per corpus frame (%d)\nstdout:\n%s", len(responses), len(frames), stdout)
	}

	expected := make(map[string]modernCorpusFrame, len(frames))
	for _, frame := range frames {
		key := canonicalJSON(frame.id)
		if _, exists := expected[key]; exists {
			t.Fatalf("corpus reuses request id %s", key)
		}
		expected[key] = frame
	}
	for i, response := range responses {
		if response.JSONRPC != "2.0" {
			t.Errorf("response[%d] jsonrpc = %q, want 2.0", i, response.JSONRPC)
		}
		if response.Method != "" {
			t.Errorf("response[%d] unexpectedly is notification %q", i, response.Method)
			continue
		}
		frame, ok := expected[canonicalJSON(response.ID)]
		if !ok {
			t.Errorf("response[%d] has unknown id %s", i, canonicalJSON(response.ID))
			continue
		}
		delete(expected, canonicalJSON(response.ID))
		if len(response.Error) != 0 {
			t.Errorf("response[%d] for %s returned error %s", i, frame.method, response.Error)
			continue
		}
		assertNativeResult(t, frame.method, response.Result)
	}
	if len(expected) != 0 {
		keys := make([]string, 0, len(expected))
		for key := range expected {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		t.Errorf("fixture did not return native results for ids %s", strings.Join(keys, ", "))
	}
}

func TestModernFixtureFlushesEachResponseBeforeInputCloses(t *testing.T) {
	fixtureDir := modernFixtureDir(t)
	binary := buildModernFixture(t, fixtureDir)
	cmd := exec.Command(binary)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("fixture stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("fixture stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	responseDone := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		responseDone <- line
	}()

	request := modernRequest("flush-before-eof", "tools/list", false)
	if _, err := fmt.Fprintln(stdin, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case line := <-responseDone:
		responses := decodeNDJSON(t, line)
		if len(responses) != 1 || string(responses[0].ID) != `"flush-before-eof"` || len(responses[0].Error) != 0 {
			t.Fatalf("flushed response = %q, want one successful response before EOF", line)
		}
	case <-time.After(time.Second):
		t.Fatal("modern fixture buffered its response until input EOF")
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("close fixture stdin: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("fixture after input close: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("modern fixture did not stop after input EOF")
	}
}

func TestModernFixtureControlledModes(t *testing.T) {
	fixtureDir := modernFixtureDir(t)
	binary := buildModernFixture(t, fixtureDir)

	t.Run("input required", func(t *testing.T) {
		stdout, stderr, err := runModernFixture(binary, []string{modernRequest("input", "tools/call", false)}, map[string]string{
			modernModeEnv: "input_required",
		})
		if err != nil {
			t.Fatalf("run input_required fixture: %v\nstderr:\n%s", err, stderr)
		}
		responses := decodeNDJSON(t, stdout)
		if len(responses) != 1 || len(responses[0].Error) != 0 {
			t.Fatalf("input_required output = %s", stdout)
		}
		var result struct {
			ResultType    string                     `json:"resultType"`
			InputRequests map[string]json.RawMessage `json:"inputRequests"`
			RequestState  string                     `json:"requestState"`
		}
		if err := json.Unmarshal(responses[0].Result, &result); err != nil {
			t.Fatalf("decode input_required result: %v", err)
		}
		if result.ResultType != "input_required" {
			t.Errorf("resultType = %q, want input_required", result.ResultType)
		}
		if _, ok := result.InputRequests["fixture_confirmation"]; !ok {
			t.Errorf("inputRequests = %s, want fixture_confirmation", responses[0].Result)
		}
		if result.RequestState != "fixture-opaque-request-state-v1" {
			t.Errorf("requestState = %q, want opaque fixture state", result.RequestState)
		}
	})

	t.Run("request scoped log", func(t *testing.T) {
		stdout, stderr, err := runModernFixture(binary, []string{modernRequest("without-log", "tools/list", false)}, map[string]string{
			modernModeEnv: "request_log",
		})
		if err != nil {
			t.Fatalf("run unopted log fixture: %v\nstderr:\n%s", err, stderr)
		}
		if got := decodeNDJSON(t, stdout); len(got) != 1 {
			t.Fatalf("unopted request wrote %d frame(s), want only its result\n%s", len(got), stdout)
		}

		stdout, stderr, err = runModernFixture(binary, []string{modernRequest("with-log", "tools/list", true)}, map[string]string{
			modernModeEnv: "request_log",
		})
		if err != nil {
			t.Fatalf("run opted log fixture: %v\nstderr:\n%s", err, stderr)
		}
		responses := decodeNDJSON(t, stdout)
		if len(responses) != 2 {
			t.Fatalf("opted request wrote %d frame(s), want log then result\n%s", len(responses), stdout)
		}
		if responses[0].Method != "notifications/message" {
			t.Fatalf("first opted frame method = %q, want notifications/message", responses[0].Method)
		}
		var params struct {
			Level  string `json:"level"`
			Logger string `json:"logger"`
			Data   string `json:"data"`
		}
		if err := json.Unmarshal(responses[0].Params, &params); err != nil {
			t.Fatalf("decode log params: %v", err)
		}
		if params.Level != "info" || params.Logger != "mock-modern-server" || params.Data != "request-scoped fixture log" {
			t.Errorf("log params = %+v, want deterministic request-scoped log", params)
		}
		if len(responses[1].Error) != 0 {
			t.Errorf("opted request result error = %s", responses[1].Error)
		}
	})

	t.Run("contained server request", func(t *testing.T) {
		stdout, stderr, err := runModernFixture(binary, []string{modernRequest("server-request", "tools/call", false)}, map[string]string{
			modernModeEnv: "server_request",
		})
		if err != nil {
			t.Fatalf("run server_request fixture: %v\nstderr:\n%s", err, stderr)
		}
		responses := decodeNDJSON(t, stdout)
		if len(responses) != 2 {
			t.Fatalf("server_request wrote %d frame(s), want request then result\n%s", len(responses), stdout)
		}
		if responses[0].Method != "sampling/createMessage" || len(responses[0].ID) == 0 {
			t.Errorf("first frame = method %q id %s, want contained server request", responses[0].Method, responses[0].ID)
		}
		if len(responses[1].Error) != 0 {
			t.Errorf("native result after contained request has error %s", responses[1].Error)
		}
	})

	for _, mode := range []struct {
		name       string
		wantResult bool
	}{
		{name: "loss_before_result"},
		{name: "loss_after_result", wantResult: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			stdout, stderr, err := runModernFixture(binary, []string{modernRequest(mode.name, "tools/call", false)}, map[string]string{
				modernModeEnv: mode.name,
			})
			if err == nil {
				t.Fatalf("%s fixture unexpectedly exited successfully\nstdout:\n%s\nstderr:\n%s", mode.name, stdout, stderr)
			}
			responses := decodeNDJSON(t, stdout)
			if mode.wantResult {
				if len(responses) != 1 || len(responses[0].Error) != 0 {
					t.Errorf("%s output = %s, want exactly one native result before loss", mode.name, stdout)
				}
			} else if len(responses) != 0 {
				t.Errorf("%s output = %s, want no result before loss", mode.name, stdout)
			}
		})
	}
}

func modernFixtureDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate fixture test source")
	}
	if !filepath.IsAbs(file) {
		absolute, err := filepath.Abs(file)
		if err != nil {
			t.Fatalf("resolve fixture test source: %v", err)
		}
		file = absolute
	}
	return filepath.Dir(file)
}

func loadModernCorpus(t *testing.T, path string) []modernCorpusFrame {
	t.Helper()
	lines, err := readNDJSON(path)
	if err != nil {
		t.Fatalf("read modern opening corpus: %v", err)
	}
	if len(lines) < 100 {
		t.Fatalf("modern opening corpus contains %d frame(s), want at least 100", len(lines))
	}

	frames := make([]modernCorpusFrame, 0, len(lines))
	for i, raw := range lines {
		frame := validateModernOpeningFrame(t, i, raw)
		frames = append(frames, frame)
	}
	return frames
}

func validateModernCorpus(t *testing.T, frames []modernCorpusFrame) {
	t.Helper()
	var direct, discover, absentClientInfo, presentClientInfo, whitespace, compact int
	orders := make(map[string]struct{})
	for i, frame := range frames {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(frame.raw), &envelope); err != nil {
			t.Fatalf("corpus frame %d stopped being valid JSON: %v", i, err)
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal(envelope["params"], &params); err != nil {
			t.Fatalf("corpus frame %d params stopped being an object: %v", i, err)
		}
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(params["_meta"], &meta); err != nil {
			t.Fatalf("corpus frame %d _meta stopped being an object: %v", i, err)
		}
		if _, ok := meta["io.modelcontextprotocol/clientInfo"]; ok {
			presentClientInfo++
		} else {
			absentClientInfo++
		}
		if frame.method == "server/discover" {
			discover++
		} else {
			direct++
		}
		orders[topLevelPropertyOrder(t, i, frame.raw)] = struct{}{}
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, []byte(frame.raw)); err != nil {
			t.Fatalf("compact corpus frame %d: %v", i, err)
		}
		if compacted.String() == frame.raw {
			compact++
		} else {
			whitespace++
		}
	}

	if direct == 0 || discover == 0 {
		t.Errorf("corpus coverage direct=%d discover=%d, want both opening classes", direct, discover)
	}
	if absentClientInfo == 0 || presentClientInfo == 0 {
		t.Errorf("corpus coverage clientInfo absent=%d present=%d, want both", absentClientInfo, presentClientInfo)
	}
	if len(orders) < 2 {
		t.Errorf("corpus has %d top-level property-order variant(s), want at least 2", len(orders))
	}
	if whitespace == 0 || compact == 0 {
		t.Errorf("corpus whitespace coverage formatted=%d compact=%d, want both", whitespace, compact)
	}
}

func validateModernOpeningFrame(t *testing.T, index int, raw string) modernCorpusFrame {
	t.Helper()
	if raw == "" || strings.TrimSpace(raw) == "" {
		t.Fatalf("corpus frame %d is blank", index)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("corpus frame %d is not valid JSON: %v", index, err)
	}
	var jsonrpc, method string
	if err := json.Unmarshal(envelope["jsonrpc"], &jsonrpc); err != nil || jsonrpc != "2.0" {
		t.Fatalf("corpus frame %d jsonrpc = %s, want 2.0", index, envelope["jsonrpc"])
	}
	id, hasID := envelope["id"]
	if !hasID || len(id) == 0 || bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		t.Fatalf("corpus frame %d has no JSON-RPC request id", index)
	}
	if err := json.Unmarshal(envelope["method"], &method); err != nil || method == "" {
		t.Fatalf("corpus frame %d has invalid method %s", index, envelope["method"])
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(envelope["params"], &params); err != nil || params == nil {
		t.Fatalf("corpus frame %d params must be an object", index)
	}
	metaRaw, hasMeta := params["_meta"]
	if !hasMeta {
		t.Fatalf("corpus frame %d has no params._meta", index)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(metaRaw, &meta); err != nil || meta == nil {
		t.Fatalf("corpus frame %d _meta must be an object", index)
	}
	var version string
	if err := json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &version); err != nil || version != modernProtocolVersion {
		t.Fatalf("corpus frame %d protocol version = %s, want %q", index, meta["io.modelcontextprotocol/protocolVersion"], modernProtocolVersion)
	}
	capabilitiesRaw, hasCapabilities := meta["io.modelcontextprotocol/clientCapabilities"]
	if !hasCapabilities {
		t.Fatalf("corpus frame %d has no clientCapabilities", index)
	}
	var capabilities map[string]json.RawMessage
	if err := json.Unmarshal(capabilitiesRaw, &capabilities); err != nil || capabilities == nil {
		t.Fatalf("corpus frame %d clientCapabilities must be an object", index)
	}
	if clientInfoRaw, present := meta["io.modelcontextprotocol/clientInfo"]; present {
		var clientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(clientInfoRaw, &clientInfo); err != nil || clientInfo.Name == "" || clientInfo.Version == "" {
			t.Fatalf("corpus frame %d clientInfo must have string name and version", index)
		}
	}
	return modernCorpusFrame{raw: raw, id: id, method: method}
}

func topLevelPropertyOrder(t *testing.T, index int, raw string) string {
	t.Helper()
	keys := []string{"jsonrpc", "id", "method", "params"}
	type keyedPosition struct {
		key string
		pos int
	}
	positions := make([]keyedPosition, 0, len(keys))
	for _, key := range keys {
		pos := strings.Index(raw, fmt.Sprintf("\"%s\"", key))
		if pos < 0 {
			t.Fatalf("corpus frame %d lacks top-level %q", index, key)
		}
		positions = append(positions, keyedPosition{key: key, pos: pos})
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].pos < positions[j].pos })
	ordered := make([]string, len(positions))
	for i, position := range positions {
		ordered[i] = position.key
	}
	return strings.Join(ordered, ",")
}

func buildModernFixture(t *testing.T, fixtureDir string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mock-modern-server")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binary, "mock_modern_server.go")
	cmd.Dir = fixtureDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build modern fixture: %v\n%s", err, output)
	}
	return binary
}

func runModernFixture(binary string, frames []string, vars map[string]string) (stdout, stderr string, err error) {
	cmd := exec.Command(binary)
	cmd.Stdin = strings.NewReader(strings.Join(frames, "\n") + "\n")
	cmd.Env = replaceEnv(os.Environ(), vars)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func replaceEnv(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[name]; overridden {
				continue
			}
		}
		result = append(result, entry)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func rawFrames(frames []modernCorpusFrame) []string {
	raw := make([]string, len(frames))
	for i, frame := range frames {
		raw[i] = frame.raw
	}
	return raw
}

func readNDJSON(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("%s must end with one newline-delimited JSON frame", path)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for i, line := range lines {
		if strings.HasSuffix(line, "\r") {
			return nil, fmt.Errorf("%s frame %d uses CRLF; corpus and capture use LF NDJSON", path, i)
		}
		if strings.TrimSpace(line) == "" {
			return nil, fmt.Errorf("%s frame %d is blank", path, i)
		}
	}
	return lines, nil
}

func decodeNDJSON(t *testing.T, output string) []modernResponse {
	t.Helper()
	if output == "" {
		return nil
	}
	if !strings.HasSuffix(output, "\n") {
		t.Fatalf("fixture output is not newline-delimited: %q", output)
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	responses := make([]modernResponse, 0, len(lines))
	for i, line := range lines {
		var response modernResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("decode fixture output frame %d: %v\n%s", i, err, line)
		}
		responses = append(responses, response)
	}
	return responses
}

func assertNativeResult(t *testing.T, method string, raw json.RawMessage) {
	t.Helper()
	switch method {
	case "ping":
		if string(raw) != "{}" {
			t.Errorf("ping result = %s, want {}", raw)
		}
	case "tools/list":
		var result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Errorf("decode tools/list result: %v", err)
			return
		}
		if len(result.Tools) != 1 || result.Tools[0].Name != "modern_echo" {
			t.Errorf("tools/list result = %s, want modern_echo tool", raw)
		}
	case "server/discover":
		var result struct {
			ResultType        string   `json:"resultType"`
			SupportedVersions []string `json:"supportedVersions"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Errorf("decode server/discover result: %v", err)
			return
		}
		if result.ResultType != "complete" || !contains(result.SupportedVersions, modernProtocolVersion) {
			t.Errorf("server/discover result = %s, want complete support for %q", raw, modernProtocolVersion)
		}
	default:
		t.Errorf("corpus uses unsupported native fixture method %q", method)
	}
}

func modernRequest(id, method string, logLevel bool) string {
	meta := `"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}`
	if logLevel {
		meta += `,"io.modelcontextprotocol/logLevel":"info"`
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":%q,"params":{"_meta":{%s}}}`, id, method, meta)
}

func canonicalJSON(raw json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
