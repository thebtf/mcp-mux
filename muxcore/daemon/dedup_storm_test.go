package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

// mockServerAbsPath returns the absolute path to the mock server source so
// the upstream subprocess can find it regardless of the per-test working
// directory used in Spawn requests.
func mockServerAbsPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../testdata/mock_server.go")
	if err != nil {
		t.Fatalf("resolve mock_server.go path: %v", err)
	}
	return abs
}

// TestSpawn_ParallelStormSharedConverges is the integration regression test
// for Engram #244 Bug 1 (shared classification does not share). Under the
// post-CR-002-phase-A default Mode flip from ModeCwd to ModeGlobal, N
// concurrent Spawn calls for the same (cmd, args) — regardless of cwd —
// compute the same global sid, hit `d.owners[sid]` exactly, and either
// create-as-placeholder or bind-as-secondary. The end state is ONE owner.
//
// Spec evidence (AC1, verbatim from spec.md):
//
//	"A parallel-spawn storm of N goroutines calling `Spawn` for the same
//	 `(cmd, args)` from N distinct cwds — where the upstream's `tools/list`
//	 classifies as `shared` — produces exactly **1** owner. OwnerCount() == 1,
//	 owner.cwdSet contains all N canonicalized cwds, every Spawn call returns
//	 the same server_id."
//
// I am proving that contract by running 4 goroutines through a barrier so
// they hit Spawn at the same instant (recreating the workstation-startup
// parallel-spawn storm). Each goroutine omits Request.Mode so the daemon
// applies the new ModeGlobal default per CR-002 phase A.
//
// Note: this test does NOT exercise the cross-cwd admission gate (CR-002
// phase B). It exercises the simpler convergence-via-exact-sid path that
// global-first default unlocks. The admission gate matters only for
// upstreams that classify as isolated mid-storm; shareable upstreams
// (which mock_server.go is, per its tools/list with no isolated patterns)
// converge correctly via the direct sid hit.
func TestSpawn_ParallelStormSharedConverges(t *testing.T) {
	orig := concurrentCreateWaitTimeout
	concurrentCreateWaitTimeout = 3 * time.Second
	t.Cleanup(func() { concurrentCreateWaitTimeout = orig })

	const N = 4
	results := make([]string, N)
	errs := make([]error, N)

	var wg sync.WaitGroup
	start := make(chan struct{})

	mockServer := mockServerAbsPath(t)

	// TempDirs allocated BEFORE testDaemon so the daemon's Shutdown LIFO
	// cleanup terminates upstream subprocesses BEFORE TempDir rm-rf runs
	// on Windows. Otherwise the subprocess's open cwd handle fails the
	// rm-rf and marks the test FAIL post-assertions.
	cwds := make([]string, N)
	for i := range N {
		cwds[i] = t.TempDir()
	}

	d := testDaemon(t)

	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := control.Request{
				Cmd:     "spawn",
				Command: "go",
				Args:    []string{"run", mockServer},
				Cwd:     cwds[i],
				// Mode omitted — relies on CR-002 phase A default flip to
				// ModeGlobal. Under the prior ModeCwd default this storm
				// would produce 4 distinct sids; under ModeGlobal it
				// converges to 1.
			}
			_, sid, _, err := d.Spawn(req)
			results[i] = sid
			errs[i] = err
		}()
	}

	close(start) // release the storm
	wg.Wait()

	// Surface any individual Spawn failures with full context.
	for i, e := range errs {
		if e != nil {
			t.Fatalf("Spawn %d failed: %v", i, e)
		}
	}

	// All goroutines must have converged to a single shared owner sid.
	seen := make(map[string]int)
	for _, sid := range results {
		seen[sid]++
	}
	if len(seen) != 1 {
		t.Errorf("parallel storm produced %d distinct shared-owner sids (want 1): %v",
			len(seen), seen)
	}

	// Live owner count must also be 1 — no extra orphans created on
	// losing-race paths.
	if got := d.OwnerCount(); got != 1 {
		t.Errorf("OwnerCount = %d after storm, want 1", got)
	}

	var sharedSID string
	for sid := range seen {
		sharedSID = sid
	}
	entry := d.Entry(sharedSID)
	if entry == nil || entry.Owner == nil {
		t.Fatalf("shared owner %q missing after storm", sharedSID)
	}
	wantCwds := make(map[string]struct{}, N)
	for _, cwd := range cwds {
		wantCwds[serverid.CanonicalizePath(cwd)] = struct{}{}
	}
	gotCwds := entry.Owner.ExportSnapshot().CwdSet
	if len(gotCwds) != len(wantCwds) {
		t.Fatalf("shared owner cwdSet = %v, want %d distinct roots", gotCwds, len(wantCwds))
	}
	for _, cwd := range gotCwds {
		if _, ok := wantCwds[cwd]; !ok {
			t.Fatalf("shared owner cwdSet contains unexpected root %q; want %v", cwd, wantCwds)
		}
	}
}

type templateStormEndpoint struct {
	path    string
	sid     string
	token   string
	conn    net.Conn
	scanner *bufio.Scanner
}

