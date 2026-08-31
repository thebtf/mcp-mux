package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

const modernPolicyHelperEnv = "MCP_MUX_MODERN_POLICY_HELPER"

type recordedSpawnRequest struct {
	ProtocolEra string   `json:"protocol_era"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
}

type modernPolicySpawnHandler struct {
	refreshTestHandler
	recordPath string
}

func (h *modernPolicySpawnHandler) HandleSpawn(req control.Request) (string, string, string, error) {
	record := recordedSpawnRequest{
		ProtocolEra: req.ProtocolEra,
		Command:     req.Command,
		Args:        req.Args,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return "", "", "", err
	}
	if err := os.WriteFile(h.recordPath, data, 0o600); err != nil {
		return "", "", "", err
	}
	return "", "", "", errors.New("modern policy helper captured spawn")
}

// TestMCPProtocolPolicyHelper deliberately invokes main only in a child process:
// main exits after the fake daemon records the spawn request.
func TestMCPProtocolPolicyHelper(t *testing.T) {
	if os.Getenv(modernPolicyHelperEnv) != "1" {
		return
	}

	recordPath := os.Getenv("MCP_MUX_MODERN_POLICY_RECORD")
	if recordPath == "" {
		t.Fatal("MCP_MUX_MODERN_POLICY_RECORD is required")
	}

	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = append([]string{"mcp-mux"}, args[i+1:]...)
			break
		}
	}
	if len(args) == len(os.Args) {
		t.Fatal("missing mcp-mux arguments after --")
	}
	os.Args = args

	startFakeDaemon(t, filepath.Dir(recordPath), &modernPolicySpawnHandler{recordPath: recordPath})
	main()
}

func runMCPProtocolPolicyHelper(t *testing.T, args ...string) (recordedSpawnRequest, string) {
	t.Helper()

	recordPath := filepath.Join(t.TempDir(), "spawn.json")
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestMCPProtocolPolicyHelper$", "--"}, args...)...)
	cmd.Env = append(os.Environ(),
		modernPolicyHelperEnv+"=1",
		"MCP_MUX_MODERN_POLICY_RECORD="+recordPath,
		"MCPMUX_ENGINE=1",
		"MCP_MUX_NO_DAEMON=0",
		"MCP_MUX_ISOLATED=0",
		"MCP_MUX_STATELESS=0",
		"MCP_MUX_DAEMON=0",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}
	for _, arg := range args {
		if arg == "--mcp-protocol=2026-07-28" {
			cmd.Stdin = bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}` + "\n")
			break
		}
	}
	if err := cmd.Run(); err == nil {
		t.Fatalf("helper main exited successfully; stderr:\n%s", stderr.String())
	}

	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("helper did not reach daemon spawn: %v; stderr:\n%s", err, stderr.String())
	}
	var record recordedSpawnRequest
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode recorded spawn: %v", err)
	}
	return record, stderr.String()
}

func TestMCPProtocolSelectorIsAdditiveAndReachesPreSpawn(t *testing.T) {
	record, stderr := runMCPProtocolPolicyHelper(t,
		"--mcp-protocol=2026-07-28",
		"fixture-mcp",
		"--upstream-flag=value",
	)

	if record.ProtocolEra != "2026-07-28" {
		t.Fatalf("daemon spawn protocol era = %q, want %q; stderr:\n%s", record.ProtocolEra, "2026-07-28", stderr)
	}
	if record.Command != "fixture-mcp" {
		t.Fatalf("daemon spawn command = %q, want fixture-mcp", record.Command)
	}
	if len(record.Args) != 1 || record.Args[0] != "--upstream-flag=value" {
		t.Fatalf("daemon spawn args = %q, want %q", record.Args, []string{"--upstream-flag=value"})
	}
}

func TestMCPProtocolDefaultsToLegacyAtPreSpawn(t *testing.T) {
	record, stderr := runMCPProtocolPolicyHelper(t, "fixture-mcp")

	if record.ProtocolEra != "" {
		t.Fatalf("daemon spawn protocol era = %q, want legacy omission; stderr:\n%s", record.ProtocolEra, stderr)
	}
}
