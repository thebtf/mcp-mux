package session

import (
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

	sm.PreRegister("tok-abc", "/project/b", nil)

	ok := sm.Bind("tok-abc", s)
	if !ok {
		t.Fatal("Bind returned false for a registered token")
	}
	if s.Cwd != "/project/b" {
		t.Errorf("session.Cwd after Bind: got %q, want %q", s.Cwd, "/project/b")
	}

	ctx := sm.GetSession(2)
	if ctx == nil {
		t.Fatal("GetSession returned nil after bind")
	}
	if ctx.Cwd != "/project/b" {
		t.Errorf("Context.Cwd after Bind: got %q, want %q", ctx.Cwd, "/project/b")
	}

	// Token must be consumed — second Bind must return false.
	ok2 := sm.Bind("tok-abc", s)
	if ok2 {
		t.Fatal("Bind returned true for an already-consumed token")
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
	ok := sm.Bind("no-such-token", s)
	if ok {
		t.Fatal("Bind should return false for an unknown token")
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