func completeStormTemplate(classification classify.SharingMode, cwd string, env map[string]string) mcpsnapshot.OwnerSnapshot {
	snap := daemonMaterializationSnapshot(false)
	snap.Classification = classification
	snap.Cwd = cwd
	snap.Env = env
	snap.CachedPrompts = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":"cached-prompts","result":{"prompts":[{"name":"cached-prompt"}]}}`))
	snap.CachedResources = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":"cached-resources","result":{"resources":[{"uri":"file:///cached"}]}}`))
	snap.CachedResourceTemplates = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":"cached-resource-templates","result":{"resourceTemplates":[{"uriTemplate":"file:///{path}"}]}}`))
	return snap
}

func stormMaterializationHandler(starts *atomic.Int32, classification classify.SharingMode) func(context.Context, io.Reader, io.Writer) error {
	return func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			var result string
			switch req.Method {
			case "initialize":
				result = fmt.Sprintf(`{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":%q}},"serverInfo":{"name":"storm","version":"1"}}`, classification)
			case "tools/list":
				result = `{"tools":[{"name":"live-tool"}]}`
			case "prompts/list":
				result = `{"prompts":[]}`
			case "resources/list":
				result = `{"resources":[]}`
			case "resources/templates/list":
				result = `{"resourceTemplates":[]}`
			case "custom/wake":
				result = `{"woke":true}`
			default:
				continue
			}
			if err := writeDaemonResponse(stdout, req.ID, result); err != nil {
				return err
			}
		}
		return scanner.Err()
	}
}

func writeStormDiscoveryBurst(t *testing.T, conn io.Writer, base int) {
	t.Helper()
	frames := []string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"codex","version":"1"}}}`, base),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`, base+1),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"prompts/list","params":{}}`, base+2),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"resources/list","params":{}}`, base+3),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"resources/templates/list","params":{}}`, base+4),
	}
	for _, frame := range frames {
		if _, err := fmt.Fprintln(conn, frame); err != nil {
			t.Fatalf("write discovery burst: %v", err)
		}
	}
}

func readStormDiscoveryBurst(t *testing.T, scanner *bufio.Scanner, base int) {
	t.Helper()
	for offset := range 5 {
		readDaemonResponseID(t, scanner, fmt.Sprintf("%d", base+offset))
	}
}

func countLiveStormProcesses(d *Daemon) int {
	d.mu.RLock()
	owners := make([]*owner.Owner, 0, len(d.owners))
	for _, entry := range d.owners {
		if entry != nil && entry.Owner != nil {
			owners = append(owners, entry.Owner)
		}
	}
	d.mu.RUnlock()
	live := 0
	for _, o := range owners {
		if value, _ := o.Status()["upstream_live"].(bool); value {
			live++
		}
	}
	return live
}

func TestTemplateBackedIsolatedStormMaterializesOnlyDemandedOwner(t *testing.T) {
	const n = 8
	const command = "isolated-template-storm"
	env := map[string]string{"STORM_CONFIG_PATH": "/cfg/isolated"}
	var starts atomic.Int32
	d := testDaemon(t)
	d.handlerFunc = stormMaterializationHandler(&starts, "isolated")
	cwds := make([]string, n)
	for i := range cwds {
		cwds[i] = t.TempDir()
		d.updateTemplate(command, nil, completeStormTemplate("isolated", cwds[i], env))
	}

	results := make([]templateStormEndpoint, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].path, results[i].sid, results[i].token, errs[i] = d.Spawn(control.Request{Command: command, Mode: "isolated", Cwd: cwds[i], Env: env})
		}(i)
	}
	close(start)
	wg.Wait()
	seen := make(map[string]struct{}, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("isolated Spawn %d: %v", i, err)
		}
		seen[results[i].sid] = struct{}{}
		results[i].conn, results[i].scanner = connectSpawnedOwner(t, results[i].path, results[i].token)
		defer results[i].conn.Close()
		writeStormDiscoveryBurst(t, results[i].conn, 100+i*10)
	}
	for i := range results {
		readStormDiscoveryBurst(t, results[i].scanner, 100+i*10)
	}
	if len(seen) != n || d.OwnerCount() != n {
		t.Fatalf("isolated storm owners: sids=%d count=%d, want %d", len(seen), d.OwnerCount(), n)
	}
	if starts.Load() != 0 || countLiveStormProcesses(d) != 0 {
		t.Fatalf("handshake-only isolated storm started upstreams: starts=%d live=%d", starts.Load(), countLiveStormProcesses(d))
	}

	target := results[n/2]
	if _, err := fmt.Fprintln(target.conn, `{"jsonrpc":"2.0","id":999,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write wake: %v", err)
	}
	if got := readDaemonResponseID(t, target.scanner, "999"); !strings.Contains(string(got), `"woke":true`) {
		t.Fatalf("wake response=%s", got)
	}
	waitForDaemonCondition(t, time.Second, func() bool { return starts.Load() == 1 && countLiveStormProcesses(d) == 1 }, "isolated demand did not materialize exactly one owner")
	for _, endpoint := range results {
		entry := d.Entry(endpoint.sid)
		if entry == nil || entry.Owner == nil {
			t.Fatalf("missing isolated owner %s", endpoint.sid)
		}
		generation := entry.Owner.Status()["materialization_generation"].(uint64)
		if endpoint.sid == target.sid && generation != 1 {
			t.Fatalf("demanded owner generation=%d, want 1", generation)
		}
		if endpoint.sid != target.sid && generation != 0 {
			t.Fatalf("unused owner %s generation=%d, want 0", endpoint.sid, generation)
		}
	}
}

