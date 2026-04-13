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
	d := shouldEvict(baseSample(), time.Now(), 10*time.Minute)
	if !d.evict {
		t.Fatal("want evict for truly-idle owner")
	}
}

func TestShouldEvict_SessionAttached(t *testing.T) {
	s := baseSample()
	s.Sessions = 1
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("session attached should block eviction")
	}
}

func TestShouldEvict_Persistent(t *testing.T) {
	s := baseSample()
	s.Persistent = true
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("persistent should block eviction")
	}
}

func TestShouldEvict_PendingRequests(t *testing.T) {
	s := baseSample()
	s.PendingRequests = 1
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("pending requests should block eviction")
	}
}

func TestShouldEvict_ActiveProgressTokens(t *testing.T) {
	s := baseSample()
	s.ActiveProgressTokens = 2
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("active progress tokens should block eviction")
	}
}

func TestShouldEvict_HasBusyWork(t *testing.T) {
	s := baseSample()
	s.HasBusyWork = true
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("busy declaration should block eviction")
	}
}

func TestShouldEvict_WithinIdleWindow(t *testing.T) {
	s := baseSample()
	s.LastSession = time.Now().Add(-5 * time.Minute)
	s.LastActivity = time.Now().Add(-5 * time.Minute)
	// IdleTimeout 10 min, elapsed 5 min → keep alive.
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("within idle window should not evict")
	}
}

func TestShouldEvict_ActivityResetsClock(t *testing.T) {
	s := baseSample()
	// Last session way in the past...
	s.LastSession = time.Now().Add(-1 * time.Hour)
	// ...but recent activity → keep alive.
	s.LastActivity = time.Now().Add(-30 * time.Second)
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
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
	if shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("x-mux.idleTimeout override should keep owner alive")
	}
}

func TestShouldEvict_FallbackToDaemonDefault(t *testing.T) {
	s := baseSample()
	s.IdleTimeout = 0         // entry has no per-owner timeout
	s.OwnerIdleOverride = 0   // no override
	s.LastSession = time.Now().Add(-1 * time.Hour)
	s.LastActivity = time.Now().Add(-1 * time.Hour)
	// Daemon default 10 min → 1 hour > 10 min → evict.
	if !shouldEvict(s, time.Now(), 10*time.Minute).evict {
		t.Fatal("expected fallback to daemon default")
	}
}

func TestShouldEvict_ZeroTimeoutEverywhere(t *testing.T) {
	s := baseSample()
	s.IdleTimeout = 0
	s.OwnerIdleOverride = 0
	// Daemon default 0 → should never evict (no timeout configured).
	if shouldEvict(s, time.Now(), 0).evict {
		t.Fatal("zero timeout everywhere should not evict")
	}
}

func TestShouldEvict_ReportsElapsedAndTimeout(t *testing.T) {
	s := baseSample()
	s.LastSession = time.Now().Add(-20 * time.Minute)
	s.LastActivity = time.Now().Add(-20 * time.Minute)
	s.IdleTimeout = 5 * time.Minute
	d := shouldEvict(s, time.Now(), 10*time.Minute)
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
