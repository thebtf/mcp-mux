package daemon

import (
	"strings"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
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
	if len(sid1) < 16 {
		t.Fatalf("sid1 too short to be a valid global sid: %q (want ≥16 hex chars)", sid1)
	}
	if !strings.HasPrefix(sid2, sid1[:16]+"-env-") {
		// sid2 should be `{baseSid}-env-{8hex}`. sid1 is the base global sid
		// (16 hex chars) and its first 16 chars form the base prefix.
		t.Fatalf("unexpected env-bucket sid format: sid1=%s sid2=%s — sid2 must start with sid1[:16]+\"-env-\"", sid1, sid2)
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

// TestGlobalFirst_NonIdentityEnvNoiseSharedSingleOwner proves the process
// explosion guard: global/shared owners must not split merely because MCP host
// sessions carry different launch noise. Credential/config keys still split in
// the tests above; this test covers PATH/temp/shell/session-style vars that
// should not create one owner per host session.
func TestGlobalFirst_NonIdentityEnvNoiseSharedSingleOwner(t *testing.T) {
	cwd1 := t.TempDir()
	cwd2 := t.TempDir()
	d := testDaemon(t)
	mockServer := mockServerAbsPath(t)
	tempA := t.TempDir()
	tempB := t.TempDir()
	homeA := t.TempDir()
	homeB := t.TempDir()

	env1 := map[string]string{
		"PATH":            "/tools/a",
		"TEMP":            tempA,
		"TMP":             tempA,
		"USERPROFILE":     homeA,
		"NVMD_SESSION_ID": "session-a",
	}
	env2 := map[string]string{
		"PATH":            "/tools/b",
		"TEMP":            tempB,
		"TMP":             tempB,
		"USERPROFILE":     homeB,
		"NVMD_SESSION_ID": "session-b",
	}

	_, sid1, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd1,
		Env:     env1,
	})
	if err != nil {
		t.Fatalf("Spawn 1: %v", err)
	}

	_, sid2, _, err := d.Spawn(control.Request{
		Cmd:     "spawn",
		Command: "go",
		Args:    []string{"run", mockServer},
		Cwd:     cwd2,
		Env:     env2,
	})
	if err != nil {
		t.Fatalf("Spawn 2: %v", err)
	}

	if sid1 != sid2 {
		t.Errorf("non-identity env noise produced distinct sids %q vs %q", sid1, sid2)
	}
	if got := d.OwnerCount(); got != 1 {
		t.Errorf("OwnerCount = %d, want 1 when only env noise differs", got)
	}
}

// TestSemanticEnvHash_Deterministic asserts that the helper produces
// identical hashes for identical inputs and different hashes for
// different inputs on a semantically significant key.
func TestSemanticEnvHash_Deterministic(t *testing.T) {
	a := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "abc", "MCP_SERVER_CONFIG": "/cfg/a.json"})
	b := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "abc", "MCP_SERVER_CONFIG": "/cfg/a.json"})
	if a != b {
		t.Errorf("identical inputs produced different hashes: %q vs %q", a, b)
	}

	c := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "xyz", "MCP_SERVER_CONFIG": "/cfg/a.json"})
	if a == c {
		t.Errorf("different GITHUB_TOKEN produced identical hash: %q", a)
	}

	d := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "abc", "MCP_SERVER_CONFIG": "/cfg/b.json"})
	if a == d {
		t.Errorf("different config path produced identical hash: %q", a)
	}
}

