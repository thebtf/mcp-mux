package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/control"
)

func isolatedHandler(_ context.Context, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	encoder := json.NewEncoder(stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || len(request.ID) == 0 {
			continue
		}
		result := any(map[string]any{})
		if request.Method == "initialize" {
			result = map[string]any{"capabilities": map[string]any{"tools": map[string]any{}}}
		} else if request.Method == "tools/list" {
			time.Sleep(50 * time.Millisecond)
			result = map[string]any{"tools": []map[string]any{{"name": "activate_project"}}}
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func blockedToolsHandler(gate <-chan struct{}) func(context.Context, io.Reader, io.Writer) error {
	return func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		scanner := bufio.NewScanner(stdin)
		encoder := json.NewEncoder(stdout)
		for scanner.Scan() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || len(request.ID) == 0 {
				continue
			}
			if request.Method == "tools/list" {
				<-gate
			}
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{}}); err != nil {
				return err
			}
		}
		return scanner.Err()
	}
}

func lifecycleRequest(t *testing.T, conn io.ReadWriter, id int, method string) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":%d,"method":%q}`+"\n", id, method); err != nil {
		t.Fatalf("write request %d: %v", id, err)
	}
	var response struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatalf("read response %d: %v", id, err)
	}
	if response.ID != id || len(response.Result) == 0 || len(response.Error) != 0 {
		t.Fatalf("response = id=%d result=%s error=%s, want result for id=%d", response.ID, response.Result, response.Error, id)
	}
}

func TestSpawn_ExplicitIsolatedFreshStormGetsPrivateOwners(t *testing.T) {
	const count = 8
	var calls struct {
		sync.Mutex
		count int
	}
	countingHandler := func(_ context.Context, stdin io.Reader, stdout io.Writer) error {
		scanner := bufio.NewScanner(stdin)
		encoder := json.NewEncoder(stdout)
		for scanner.Scan() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || len(request.ID) == 0 {
				continue
			}
			if request.Method == "private/request" {
				calls.Lock()
				calls.count++
				calls.Unlock()
			}
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{}}); err != nil {
				return err
			}
		}
		return scanner.Err()
	}
	d, err := New(Config{
		Name:         "explicit-isolated-fresh",
		ControlPath:  shortSocketPath(t, "explicit-isolated-fresh.ctl.sock"),
		HandlerFunc:  countingHandler,
		SkipSnapshot: true,
		Logger:       testLogger(t),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	paths := make([]string, count)
	sids := make([]string, count)
	tokens := make([]string, count)
	errs := make([]error, count)
	cwd := t.TempDir()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			paths[i], sids[i], tokens[i], errs[i] = d.Spawn(control.Request{
				Cmd:     "spawn",
				Command: "explicit-isolated-handler",
				Cwd:     cwd,
				Mode:    "isolated",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	seenSIDs := make(map[string]struct{}, count)
	seenPaths := make(map[string]struct{}, count)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Spawn %d error: %v", i, err)
		}
		if _, ok := seenSIDs[sids[i]]; ok {
			t.Fatalf("Spawn %d reused server ID %q before any dial", i, sids[i])
		}
		if _, ok := seenPaths[paths[i]]; ok {
			t.Fatalf("Spawn %d reused IPC path %q before any dial", i, paths[i])
		}
		seenSIDs[sids[i]] = struct{}{}
		seenPaths[paths[i]] = struct{}{}
	}
	if got := d.OwnerCount(); got != count {
		t.Fatalf("OwnerCount before any dial = %d, want %d", got, count)
	}

	connections := make([]io.ReadWriteCloser, count)
	for i := range count {
		conn := dialLifecycleSession(t, paths[i], tokens[i])
		connections[i] = conn
		lifecycleRequest(t, conn, i+1, "private/request")
	}
	connections[0].Close()

	refreshed, err := d.HandleRefreshSessionToken(tokens[0])
	if err != nil {
		t.Fatalf("HandleRefreshSessionToken() error: %v", err)
	}
	entry, ownerKey := d.lookupReconnectOwner(refreshed)
	if entry == nil || ownerKey != sids[0] {
		t.Fatalf("refresh token owner = %q entry=%v, want original owner %q", ownerKey, entry != nil, sids[0])
	}
	if got := d.OwnerCount(); got != count {
		t.Fatalf("OwnerCount after refresh = %d, want %d", got, count)
	}
	refreshedConn := dialLifecycleSession(t, paths[0], refreshed)
	lifecycleRequest(t, refreshedConn, count+1, "private/request")
	refreshedConn.Close()
	for i := 1; i < count; i++ {
		connections[i].Close()
	}
	calls.Lock()
	defer calls.Unlock()
	if calls.count != count+1 {
		t.Fatalf("private requests executed %d times, want exactly %d", calls.count, count+1)
	}
}

func TestSpawn_GlobalAdmissionWaitsForSameCwdClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       classify.SharingMode
		wantShared bool
	}{
		{name: "shared", mode: classify.ModeShared, wantShared: true},
		{name: "isolated", mode: classify.ModeIsolated, wantShared: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := make(chan struct{})
			defer close(gate)
			name := fmt.Sprintf("same-cwd-admission-%s-%d", tc.name, time.Now().UnixNano())
			d, err := New(Config{
				Name:                   name,
				ControlPath:            shortSocketPath(t, "same-cwd-admission-"+tc.name+".ctl.sock"),
				HandlerFunc:            blockedToolsHandler(gate),
				AdmissionBufferTimeout: time.Second,
				SkipSnapshot:           true,
				Logger:                 testLogger(t),
			})
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			t.Cleanup(func() { d.Shutdown() })

			cwd := t.TempDir()
			req := control.Request{Cmd: "spawn", Command: "same-cwd-handler", Cwd: cwd}
			path1, sid1, token1, err := d.Spawn(req)
			if err != nil {
				t.Fatalf("first Spawn() error: %v", err)
			}

			type spawnResult struct {
				path, sid, token string
				err              error
			}
			secondResult := make(chan spawnResult, 1)
			go func() {
				path, sid, token, err := d.Spawn(req)
				secondResult <- spawnResult{path: path, sid: sid, token: token, err: err}
			}()
			select {
			case result := <-secondResult:
				t.Fatalf("second Spawn returned before classification: %+v", result)
			case <-time.After(50 * time.Millisecond):
			}

			d.mu.RLock()
			firstEntry := d.owners[sid1]
			d.mu.RUnlock()
			if firstEntry == nil || firstEntry.Owner == nil {
				t.Fatal("first owner disappeared before classification")
			}
			firstEntry.Owner.MarkClassifiedAs(tc.mode)
			result := <-secondResult
			if result.err != nil {
				t.Fatalf("second Spawn() error: %v", result.err)
			}
			if !firstEntry.Owner.SessionMgr().IsPreRegistered(token1) {
				t.Fatal("first fresh admission token was evicted while the second spawn waited")
			}
			if tc.wantShared {
				if result.sid != sid1 || result.path != path1 {
					t.Fatalf("shared classification returned sid/path %q/%q, want %q/%q", result.sid, result.path, sid1, path1)
				}
				return
			}
			if result.sid == sid1 || result.path == path1 {
				t.Fatalf("isolated classification reused sid/path %q/%q", result.sid, result.path)
			}
			if got := d.OwnerCount(); got != 2 {
				t.Fatalf("OwnerCount after isolated fork = %d, want 2", got)
			}
		})
	}
}

