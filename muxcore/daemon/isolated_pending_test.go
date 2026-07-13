package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

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
