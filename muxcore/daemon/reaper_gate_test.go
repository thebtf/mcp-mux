package daemon

import (
	"testing"
	"time"
)

// Baseline sample: fully idle, no sessions, no activity, 10 min ago.
func baseSample() evictionSample {
	past := time.Now().Add(-1 * time.Hour)
	return evictionSample{
		Sessions:             0,
		Persistent:           false,
		PendingRequests:      0,
		ActiveProgressTokens: 0,
		HasBusyWork:          false,
		IdleTimeout:          10 * time.Minute,
		OwnerIdleOverride:    0,
		LastSession:          past,
		LastActivity:         past,
	}
}

func TestShouldEvict_BaselineIdle(t *testing.T) {
	d := shouldEvict(baseSample(), time.Now(), 10*time.Minute, 60*time.Second)
	if !d.evict {
		t.Fatal("want evict for truly-idle owner")
	}
}

func TestShouldEvict_SessionAttached(t *testing.T) {
	s := baseSample()
	s.Sessions = 1
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("session attached should block eviction")
	}
}

func TestShouldEvict_Persistent(t *testing.T) {
	s := baseSample()
	s.Persistent = true
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("persistent should block eviction")
	}
}

func TestShouldEvict_PendingRequests(t *testing.T) {
	s := baseSample()
	s.PendingRequests = 1
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("pending requests should block eviction")
	}
}

func TestShouldEvict_PendingSession(t *testing.T) {
	s := baseSample()
	s.PendingSessions = 1
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("pending session reservation should block eviction")
	}
}

func TestShouldEvict_ActiveProgressTokens(t *testing.T) {
	s := baseSample()
	s.ActiveProgressTokens = 2
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("active progress tokens should block eviction")
	}
}

func TestShouldEvict_HasBusyWork(t *testing.T) {
	s := baseSample()
	s.HasBusyWork = true
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("busy declaration should block eviction")
	}
}

func TestShouldEvict_WithinIdleWindow(t *testing.T) {
	s := baseSample()
	s.LastSession = time.Now().Add(-5 * time.Minute)
	s.LastActivity = time.Now().Add(-5 * time.Minute)
	// IdleTimeout 10 min, elapsed 5 min → keep alive.
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("within idle window should not evict")
	}
}

func TestShouldEvict_ActivityResetsClock(t *testing.T) {
	s := baseSample()
	// Last session way in the past...
	s.LastSession = time.Now().Add(-1 * time.Hour)
	// ...but recent activity → keep alive.
	s.LastActivity = time.Now().Add(-30 * time.Second)
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("recent activity should reset idle clock")
	}
}

func TestShouldEvict_OverridePreferred(t *testing.T) {
	s := baseSample()
	// Entry idle = 10 min, elapsed 15 min → normally evict.
	// But x-mux.idleTimeout override = 30 min → should keep alive.
	s.LastSession = time.Now().Add(-15 * time.Minute)
	s.LastActivity = time.Now().Add(-15 * time.Minute)
	s.IdleTimeout = 10 * time.Minute
	s.OwnerIdleOverride = 30 * time.Minute
	if shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("x-mux.idleTimeout override should keep owner alive")
	}
}

func TestShouldEvict_FallbackToDaemonDefault(t *testing.T) {
	s := baseSample()
	s.IdleTimeout = 0       // entry has no per-owner timeout
	s.OwnerIdleOverride = 0 // no override
	s.LastSession = time.Now().Add(-1 * time.Hour)
	s.LastActivity = time.Now().Add(-1 * time.Hour)
	// Daemon default 10 min → 1 hour > 10 min → evict.
	if !shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second).evict {
		t.Fatal("expected fallback to daemon default")
	}
}

func TestShouldEvict_ZeroTimeoutEverywhere(t *testing.T) {
	s := baseSample()
	s.IdleTimeout = 0
	s.OwnerIdleOverride = 0
	// Daemon default 0 AND isolated default 0 → should never evict.
	if shouldEvict(s, time.Now(), 0, 0).evict {
		t.Fatal("zero timeout everywhere should not evict")
	}
}

func TestShouldEvict_ReportsElapsedAndTimeout(t *testing.T) {
	s := baseSample()
	s.LastSession = time.Now().Add(-20 * time.Minute)
	s.LastActivity = time.Now().Add(-20 * time.Minute)
	s.IdleTimeout = 5 * time.Minute
	d := shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second)
	if !d.evict {
		t.Fatal("want evict")
	}
	if d.idleTimeout != 5*time.Minute {
		t.Errorf("idleTimeout = %s, want 5m", d.idleTimeout)
	}
	if d.elapsed < 19*time.Minute || d.elapsed > 21*time.Minute {
		t.Errorf("elapsed = %s, want ~20m", d.elapsed)
	}
}

// --- Fix C: early-reap isolated owners (Engram #244 Bug 2) ---