func TestSpawn_IsolatedStormLeavesNoPendingAndReaps(t *testing.T) {
	shortIdle := 20 * time.Millisecond
	d, err := New(Config{
		Name:                "isolated-pending-test",
		ControlPath:         shortSocketPath(t, "isolated-pending.ctl.sock"),
		HandlerFunc:         isolatedHandler,
		OwnerIdleTimeout:    time.Hour,
		IsolatedIdleTimeout: &shortIdle,
		SkipSnapshot:        true,
		Logger:              testLogger(t),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { d.Shutdown() })

	basePath, baseSID, baseToken, err := d.Spawn(control.Request{Cmd: "spawn", Command: "isolated-handler"})
	if err != nil {
		t.Fatalf("base Spawn error: %v", err)
	}
	baseConn := dialLifecycleSession(t, basePath, baseToken)
	defer baseConn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		d.mu.RLock()
		entry := d.owners[baseSID]
		classified := entry != nil && entry.Owner.IsClassifiedIsolated()
		accepting := entry != nil && entry.Owner.IsAccepting()
		d.mu.RUnlock()
		_, templateReady := d.getTemplate("isolated-handler", nil)
		if classified && !accepting && templateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("base owner did not classify isolated and populate its template")
		}
		time.Sleep(10 * time.Millisecond)
	}

	const count = 8
	paths := make([]string, count)
	sids := make([]string, count)
	tokens := make([]string, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			paths[i], sids[i], tokens[i], errs[i] = d.Spawn(control.Request{Cmd: "spawn", Command: "isolated-handler"})
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[string]struct{}, count)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Spawn %d error: %v", i, err)
		}
		seen[sids[i]] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("distinct owner count = %d, want %d", len(seen), count)
	}

	connections := make([]io.Closer, 0, count)
	for i := range count {
		connections = append(connections, dialLifecycleSession(t, paths[i], tokens[i]))
	}
	for _, conn := range connections {
		conn.Close()
	}
	baseConn.Close()

	deadline = time.Now().Add(3 * time.Second)
	for {
		allClosed := true
		d.mu.RLock()
		for _, entry := range d.owners {
			if entry.Owner.IsAccepting() || entry.Owner.SessionCount() != 0 {
				allClosed = false
				break
			}
		}
		d.mu.RUnlock()
		if allClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated owners did not reach closed zero-session state")
		}
		time.Sleep(10 * time.Millisecond)
	}

	d.mu.RLock()
	for sid, entry := range d.owners {
		if got := entry.Owner.SessionMgr().PendingCount(); got != 0 {
			d.mu.RUnlock()
			t.Fatalf("owner %s PendingCount = %d, want 0", sid, got)
		}
	}
	d.mu.RUnlock()

	time.Sleep(shortIdle + 20*time.Millisecond)
	(&Reaper{daemon: d, logger: d.logger}).sweep()
	if got := d.OwnerCount(); got != 0 {
		t.Fatalf("OwnerCount after isolated idle reap = %d, want 0", got)
	}
}
