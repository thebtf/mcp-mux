package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const (
	modernAdmissionHelperEnv         = "MCP_MUX_MODERN_ADMISSION_HELPER"
	modernAdmissionModeEnv           = "MCP_MUX_MODERN_ADMISSION_MODE"
	modernAdmissionControlDirEnv     = "MCP_MUX_MODERN_ADMISSION_CONTROL_DIR"
	modernAdmissionPingMarkerEnv     = "MCP_MUX_MODERN_ADMISSION_PING_MARKER"
	modernAdmissionSpawnMarkerEnv    = "MCP_MUX_MODERN_ADMISSION_SPAWN_MARKER"
	modernAdmissionUpstreamHelperEnv = "MCP_MUX_MODERN_ADMISSION_UPSTREAM_HELPER"
	modernAdmissionUpstreamMarkerEnv = "MCP_MUX_MODERN_ADMISSION_UPSTREAM_MARKER"

	modernAdmissionControlMismatch = "control-era-mismatch"
)

type modernAdmissionRun struct {
	stdout          string
	stderr          string
	err             error
	controlPinged   bool
	controlSpawned  bool
	upstreamStarted bool
}

type modernAdmissionJSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type modernAdmissionJSONRPCResponse struct {
	JSONRPC string                       `json:"jsonrpc"`
	ID      json.RawMessage              `json:"id"`
	Error   *modernAdmissionJSONRPCError `json:"error"`
}

// TestModernAdmissionHelper invokes main in an isolated child process so an
// admission refusal cannot terminate the parent test binary.
func TestModernAdmissionHelper(t *testing.T) {
	if os.Getenv(modernAdmissionHelperEnv) != "1" {
		return
	}

	controlDir := os.Getenv(modernAdmissionControlDirEnv)
	pingMarker := os.Getenv(modernAdmissionPingMarkerEnv)
	spawnMarker := os.Getenv(modernAdmissionSpawnMarkerEnv)
	if controlDir == "" || pingMarker == "" || spawnMarker == "" {
		t.Fatal("modern admission helper requires control directory and marker paths")
	}

	startModernAdmissionControl(t, controlDir, pingMarker, spawnMarker, os.Getenv(modernAdmissionModeEnv) == modernAdmissionControlMismatch)
	os.Args = modernAdmissionMainArgs(t)
	main()
	os.Exit(0)
}

// TestModernAdmissionUpstreamSentinel marks any legacy/direct upstream launch.
// The admission tests use this test binary as their positional upstream command
// so the marker works on every supported platform without a shell fixture.
func TestModernAdmissionUpstreamSentinel(t *testing.T) {
	if os.Getenv(modernAdmissionUpstreamHelperEnv) != "1" {
		return
	}

	marker := os.Getenv(modernAdmissionUpstreamMarkerEnv)
	if marker == "" {
		t.Fatal("modern admission upstream sentinel requires a marker path")
	}
	if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
		t.Fatalf("write upstream sentinel marker: %v", err)
	}
	os.Exit(0)
}

func TestModernAdmissionMalformedParamsReturnsInvalidParamsBeforeSideEffects(t *testing.T) {
	const secret = "modern-admission-malformed-secret"
	result := runModernAdmissionChild(t, `{"jsonrpc":"2.0","id":41,"method":"tools/list","params":{"_meta":null,"private":"`+secret+`"}}`, "")

	assertModernAdmissionNoPreSpawnSideEffects(t, result)
	response, ok := parseModernAdmissionError(t, result)
	if !ok {
		return
	}
	assertModernAdmissionID(t, response, "41")
	if response.Error.Code != -32602 {
		t.Errorf("malformed modern params error code = %d, want -32602", response.Error.Code)
	}
	assertModernAdmissionRedacted(t, result, response, secret)
}

func TestModernAdmissionUnsupportedVersionReturnsSupportedRequestedBeforeSideEffects(t *testing.T) {
	const secret = "modern-admission-unsupported-secret"
	result := runModernAdmissionChild(t, `{"jsonrpc":"2.0","id":"unsupported-version","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2030-01-01","io.modelcontextprotocol/clientCapabilities":{}},"private":"`+secret+`"}}`, "")

	assertModernAdmissionNoPreSpawnSideEffects(t, result)
	response, ok := parseModernAdmissionError(t, result)
	if !ok {
		return
	}
	assertModernAdmissionID(t, response, `"unsupported-version"`)
	if response.Error.Code != -32022 {
		t.Errorf("unsupported modern version error code = %d, want -32022", response.Error.Code)
	}

	var data struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}
	if err := json.Unmarshal(response.Error.Data, &data); err != nil {
		t.Errorf("decode unsupported-version error data %q: %v", response.Error.Data, err)
	} else {
		if len(data.Supported) != 1 || data.Supported[0] != "2026-07-28" {
			t.Errorf("unsupported-version supported = %q, want [2026-07-28]", data.Supported)
		}
		if data.Requested != "2030-01-01" {
			t.Errorf("unsupported-version requested = %q, want 2030-01-01", data.Requested)
		}
	}
	assertModernAdmissionRedacted(t, result, response, secret)
}

