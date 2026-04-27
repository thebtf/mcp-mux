package mcpserver

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// crossEngineSessionHandler is a minimal noop SessionHandler for cross-engine
// integration tests. Returns a static JSON response so daemon.Spawn() succeeds
// without spawning a real subprocess.
type crossEngineSessionHandler struct{}

func (crossEngineSessionHandler) HandleRequest(_ context.Context, _ muxcore.ProjectContext, _ []byte) ([]byte, error) {
	return []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), nil
}

// TestCrossEngineIsolation proves that two daemon engines with distinct Names
// and BaseDirs do NOT see each other's owners via HandleListOwners or toolMuxList.
//
// This is the end-to-end assertion for T012 (muxcore multi-tenant FS isolation):
//   - daemon1: Name="mcp-mux",    BaseDir=dir1
//   - daemon2: Name="aimux-test", BaseDir=dir2
//
// After spawning one owner in each daemon:
//   - d1.HandleListOwners returns sid1 only (not sid2)
//   - d2.HandleListOwners returns sid2 only (not sid1)
//   - mcpserver.Server backed by ctl1 mux_list returns sid1 only
//   - mcpserver.Server backed by ctl2 mux_list returns sid2 only
func TestCrossEngineIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-engine integration test skipped in short mode")
	}

	// Two distinct BaseDirs provide FS namespace partitioning — the core
	// invariant being tested. Each engine's socket files are scoped to its own dir.
	dir1 := shortBaseDir(t, "ceisol1-")
	dir2 := shortBaseDir(t, "ceisol2-")

	logger := log.New(os.Stderr, "[cross-engine-test] ", 0)

	// Derive control socket paths: each is scoped to its own BaseDir+Name.
	ctl1 := serverid.DaemonControlPath(dir1, "mcp-mux")
	ctl2 := serverid.DaemonControlPath(dir2, "aimux-test")

	// Create daemon 1: engine name "mcp-mux".
	d1, err := daemon.New(daemon.Config{
		ControlPath:    ctl1,
		GracePeriod:    1 * time.Second,
		IdleTimeout:    5 * time.Second,
		SkipSnapshot:   true,
		Logger:         logger,
		SessionHandler: crossEngineSessionHandler{},
		Name:           "mcp-mux",
	})
	if err != nil {
		t.Fatalf("daemon1 New: %v", err)
	}
	t.Cleanup(func() { d1.Shutdown() })

	// Create daemon 2: engine name "aimux-test".
	d2, err := daemon.New(daemon.Config{
		ControlPath:    ctl2,
		GracePeriod:    1 * time.Second,
		IdleTimeout:    5 * time.Second,
		SkipSnapshot:   true,
		Logger:         logger,
		SessionHandler: crossEngineSessionHandler{},
		Name:           "aimux-test",
	})
	if err != nil {
		t.Fatalf("daemon2 New: %v", err)
	}
	t.Cleanup(func() { d2.Shutdown() })

	// Spawn one SessionHandler-topology owner in each daemon.
	// Mode="global" + SessionHandler set → in-process owner, no subprocess.
	_, sid1, _, err := d1.Spawn(control.Request{
		Cmd:  "spawn",
		Args: []string{"mcp-mux-server"},
		Mode: "global",
	})
	if err != nil {
		t.Fatalf("d1.Spawn: %v", err)
	}
	t.Logf("d1 (mcp-mux) owner sid: %s", sid1)

	_, sid2, _, err := d2.Spawn(control.Request{
		Cmd:  "spawn",
		Args: []string{"aimux-test-server"},
		Mode: "global",
	})
	if err != nil {
		t.Fatalf("d2.Spawn: %v", err)
	}
	t.Logf("d2 (aimux-test) owner sid: %s", sid2)

	// --- Assertion Path A: direct HandleListOwners (daemon-layer isolation) ---

	listResp1, err := d1.HandleListOwners(control.Request{Cmd: "list_owners"})
	if err != nil {
		t.Fatalf("d1.HandleListOwners: %v", err)
	}
	listResp2, err := d2.HandleListOwners(control.Request{Cmd: "list_owners"})
	if err != nil {
		t.Fatalf("d2.HandleListOwners: %v", err)
	}

	// d1 owns sid1, must not see sid2.
	ceAssertPresent(t, "mcp-mux daemon (HandleListOwners)", listResp1, sid1)
	ceAssertAbsent(t, "mcp-mux daemon (HandleListOwners)", listResp1, sid2)

	// d2 owns sid2, must not see sid1.
	ceAssertPresent(t, "aimux-test daemon (HandleListOwners)", listResp2, sid2)
	ceAssertAbsent(t, "aimux-test daemon (HandleListOwners)", listResp2, sid1)

	// --- Assertion Path B: full toolMuxList via mcpserver.Server ---
	// This exercises the complete stack: toolMuxList → control.Send → daemon
	// HandleListOwners → JSON serialization back through mcpserver.

	clientW1, clientR1, _ := newTestServerFull(t, ctl1, dir1)
	defer clientW1.Close()

	clientW2, clientR2, _ := newTestServerFull(t, ctl2, dir2)
	defer clientW2.Close()

	// Query mux_list from the mcp-mux server (all=true bypasses cwd filter).
	sendLine(t, clientW1, `{"jsonrpc":"2.0","id":100,"method":"tools/call","params":{"name":"mux_list","arguments":{"all":true}}}`)
	line1 := readLine(t, clientR1)
	resp1 := parseResponse(t, line1)
	assertID(t, resp1, 100)
	assertNoError(t, resp1)
	text1 := ceExtractText(t, resp1)
	t.Logf("mcp-mux mux_list output: %s", text1)

	// Query mux_list from the aimux-test server.
	sendLine(t, clientW2, `{"jsonrpc":"2.0","id":200,"method":"tools/call","params":{"name":"mux_list","arguments":{"all":true}}}`)
	line2 := readLine(t, clientR2)
	resp2 := parseResponse(t, line2)
	assertID(t, resp2, 200)
	assertNoError(t, resp2)
	text2 := ceExtractText(t, resp2)
	t.Logf("aimux-test mux_list output: %s", text2)

	// mcp-mux server sees its own owner (sid1), not the aimux-test owner (sid2).
	if !strings.Contains(text1, sid1) {
		t.Errorf("mcp-mux mux_list: own owner %s not found in output: %s", sid1, text1)
	}
	if strings.Contains(text1, sid2) {
		t.Errorf("mcp-mux mux_list: foreign aimux-test owner %s MUST NOT appear: %s", sid2, text1)
	}

	// aimux-test server sees its own owner (sid2), not the mcp-mux owner (sid1).
	if !strings.Contains(text2, sid2) {
		t.Errorf("aimux-test mux_list: own owner %s not found in output: %s", sid2, text2)
	}
	if strings.Contains(text2, sid1) {
		t.Errorf("aimux-test mux_list: foreign mcp-mux owner %s MUST NOT appear: %s", sid1, text2)
	}
}

// ceAssertPresent fails the test if sid is not found in the owners list.
func ceAssertPresent(t *testing.T, label string, resp control.ListOwnersResponse, sid string) {
	t.Helper()
	for _, o := range resp.Owners {
		if o.ServerID == sid {
			return
		}
	}
	t.Errorf("%s: own owner %s not found in response (%d total owners)", label, sid, len(resp.Owners))
}

// ceAssertAbsent fails the test if sid IS found in the owners list.
func ceAssertAbsent(t *testing.T, label string, resp control.ListOwnersResponse, sid string) {
	t.Helper()
	for _, o := range resp.Owners {
		if o.ServerID == sid {
			t.Errorf("%s: foreign owner %s MUST NOT appear in response", label, sid)
			return
		}
	}
}

// ceExtractText extracts the first content text string from a tools/call result.
func ceExtractText(t *testing.T, resp map[string]json.RawMessage) string {
	t.Helper()
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if len(result.Content) == 0 {
		return ""
	}
	return result.Content[0].Text
}
