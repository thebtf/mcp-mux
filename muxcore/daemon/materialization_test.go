package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

func daemonMaterializationSnapshot(persistent bool) mcpsnapshot.OwnerSnapshot {
	initResp := `{"jsonrpc":"2.0","id":"cached-init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"cached","version":"1"}}}`
	toolsResp := `{"jsonrpc":"2.0","id":"cached-tools","result":{"tools":[{"name":"cached-tool"}]}}`
	return mcpsnapshot.OwnerSnapshot{
		Classification: "shared",
		CachedInit:     base64.StdEncoding.EncodeToString([]byte(initResp)),
		CachedTools:    base64.StdEncoding.EncodeToString([]byte(toolsResp)),
		Persistent:     persistent,
	}
}

type testReadDeadlineConn struct {
	net.Conn
}

func (c *testReadDeadlineConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func connectSpawnedOwner(t *testing.T, path, token string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	rawConn, err := ipc.Dial(path)
	if err != nil {
		t.Fatalf("ipc.Dial: %v", err)
	}
	conn := &testReadDeadlineConn{Conn: rawConn}
	if _, err := fmt.Fprintln(conn, token); err != nil {
		_ = conn.Close()
		t.Fatalf("write handshake token: %v", err)
	}
	return conn, bufio.NewScanner(conn)
}

func waitForDaemonCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}

func readDaemonResponseID(t *testing.T, scanner *bufio.Scanner, id string) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			t.Fatalf("response stream ended: %v", scanner.Err())
		}
		line := append([]byte(nil), scanner.Bytes()...)
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("invalid JSON-RPC response %q: %v", line, err)
		}
		if string(msg["id"]) == id {
			return line
		}
	}
	t.Fatalf("timed out waiting for response id %s", id)
	return nil
}

func writeDaemonResponse(w io.Writer, id json.RawMessage, result string) error {
	_, err := fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
	return err
}

func TestRejectedOwnerCachePublishDoesNotCommitPersistence(t *testing.T) {
	d := testDaemon(t)
	command := "rejected-owner-cache-publish"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(false))
	_, sid, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer d.Remove(sid) //nolint:errcheck

	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatalf("spawned owner %q missing", sid)
	}
	rejected := daemonMaterializationSnapshot(true)
	rejected.CachedTools = ""
	if d.publishOwnerCache(entry.Owner, rejected) {
		t.Fatal("invalid cache snapshot was published")
	}
	if entry.Persistent {
		t.Fatal("rejected cache publication committed owner persistence")
	}
}

func TestSpawnTemplateCacheHitStaysCacheOnlyAndServesTools(t *testing.T) {
	d := testDaemon(t)
	command := "definitely-not-a-real-template-command"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(false))

	path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatal("template-backed owner missing")
	}
	if state := entry.Owner.MaterializationState(); state != owner.MaterializationCacheOnly {
		t.Fatalf("materialization state=%s, want cache-only", state)
	}
	status := entry.Owner.Status()
	if got := status["materialization_generation"]; got != uint64(0) {
		t.Fatalf("materialization generation=%v, want 0", got)
	}
	if got := status["upstream_pid"]; got != 0 {
		t.Fatalf("upstream pid=%v, want 0", got)
	}

	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":41,"method":"tools/list","params":{}}`); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}
	resp := readDaemonResponseID(t, scanner, "41")
	if !strings.Contains(string(resp), "cached-tool") {
		t.Fatalf("cached tools/list response=%s", resp)
	}
	if state := entry.Owner.MaterializationState(); state != owner.MaterializationCacheOnly {
		t.Fatalf("cached hit changed state to %s", state)
	}
}

func TestTemplateCacheRequiresCompatibleContext(t *testing.T) {
	t.Run("shared env bucket may cross cwd", func(t *testing.T) {
		d := testDaemon(t)
		command := "template-shared-compatible"
		cwdA := t.TempDir()
		snap := daemonMaterializationSnapshot(false)
		snap.Cwd = cwdA
		snap.Env = map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "same"}
		d.updateTemplate(command, nil, snap)

		_, sid, _, err := d.Spawn(control.Request{
			Command: command,
			Mode:    "global",
			Cwd:     t.TempDir(),
			Env:     map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "same"},
		})
		if err != nil {
			t.Fatalf("Spawn compatible shared template: %v", err)
		}
		if entry := d.Entry(sid); entry == nil || entry.Owner.MaterializationState() != owner.MaterializationCacheOnly {
			t.Fatalf("compatible shared template did not stay cache-only: %#v", entry)
		}
	})

	t.Run("different env gets no cached replay", func(t *testing.T) {
		d := testDaemon(t)
		command := "definitely-not-a-real-env-mismatch-command"
		snap := daemonMaterializationSnapshot(false)
		snap.Cwd = t.TempDir()
		snap.Env = map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "A"}
		d.updateTemplate(command, nil, snap)

		if _, _, _, err := d.Spawn(control.Request{
			Command: command,
			Mode:    "global",
			Cwd:     t.TempDir(),
			Env:     map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "B"},
		}); err == nil {
			t.Fatal("Spawn with incompatible env unexpectedly replayed cached template")
		}
	})

	t.Run("isolated template requires exact canonical cwd", func(t *testing.T) {
		d := testDaemon(t)
		command := "definitely-not-a-real-isolated-cwd-command"
		cwdA := t.TempDir()
		snap := daemonMaterializationSnapshot(false)
		snap.Classification = "isolated"
		snap.Cwd = cwdA
		snap.Env = map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "same"}
		d.updateTemplate(command, nil, snap)

		if _, _, _, err := d.Spawn(control.Request{
			Command: command,
			Mode:    "global",
			Cwd:     t.TempDir(),
			Env:     map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "same"},
		}); err == nil {
			t.Fatal("isolated template from another cwd was replayed")
		}
	})
}

func TestTemplateRevisionRevalidatedBeforePromotion(t *testing.T) {
	d := testDaemon(t)
	command := "template-revision-refresh"
	cwd := t.TempDir()
	env := map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "same"}
	oldSnap := daemonMaterializationSnapshot(false)
	oldSnap.Cwd = cwd
	oldSnap.Env = env
	d.updateTemplate(command, nil, oldSnap)

	newSnap := daemonMaterializationSnapshot(false)
	newSnap.Cwd = cwd
	newSnap.Env = env
	newTools := `{"jsonrpc":"2.0","id":"cached-tools","result":{"tools":[{"name":"new-template-tool"}]}}`
	newSnap.CachedTools = base64.StdEncoding.EncodeToString([]byte(newTools))
	var hookCalls atomic.Int32
	d.beforeTemplatePromotion = func() {
		if hookCalls.Add(1) == 1 {
			d.updateTemplate(command, nil, newSnap)
		}
	}

	path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwd, Env: env})
	if err != nil {
		t.Fatalf("Spawn after revision refresh: %v", err)
	}
	if got := hookCalls.Load(); got != 2 {
		t.Fatalf("promotion hook calls=%d, want 2 template constructions", got)
	}
	if got := d.OwnerCount(); got != 1 {
		t.Fatalf("owner count=%d, want exactly one promoted owner", got)
	}
	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":91,"method":"tools/list","params":{}}`); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}
	resp := readDaemonResponseID(t, scanner, "91")
	if !strings.Contains(string(resp), "new-template-tool") {
		t.Fatalf("promoted stale template for owner %s: %s", sid, resp)
	}
}

func TestTemplateInvalidationPreventsStalePromotion(t *testing.T) {
	d := testDaemon(t)
	command := "definitely-not-a-real-invalidated-template-command"
	cwd := t.TempDir()
	env := map[string]string{"MCPMUX_TEMPLATE_TEST_TOKEN": "same"}
	snap := daemonMaterializationSnapshot(false)
	snap.Cwd = cwd
	snap.Env = env
	d.updateTemplate(command, nil, snap)

	var hookCalls atomic.Int32
	d.beforeTemplatePromotion = func() {
		if hookCalls.Add(1) == 1 {
			d.invalidateTemplate(command, nil)
		}
	}
	if _, _, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwd, Env: env}); err == nil {
		t.Fatal("invalidated template was promoted instead of taking the cold path")
	}
	if got := d.OwnerCount(); got != 0 {
		t.Fatalf("owner count=%d after invalidation race, want 0", got)
	}
}

