package session

import (
	"errors"
	"testing"
	"time"
)

func TestLiveBoundTokenDoesNotExpire(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	sm := NewManager()
	sm.now = func() time.Time { return now }

	s := &Session{ID: 201}
	sm.RegisterSession(s, "")
	sm.PreRegisterForOwner("live-token", "owner-live", "/project/live", nil)
	if !sm.Bind("live-token", "owner-live", s) {
		t.Fatal("Bind returned false")
	}

	now = now.Add(2 * boundTokenTTL)
	if _, _, _, ok := sm.LookupHistory("live-token"); !ok {
		t.Fatal("live token expired")
	}
	if swept := sm.SweepExpiredBound(); swept != 0 {
		t.Fatalf("SweepExpiredBound() = %d, want 0 for live token", swept)
	}
	if _, err := sm.RegisterReconnect("live-token", func(string) bool { return true }); err != nil {
		t.Fatalf("RegisterReconnect(live token): %v", err)
	}
}

func TestRemoveSessionStartsHistoricalTokenTTL(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	sm := NewManager()
	sm.now = func() time.Time { return now }

	s := &Session{ID: 202}
	sm.RegisterSession(s, "")
	sm.PreRegisterForOwner("ended-token", "owner-ended", "/project/ended", nil)
	if !sm.Bind("ended-token", "owner-ended", s) {
		t.Fatal("Bind returned false")
	}

	now = now.Add(2 * boundTokenTTL)
	sm.RemoveSession(s.ID)
	now = now.Add(boundTokenTTL - time.Second)
	if _, _, _, ok := sm.LookupHistory("ended-token"); !ok {
		t.Fatal("token expired before the post-disconnect TTL elapsed")
	}
	now = now.Add(2 * time.Second)
	if _, _, _, ok := sm.LookupHistory("ended-token"); ok {
		t.Fatal("inactive token survived beyond the post-disconnect TTL")
	}
	if _, err := sm.RegisterReconnect("ended-token", func(string) bool { return true }); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("RegisterReconnect() error = %v, want ErrUnknownToken", err)
	}
}

func TestSnapshotRenewsOnlyLiveAgedToken(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	sm := NewManager()
	sm.now = func() time.Time { return now }

	live := &Session{ID: 203}
	sm.RegisterSession(live, "")
	sm.PreRegisterForOwner("snapshot-live", "owner-live", "/project/live", nil)
	if !sm.Bind("snapshot-live", "owner-live", live) {
		t.Fatal("Bind live token returned false")
	}

	inactive := &Session{ID: 204}
	sm.RegisterSession(inactive, "")
	sm.PreRegisterForOwner("snapshot-expired", "owner-expired", "/project/expired", nil)
	if !sm.Bind("snapshot-expired", "owner-expired", inactive) {
		t.Fatal("Bind inactive token returned false")
	}
	sm.RemoveSession(inactive.ID)

	now = now.Add(2 * boundTokenTTL)
	exported := sm.ExportBoundHistory()
	if len(exported) != 1 || exported[0].Token != "snapshot-live" {
		t.Fatalf("ExportBoundHistory() = %+v, want only snapshot-live", exported)
	}
	if !exported[0].LastUsed.Equal(now) {
		t.Fatalf("live snapshot LastUsed = %v, want export time %v", exported[0].LastUsed, now)
	}

	successor := NewManager()
	successor.now = func() time.Time { return now }
	if imported := successor.ImportBoundHistory(exported); imported != 1 {
		t.Fatalf("ImportBoundHistory() = %d, want 1", imported)
	}
	if _, err := successor.RegisterReconnect("snapshot-live", func(key string) bool {
		return key == "owner-live"
	}); err != nil {
		t.Fatalf("successor RegisterReconnect(): %v", err)
	}
}
