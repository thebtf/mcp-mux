package session

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterRemoveSession(t *testing.T) {
	sm := NewManager()

	s := &Session{ID: 1}
	sm.RegisterSession(s, "/project/a")

	if got := sm.SessionCount(); got != 1 {
		t.Fatalf("SessionCount after register: got %d, want 1", got)
	}

	ctx := sm.GetSession(1)
	if ctx == nil {
		t.Fatal("GetSession returned nil after register")
	}
	if ctx.Cwd != "/project/a" {
		t.Errorf("Cwd: got %q, want %q", ctx.Cwd, "/project/a")
	}

	sm.RemoveSession(1)

	if got := sm.SessionCount(); got != 0 {
		t.Fatalf("SessionCount after remove: got %d, want 0", got)
	}
	if sm.GetSession(1) != nil {
		t.Fatal("GetSession returned non-nil after remove")
	}
}

func TestPreRegisterBind(t *testing.T) {
	sm := NewManager()

	s := &Session{ID: 2}
	sm.RegisterSession(s, "")

	env := map[string]string{"API_KEY": "secret"}
	sm.PreRegister("tok-abc", "/project/b", env)

	ok := sm.Bind("tok-abc", "owner-1", s)
	if !ok {
		t.Fatal("Bind returned false for a registered token")
	}
	if s.Cwd != "/project/b" {
		t.Errorf("session.Cwd after Bind: got %q, want %q", s.Cwd, "/project/b")
	}
	if got := s.Env["API_KEY"]; got != "secret" {
		t.Fatalf("session.Env[API_KEY] = %q, want %q", got, "secret")
	}

	ctx := sm.GetSession(2)
	if ctx == nil {
		t.Fatal("GetSession returned nil after bind")
	}
	if ctx.Cwd != "/project/b" {
		t.Errorf("Context.Cwd after Bind: got %q, want %q", ctx.Cwd, "/project/b")
	}
	if got := ctx.Env["API_KEY"]; got != "secret" {
		t.Fatalf("context.Env[API_KEY] = %q, want %q", got, "secret")
	}
	hist, ok := sm.bound["tok-abc"]
	if !ok {
		t.Fatal("bound history missing for consumed token")
	}
	if hist.OwnerKey != "owner-1" {
		t.Fatalf("bound owner key = %q, want %q", hist.OwnerKey, "owner-1")
	}
	if hist.BoundAt.IsZero() || hist.LastUsed.IsZero() {
		t.Fatal("bound timestamps must be populated")
	}

	// Token must be consumed — second Bind must return false.
	ok2 := sm.Bind("tok-abc", "owner-1", s)
	if ok2 {
		t.Fatal("Bind returned true for an already-consumed token")
	}
}

func TestIsPreRegistered(t *testing.T) {
	sm := NewManager()

	sm.PreRegister("tok-pending", "/project/g", nil)

	if !sm.IsPreRegistered("tok-pending") {
		t.Fatal("IsPreRegistered returned false for pre-registered token")
	}

	s := &Session{ID: 8}
	sm.RegisterSession(s, "")
	if ok := sm.Bind("tok-pending", "owner-pending", s); !ok {
		t.Fatal("Bind returned false for a pre-registered token")
	}

	if sm.IsPreRegistered("tok-pending") {
		t.Fatal("IsPreRegistered returned true after token was consumed")
	}
}

func TestTrackCompleteRequest(t *testing.T) {
	sm := NewManager()

	s := &Session{ID: 3}
	sm.RegisterSession(s, "/project/c")
	sm.TrackRequest("s3:n:1", 3)

	ctx := sm.ResolveCallback()
	if ctx == nil {
		t.Fatal("ResolveCallback returned nil with one inflight request")
	}
	if ctx.Session.ID != 3 {
		t.Errorf("ResolveCallback session ID: got %d, want 3", ctx.Session.ID)
	}

	sm.CompleteRequest("s3:n:1")

	if sm.ResolveCallback() != nil {
		t.Fatal("ResolveCallback should return nil after CompleteRequest")
	}
}

func TestResolveCallbackSingleSession(t *testing.T) {
	sm := NewManager()

	s := &Session{ID: 4}
	sm.RegisterSession(s, "/project/d")
	sm.TrackRequest("s4:n:10", 4)
	sm.TrackRequest("s4:n:11", 4)

	ctx := sm.ResolveCallback()
	if ctx == nil {
		t.Fatal("ResolveCallback returned nil")
	}
	if ctx.Session.ID != 4 {
		t.Errorf("expected session 4, got %d", ctx.Session.ID)
	}
}