func TestListChangedInvalidatesDaemonTemplate(t *testing.T) {
	var starts atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"live","version":"2"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"live-tool"}]}`); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(stdout, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeDaemonResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	d, err := New(Config{
		Name:             "materialization-invalidation-test",
		ControlPath:      shortSocketPath(t, "materialization-invalidation.ctl"),
		SkipSnapshot:     true,
		HandlerFunc:      handler,
		OwnerIdleTimeout: time.Minute,
		Logger:           testLogger(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	command := "in-process-invalidation"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(false))
	path, _, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":51,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write wake request: %v", err)
	}
	_ = readDaemonResponseID(t, scanner, "51")
	waitForDaemonCondition(t, 2*time.Second, func() bool {
		_, ok := d.getTemplate(command, nil)
		return !ok
	}, "list_changed did not invalidate daemon template")
	if got := starts.Load(); got != 1 {
		t.Fatalf("upstream starts=%d, want 1", got)
	}
}

func TestDynamicPersistentDeclarationRestartsWithoutDemand(t *testing.T) {
	var starts atomic.Int32
	allowFirstExit := make(chan struct{})
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				result := `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared","persistent":true}},"serverInfo":{"name":"persistent","version":"1"}}`
				if err := writeDaemonResponse(stdout, req.ID, result); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeDaemonResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
				if generation == 1 {
					<-allowFirstExit
					return nil
				}
			}
		}
		return scanner.Err()
	}

	d, err := New(Config{
		Name:             "dynamic-persistent-test",
		ControlPath:      shortSocketPath(t, "dynamic-persistent.ctl"),
		SkipSnapshot:     true,
		HandlerFunc:      handler,
		OwnerIdleTimeout: time.Minute,
		Logger:           testLogger(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	command := "in-process-persistent"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(false))
	path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("template hit eagerly started %d generations before demand", got)
	}
	conn, scanner := connectSpawnedOwner(t, path, token)
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":61,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write wake request: %v", err)
	}
	_ = readDaemonResponseID(t, scanner, "61")
	entry := d.Entry(sid)
	waitForDaemonCondition(t, 2*time.Second, func() bool { return entry != nil && entry.Persistent }, "persistent declaration was not applied")
	_ = conn.Close()
	waitForDaemonCondition(t, 2*time.Second, func() bool { return entry.Owner.SessionCount() == 0 }, "session did not disconnect")
	close(allowFirstExit)
	waitForDaemonCondition(t, 2*time.Second, func() bool { return starts.Load() >= 2 }, "persistent owner did not restart without demand")
}

func TestPersistentTemplateMaterializesEagerly(t *testing.T) {
	var starts atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"persistent":true}},"serverInfo":{"name":"persistent-template","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	d, err := New(Config{
		Name:             "persistent-template-test",
		ControlPath:      shortSocketPath(t, "persistent-template.ctl"),
		SkipSnapshot:     true,
		HandlerFunc:      handler,
		OwnerIdleTimeout: time.Minute,
		Logger:           testLogger(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	command := "persistent-template"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(true))
	_, sid, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	entry := d.Entry(sid)
	waitForDaemonCondition(t, 2*time.Second, func() bool {
		return starts.Load() == 1 && entry != nil && entry.Owner.MaterializationState() == owner.MaterializationReady
	}, "persistent template did not materialize eagerly")
	if !entry.Persistent {
		t.Fatal("persistent template entry was not marked persistent")
	}
}

func TestSerializeSnapshotDuringMaterializationPreservesLiveSession(t *testing.T) {
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })

	firstInit := make(chan struct{})
	var starts atomic.Int32
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				if generation == 1 {
					select {
					case <-firstInit:
					default:
						close(firstInit)
					}
					continue
				}
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"restart","version":"2"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeDaemonResponse(stdout, req.ID, `{"generation":2}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	d, err := New(Config{
		Name:             "snapshot-materialization-race",
		ControlPath:      shortSocketPath(t, "snapshot-materialization-race.ctl"),
		SkipSnapshot:     true,
		HandlerFunc:      handler,
		OwnerIdleTimeout: time.Minute,
		Logger:           testLogger(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })
	command := "snapshot-materialization-upstream"
	template := daemonMaterializationSnapshot(false)
	template.Env = map[string]string{"GITHUB_TOKEN": "snapshot-secret"}
	d.updateTemplate(command, nil, template)
	path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir(), Env: map[string]string{"GITHUB_TOKEN": "snapshot-secret"}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":71,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write first demand: %v", err)
	}
	select {
	case <-firstInit:
	case <-time.After(time.Second):
		t.Fatal("first generation did not reach initialize")
	}
	snapshotPath, err := d.SerializeSnapshot()
	if err != nil {
		t.Fatalf("SerializeSnapshot: %v", err)
	}
	if snapshotPath == "" {
		t.Fatal("SerializeSnapshot returned empty path")
	}
	first := readDaemonResponseID(t, scanner, "71")
	if !strings.Contains(string(first), "cancelled for restart") {
		t.Fatalf("first demand was not terminated by restart barrier: %s", first)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner.SessionCount() != 1 || !entry.Owner.CacheReady() {
		t.Fatalf("restart snapshot lost live owner/session/cache: entry=%+v", entry)
	}
	if entry.Owner.Status()["restart_pin_count"] != int64(0) {
		t.Fatalf("restart pin remained after serialization: %#v", entry.Owner.Status())
	}
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":72,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write post-snapshot demand: %v", err)
	}
	second := readDaemonResponseID(t, scanner, "72")
	if !strings.Contains(string(second), `"generation":2`) {
		t.Fatalf("live session did not recover after snapshot: %s", second)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("upstream starts=%d, want exactly 2 generations", got)
	}
}

func TestColdFastHandlerPublishesOnlyAfterExactOwnerRegistration(t *testing.T) {
	d := testDaemon(t)
	var starts atomic.Int32
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"fast","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"fast-tool"}]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	promotionCheck := make(chan error, 1)
	d.beforeColdOwnerPromotion = func(got *owner.Owner) {
		if state := got.MaterializationState(); state != owner.MaterializationCacheOnly {
			promotionCheck <- fmt.Errorf("cold owner state before promotion=%s, want cache_only", state)
			return
		}
		promotionCheck <- nil
	}
	publishCheck := make(chan error, 1)
	d.beforeOwnerCachePublish = func(got *owner.Owner) {
		entry := d.Entry(got.ServerID())
		if entry == nil || entry.Owner != got {
			publishCheck <- fmt.Errorf("cache publish observed entry=%+v for owner=%p", entry, got)
			return
		}
		publishCheck <- nil
	}

	const secret = "raw-config-secret-value"
	command := "cold-fast-publish"
	_, sid, _, err := d.Spawn(control.Request{
		Command: command,
		Mode:    "global",
		Cwd:     t.TempDir(),
		Env:     map[string]string{"CONFIG_PATH": secret},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case err := <-promotionCheck:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cold owner promotion check did not complete")
	}
	select {
	case err := <-publishCheck:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fast handler did not publish cache")
	}
	entry := d.Entry(sid)
	if entry == nil {
		t.Fatal("cold owner missing after spawn")
	}
	waitForDaemonCondition(t, 3*time.Second, entry.Owner.CacheReady, "cold owner cache did not become ready")
	if _, ok := d.getTemplate(command, nil); !ok {
		t.Fatal("cold owner did not publish template")
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("upstream starts=%d, want one exact owner generation", got)
	}
	statusJSON, marshalErr := json.Marshal(d.HandleStatus())
	if marshalErr != nil {
		t.Fatalf("marshal status: %v", marshalErr)
	}
	if strings.Contains(string(statusJSON), secret) {
		t.Fatal("raw identity value leaked into public status")
	}
}

func TestCompatibleTemplateCachedBurstThenUncachedWakeSameIPCSession(t *testing.T) {
	d := testDaemon(t)
	var starts atomic.Int32
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"live","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"live-tool"}]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeDaemonResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	command := "same-session-template-wake"
	template := daemonMaterializationSnapshot(false)
	template.CachedPrompts = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":1,"result":{"prompts":[]}}`))
	template.CachedResources = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":1,"result":{"resources":[]}}`))
	template.CachedResourceTemplates = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":1,"result":{"resourceTemplates":[]}}`))
	template.Cwd = t.TempDir()
	d.updateTemplate(command, nil, template)
	path, _, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: template.Cwd})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	requests := []struct {
		id     int
		method string
	}{
		{1, "initialize"},
		{2, "tools/list"},
		{3, "prompts/list"},
		{4, "resources/list"},
		{5, "resources/templates/list"},
	}
	for _, request := range requests {
		if _, err := fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}`+"\n", request.id, request.method); err != nil {
			t.Fatalf("write %s: %v", request.method, err)
		}
		_ = readDaemonResponseID(t, scanner, fmt.Sprint(request.id))
		if request.method == "initialize" {
			if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
				t.Fatalf("write initialized: %v", err)
			}
		}
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("cached host burst started upstream %d times, want 0", got)
	}
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":6,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write uncached wake: %v", err)
	}
	wake := readDaemonResponseID(t, scanner, "6")
	if !strings.Contains(string(wake), `"ok":true`) {
		t.Fatalf("uncached wake response=%s", wake)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("uncached wake starts=%d, want exactly 1 on same IPC session", got)
	}
}

func TestStaleOwnerCallbacksCannotMutateReplacementGeneration(t *testing.T) {
	d := testDaemon(t)
	d.supervisor = nil
	sid := "stale-callback-generation"
	stale := testReconnectOwner(t, sid)
	replacement := testReconnectOwner(t, sid)
	command := "stale-callback-command"
	lastSession := time.Now().Add(-time.Hour)
	entry := &OwnerEntry{
		Owner:           replacement,
		ServerID:        sid,
		Command:         command,
		LastSession:     lastSession,
		OwnerGeneration: "owner_gen_replacement",
	}
	d.mu.Lock()
	d.owners[sid] = entry
	d.mu.Unlock()

	snap := daemonMaterializationSnapshot(false)
	snap.Cwd = t.TempDir()
	if !d.publishOwnerCache(replacement, snap) {
		t.Fatal("replacement cache publish was rejected")
	}
	wantTemplate, ok := d.getTemplate(command, nil)
	if !ok {
		t.Fatal("replacement template missing")
	}

	if d.setOwnerPersistent(stale, true) {
		t.Fatal("stale persistent callback was accepted")
	}
	d.resolveOwnerPersistent(stale, true)
	d.invalidateOwnerTemplate(stale)
	staleSnap := snap
	staleSnap.CachedTools = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"stale"}]}}`))
	if d.publishOwnerCache(stale, staleSnap) {
		t.Fatal("stale cache publish was accepted")
	}
	d.onZeroSessions(stale)
	d.onUpstreamExit(stale)

	current := d.Entry(sid)
	if current != entry || current.Owner != replacement {
		t.Fatal("stale callback replaced or removed the current owner")
	}
	if current.Persistent {
		t.Fatal("stale callback changed replacement persistence")
	}
	if !current.LastSession.Equal(lastSession) {
		t.Fatal("stale zero-session callback changed replacement lifecycle time")
	}
	gotTemplate, ok := d.getTemplate(command, nil)
	if !ok || gotTemplate.CachedTools != wantTemplate.CachedTools {
		t.Fatal("stale callback invalidated or overwrote replacement template")
	}
}