// TestShouldEvict_IsolatedClassifiedShortTimeout proves an isolated owner with
// 0 sessions and elapsed > isolatedIdleTimeout is evicted on the SHORT clock,
// even when the entry's IdleTimeout (and the daemon default) are much longer.
// Before Fix C: an isolated owner sat for the full 10-minute general timeout.
// After Fix C: it is reaped at 60s (default isolatedIdleTimeout).
func TestShouldEvict_IsolatedClassifiedShortTimeout(t *testing.T) {
	s := baseSample()
	s.IsolatedClassified = true
	s.IdleTimeout = 10 * time.Minute
	s.LastSession = time.Now().Add(-90 * time.Second)
	s.LastActivity = time.Now().Add(-90 * time.Second)

	d := shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second)
	if !d.evict {
		t.Fatal("isolated-classified owner past isolated timeout should evict")
	}
	if d.idleTimeout != 60*time.Second {
		t.Errorf("idleTimeout = %s, want 60s", d.idleTimeout)
	}
	if d.reason != "idle-isolated" {
		t.Errorf("reason = %q, want %q", d.reason, "idle-isolated")
	}
}

// TestShouldEvict_IsolatedWithinShortWindow confirms the symmetric case:
// inside the short window, isolated owners are kept alive (so a quick session
// reconnect can still reattach).
func TestShouldEvict_IsolatedWithinShortWindow(t *testing.T) {
	s := baseSample()
	s.IsolatedClassified = true
	s.IdleTimeout = 10 * time.Minute
	s.LastSession = time.Now().Add(-30 * time.Second)
	s.LastActivity = time.Now().Add(-30 * time.Second)

	d := shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second)
	if d.evict {
		t.Fatal("isolated owner inside short window should not evict")
	}
}

// TestShouldEvict_SharedNotAffectedByIsolatedTimeout confirms the negation:
// when IsolatedClassified is false, the shorter timeout is ignored and the
// owner only evicts at the general IdleTimeout. This is the safety net that
// prevents Fix C from accidentally killing shared (cross-CWD) owners that
// future Spawn calls would otherwise reuse.
func TestShouldEvict_SharedNotAffectedByIsolatedTimeout(t *testing.T) {
	s := baseSample()
	s.IsolatedClassified = false
	s.IdleTimeout = 10 * time.Minute
	s.LastSession = time.Now().Add(-90 * time.Second)
	s.LastActivity = time.Now().Add(-90 * time.Second)

	d := shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second)
	if d.evict {
		t.Fatal("shared owner inside general window should not evict regardless of isolated timeout")
	}
}

// TestShouldEvict_IsolatedOverrideStillWins confirms that an explicit
// x-mux.idleTimeout from the upstream still wins even on isolated owners.
// Servers know their own state best — if an isolated server explicitly
// declares a long idle timeout (e.g. holding async state), respect it.
func TestShouldEvict_IsolatedOverrideStillWins(t *testing.T) {
	s := baseSample()
	s.IsolatedClassified = true
	s.IdleTimeout = 10 * time.Minute
	s.OwnerIdleOverride = 30 * time.Minute
	s.LastSession = time.Now().Add(-15 * time.Minute)
	s.LastActivity = time.Now().Add(-15 * time.Minute)

	d := shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second)
	if d.evict {
		t.Fatal("explicit x-mux.idleTimeout should keep isolated owner alive")
	}
	if d.idleTimeout != 30*time.Minute {
		t.Errorf("idleTimeout = %s, want 30m (override)", d.idleTimeout)
	}
}

// TestShouldEvict_IsolatedDisabledByZero confirms that setting
// isolatedIdleTimeout = 0 disables the optimization — isolated owners then
// fall back to the general IdleTimeout, matching pre-Fix-C behavior.
func TestShouldEvict_IsolatedDisabledByZero(t *testing.T) {
	s := baseSample()
	s.IsolatedClassified = true
	s.IdleTimeout = 10 * time.Minute
	s.LastSession = time.Now().Add(-90 * time.Second)
	s.LastActivity = time.Now().Add(-90 * time.Second)

	d := shouldEvict(s, time.Now(), 10*time.Minute, 0)
	if d.evict {
		t.Fatal("isolatedIdleTimeout=0 should disable the optimization (no early reap)")
	}
	if d.reason != "idle" {
		t.Errorf("reason = %q, want %q when isolated short-timeout disabled", d.reason, "idle")
	}
}

func TestShouldEvict_MaterializationBlocksZombieAndIdleRemoval(t *testing.T) {
	s := baseSample()
	s.UpstreamDead = true
	s.MaterializationBlocked = true
	if decision := shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second); decision.evict {
		t.Fatalf("materializing owner was evicted: %+v", decision)
	}
}

func TestShouldEvict_CacheOnlyOwnerIsNotZombie(t *testing.T) {
	s := baseSample()
	s.UpstreamDead = true
	s.CacheReady = true
	decision := shouldEvict(s, time.Now(), 10*time.Minute, 60*time.Second)
	if !decision.evict {
		t.Fatal("cache-only owner past idle timeout should follow ordinary idle eviction")
	}
	if decision.reason != "idle" {
		t.Fatalf("cache-only owner reason=%q, want idle", decision.reason)
	}
}