func TestTemplateBackedSharedStormConvergesOneOwnerOneGeneration(t *testing.T) {
	const n = 8
	const command = "shared-template-storm"
	env := map[string]string{"STORM_CONFIG_PATH": "/cfg/shared"}
	var starts atomic.Int32
	cwds := make([]string, n)
	for i := range cwds {
		cwds[i] = t.TempDir()
	}
	d := testDaemon(t)
	d.handlerFunc = stormMaterializationHandler(&starts, "shared")
	d.updateTemplate(command, nil, completeStormTemplate("shared", cwds[0], env))

	results := make([]templateStormEndpoint, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].path, results[i].sid, results[i].token, errs[i] = d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwds[i], Env: env})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("shared Spawn %d: %v", i, err)
		}
		if results[i].sid != results[0].sid {
			t.Fatalf("shared storm sid[%d]=%s, want %s", i, results[i].sid, results[0].sid)
		}
		results[i].conn, results[i].scanner = connectSpawnedOwner(t, results[i].path, results[i].token)
		defer results[i].conn.Close()
		writeStormDiscoveryBurst(t, results[i].conn, 300+i*10)
	}
	for i := range results {
		readStormDiscoveryBurst(t, results[i].scanner, 300+i*10)
	}
	if d.OwnerCount() != 1 || starts.Load() != 0 || countLiveStormProcesses(d) != 0 {
		t.Fatalf("shared handshake storm owners=%d starts=%d live=%d, want 1/0/0", d.OwnerCount(), starts.Load(), countLiveStormProcesses(d))
	}
	if _, err := fmt.Fprintln(results[0].conn, `{"jsonrpc":"2.0","id":777,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write shared wake: %v", err)
	}
	readDaemonResponseID(t, results[0].scanner, "777")
	waitForDaemonCondition(t, time.Second, func() bool { return starts.Load() == 1 && countLiveStormProcesses(d) == 1 }, "shared storm did not converge to one live generation")
	entry := d.Entry(results[0].sid)
	if got := entry.Owner.Status()["materialization_generation"]; got != uint64(1) {
		t.Fatalf("shared owner generation=%v, want 1", got)
	}
}

func TestCodexEightEntryHandshakeBurstSameTransportWake(t *testing.T) {
	const n = 8
	env := map[string]string{"CODEX_CONFIG_PATH": "/cfg/codex"}
	var starts atomic.Int32
	d := testDaemon(t)
	d.handlerFunc = stormMaterializationHandler(&starts, "shared")
	endpoints := make([]templateStormEndpoint, n)
	for i := range endpoints {
		command := fmt.Sprintf("codex-entry-%d", i)
		cwd := t.TempDir()
		d.updateTemplate(command, nil, completeStormTemplate("shared", cwd, env))
		path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwd, Env: env})
		if err != nil {
			t.Fatalf("Spawn entry %d: %v", i, err)
		}
		endpoints[i] = templateStormEndpoint{path: path, sid: sid, token: token}
		endpoints[i].conn, endpoints[i].scanner = connectSpawnedOwner(t, path, token)
		defer endpoints[i].conn.Close()
		writeStormDiscoveryBurst(t, endpoints[i].conn, 500+i*10)
	}
	for i := range endpoints {
		readStormDiscoveryBurst(t, endpoints[i].scanner, 500+i*10)
	}
	beforeOwners, beforeProcesses := d.OwnerCount(), countLiveStormProcesses(d)
	if beforeOwners != n || beforeProcesses != 0 || starts.Load() != 0 {
		t.Fatalf("Codex burst before wake owners=%d processes=%d starts=%d, want %d/0/0", beforeOwners, beforeProcesses, starts.Load(), n)
	}

	target := &endpoints[3]
	if _, err := fmt.Fprintln(target.conn, `{"jsonrpc":"2.0","id":909,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("same-transport wake write: %v", err)
	}
	response := readDaemonResponseID(t, target.scanner, "909")
	if !strings.Contains(string(response), `"woke":true`) {
		t.Fatalf("same-transport wake response=%s", response)
	}
	afterOwners, afterProcesses := d.OwnerCount(), countLiveStormProcesses(d)
	if afterOwners != n || afterProcesses != 1 || starts.Load() != 1 {
		t.Fatalf("Codex burst after wake owners=%d processes=%d starts=%d, want %d/1/1", afterOwners, afterProcesses, starts.Load(), n)
	}
}