func TestMaterializationFreshSharedTemplateKeepsLiveOwnerIsolated(t *testing.T) {
	d := testDaemon(t)
	const sid = "t044-live-isolated"
	const command = "t044-command"
	args := []string{"--serve"}
	env := map[string]string{"SERVICE_CONFIG_PATH": "/cfg/t044"}
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"fresh","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"fresh-shared"}]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	snap := daemonMaterializationSnapshot(false)
	snap.Classification = "isolated"
	snap.ClassificationSource = "capability"
	snap.Cwd = "/project/live"
	snap.Env = env
	o, err := owner.NewOwnerFromSnapshot(owner.OwnerConfig{
		Command:               command,
		Args:                  args,
		Cwd:                   snap.Cwd,
		Env:                   env,
		HandlerFunc:           handler,
		IPCPath:               shortSocketPath(t, "t044-owner.sock"),
		ServerID:              sid,
		MaterializationPolicy: owner.MaterializationOnDemand,
		OnCacheReady:          d.publishOwnerCache,
		Logger:                d.logger,
	}, snap)
	if err != nil {
		t.Fatalf("NewOwnerFromSnapshot: %v", err)
	}
	defer o.Shutdown()
	d.mu.Lock()
	d.owners[sid] = &OwnerEntry{Owner: o, ServerID: sid, Command: command, Args: args, Cwd: snap.Cwd, Env: env}
	d.mu.Unlock()

	if err := o.StartInitialMaterialization(); err != nil {
		t.Fatalf("StartInitialMaterialization: %v", err)
	}
	waitForDaemonCondition(t, time.Second, func() bool {
		return o.MaterializationState() == owner.MaterializationReady
	}, "fresh shared generation did not commit")
	if !o.IsClassifiedIsolated() {
		t.Fatal("fresh less-restrictive classification reopened live isolated owner")
	}
	if o.PreRegister("feedface", "/project/other", env) {
		t.Fatal("live isolated owner accepted fresh fan-in after shared refresh")
	}
	match, ok := d.getCompatibleTemplate(command, args, "/project/future", mergeEnv(env))
	if !ok {
		t.Fatal("fresh shared template was not published for future owner")
	}
	if match.snapshot.Classification != "shared" {
		t.Fatalf("future template classification = %q, want fresh shared", match.snapshot.Classification)
	}
}

func TestMaterializingOwnerAdmissionFreezeDoesNotTriggerReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{name: "exact-global", mode: "global"},
		{name: "legacy-cwd-shared-scan", mode: "cwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDaemon(t)
			var starts atomic.Int32
			d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
					switch req.Method {
					case "initialize":
						if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"admission-freeze","version":"1"}}`); err != nil {
							return err
						}
					case "tools/list":
						if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"probe"}]}`); err != nil {
							return err
						}
					}
				}
				return scanner.Err()
			}
			publishEntered := make(chan struct{})
			releasePublish := make(chan struct{})
			var enteredOnce sync.Once
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(releasePublish) }) })
			d.beforeOwnerCachePublish = func(*owner.Owner) {
				enteredOnce.Do(func() { close(publishEntered) })
				<-releasePublish
			}

			command := "admission-freeze-" + tc.name
			path, sid, _, err := d.Spawn(control.Request{Command: command, Mode: tc.mode, Cwd: t.TempDir()})
			if err != nil {
				t.Fatalf("Spawn first: %v", err)
			}
			select {
			case <-publishEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("owner did not enter cache-commit admission freeze")
			}
			entry := d.Entry(sid)
			type spawnResult struct {
				path  string
				sid   string
				token string
				err   error
			}
			resultCh := make(chan spawnResult, 1)
			go func() {
				path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: tc.mode, Cwd: t.TempDir()})
				resultCh <- spawnResult{path: path, sid: sid, token: token, err: err}
			}()
			time.Sleep(50 * time.Millisecond)
			select {
			case result := <-resultCh:
				t.Fatalf("second Spawn bypassed cache-commit barrier: %+v", result)
			default:
			}
			releaseOnce.Do(func() { close(releasePublish) })

			select {
			case result := <-resultCh:
				if result.err != nil {
					t.Fatalf("Spawn second: %v", result.err)
				}
				if result.path != path || result.sid != sid || result.token == "" {
					t.Fatalf("second Spawn = (%q, %q, token=%v), want existing (%q, %q)", result.path, result.sid, result.token != "", path, sid)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("second Spawn did not resume after cache commit")
			}
			if d.Entry(sid) != entry || d.OwnerCount() != 1 || starts.Load() != 1 {
				t.Fatalf("admission freeze replaced owner: same=%v owners=%d starts=%d", d.Entry(sid) == entry, d.OwnerCount(), starts.Load())
			}
		})
	}
}

