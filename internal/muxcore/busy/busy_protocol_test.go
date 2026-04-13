package busy_test

import (
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/internal/muxcore/busy"
)

func TestParseBusyNotification_Full(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{"id":"job-42","estimatedDurationMs":60000,"task":"long inference"}}`)
	p, err := busy.ParseBusyNotification(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "job-42" {
		t.Errorf("id want %q, got %q", "job-42", p.ID)
	}
	if p.Task != "long inference" {
		t.Errorf("task want %q, got %q", "long inference", p.Task)
	}
	// estimatedDurationMs=60000 → hard cap 120 000 ms
	if p.EstimatedDuration != 60*time.Second {
		t.Errorf("estimated want 60s, got %s", p.EstimatedDuration)
	}
}

func TestParseBusyNotification_MissingID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{}}`)
	_, err := busy.ParseBusyNotification(raw)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestParseBusyNotification_DurationCappedAt24h(t *testing.T) {
	// estimatedDurationMs = 200_000_000 (> 86_400_000 ms = 24 h) must be clamped.
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{"id":"huge","estimatedDurationMs":200000000}}`)
	p, err := busy.ParseBusyNotification(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Duration(busy.MaxBusyDurationMs) * time.Millisecond
	if p.EstimatedDuration != want {
		t.Errorf("clamped duration want %s, got %s", want, p.EstimatedDuration)
	}
}

func TestParseBusyNotification_StartedAt(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{"id":"ts-test","startedAt":"2026-01-01T00:00:00Z","estimatedDurationMs":5000}}`)
	p, err := busy.ParseBusyNotification(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !p.StartedAt.Equal(want) {
		t.Errorf("startedAt want %v, got %v", want, p.StartedAt)
	}
}

func TestParseIdleNotification_OK(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/idle","params":{"id":"cleanup-me"}}`)
	p, err := busy.ParseIdleNotification(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "cleanup-me" {
		t.Errorf("id want %q, got %q", "cleanup-me", p.ID)
	}
}

func TestParseIdleNotification_MissingID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/idle","params":{}}`)
	_, err := busy.ParseIdleNotification(raw)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestParseBusyNotification_ZeroDuration(t *testing.T) {
	// Zero estimatedDurationMs → zero duration returned (caller applies default).
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/x-mux/busy","params":{"id":"zero-dur"}}`)
	p, err := busy.ParseBusyNotification(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.EstimatedDuration != 0 {
		t.Errorf("want zero duration, got %s", p.EstimatedDuration)
	}
}
