package envidentity

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBuildUsesFullVersionedExactDigest(t *testing.T) {
	aEnv := map[string]string{"GITHUB_TOKEN": "ab", "CONFIG_PATH": "c", "PWD": "/session/a"}
	bEnv := map[string]string{"GITHUB_TOKEN": "a", "CONFIG_PATH": "bc", "PWD": "/session/b"}
	a := Build(aEnv)
	b := Build(bEnv)
	if a.Fingerprint == b.Fingerprint {
		t.Fatalf("length-distinct identities collided: %q", a.Fingerprint)
	}
	if len(a.Fingerprint) != len("v1:")+64 {
		t.Fatalf("fingerprint length=%d, want %d", len(a.Fingerprint), len("v1:")+64)
	}
	noiseOnly := Build(map[string]string{"GITHUB_TOKEN": "ab", "CONFIG_PATH": "c", "PWD": "/session/b"})
	if a.Fingerprint != noiseOnly.Fingerprint {
		t.Fatal("transient PWD changed identity digest")
	}
}

func TestIdentityRetainsNoRawProjection(t *testing.T) {
	const secret = "raw-credential-value"
	identity := Build(map[string]string{"GITHUB_TOKEN": secret, "CONFIG_PATH": "/config"})
	typeInfo := reflect.TypeOf(identity)
	if typeInfo.NumField() != 1 || typeInfo.Field(0).Name != "Fingerprint" {
		t.Fatalf("Identity exposes unexpected retained fields: %#v", typeInfo)
	}
	if strings.Contains(fmt.Sprintf("%+v", identity), secret) {
		t.Fatal("Identity retained a raw credential value")
	}
}

func TestEqualFailsClosedOnDigestCollision(t *testing.T) {
	aEnv := map[string]string{"GITHUB_TOKEN": "secret", "CONFIG_PATH": "/a"}
	bEnv := map[string]string{"GITHUB_TOKEN": "secret", "CONFIG_PATH": "/a"}
	a := Build(aEnv)
	b := Build(bEnv)
	if !Equal(a, b, aEnv, bEnv) {
		t.Fatal("identical identities were incompatible")
	}
	// Simulate a digest collision: exact on-demand projection comparison must
	// still reject different existing launch environments.
	bEnv["CONFIG_PATH"] = "/b"
	if Equal(a, a, aEnv, bEnv) {
		t.Fatal("exact projection mismatch passed identical-digest fast path")
	}
}

func TestCompatiblePreservesCredentialPresenceBoundary(t *testing.T) {
	if Compatible(map[string]string{"GITHUB_TOKEN": "secret"}, nil) {
		t.Fatal("credential presence mismatch was compatible")
	}
	if !Compatible(map[string]string{"CONFIG_PATH": "/a"}, nil) {
		t.Fatal("optional non-secret presence should remain compatible for owner dedup")
	}
}

func TestDigestCoversMixedSecurityRelevantKeysAndExcludesLaunchNoise(t *testing.T) {
	base := map[string]string{
		"GITHUB_TOKEN":        "token",
		"SERVICE_CONFIG_PATH": "/cfg/service.json",
		"HTTPS_PROXY":         "https://proxy.example",
		"NODE_EXTRA_CA_CERTS": "/certs/ca.pem",
		"SERVICE_ENDPOINT":    "https://api.example",
		"NPM_CONFIG_REGISTRY": "https://registry.example",
		"PWD":                 "/project/a",
	}
	baseIdentity := Build(base)
	for _, key := range []string{"GITHUB_TOKEN", "SERVICE_CONFIG_PATH", "HTTPS_PROXY", "NODE_EXTRA_CA_CERTS", "SERVICE_ENDPOINT", "NPM_CONFIG_REGISTRY"} {
		changed := make(map[string]string, len(base))
		for k, value := range base {
			changed[k] = value
		}
		changed[key] += "-changed"
		if Build(changed).Fingerprint == baseIdentity.Fingerprint {
			t.Fatalf("identity digest ignored %s", key)
		}
	}
	noise := make(map[string]string, len(base)+2)
	for key, value := range base {
		noise[key] = value
	}
	noise["PWD"] = "/project/b"
	noise["CLAUDE_CODE_SESSION"] = "session-b"
	if Build(noise).Fingerprint != baseIdentity.Fingerprint {
		t.Fatal("launch noise changed identity digest")
	}
}

func TestCompatibleRejectsChangedSecurityValuesButIgnoresTransientNoise(t *testing.T) {
	base := map[string]string{"GITHUB_TOKEN": "token-a", "SERVICE_CONFIG_PATH": "/cfg/a", "PWD": "/project/a"}
	noiseOnly := map[string]string{"GITHUB_TOKEN": "token-a", "SERVICE_CONFIG_PATH": "/cfg/a", "PWD": "/project/b", "CLAUDE_CODE_SESSION": "other-session"}
	if !Compatible(base, noiseOnly) {
		t.Fatal("transient launch noise split a compatible environment")
	}
	if Compatible(base, map[string]string{"GITHUB_TOKEN": "token-b", "SERVICE_CONFIG_PATH": "/cfg/a"}) {
		t.Fatal("changed credential remained compatible")
	}
	if Compatible(base, map[string]string{"GITHUB_TOKEN": "token-a", "SERVICE_CONFIG_PATH": "/cfg/b"}) {
		t.Fatal("changed configuration remained compatible")
	}
}