func TestLiveSessionsJoinSingleReplacementControllerDuringUnexpectedExit(t *testing.T) {
	d := testDaemon(t)
	var starts atomic.Int32
	gen2Init := make(chan struct{})
	releaseGen2 := make(chan struct{})
	var gen2Once sync.Once
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				if generation == 2 {
					gen2Once.Do(func() { close(gen2Init) })
					<-releaseGen2
				}
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"recovery","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"crash"}]}`); err != nil {
					return err
				}
			case "tools/call":
				if generation == 1 {
					return fmt.Errorf("synthetic unexpected exit")
				}
			case "ping":
				if err := writeDaemonResponse(stdout, req.ID, fmt.Sprintf(`{"generation":%d}`, generation)); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	path, sid, token1, err := d.Spawn(control.Request{Command: "live-recovery", Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn first: %v", err)
	}
	_, sid2, token2, err := d.Spawn(control.Request{Command: "live-recovery", Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn second: %v", err)
	}
	if sid2 != sid {
		t.Fatalf("second sid=%s, want shared sid %s", sid2, sid)
	}
	entry := d.Entry(sid)
	readyDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(readyDeadline) && entry.Owner.MaterializationState() != owner.MaterializationReady {
		time.Sleep(10 * time.Millisecond)
	}
	if entry.Owner.MaterializationState() != owner.MaterializationReady {
		t.Fatalf("initial generation did not become ready: starts=%d status=%#v", starts.Load(), entry.Owner.Status())
	}
	conn1, scan1 := connectSpawnedOwner(t, path, token1)
	defer conn1.Close()
	conn2, scan2 := connectSpawnedOwner(t, path, token2)
	defer conn2.Close()
	for id, conn := range map[int]net.Conn{1: conn1, 2: conn2} {
		if _, err := fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"live","version":"1"}}}`+"\n", id); err != nil {
			t.Fatalf("initialize session %d: %v", id, err)
		}
	}
	readDaemonResponseID(t, scan1, "1")
	readDaemonResponseID(t, scan2, "2")
	if _, err := fmt.Fprintln(conn1, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"crash","arguments":{}}}`); err != nil {
		t.Fatalf("write crash: %v", err)
	}
	crash := readDaemonResponseID(t, scan1, "3")
	if !strings.Contains(string(crash), "upstream process exited") {
		t.Fatalf("crash response=%s", crash)
	}
	select {
	case <-gen2Init:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement controller did not start")
	}
	if _, err := fmt.Fprintln(conn1, `{"jsonrpc":"2.0","id":4,"method":"ping","params":{}}`); err != nil {
		t.Fatalf("write recovery ping 1: %v", err)
	}
	if _, err := fmt.Fprintln(conn2, `{"jsonrpc":"2.0","id":5,"method":"ping","params":{}}`); err != nil {
		t.Fatalf("write recovery ping 2: %v", err)
	}
	close(releaseGen2)
	resp1 := readDaemonResponseID(t, scan1, "4")
	resp2 := readDaemonResponseID(t, scan2, "5")
	if !strings.Contains(string(resp1), `"generation":2`) || !strings.Contains(string(resp2), `"generation":2`) {
		t.Fatalf("recovery responses=(%s, %s), want generation 2", resp1, resp2)
	}
	if starts.Load() != 2 || d.OwnerCount() != 1 || d.Entry(sid) != entry || entry.Owner.SessionCount() != 2 {
		t.Fatalf("recovery state starts=%d owners=%d same_entry=%v sessions=%d", starts.Load(), d.OwnerCount(), d.Entry(sid) == entry, entry.Owner.SessionCount())
	}
}

func TestPersistentZeroSessionCrashRacingReaperUsesOneReplacementController(t *testing.T) {
	d := testDaemon(t)
	var starts atomic.Int32
	gen1Init := make(chan struct{})
	releaseGen1Init := make(chan struct{})
	crashFirst := make(chan struct{})
	gen2Init := make(chan struct{})
	releaseGen2 := make(chan struct{})
	closeSignal := func(ch chan struct{}) {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	defer closeSignal(releaseGen1Init)
	defer closeSignal(crashFirst)
	defer closeSignal(releaseGen2)
	var gen1Once sync.Once
	var gen2Once sync.Once
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				if generation == 1 {
					gen1Once.Do(func() { close(gen1Init) })
					<-releaseGen1Init
				} else if generation == 2 {
					gen2Once.Do(func() { close(gen2Init) })
					<-releaseGen2
				}
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared","persistent":true}},"serverInfo":{"name":"persistent-recovery","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[]}`); err != nil {
					return err
				}
				if generation == 1 {
					<-crashFirst
					return fmt.Errorf("persistent generation crashed")
				}
			}
		}
		return scanner.Err()
	}
	command := "persistent-zero-session-recovery"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(true))
	path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	entry := d.Entry(sid)
	select {
	case <-gen1Init:
	case <-time.After(2 * time.Second):
		t.Fatal("initial persistent generation did not reach initialize")
	}
	conn, _ := connectSpawnedOwner(t, path, token)
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == 1 }, "persistent owner did not admit session before readiness")
	closeSignal(releaseGen1Init)
	waitForDaemonCondition(t, 2*time.Second, func() bool {
		return entry.Owner.MaterializationState() == owner.MaterializationReady && entry.Owner.SessionCount() == 1
	}, "persistent owner did not become ready")
	_ = conn.Close()
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == 0 }, "persistent owner retained a session")
	closeSignal(crashFirst)
	select {
	case <-gen2Init:
	case <-time.After(3 * time.Second):
		t.Fatal("persistent owner did not replace crashed generation without demand")
	}
	if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 0 {
		t.Fatalf("reaper affected=%d during persistent recovery", affected)
	}
	if d.Entry(sid) != entry || d.OwnerCount() != 1 || starts.Load() != 2 {
		t.Fatalf("persistent recovery before release: same=%v owners=%d starts=%d", d.Entry(sid) == entry, d.OwnerCount(), starts.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if starts.Load() != 2 {
		t.Fatalf("competing persistent replacement generations=%d, want 2 total", starts.Load())
	}
	closeSignal(releaseGen2)
	waitForDaemonCondition(t, 2*time.Second, func() bool { return entry.Owner.MaterializationState() == owner.MaterializationReady }, "persistent replacement did not become ready")
	if d.Entry(sid) != entry || starts.Load() != 2 {
		t.Fatalf("persistent recovery final same=%v starts=%d", d.Entry(sid) == entry, starts.Load())
	}
}

func TestPendingPersistenceConcurrentReaperRetriesWithoutDemandAndDeniesSuspend(t *testing.T) {
	d := testDaemon(t)
	var starts atomic.Int32
	firstTools := make(chan struct{})
	releaseFailure := make(chan struct{})
	var firstToolsOnce sync.Once
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				result := `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"pending-persistent","version":"1"}}`
				if generation == 1 {
					result = `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared","persistent":true}},"serverInfo":{"name":"pending-persistent","version":"1"}}`
				}
				if err := writeDaemonResponse(stdout, req.ID, result); err != nil {
					return err
				}
			case "tools/list":
				if generation == 1 {
					firstToolsOnce.Do(func() { close(firstTools) })
					<-releaseFailure
					return fmt.Errorf("first discovery failed")
				}
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"recovered"}]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeDaemonResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	command := "pending-persistent-reaper"
	d.updateTemplate(command, nil, daemonMaterializationSnapshot(false))
	path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	entry := d.Entry(sid)
	conn, _ := connectSpawnedOwner(t, path, token)
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":61,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write wake: %v", err)
	}
	select {
	case <-firstTools:
	case <-time.After(2 * time.Second):
		t.Fatal("persistent declaration did not reach blocked discovery")
	}
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.PersistentPending() }, "pending persistence was not latched")
	_ = conn.Close()
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == 0 }, "pending-persistent session did not close")
	if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 0 {
		t.Fatalf("reaper affected=%d with pending persistence", affected)
	}
	if d.Entry(sid) != entry {
		t.Fatal("pending-persistent owner was replaced or removed")
	}
	suspend, err := d.HandleCanSuspendForOwner(token, sid)
	if err != nil {
		t.Fatalf("HandleCanSuspendForOwner: %v", err)
	}
	if suspend.Allowed || suspend.Reason != "pending_persistent" {
		t.Fatalf("suspend=%+v, want pending_persistent denial", suspend)
	}
	close(releaseFailure)
	waitForDaemonCondition(t, 3*time.Second, func() bool {
		return starts.Load() == 2 && entry.Owner.MaterializationState() == owner.MaterializationReady
	}, "pending persistence did not retry eagerly without demand")
	if d.Entry(sid) != entry || starts.Load() != 2 {
		t.Fatalf("pending persistence recovery same=%v starts=%d", d.Entry(sid) == entry, starts.Load())
	}
	if !entry.Persistent {
		t.Fatal("successful retry downgraded the latched daemon persistence obligation")
	}
}

