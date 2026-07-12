package main

import (
	"errors"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/owner"
)

func TestShimLifecycleDurationsDefaults(t *testing.T) {
	idle, grace := shimLifecycleDurations(func(string) string { return "" })
	if idle != 10*time.Minute {
		t.Fatalf("idle timeout = %s, want 10m", idle)
	}
	if grace != 30*time.Second {
		t.Fatalf("dormant grace = %s, want 30s", grace)
	}
}

func TestShimLifecycleDurationsNegativeDisables(t *testing.T) {
	values := map[string]string{
		envShimIdleTimeout:  "-1s",
		envShimDormantGrace: "-1s",
	}
	idle, grace := shimLifecycleDurations(func(key string) string { return values[key] })
	if idle >= 0 || grace >= 0 {
		t.Fatalf("negative env must disable: idle=%s grace=%s", idle, grace)
	}
}

func TestShimLifecycleDurationsInvalidFallsBackSafely(t *testing.T) {
	values := map[string]string{
		envShimIdleTimeout:  "garbage",
		envShimDormantGrace: "also-garbage",
	}
	idle, grace := shimLifecycleDurations(func(key string) string { return values[key] })
	if idle != 10*time.Minute || grace != 30*time.Second {
		t.Fatalf("invalid env fallback = (%s, %s), want (10m, 30s)", idle, grace)
	}
}

func TestResilientClientExitCodeMapsDormantSentinel(t *testing.T) {
	if got := resilientClientExitCode(owner.ErrIdleDormant); got != launcherDormantExitCode {
		t.Fatalf("dormant exit code = %d, want %d", got, launcherDormantExitCode)
	}
	if got := resilientClientExitCode(errors.New("boom")); got != 1 {
		t.Fatalf("ordinary error exit code = %d, want 1", got)
	}
}