// TestSemanticEnvHash_NPMConfigIdentityKeys asserts that npm config settings
// remain identity keys while lifecycle/package metadata is
// still ignored. This preserves the credential/config boundary without
// restoring broad npm launch-noise fanout.
func TestSemanticEnvHash_NPMConfigIdentityKeys(t *testing.T) {
	registryA := semanticEnvHash(map[string]string{
		"npm_config_registry":        "https://registry-a.example/",
		"npm_lifecycle_event":        "start",
		"npm_package_json":           "D:/repo-a/package.json",
		"npm_package_integrity_hash": "session-a",
	})
	registryB := semanticEnvHash(map[string]string{
		"npm_config_registry":        "https://registry-b.example/",
		"npm_lifecycle_event":        "start",
		"npm_package_json":           "D:/repo-b/package.json",
		"npm_package_integrity_hash": "session-b",
	})
	if registryA == registryB {
		t.Fatalf("different npm registry values produced identical hash %q", registryA)
	}

	strictSSLFalse := semanticEnvHash(map[string]string{
		"npm_config_registry":   "https://registry-a.example/",
		"npm_config_strict_ssl": "false",
	})
	if strictSSLFalse == registryA {
		t.Fatalf("different npm strict_ssl values produced identical hash %q", strictSSLFalse)
	}

	withNoise := semanticEnvHash(map[string]string{
		"npm_config_registry":        "https://registry-a.example/",
		"npm_lifecycle_event":        "test",
		"npm_package_json":           "D:/repo-c/package.json",
		"npm_package_integrity_hash": "session-c",
	})
	if withNoise != registryA {
		t.Fatalf("npm launch/package noise changed registry identity hash: got %q want %q", withNoise, registryA)
	}
}

// TestSemanticEnvHash_ConfigExactAndCertIdentityKeys covers common
// configuration keys that do not fit the underscore-suffix pattern but still
// change the upstream's process identity.
func TestSemanticEnvHash_ConfigExactAndCertIdentityKeys(t *testing.T) {
	cases := []struct {
		name string
		a    map[string]string
		b    map[string]string
	}{
		{
			name: "exact-port",
			a:    map[string]string{"PORT": "3000"},
			b:    map[string]string{"PORT": "3001"},
		},
		{
			name: "exact-host",
			a:    map[string]string{"HOST": "127.0.0.1"},
			b:    map[string]string{"HOST": "0.0.0.0"},
		},
		{
			name: "exact-config",
			a:    map[string]string{"CONFIG": "D:/configs/a.json"},
			b:    map[string]string{"CONFIG": "D:/configs/b.json"},
		},
		{
			name: "exact-region",
			a:    map[string]string{"REGION": "us-east-1"},
			b:    map[string]string{"REGION": "eu-west-1"},
		},
		{
			name: "exact-profile",
			a:    map[string]string{"PROFILE": "dev"},
			b:    map[string]string{"PROFILE": "prod"},
		},
		{
			name: "suffixed-port",
			a:    map[string]string{"MCP_SERVER_PORT": "8811"},
			b:    map[string]string{"MCP_SERVER_PORT": "8812"},
		},
		{
			name: "kubeconfig",
			a:    map[string]string{"KUBECONFIG": "D:/clusters/dev.yaml"},
			b:    map[string]string{"KUBECONFIG": "D:/clusters/prod.yaml"},
		},
		{
			name: "docker-cert-path",
			a:    map[string]string{"DOCKER_CERT_PATH": "D:/certs/a"},
			b:    map[string]string{"DOCKER_CERT_PATH": "D:/certs/b"},
		},
		{
			name: "node-extra-ca-certs",
			a:    map[string]string{"NODE_EXTRA_CA_CERTS": "D:/ca/a.pem"},
			b:    map[string]string{"NODE_EXTRA_CA_CERTS": "D:/ca/b.pem"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hashA := semanticEnvHash(tc.a)
			hashB := semanticEnvHash(tc.b)
			if hashA == hashB {
				t.Fatalf("different %s env values produced identical hash %q", tc.name, hashA)
			}
		})
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

// TestSemanticEnvHash_IgnoresNonIdentityNoise asserts that common process
// launch noise does not participate in the env bucket suffix. This keeps
// shared/global owners from multiplying across MCP host sessions that differ
// only by shell, temp, path, or local session metadata.
func TestSemanticEnvHash_IgnoresNonIdentityNoise(t *testing.T) {
	baseline := semanticEnvHash(map[string]string{"GITHUB_TOKEN": "abc"})
	noisy := semanticEnvHash(map[string]string{
		"GITHUB_TOKEN":    "abc",
		"PATH":            "/tools/a",
		"TEMP":            "/tmp/a",
		"TMP":             "/tmp/a",
		"USERPROFILE":     "/users/a",
		"NVMD_SESSION_ID": "session-a",
	})
	if noisy != baseline {
		t.Errorf("non-identity env noise leaked into hash: got %q, want %q", noisy, baseline)
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

func TestReusedOwnerPreRegisterKeepsEffectiveEnv(t *testing.T) {
	const inheritedKey = "MCPMUX_REUSE_CONFIG_PATH"
	t.Setenv(inheritedKey, "daemon-config")

	d := testDaemon(t)
	command := "reuse-effective-env"
	snap := daemonMaterializationSnapshot(false)
	d.updateTemplate(command, nil, snap)
	cwd := t.TempDir()

	_, sid, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwd})
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	_, reusedSID, token, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwd})
	if err != nil {
		t.Fatalf("reused Spawn: %v", err)
	}
	if reusedSID != sid {
		t.Fatalf("reused Spawn sid = %q, want %q", reusedSID, sid)
	}

	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatalf("owner %q missing after reuse", sid)
	}
	session := &owner.Session{ID: 991}
	entry.Owner.SessionMgr().RegisterSession(session, "")
	if !entry.Owner.SessionMgr().Bind(token, sid, session) {
		t.Fatal("reused token did not bind")
	}
	if got := session.Env[inheritedKey]; got != "daemon-config" {
		t.Fatalf("reused session effective env %s = %q, want daemon-config", inheritedKey, got)
	}
}