func TestRestartRacingSharedToIsolatedCommitCapturesMatchingGeneration(t *testing.T) {
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
	d := testDaemon(t)
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	var publishOnce sync.Once
	d.beforeOwnerCachePublish = func(*owner.Owner) {
		publishOnce.Do(func() { close(publishEntered) })
		<-releasePublish
	}
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"new-coherent","version":"2"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"new-coherent-tool"}]}`); err != nil {
					return err
				}
			case "custom/wake":
				if err := writeDaemonResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	command := "snapshot-cache-commit-race"
	oldSnapshot := daemonMaterializationSnapshot(false)
	d.updateTemplate(command, nil, oldSnapshot)
	path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	entry := d.Entry(sid)
	conn, scanner := connectSpawnedOwner(t, path, token)
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":71,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write wake: %v", err)
	}
	select {
	case <-publishEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("cache/template commit did not reach publish barrier")
	}
	type serializeResult struct {
		err error
	}
	serialized := make(chan serializeResult, 1)
	go func() {
		_, serializeErr := d.SerializeSnapshot()
		serialized <- serializeResult{err: serializeErr}
	}()
	time.Sleep(30 * time.Millisecond)
	close(releasePublish)
	select {
	case result := <-serialized:
		if result.err != nil {
			t.Fatalf("SerializeSnapshot: %v", result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cache commit and SerializeSnapshot deadlocked")
	}
	readDaemonResponseID(t, scanner, "71")
	snapshot, err := DeserializeSnapshot(d.logger)
	if err != nil || snapshot == nil || len(snapshot.Owners) != 1 {
		t.Fatalf("DeserializeSnapshot=(%+v,%v)", snapshot, err)
	}
	captured := snapshot.Owners[0]
	tools, err := base64.StdEncoding.DecodeString(captured.CachedTools)
	if err != nil || !strings.Contains(string(tools), "new-coherent-tool") {
		t.Fatalf("captured tools=%s err=%v, want wholly new cache", tools, err)
	}
	if captured.Classification != "isolated" || captured.OwnerGeneration != entry.OwnerGeneration {
		t.Fatalf("captured metadata classification=%q owner_generation=%q, want isolated/%q", captured.Classification, captured.OwnerGeneration, entry.OwnerGeneration)
	}
	template, ok := d.getTemplate(command, nil)
	if !ok || template.CachedInit != captured.CachedInit || template.CachedTools != captured.CachedTools || template.Classification != captured.Classification {
		t.Fatalf("snapshot/template mixed: snapshot=%+v template=%+v ok=%v", captured, template, ok)
	}
}

func TestTwoTemplateRevisionMismatchesThenOneColdBypassWithoutStaleFrames(t *testing.T) {
	d := testDaemon(t)
	var starts atomic.Int32
	initSeen := make(chan struct{})
	releaseInit := make(chan struct{})
	var initOnce sync.Once
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
			switch req.Method {
			case "initialize":
				initOnce.Do(func() { close(initSeen) })
				<-releaseInit
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"fresh-cold","version":"4"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"fresh-cold-tool"}]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	command := "double-template-revision-mismatch"
	cwd := t.TempDir()
	env := map[string]string{"SERVICE_CONFIG_PATH": "/cfg/revision"}
	makeSnapshot := func(label string) mcpsnapshot.OwnerSnapshot {
		snapshot := daemonMaterializationSnapshot(false)
		snapshot.Cwd = cwd
		snapshot.Env = mergeEnv(env)
		snapshot.CachedInit = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"%s-init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"%s","version":"1"}}}`, label, label)))
		snapshot.CachedTools = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"%s-tools","result":{"tools":[{"name":"%s-tool"}]}}`, label, label)))
		return snapshot
	}
	d.updateTemplate(command, nil, makeSnapshot("stale-one"))
	var hookCalls atomic.Int32
	d.beforeTemplatePromotion = func() {
		switch hookCalls.Add(1) {
		case 1:
			d.updateTemplate(command, nil, makeSnapshot("stale-two"))
		case 2:
			d.updateTemplate(command, nil, makeSnapshot("stale-three"))
		default:
			t.Error("template promotion hook called after bounded bypass")
		}
	}
	type spawnResult struct {
		path  string
		sid   string
		token string
		err   error
	}
	spawned := make(chan spawnResult, 1)
	go func() {
		path, sid, token, spawnErr := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwd, Env: env})
		spawned <- spawnResult{path: path, sid: sid, token: token, err: spawnErr}
	}()
	var result spawnResult
	select {
	case result = <-spawned:
		if result.err != nil {
			t.Fatalf("Spawn: %v", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second template mismatch livelocked instead of taking cold bypass")
	}
	select {
	case <-initSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("cold bypass did not start one fresh generation")
	}
	entry := d.Entry(result.sid)
	preCommit := entry.Owner.ExportSnapshot()
	if preCommit.CachedInit != "" || preCommit.CachedTools != "" {
		t.Fatalf("cold bypass admitted stale cached frames: %+v", preCommit)
	}
	if hookCalls.Load() != 2 || starts.Load() != 1 || d.OwnerCount() != 1 {
		t.Fatalf("bounded bypass hook_calls=%d starts=%d owners=%d", hookCalls.Load(), starts.Load(), d.OwnerCount())
	}
	conn, scanner := connectSpawnedOwner(t, result.path, result.token)
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":81,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fresh","version":"1"}}}`); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	close(releaseInit)
	response := readDaemonResponseID(t, scanner, "81")
	if !strings.Contains(string(response), "fresh-cold") || strings.Contains(string(response), "stale-") {
		t.Fatalf("cold bypass response=%s", response)
	}
}