func TestResolveCallbackNoSessions(t *testing.T) {
	sm := NewManager()

	ctx := sm.ResolveCallback()
	if ctx != nil {
		t.Fatalf("ResolveCallback with no inflight requests should return nil, got session %d", ctx.Session.ID)
	}
}

func TestResolveCallbackMultipleSessions(t *testing.T) {
	sm := NewManager()

	s1 := &Session{ID: 5}
	s2 := &Session{ID: 6}
	sm.RegisterSession(s1, "/project/e")
	sm.RegisterSession(s2, "/project/f")

	sm.TrackRequest("s5:n:1", 5)
	// Ensure session 6 is tracked more recently.
	time.Sleep(2 * time.Millisecond)
	sm.TrackRequest("s6:n:1", 6)

	ctx := sm.ResolveCallback()
	if ctx == nil {
		t.Fatal("ResolveCallback returned nil with two inflight sessions")
	}
	if ctx.Session.ID != 6 {
		t.Errorf("expected most-recently-tracked session 6, got %d", ctx.Session.ID)
	}
}

func TestBindUnknownToken(t *testing.T) {
	sm := NewManager()

	s := &Session{ID: 7}
	ok := sm.Bind("no-such-token", "owner-missing", s)
	if ok {
		t.Fatal("Bind should return false for an unknown token")
	}
}

func TestLookupHistory_AfterBind(t *testing.T) {
	sm := NewManager()
	s := &Session{ID: 9}
	sm.RegisterSession(s, "")
	env := map[string]string{"TOKEN": "value"}
	sm.PreRegister("tok-history", "/project/history", env)
	if ok := sm.Bind("tok-history", "owner-history", s); !ok {
		t.Fatal("Bind returned false")
	}

	env["TOKEN"] = "mutated"
	ownerKey, cwd, gotEnv, ok := sm.LookupHistory("tok-history")
	if !ok {
		t.Fatal("LookupHistory returned ok=false")
	}
	if ownerKey != "owner-history" {
		t.Fatalf("ownerKey = %q, want %q", ownerKey, "owner-history")
	}
	if cwd != "/project/history" {
		t.Fatalf("cwd = %q, want %q", cwd, "/project/history")
	}
	if gotEnv["TOKEN"] != "value" {
		t.Fatalf("env[TOKEN] = %q, want %q", gotEnv["TOKEN"], "value")
	}
}

func TestRegisterReconnect_NewTokenFresh(t *testing.T) {
	sm := NewManager()
	s := &Session{ID: 10}
	sm.RegisterSession(s, "")
	sm.PreRegister("tok-prev", "/project/reconnect", map[string]string{"X": "1"})
	if ok := sm.Bind("tok-prev", "owner-reconnect", s); !ok {
		t.Fatal("Bind returned false")
	}

	called := 0
	newToken, err := sm.RegisterReconnect("tok-prev", func(key string) bool {
		called++
		return key == "owner-reconnect"
	})
	if err != nil {
		t.Fatalf("RegisterReconnect() error = %v", err)
	}
	if called != 1 {
		t.Fatalf("ownerAlive call count = %d, want 1", called)
	}
	if newToken == "" || newToken == "tok-prev" {
		t.Fatalf("newToken = %q, want distinct non-empty token", newToken)
	}
	if !sm.IsPreRegistered(newToken) {
		t.Fatal("new reconnect token must be pending")
	}
	ownerKey, cwd, env, ok := sm.LookupHistory(newToken)
	if !ok {
		t.Fatal("LookupHistory returned ok=false for refreshed token")
	}
	if ownerKey != "owner-reconnect" || cwd != "/project/reconnect" || env["X"] != "1" {
		t.Fatalf("LookupHistory(newToken) = (%q, %q, %v), want owner-reconnect, /project/reconnect, X=1", ownerKey, cwd, env)
	}

	s2 := &Session{ID: 11}
	sm.RegisterSession(s2, "")
	if ok := sm.Bind(newToken, "owner-reconnect", s2); !ok {
		t.Fatal("Bind on refreshed token returned false")
	}
	if s2.Cwd != "/project/reconnect" {
		t.Fatalf("session Cwd = %q, want %q", s2.Cwd, "/project/reconnect")
	}
	if s2.Env["X"] != "1" {
		t.Fatalf("session Env[X] = %q, want %q", s2.Env["X"], "1")
	}
}

