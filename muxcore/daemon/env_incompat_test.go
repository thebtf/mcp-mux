package daemon

import (
	"strings"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

// TestGlobalFirst_EnvIncompatibleOwnersDistinct proves spec AC8 (CR-002):
//
//	"Two Spawn calls with identical (cmd, args) from any combination of
//	 cwds but mutually incompatible env per the existing envCompatible()
//	 predicate (e.g., GITHUB_TOKEN=abc vs GITHUB_TOKEN=xyz) MUST produce
//	 DISTINCT owners. The global-first default does NOT collapse env-
//	 incompatible sessions onto one upstream — credentials remain
//	 partitioned at the daemon-owner boundary."
//
// I am running two Spawns from the SAME cwd with the SAME (cmd, args)
// but explicitly different GITHUB_TOKEN values. Under the pre-AC8 path,
// global-first would have produced one shared owner because the base
// global sid is identical. Under AC8, the second spawn's env-bucketed
// sid suffix differentiates the owners. Expected: two distinct sids,
// OwnerCount == 2.
func TestGlobalFirst_EnvIncompatibleOwnersDistinct(t *testing.T) {
	// TempDir BEFORE testDaemon for Windows cleanup ordering.
	cwd := t.TempDir()
	d := testDaemon(t)
	mockServer := mockServerAbsPath(t)

	_, sid1, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd,
		Env:     map[string]string{"GITHUB_TOKEN": "tokenA"},
		// Mode omitted — relies on default ModeGlobal (CR-002 phase A).
	})
	if err != nil {
		t.Fatalf("Spawn tokenA: %v", err)
	}

	_, sid2, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd,
		Env:     map[string]string{"GITHUB_TOKEN": "tokenB"},
	})
	if err != nil {
		t.Fatalf("Spawn tokenB: %v", err)
	}

	if sid1 == sid2 {
		t.Fatalf("env-incompatible Spawns produced identical sid %q — AC8 violated", sid1)
	}
	if !strings.HasPrefix(sid2, sid1[:16]+"-env-") {
		// sid2 should be `{baseSid}-env-{8hex}`. sid1 is the base global sid
		// (16 hex chars). If sid1 itself contains an env suffix that's not
		// expected for the first spawn — log for diagnostics.
		t.Logf("sid1=%s sid2=%s — sid2 should be sid1 + '-env-' + 8hex", sid1, sid2)
	}
	if got := d.OwnerCount(); got != 2 {
		t.Errorf("OwnerCount = %d, want 2 for env-incompatible Spawns under global-first", got)
	}
}

// TestGlobalFirst_EnvCompatibleSharedSingleOwner proves the symmetric AC8
// invariant: two Spawns with identical (cmd, args) AND compatible env
// (same values for semantically significant keys) MUST collapse onto one
// owner under global-first. This is the happy path — sessions with the
// same credentials share an upstream.
func TestGlobalFirst_EnvCompatibleSharedSingleOwner(t *testing.T) {
	cwd1 := t.TempDir()
	cwd2 := t.TempDir()
	d := testDaemon(t)
	mockServer := mockServerAbsPath(t)

	identical := map[string]string{"GITHUB_TOKEN": "shared-token"}

	_, sid1, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd1,
		Env:     identical,
	})
	if err != nil {
		t.Fatalf("Spawn 1: %v", err)
	}

	// Second Spawn from a different cwd but with identical env. Note: this
	// path may engage the admission gate (cross-cwd, classification not
	// yet known); the gate's wait allows the classification to land and
	// the bind to proceed under the same (env-compatible) global sid.
	_, sid2, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd2,
		Env:     identical,
	})
	if err != nil {
		t.Fatalf("Spawn 2: %v", err)
	}

	if sid1 != sid2 {
		t.Errorf("env-compatible Spawns produced distinct sids %q vs %q — env-bucket false positive?",
			sid1, sid2)
	}
	if got := d.OwnerCount(); got != 1 {
		t.Errorf("OwnerCount = %d, want 1 for env-compatible Spawns under global-first", got)
	}
}

// TestSemanticEnvHash_Deterministic asserts that the helper produces
// identical hashes for identical inputs and different hashes for
// different inputs on a semantically significant key.
func TestSemanticEnvHash_Deterministic(t *testing.T) {
	a := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "abc", "PATH": "/usr/bin"})
	b := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "abc", "PATH": "/usr/bin"})
	if a != b {
		t.Errorf("identical inputs produced different hashes: %q vs %q", a, b)
	}

	c := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "xyz", "PATH": "/usr/bin"})
	if a == c {
		t.Errorf("different GITHUB_TOKEN produced identical hash: %q", a)
	}
}

// TestSemanticEnvHash_IgnoresTransient asserts the helper skips transient
// keys (CLAUDE_CODE_*, WT_*, SESSIONNAME, WSLENV) so per-session var
// variation does not fragment owner identity.
func TestSemanticEnvHash_IgnoresTransient(t *testing.T) {
	withTransient := semanticEnvHash(map[string]string{
		"GITHUB_TOKEN":           "abc",
		"CLAUDE_CODE_ENTRYPOINT": "cli",
		"WT_SESSION":             "session-1",
	})
	withoutTransient := semanticEnvHash(map[string]string{
		"GITHUB_TOKEN": "abc",
	})
	if withTransient != withoutTransient {
		t.Errorf("transient keys leaked into hash: with=%q vs without=%q",
			withTransient, withoutTransient)
	}
}

// TestSemanticEnvHash_EmptyEnv asserts both nil and empty maps produce
// a stable sentinel value.
func TestSemanticEnvHash_EmptyEnv(t *testing.T) {
	if got := semanticEnvHash(nil); got != "00000000" {
		t.Errorf("nil env hash = %q, want %q", got, "00000000")
	}
	if got := semanticEnvHash(map[string]string{}); got != "00000000" {
		t.Errorf("empty env hash = %q, want %q", got, "00000000")
	}
	// All-transient env also produces the sentinel — no semantic content.
	allTransient := semanticEnvHash(map[string]string{
		"CLAUDE_CODE_ENTRYPOINT": "cli",
		"WT_SESSION":             "session-1",
	})
	if allTransient != "00000000" {
		t.Errorf("all-transient env hash = %q, want %q (no semantic content)",
			allTransient, "00000000")
	}
}

// TestDeriveEnvBucketedSid_NoExistingEntry confirms the helper returns the
// base sid unchanged when d.owners has no entry for the base.
func TestDeriveEnvBucketedSid_NoExistingEntry(t *testing.T) {
	d := testDaemon(t)
	base := "00000000aaaaaaaa"
	got := d.deriveEnvBucketedSid(base, map[string]string{"GITHUB_TOKEN": "x"})
	if got != base {
		t.Errorf("no existing entry: got %q, want %q", got, base)
	}
}