func TestIncompatibleTemplateContextGetsNoCachedFramesAndOneColdProcess(t *testing.T) {
	d := testDaemon(t)
	var starts atomic.Int32
	d.handlerFunc = func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
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
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"fresh-B","version":"1"}}`); err != nil {
					return err
				}
			case "tools/list":
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"fresh-B-tool"}]}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
	command := "scoped-template-context"
	cwdA, cwdB := t.TempDir(), t.TempDir()
	envA := map[string]string{"SERVICE_CONFIG_PATH": "/cfg/A"}
	envB := map[string]string{"SERVICE_CONFIG_PATH": "/cfg/B"}
	snapshotA := daemonMaterializationSnapshot(false)
	snapshotA.Classification = "isolated"
	snapshotA.Cwd = cwdA
	snapshotA.Env = mergeEnv(envA)
	snapshotA.CachedInit = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":"A-init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"cached-A","version":"1"}}}`))
	snapshotA.CachedTools = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","id":"A-tools","result":{"tools":[{"name":"cached-A-tool"}]}}`))
	d.updateTemplate(command, nil, snapshotA)

	pathA, sidA, tokenA, err := d.Spawn(control.Request{Command: command, Mode: "isolated", Cwd: cwdA, Env: envA})
	if err != nil {
		t.Fatalf("Spawn compatible A: %v", err)
	}
	connA, scanA := connectSpawnedOwner(t, pathA, tokenA)
	defer connA.Close()
	if _, err := fmt.Fprintln(connA, `{"jsonrpc":"2.0","id":91,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"A","version":"1"}}}`); err != nil {
		t.Fatalf("write A initialize: %v", err)
	}
	if got := readDaemonResponseID(t, scanA, "91"); !strings.Contains(string(got), "cached-A") {
		t.Fatalf("compatible A response=%s", got)
	}
	if starts.Load() != 0 || d.Entry(sidA).Owner.MaterializationState() != owner.MaterializationCacheOnly {
		t.Fatalf("compatible A starts=%d state=%s, want handshake-only cache", starts.Load(), d.Entry(sidA).Owner.MaterializationState())
	}

	pathB, sidB, tokenB, err := d.Spawn(control.Request{Command: command, Mode: "isolated", Cwd: cwdB, Env: envB})
	if err != nil {
		t.Fatalf("Spawn incompatible B: %v", err)
	}
	if sidB == sidA {
		t.Fatalf("incompatible B reused A sid %s", sidA)
	}
	connB, scanB := connectSpawnedOwner(t, pathB, tokenB)
	defer connB.Close()
	if _, err := fmt.Fprintln(connB, `{"jsonrpc":"2.0","id":92,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"B","version":"1"}}}`); err != nil {
		t.Fatalf("write B initialize: %v", err)
	}
	responseB := readDaemonResponseID(t, scanB, "92")
	if !strings.Contains(string(responseB), "fresh-B") || strings.Contains(string(responseB), "cached-A") || strings.Contains(string(responseB), "/cfg/A") {
		t.Fatalf("incompatible B response leaked A state: %s", responseB)
	}
	waitForDaemonCondition(t, 2*time.Second, func() bool { return d.Entry(sidB).Owner.MaterializationState() == owner.MaterializationReady }, "B cold generation did not become ready")
	if starts.Load() != 1 {
		t.Fatalf("incompatible B process starts=%d, want exactly 1", starts.Load())
	}
}