func TestModernAdmissionConflictingLegacySignalIsLocalRefusalBeforeSideEffects(t *testing.T) {
	const secret = "modern-admission-conflict-secret"
	result := runModernAdmissionChild(t, `{"jsonrpc":"2.0","id":73,"method":"initialize","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"private":"`+secret+`"}}`, "")

	assertModernAdmissionNoPreSpawnSideEffects(t, result)
	response, ok := parseModernAdmissionError(t, result)
	if !ok {
		return
	}
	assertModernAdmissionID(t, response, "73")
	if response.Error.Code == -32022 {
		t.Errorf("conflicting modern/legacy opener error code = -32022, want local refusal")
	}
	assertModernAdmissionRedacted(t, result, response, secret)
}

func TestModernAdmissionControlMismatchIsLocalRefusalWithoutFallback(t *testing.T) {
	const secret = "modern-admission-control-secret"
	result := runModernAdmissionChild(t, `{"jsonrpc":"2.0","id":92,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"private":"`+secret+`"}}`, modernAdmissionControlMismatch)

	if !result.controlSpawned {
		t.Errorf("modern control-mismatch route did not reach the fake daemon control echo; exit=%v stderr=%q", result.err, result.stderr)
	}
	if result.upstreamStarted {
		t.Errorf("control-era mismatch launched the positional upstream instead of refusing before IPC attachment; stderr=%q", result.stderr)
	}
	response, ok := parseModernAdmissionError(t, result)
	if !ok {
		return
	}
	assertModernAdmissionID(t, response, "92")
	if response.Error.Code == -32022 {
		t.Errorf("control-era mismatch error code = -32022, want local admission refusal")
	}
	assertModernAdmissionRedacted(t, result, response, secret)
}

func runModernAdmissionChild(t *testing.T, opening, mode string) modernAdmissionRun {
	t.Helper()

	tempDir := shortTempDir(t, "modern-admission")
	pingMarker := filepath.Join(tempDir, "daemon-ping.marker")
	spawnMarker := filepath.Join(tempDir, "daemon-spawn.marker")
	upstreamMarker := filepath.Join(tempDir, "upstream-started.marker")

	args := []string{
		"-test.run=^TestModernAdmissionHelper$",
		"--",
		"--mcp-protocol=2026-07-28",
		os.Args[0],
		"-test.run=^TestModernAdmissionUpstreamSentinel$",
	}
	cmd := exec.Command(os.Args[0], args...)
	env := os.Environ()
	for key, value := range map[string]string{
		modernAdmissionHelperEnv:         "1",
		modernAdmissionModeEnv:           mode,
		modernAdmissionControlDirEnv:     tempDir,
		modernAdmissionPingMarkerEnv:     pingMarker,
		modernAdmissionSpawnMarkerEnv:    spawnMarker,
		modernAdmissionUpstreamHelperEnv: "1",
		modernAdmissionUpstreamMarkerEnv: upstreamMarker,
		"MCPMUX_ENGINE":                  "1",
		"MCP_MUX_TEST_MAIN":              "0",
		"MCP_MUX_NO_DAEMON":              "0",
		"MCP_MUX_ISOLATED":               "0",
		"MCP_MUX_STATELESS":              "0",
		"MCP_MUX_DAEMON":                 "0",
		"MCP_MUX_DEFAULT_MODE":           "global",
		"MCP_MUX_SHIM_LOG":               "",
		"TEMP":                           tempDir,
		"TMP":                            tempDir,
		"TMPDIR":                         tempDir,
	} {
		env = setEnv(env, key, value)
	}
	cmd.Env = env
	cmd.Dir = tempDir
	cmd.Stdin = strings.NewReader(opening + "\n")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return modernAdmissionRun{
		stdout:          stdout.String(),
		stderr:          stderr.String(),
		err:             err,
		controlPinged:   modernAdmissionMarkerExists(pingMarker),
		controlSpawned:  modernAdmissionMarkerExists(spawnMarker),
		upstreamStarted: modernAdmissionMarkerExists(upstreamMarker),
	}
}

func modernAdmissionMainArgs(t *testing.T) []string {
	t.Helper()
	for index, arg := range os.Args {
		if arg == "--" {
			return append([]string{"mcp-mux"}, os.Args[index+1:]...)
		}
	}
	t.Fatal("modern admission helper missing mcp-mux arguments after --")
	return nil
}