func TestRegisterReconnect_UnknownToken(t *testing.T) {
	sm := NewManager()
	_, err := sm.RegisterReconnect("missing-token", func(string) bool { return true })
	if !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("RegisterReconnect() error = %v, want ErrUnknownToken", err)
	}
}

func TestRegisterReconnect_OwnerGone(t *testing.T) {
	sm := NewManager()
	s := &Session{ID: 12}
	sm.RegisterSession(s, "")
	sm.PreRegister("tok-owner-gone", "/project/owner-gone", nil)
	if ok := sm.Bind("tok-owner-gone", "owner-gone", s); !ok {
		t.Fatal("Bind returned false")
	}

	_, err := sm.RegisterReconnect("tok-owner-gone", func(key string) bool {
		return false
	})
	if !errors.Is(err, ErrOwnerGone) {
		t.Fatalf("RegisterReconnect() error = %v, want ErrOwnerGone", err)
	}
}

func TestRegisterReconnect_RaceDoubleRefresh(t *testing.T) {
	sm := NewManager()
	s := &Session{ID: 13}
	sm.RegisterSession(s, "")
	sm.PreRegister("tok-race", "/project/race", map[string]string{"K": "V"})
	if ok := sm.Bind("tok-race", "owner-race", s); !ok {
		t.Fatal("Bind returned false")
	}

	start := make(chan struct{})
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			token, err := sm.RegisterReconnect("tok-race", func(key string) bool {
				return key == "owner-race"
			})
			errs <- err
			results <- token
		}()
	}
	close(start)

	firstErr := <-errs
	secondErr := <-errs
	if firstErr != nil || secondErr != nil {
		t.Fatalf("RegisterReconnect() errs = %v, %v", firstErr, secondErr)
	}
	first := <-results
	second := <-results
	if first == second {
		t.Fatalf("distinct reconnect tokens expected, both were %q", first)
	}
	if !sm.IsPreRegistered(first) || !sm.IsPreRegistered(second) {
		t.Fatal("both reconnect tokens must be pending")
	}
}

func TestRegisterReconnect_OwnerAliveCalledWithoutLock(t *testing.T) {
	sm := NewManager()
	s := &Session{ID: 14}
	sm.RegisterSession(s, "")
	sm.PreRegister("tok-unlocked", "/project/unlocked", nil)
	if ok := sm.Bind("tok-unlocked", "owner-unlocked", s); !ok {
		t.Fatal("Bind returned false")
	}

	_, err := sm.RegisterReconnect("tok-unlocked", func(string) bool {
		acquired := make(chan struct{})
		go func() {
			sm.mu.Lock()
			sm.mu.Unlock()
			close(acquired)
		}()
		select {
		case <-acquired:
			return true
		case <-time.After(200 * time.Millisecond):
			t.Fatal("ownerAlive called while sm.mu was still held")
			return false
		}
	})
	if err != nil {
		t.Fatalf("RegisterReconnect() error = %v", err)
	}
}

func TestSweepExpiredBound(t *testing.T) {
	sm := NewManager()
	sm.bound["expired"] = &boundHistory{
		OwnerKey: "owner-expired",
		Cwd:      "/project/expired",
		BoundAt:  time.Now().Add(-31 * time.Minute),
		LastUsed: time.Now().Add(-31 * time.Minute),
	}
	sm.bound["fresh"] = &boundHistory{
		OwnerKey: "owner-fresh",
		Cwd:      "/project/fresh",
		BoundAt:  time.Now(),
		LastUsed: time.Now(),
	}

	swept := sm.SweepExpiredBound()
	if swept != 1 {
		t.Fatalf("SweepExpiredBound() = %d, want 1", swept)
	}
	if _, ok := sm.bound["expired"]; ok {
		t.Fatal("expired bound entry still present")
	}
	if _, ok := sm.bound["fresh"]; !ok {
		t.Fatal("fresh bound entry was removed")
	}
}

func TestConcurrentAccess(t *testing.T) {
	sm := NewManager()

	const goroutines = 5
	const iterations = 100

	var counter atomic.Int64
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := int(counter.Add(1))
				sess := &Session{ID: id}
				sm.RegisterSession(sess, fmt.Sprintf("/project/%d", id))

				remappedID := fmt.Sprintf("s%d:n:%d", id, id)
				sm.TrackRequest(remappedID, id)

				_ = sm.ResolveCallback()
				_ = sm.SessionCount()
				_ = sm.GetSession(id)

				sm.CompleteRequest(remappedID)
				sm.RemoveSession(id)
			}
		}()
	}

	wg.Wait()
}