func TestProcessBackedIsolatedWinnerUsesInitiatorContextAndEvictsOthers(t *testing.T) {
	const roleEnv = "MCPMUX_TEST_ISOLATED_WINNER_ROLE"
	const observationEnv = "MCPMUX_TEST_ISOLATED_WINNER_OBSERVATION"
	const releaseEnv = "MCPMUX_TEST_ISOLATED_WINNER_RELEASE"
	const requestCountEnv = "MCPMUX_TEST_ISOLATED_WINNER_REQUESTS"
	const winnerEnv = "WINNER_ONLY_MARKER"
	const evicteeEnv = "EVICTEE_ONLY_MARKER"
	const credentialValue = "daemon-credential-sentinel"
	if os.Getenv(roleEnv) == "upstream" {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		observation := map[string]string{
			"cwd":        cwd,
			"winner":     os.Getenv(winnerEnv),
			"evictee":    os.Getenv(evicteeEnv),
			"credential": os.Getenv("GITHUB_TOKEN"),
		}
		data, err := json.Marshal(observation)
		if err != nil {
			t.Fatalf("marshal observation: %v", err)
		}
		if err := os.WriteFile(os.Getenv(observationEnv), data, 0o600); err != nil {
			t.Fatalf("write observation: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv(releaseEnv)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for isolated winner release")
			}
			time.Sleep(10 * time.Millisecond)
		}
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				t.Fatalf("decode isolated winner request: %v", err)
			}
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(os.Stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"isolated"}},"serverInfo":{"name":"isolated-winner","version":"1"}}`); err != nil {
					t.Fatalf("write initialize: %v", err)
				}
			case "tools/list":
				if err := writeDaemonResponse(os.Stdout, req.ID, `{"tools":[{"name":"winner-tool"}]}`); err != nil {
					t.Fatalf("write tools: %v", err)
				}
			case "custom/wake":
				countFile, err := os.OpenFile(os.Getenv(requestCountEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatalf("open request count: %v", err)
				}
				_, _ = fmt.Fprintln(countFile, string(req.ID))
				_ = countFile.Close()
				result, _ := json.Marshal(observation)
				if err := writeDaemonResponse(os.Stdout, req.ID, string(result)); err != nil {
					t.Fatalf("write wake: %v", err)
				}
			}
		}
		return
	}

	t.Setenv("GITHUB_TOKEN", credentialValue)
	tmp := t.TempDir()
	observationPath := filepath.Join(tmp, "observation.json")
	releasePath := filepath.Join(tmp, "release")
	requestCountPath := filepath.Join(tmp, "requests")
	command := os.Args[0]
	args := []string{"-test.run=^TestProcessBackedIsolatedWinnerUsesInitiatorContextAndEvictsOthers$"}
	cwdA, cwdB := t.TempDir(), t.TempDir()
	d := testDaemon(t)
	common := map[string]string{
		roleEnv:         "upstream",
		observationEnv:  observationPath,
		releaseEnv:      releasePath,
		requestCountEnv: requestCountPath,
	}
	envA := cloneSnapshotStringMap(common)
	envA[evicteeEnv] = "evictee-only"
	envB := cloneSnapshotStringMap(common)
	envB[winnerEnv] = "winner-only"
	snapshot := daemonMaterializationSnapshot(false)
	snapshot.Classification = "shared"
	snapshot.Cwd = cwdA
	snapshot.Env = mergeEnv(envA)
	d.updateTemplate(command, args, snapshot)
	path, sid, tokenA, err := d.Spawn(control.Request{Command: command, Args: args, Mode: "global", Cwd: cwdA, Env: envA})
	if err != nil {
		t.Fatalf("Spawn A: %v", err)
	}
	_, sidB, tokenB, err := d.Spawn(control.Request{Command: command, Args: args, Mode: "global", Cwd: cwdB, Env: envB})
	if err != nil {
		t.Fatalf("Spawn B: %v", err)
	}
	if sidB != sid {
		t.Fatalf("compatible B sid=%s, want shared owner %s", sidB, sid)
	}
	entry := d.Entry(sid)
	connA, scanA := connectSpawnedOwner(t, path, tokenA)
	defer connA.Close()
	connB, scanB := connectSpawnedOwner(t, path, tokenB)
	defer connB.Close()
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == 2 }, "both sessions did not attach")
	if _, err := fmt.Fprintln(connB, `{"jsonrpc":"2.0","id":201,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write winner demand: %v", err)
	}
	waitForDaemonCondition(t, 3*time.Second, func() bool {
		_, statErr := os.Stat(observationPath)
		return statErr == nil
	}, "process-backed winner did not start")
	if _, err := fmt.Fprintln(connA, `{"jsonrpc":"2.0","id":202,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write evictee demand: %v", err)
	}
	data, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatalf("read observation: %v", err)
	}
	var observation map[string]string
	if err := json.Unmarshal(data, &observation); err != nil {
		t.Fatalf("decode observation: %v", err)
	}
	if serverid.CanonicalizePath(observation["cwd"]) != serverid.CanonicalizePath(cwdB) {
		t.Fatalf("child cwd=%q, want winner cwd %q", observation["cwd"], cwdB)
	}
	if observation["winner"] != "winner-only" || observation["evictee"] != "" || observation["credential"] != credentialValue {
		t.Fatalf("child env observation=%v", observation)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release child: %v", err)
	}
	winnerResponse := readDaemonResponseID(t, scanB, "201")
	if !strings.Contains(string(winnerResponse), `"winner":"winner-only"`) || strings.Contains(string(winnerResponse), "evictee-only") {
		t.Fatalf("winner response=%s", winnerResponse)
	}
	waitForDaemonCondition(t, 2*time.Second, func() bool { return entry.Owner.IsClassifiedIsolated() && entry.Owner.SessionCount() == 1 }, "isolated commit did not retain exactly one session")
	if err := connA.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set evictee deadline: %v", err)
	}
	for scanA.Scan() {
		if strings.Contains(scanA.Text(), `"id":202`) {
			t.Fatalf("evicted waiter received a response: %s", scanA.Text())
		}
	}
	requestData, _ := os.ReadFile(requestCountPath)
	if got := len(strings.Fields(string(requestData))); got != 1 {
		t.Fatalf("forwarded winner requests=%d, want exactly 1; data=%q", got, requestData)
	}
}

func TestRestartCaptureZeroSessionMaterializationBarrier(t *testing.T) {
	for _, readyBeforeCapture := range []bool{true, false} {
		name := "ready-commit"
		if !readyBeforeCapture {
			name = "bounded-fallback"
		}
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(SnapshotPath())
			t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
			var starts, active, maxActive, exits atomic.Int32
			toolsSeen := make(chan struct{})
			releaseTools := make(chan struct{})
			var releaseToolsOnce sync.Once
			t.Cleanup(func() { releaseToolsOnce.Do(func() { close(releaseTools) }) })
			var toolsOnce sync.Once
			handler := func(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
				starts.Add(1)
				current := active.Add(1)
				for {
					max := maxActive.Load()
					if current <= max || maxActive.CompareAndSwap(max, current) {
						break
					}
				}
				defer func() {
					active.Add(-1)
					exits.Add(1)
				}()
				generation := starts.Load()
				scanner := bufio.NewScanner(stdin)
				for scanner.Scan() {
					var req struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
						return err
					}
					if len(req.ID) == 0 {
						continue
					}
					switch req.Method {
					case "initialize":
						if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared"}},"serverInfo":{"name":"restart-capture","version":"1"}}`); err != nil {
							return err
						}
					case "tools/list":
						if generation == 1 {
							toolsOnce.Do(func() { close(toolsSeen) })
							select {
							case <-releaseTools:
							case <-ctx.Done():
								return ctx.Err()
							}
						}
						if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"restart-fresh"}]}`); err != nil {
							return err
						}
					default:
						if err := writeDaemonResponse(stdout, req.ID, `{"ok":true}`); err != nil {
							return err
						}
					}
				}
				return scanner.Err()
			}

			d, err := New(Config{
				Name:                    "restart-capture-zero-session",
				ControlPath:             shortSocketPath(t, "restart-capture.ctl"),
				SkipSnapshot:            true,
				HandlerFunc:             handler,
				OwnerIdleTimeout:        time.Minute,
				ZeroSessionCleanupDelay: 20 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("New daemon: %v", err)
			}
			t.Cleanup(d.Shutdown)
			command := "restart-capture-upstream"
			template := daemonMaterializationSnapshot(false)
			template.Cwd = "/cached/restart"
			template.Env = map[string]string{"CACHED_SENTINEL": "old"}
			d.updateTemplate(command, nil, template)
			path, sid, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: "/winner/restart", Env: map[string]string{"WINNER_SENTINEL": "winner"}})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			entry := d.Entry(sid)
			conn, _ := connectSpawnedOwner(t, path, token)
			if _, err := fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":301,"method":"custom/wake","params":{}}`); err != nil {
				t.Fatalf("write demand: %v", err)
			}
			select {
			case <-toolsSeen:
			case <-time.After(2 * time.Second):
				t.Fatal("materialization did not reach tools barrier")
			}

			type snapshotResult struct {
				path  string
				lease *snapshotRestartLease
				err   error
			}
			resultCh := make(chan snapshotResult, 1)
			go func() {
				path, lease, serializeErr := d.serializeSnapshotPinned()
				resultCh <- snapshotResult{path: path, lease: lease, err: serializeErr}
			}()
			waitForDaemonCondition(t, time.Second, func() bool {
				return entry.Owner.Status()["restart_pin_count"] == int64(1)
			}, "restart pin was not acquired")
			_ = conn.Close()
			waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == 0 }, "session did not close")
			if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 0 || d.Entry(sid) != entry {
				t.Fatalf("reaper crossed live restart pin: affected=%d same=%v", affected, d.Entry(sid) == entry)
			}
			if readyBeforeCapture {
				releaseToolsOnce.Do(func() { close(releaseTools) })
			}
			var result snapshotResult
			select {
			case result = <-resultCh:
			case <-time.After(8 * time.Second):
				t.Fatal("snapshot capture did not settle")
			}
			if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 0 || d.Entry(sid) != entry {
				t.Fatalf("reaper removed leased owner: affected=%d same=%v", affected, d.Entry(sid) == entry)
			}
			if maxActive.Load() != 1 || starts.Load() != 1 {
				t.Fatalf("pre-capture generations starts=%d max=%d", starts.Load(), maxActive.Load())
			}
			if !readyBeforeCapture && entry.Owner.MaterializationState() != owner.MaterializationCacheOnly {
				t.Fatalf("bounded capture state=%s, want CACHE_ONLY", entry.Owner.MaterializationState())
			}
			if !readyBeforeCapture {
				releaseToolsOnce.Do(func() { close(releaseTools) })
				waitForDaemonCondition(t, time.Second, func() bool { return exits.Load() == 1 }, "retired handler did not drain")
			}
			if result.err != nil || result.path == "" || result.lease == nil {
				t.Fatalf("SerializeSnapshotPinned = (%q, %v, %v)", result.path, result.lease, result.err)
			}
			snapshot, err := DeserializeSnapshot(d.logger)
			if err != nil || snapshot == nil || len(snapshot.Owners) != 1 {
				t.Fatalf("captured snapshot=%#v err=%v", snapshot, err)
			}
			captured := snapshot.Owners[0]
			if captured.Cwd != "/winner/restart" || captured.Env["WINNER_SENTINEL"] != "winner" {
				t.Fatalf("captured launch context=%+v", captured)
			}
			tools, err := base64.StdEncoding.DecodeString(captured.CachedTools)
			if err != nil {
				t.Fatalf("decode tools: %v", err)
			}
			if readyBeforeCapture {
				if !strings.Contains(string(tools), "restart-fresh") || entry.Owner.MaterializationState() != owner.MaterializationReady {
					t.Fatalf("ready capture state=%s tools=%s", entry.Owner.MaterializationState(), tools)
				}
				result.lease.Release()
				time.Sleep(50 * time.Millisecond)
				if starts.Load() != 1 || maxActive.Load() != 1 {
					t.Fatalf("ready capture spawned replacement: starts=%d max=%d", starts.Load(), maxActive.Load())
				}
				return
			}
			if captured.CachedTools != template.CachedTools {
				t.Fatalf("bounded fallback published partial tools: got=%q want=%q", captured.CachedTools, template.CachedTools)
			}
			entry.Owner.Shutdown()
			result.lease.Release()
			successor, err := owner.NewOwnerFromSnapshot(owner.OwnerConfig{
				HandlerFunc:           handler,
				IPCPath:               shortSocketPath(t, "restart-capture-successor.sock"),
				MaterializationPolicy: owner.MaterializationEager,
			}, captured)
			if err != nil {
				t.Fatalf("hydrate successor: %v", err)
			}
			t.Cleanup(successor.Shutdown)
			successor.SpawnUpstreamBackground()
			waitForDaemonCondition(t, 2*time.Second, func() bool {
				return starts.Load() == 2 && successor.MaterializationState() == owner.MaterializationReady
			}, "successor eager restore did not start after predecessor finalization")
			if maxActive.Load() != 1 || exits.Load() < 1 {
				t.Fatalf("successor overlapped predecessor: starts=%d exits=%d max=%d", starts.Load(), exits.Load(), maxActive.Load())
			}
		})
	}
}