func startModernAdmissionControl(t *testing.T, tempDir, pingMarker, spawnMarker string, mismatch bool) {
	t.Helper()
	t.Setenv("TEMP", tempDir)
	t.Setenv("TMP", tempDir)
	t.Setenv("TMPDIR", tempDir)

	path := serverid.DaemonControlPath("", engineName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove stale modern-admission control socket: %v", err)
	}
	listener, err := ipc.Listen(path)
	if err != nil {
		t.Fatalf("listen modern-admission control: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveModernAdmissionControl(conn, tempDir, pingMarker, spawnMarker, mismatch)
		}
	}()
}

func serveModernAdmissionControl(conn net.Conn, tempDir, pingMarker, spawnMarker string, mismatch bool) {
	defer conn.Close()

	var request control.Request
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		return
	}

	encoder := json.NewEncoder(conn)
	switch request.Cmd {
	case "ping":
		modernAdmissionWriteMarker(pingMarker)
		_ = encoder.Encode(control.Response{OK: true, Message: "pong"})
	case "spawn":
		modernAdmissionWriteMarker(spawnMarker)
		if mismatch {
			_ = encoder.Encode(control.Response{
				OK:          true,
				Message:     "spawned",
				IPCPath:     filepath.Join(tempDir, "unexpected-owner"),
				ServerID:    "modern-admission-test",
				Token:       "test-token",
				ProtocolEra: "2025-03-26",
			})
			return
		}
		_ = encoder.Encode(control.Response{OK: false, Message: "unexpected modern admission spawn"})
	default:
		_ = encoder.Encode(control.Response{OK: false, Message: "unexpected control request"})
	}
}

func modernAdmissionWriteMarker(path string) {
	if err := os.WriteFile(path, []byte("observed"), 0o600); err != nil {
		panic(err)
	}
}

func modernAdmissionMarkerExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func assertModernAdmissionNoPreSpawnSideEffects(t *testing.T, result modernAdmissionRun) {
	t.Helper()
	if result.controlPinged || result.controlSpawned {
		t.Errorf("modern admission contacted daemon control before local refusal: ping=%t spawn=%t exit=%v stderr=%q", result.controlPinged, result.controlSpawned, result.err, result.stderr)
	}
	if result.upstreamStarted {
		t.Errorf("modern admission fell through to a legacy/direct upstream launch; stderr=%q", result.stderr)
	}
}

func parseModernAdmissionError(t *testing.T, result modernAdmissionRun) (modernAdmissionJSONRPCResponse, bool) {
	t.Helper()

	if !strings.HasSuffix(result.stdout, "\n") {
		t.Errorf("admission stdout has no terminal NDJSON newline: %q; exit=%v stderr=%q", result.stdout, result.err, result.stderr)
		return modernAdmissionJSONRPCResponse{}, false
	}
	frame := strings.TrimSuffix(result.stdout, "\n")
	if frame == "" || strings.Contains(frame, "\n") {
		t.Errorf("admission stdout contains %q, want exactly one JSON-RPC error frame; exit=%v stderr=%q", result.stdout, result.err, result.stderr)
		return modernAdmissionJSONRPCResponse{}, false
	}

	var response modernAdmissionJSONRPCResponse
	if err := json.Unmarshal([]byte(frame), &response); err != nil {
		t.Errorf("decode admission stdout frame %q: %v", frame, err)
		return modernAdmissionJSONRPCResponse{}, false
	}
	if response.JSONRPC != "2.0" {
		t.Errorf("admission response jsonrpc = %q, want 2.0", response.JSONRPC)
	}
	if response.Error == nil {
		t.Errorf("admission response has no JSON-RPC error: %q", frame)
		return modernAdmissionJSONRPCResponse{}, false
	}
	if strings.TrimSpace(response.Error.Message) == "" {
		t.Errorf("admission response error has an empty message: %q", frame)
		return modernAdmissionJSONRPCResponse{}, false
	}
	return response, true
}

func assertModernAdmissionID(t *testing.T, response modernAdmissionJSONRPCResponse, want string) {
	t.Helper()
	if got := string(response.ID); got != want {
		t.Errorf("admission response id = %s, want %s", got, want)
	}
}

func assertModernAdmissionRedacted(t *testing.T, result modernAdmissionRun, response modernAdmissionJSONRPCResponse, secret string) {
	t.Helper()
	if strings.Contains(result.stderr, secret) {
		t.Errorf("admission stderr leaked raw request content %q: %q", secret, result.stderr)
	}
	if strings.Contains(response.Error.Message, secret) || strings.Contains(string(response.Error.Data), secret) {
		t.Errorf("admission error leaked raw request content %q: message=%q data=%q", secret, response.Error.Message, response.Error.Data)
	}
}