// TestSemanticEnvHash_IgnoresCwdDerivedVars proves the Codex PR #121 follow-up
// finding is fixed: cwd-derived env vars (PWD, OLDPWD, INIT_CWD, npm_*) MUST
// NOT fragment env-bucket identity across per-session working directories.
//
// Spec invariant (Engram #244 Bug 1 + AC1 + AC8): global-first dedup means
// two sessions with identical credentials but different cwds land on ONE
// shared owner. Without this fix, semanticEnvHash would hash PWD into the
// bucket suffix, two cwds → two suffixes → two owners → per-cwd fan-out
// recreated.
//
// I am running semanticEnvHash on a baseline credentials env, then on the
// same env enriched with a different PWD/OLDPWD/INIT_CWD/npm_lifecycle_event
// each. All four enriched hashes must equal the baseline.
func TestSemanticEnvHash_IgnoresCwdDerivedVars(t *testing.T) {
	baseline := semanticEnvHash(map[string]string{
		"GITHUB_TOKEN": "abc",
	})
	cases := []struct {
		name string
		add  map[string]string
	}{
		{"pwd", map[string]string{"PWD": "/dev/app-1"}},
		{"oldpwd", map[string]string{"OLDPWD": "/dev/prev"}},
		{"init_cwd", map[string]string{"INIT_CWD": "/dev/init"}},
		{"npm_lifecycle_event", map[string]string{"npm_lifecycle_event": "start"}},
		{"npm_package_name", map[string]string{"npm_package_name": "my-pkg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GITHUB_TOKEN": "abc"}
			for k, v := range tc.add {
				env[k] = v
			}
			got := semanticEnvHash(env)
			if got != baseline {
				t.Errorf("cwd/npm-derived var %s leaked into hash: got %q, want baseline %q (envTransient must filter)",
					tc.name, got, baseline)
			}
		})
	}
}

// TestSemanticEnvHash_IgnoresShellDerivedVars proves the same invariant for
// terminal / shell / SSH / display vars that vary per shim launch but do not
// affect upstream MCP server identity.
func TestSemanticEnvHash_IgnoresShellDerivedVars(t *testing.T) {
	baseline := semanticEnvHash(map[string]string{
		"GITHUB_TOKEN": "abc",
	})
	cases := []struct {
		name string
		add  map[string]string
	}{
		{"term", map[string]string{"TERM": "xterm-256color"}},
		{"term_program", map[string]string{"TERM_PROGRAM": "vscode"}},
		{"colorterm", map[string]string{"COLORTERM": "truecolor"}},
		{"shlvl", map[string]string{"SHLVL": "3"}},
		{"shell", map[string]string{"SHELL": "/bin/zsh"}},
		{"ssh_session", map[string]string{"SSH_CLIENT": "1.2.3.4 12345 22"}},
		{"display", map[string]string{"DISPLAY": ":0"}},
		{"wayland", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GITHUB_TOKEN": "abc"}
			for k, v := range tc.add {
				env[k] = v
			}
			got := semanticEnvHash(env)
			if got != baseline {
				t.Errorf("shell/terminal var %s leaked into hash: got %q, want baseline %q",
					tc.name, got, baseline)
			}
		})
	}
}