func TestPersistentWinningContextSurvivesRestartHydration(t *testing.T) {
	_ = os.Remove(SnapshotPath())
	t.Cleanup(func() { _ = os.Remove(SnapshotPath()) })
	t.Setenv("GITHUB_TOKEN", "daemon-credential")
	releaseFirstTools := make(chan struct{})
	var releaseFirstToolsOnce sync.Once
	t.Cleanup(func() { releaseFirstToolsOnce.Do(func() { close(releaseFirstTools) }) })
	var starts, active, maxActive, exits atomic.Int32
	initSeen := make(chan struct{})
	toolsSeen := make(chan struct{})
	var initOnce, toolsOnce sync.Once
	handler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		generation := starts.Add(1)
		current := active.Add(1)
		for {
			max := maxActive.Load()
			if current <= max || maxActive.CompareAndSwap(max, current) {
				break
			}
		}
		defer func() {
			active.Add(-1)
			exits.Add(1)
		}()
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return err
			}
			if len(req.ID) == 0 {
				continue
			}
			switch req.Method {
			case "initialize":
				if err := writeDaemonResponse(stdout, req.ID, `{"protocolVersion":"2025-11-25","capabilities":{"x-mux":{"sharing":"shared","persistent":true}},"serverInfo":{"name":"persistent-restart","version":"1"}}`); err != nil {
					return err
				}
				if generation == 1 {
					initOnce.Do(func() { close(initSeen) })
				}
			case "tools/list":
				if generation == 1 {
					toolsOnce.Do(func() { close(toolsSeen) })
					<-releaseFirstTools
					return fmt.Errorf("retired first persistent generation")
				}
				if err := writeDaemonResponse(stdout, req.ID, `{"tools":[{"name":"persistent-ready"}]}`); err != nil {
					return err
				}
			default:
				if err := writeDaemonResponse(stdout, req.ID, `{"ok":true}`); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}

	d, err := New(Config{
		Name:                    "persistent-winning-context",
		ControlPath:             shortSocketPath(t, "persistent-winning.ctl"),
		SkipSnapshot:            true,
		HandlerFunc:             handler,
		OwnerIdleTimeout:        time.Minute,
		ZeroSessionCleanupDelay: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New predecessor: %v", err)
	}
	t.Cleanup(d.Shutdown)
	command := "persistent-winning-upstream"
	template := daemonMaterializationSnapshot(false)
	d.updateTemplate(command, nil, template)
	path, sid, tokenA, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: "/project/A", Env: map[string]string{"SESSION_SENTINEL": "A"}})
	if err != nil {
		t.Fatalf("Spawn A: %v", err)
	}
	_, sidB, tokenB, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: "/project/B", Env: map[string]string{"SESSION_SENTINEL": "B"}})
	if err != nil || sidB != sid {
		t.Fatalf("Spawn B = (%s, %v), want shared owner %s", sidB, err, sid)
	}
	entry := d.Entry(sid)
	connA, _ := connectSpawnedOwner(t, path, tokenA)
	connB, _ := connectSpawnedOwner(t, path, tokenB)
	if _, err := fmt.Fprintln(connB, `{"jsonrpc":"2.0","id":401,"method":"custom/wake","params":{}}`); err != nil {
		t.Fatalf("write B demand: %v", err)
	}
	select {
	case <-initSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("persistent initialize did not complete")
	}
	select {
	case <-toolsSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("persistent generation did not block at tools")
	}
	waitForDaemonCondition(t, time.Second, entry.Owner.PersistentPending, "pending persistence was not latched")

	type snapshotResult struct {
		lease *snapshotRestartLease
		err   error
	}
	resultCh := make(chan snapshotResult, 1)
	go func() {
		_, lease, serializeErr := d.serializeSnapshotPinned()
		resultCh <- snapshotResult{lease: lease, err: serializeErr}
	}()
	waitForDaemonCondition(t, time.Second, func() bool {
		return entry.Owner.Status()["restart_pin_count"] == int64(1)
	}, "persistent restart pin was not acquired")
	_ = connA.Close()
	_ = connB.Close()
	waitForDaemonCondition(t, time.Second, func() bool { return entry.Owner.SessionCount() == 0 }, "persistent sessions did not close")
	if affected := (&Reaper{daemon: d, logger: d.logger}).sweep(); affected != 0 || d.Entry(sid) != entry {
		t.Fatalf("reaper crossed pending-persistent pin: affected=%d same=%v", affected, d.Entry(sid) == entry)
	}
	var result snapshotResult
	select {
	case result = <-resultCh:
	case <-time.After(8 * time.Second):
		t.Fatal("persistent restart capture did not settle")
	}
	if starts.Load() != 1 || maxActive.Load() != 1 {
		t.Fatalf("predecessor authority starts=%d max=%d", starts.Load(), maxActive.Load())
	}
	releaseFirstToolsOnce.Do(func() { close(releaseFirstTools) })
	waitForDaemonCondition(t, time.Second, func() bool { return exits.Load() == 1 }, "retired persistent handler did not drain")
	if result.err != nil || result.lease == nil {
		t.Fatalf("SerializeSnapshotPinned = (%v, %v)", result.lease, result.err)
	}
	snapshot, err := DeserializeSnapshot(d.logger)
	if err != nil || snapshot == nil || len(snapshot.Owners) != 1 {
		t.Fatalf("captured snapshot=%#v err=%v", snapshot, err)
	}
	captured := snapshot.Owners[0]
	if captured.Cwd != "/project/B" || captured.Env["SESSION_SENTINEL"] != "B" || captured.Env["GITHUB_TOKEN"] != "daemon-credential" || !captured.Persistent {
		t.Fatalf("persistent winning context not captured: %+v", captured)
	}
	entry.Owner.Shutdown()
	result.lease.Release()

	successorDaemon, err := New(Config{
		Name:         "persistent-winning-successor",
		ControlPath:  shortSocketPath(t, "persistent-winning-successor.ctl"),
		SkipSnapshot: true,
		HandlerFunc:  handler,
		IdleTimeout:  time.Minute,
	})
	if err != nil {
		t.Fatalf("New successor: %v", err)
	}
	t.Cleanup(successorDaemon.Shutdown)
	plan := successorDaemon.makeSnapshotRestorePlan(captured)
	successorEntry, reattached, err := successorDaemon.restoreSnapshotPlan(plan, nil, "snapshot_fallback", true, true)
	if err != nil || reattached {
		t.Fatalf("restore successor = (%v, %v)", reattached, err)
	}
	waitForDaemonCondition(t, 2*time.Second, func() bool {
		return starts.Load() == 2 && successorEntry.Owner.MaterializationState() == owner.MaterializationReady
	}, "persistent successor did not reach one ready generation")
	if maxActive.Load() != 1 || successorEntry.Owner.ExportSnapshot().Cwd != "/project/B" || successorEntry.Owner.ExportSnapshot().Env["SESSION_SENTINEL"] != "B" {
		t.Fatalf("successor context/authority mismatch: starts=%d max=%d snapshot=%+v", starts.Load(), maxActive.Load(), successorEntry.Owner.ExportSnapshot())
	}
	if !successorEntry.Persistent {
		t.Fatal("successor lost committed persistence")
	}
	if affected := (&Reaper{daemon: successorDaemon, logger: successorDaemon.logger}).sweep(); affected != 0 || successorDaemon.Entry(sid) != successorEntry {
		t.Fatalf("persistent successor was evicted: affected=%d same=%v", affected, successorDaemon.Entry(sid) == successorEntry)
	}
	time.Sleep(50 * time.Millisecond)
	if starts.Load() != 2 || successorEntry.Owner.Status()["upstream_live"] != true {
		t.Fatalf("persistent successor duplicated or suspended: starts=%d status=%+v", starts.Load(), successorEntry.Owner.Status())
	}
}
