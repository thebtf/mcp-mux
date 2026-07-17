package envidentity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
)

const fingerprintVersion = "mcp-mux-env-v1"

// Identity is a versioned full SHA-256 digest. Raw identity values remain only
// in the caller's effective launch environment or OwnerSnapshot.Env.
type Identity struct {
	Fingerprint string
}

// Build creates a deterministic, length-delimited digest from every
// security-relevant environment field without retaining a second raw copy.
func Build(env map[string]string) Identity {
	keys := identityKeys(env)
	h := sha256.New()
	writeField(h, fingerprintVersion)
	for _, key := range keys {
		writeField(h, key)
		writeField(h, env[key])
	}
	return Identity{Fingerprint: "v1:" + hex.EncodeToString(h.Sum(nil))}
}

func identityKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		if IsTransient(key) || !IsIdentityKey(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Equal uses the digest as a fast path, then fail-closes against an exact
// projection comparison computed from the two existing environment maps.
func Equal(a, b Identity, aEnv, bEnv map[string]string) bool {
	if a.Fingerprint != b.Fingerprint {
		return false
	}
	for key, value := range aEnv {
		if IsTransient(key) || !IsIdentityKey(key) {
			continue
		}
		if other, ok := bEnv[key]; !ok || other != value {
			return false
		}
	}
	for key, value := range bEnv {
		if IsTransient(key) || !IsIdentityKey(key) {
			continue
		}
		if other, ok := aEnv[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// Compatible preserves mcp-mux's global-owner compatibility rule: credential
// presence must match on both sides; optional non-secret identity keys conflict
// only when both sides provide different values.
func Compatible(a, b map[string]string) bool {
	for key, value := range a {
		if IsTransient(key) || !IsIdentityKey(key) {
			continue
		}
		other, ok := b[key]
		if !ok {
			if IsCredentialKey(key) {
				return false
			}
			continue
		}
		if value != other {
			return false
		}
	}
	for key := range b {
		if IsTransient(key) || !IsIdentityKey(key) {
			continue
		}
		if _, ok := a[key]; !ok && IsCredentialKey(key) {
			return false
		}
	}
	return true
}

func ShortHash(env map[string]string) string {
	if len(identityKeys(env)) == 0 {
		return "00000000"
	}
	identity := Build(env)
	return identity.Fingerprint[len("v1:") : len("v1:")+8]
}

func writeField(h interface{ Write([]byte) (int, error) }, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}

// IsCredentialKey identifies variables whose presence asymmetry must split
// owners because they likely carry authentication material.
func IsCredentialKey(key string) bool {
	upper := strings.ToUpper(key)
	switch {
	case upper == "TOKEN" || upper == "KEY" || upper == "API_KEY":
		return true
	case upper == "SECRET" || upper == "PASSWORD" || upper == "PASSWD":
		return true
	case upper == "CREDENTIAL" || upper == "CREDENTIALS" || upper == "AUTH":
		return true
	case strings.HasSuffix(upper, "_TOKEN"):
		return true
	case strings.HasSuffix(upper, "_KEY"):
		return true
	case strings.HasSuffix(upper, "_API_KEY"):
		return true
	case strings.HasSuffix(upper, "_SECRET"):
		return true
	case strings.HasSuffix(upper, "_PASSWORD"):
		return true
	case strings.HasSuffix(upper, "_PASSWD"):
		return true
	case strings.HasSuffix(upper, "_CREDENTIALS"):
		return true
	case strings.HasSuffix(upper, "_AUTH"):
		return true
	case upper == "GH_TOKEN" || upper == "GITHUB_TOKEN":
		return true
	case upper == "GITHUB_PERSONAL_ACCESS_TOKEN":
		return true
	case upper == "OPENAI_API_KEY" || upper == "ANTHROPIC_API_KEY":
		return true
	case upper == "TAVILY_API_KEY" || upper == "GOOGLE_API_KEY":
		return true
	case upper == "AWS_ACCESS_KEY_ID" || upper == "AWS_SECRET_ACCESS_KEY":
		return true
	case upper == "AWS_SESSION_TOKEN":
		return true
	case upper == "SSH_AUTH_SOCK" || upper == "SSH_AGENT_PID":
		return true
	case upper == "DOCKER_AUTH_CONFIG":
		return true
	}
	return false
}

// IsIdentityKey identifies configuration, endpoint, proxy, certificate, and
// credential variables that partition an upstream process identity.
func IsIdentityKey(key string) bool {
	if IsCredentialKey(key) {
		return true
	}
	upper := strings.ToUpper(key)
	switch {
	case strings.HasSuffix(upper, "_CONFIG"):
		return true
	case strings.HasSuffix(upper, "_CONFIG_FILE"):
		return true
	case strings.HasSuffix(upper, "_CONFIG_PATH"):
		return true
	case strings.HasSuffix(upper, "_CONFIG_DIR"):
		return true
	case strings.HasSuffix(upper, "_ENDPOINT"):
		return true
	case strings.HasSuffix(upper, "_BASE_URL"):
		return true
	case strings.HasSuffix(upper, "_URL"):
		return true
	case strings.HasSuffix(upper, "_HOST"):
		return true
	case strings.HasSuffix(upper, "_PORT"):
		return true
	case strings.HasSuffix(upper, "_REGION"):
		return true
	case strings.HasSuffix(upper, "_PROFILE"):
		return true
	case strings.HasSuffix(upper, "_CERT_PATH") || strings.HasSuffix(upper, "_CERT_FILE"):
		return true
	case upper == "CONFIG" || upper == "CONFIG_FILE" || upper == "CONFIG_PATH" || upper == "CONFIG_DIR":
		return true
	case upper == "HOST" || upper == "PORT" || upper == "URL" || upper == "BASE_URL" || upper == "ENDPOINT":
		return true
	case upper == "REGION" || upper == "PROFILE":
		return true
	case upper == "KUBECONFIG" || upper == "NODE_EXTRA_CA_CERTS" || upper == "CERT_PATH" || upper == "CERT_FILE":
		return true
	case upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY":
		return true
	case strings.HasPrefix(upper, "NPM_CONFIG_"):
		return true
	case upper == "SSL_CERT_FILE" || upper == "SSL_CERT_DIR":
		return true
	case upper == "REQUESTS_CA_BUNDLE" || upper == "CURL_CA_BUNDLE":
		return true
	}
	return false
}

// IsTransient identifies host/session launch noise that must not fragment
// otherwise compatible owners.
func IsTransient(key string) bool {
	switch {
	case strings.HasPrefix(key, "CLAUDE_CODE_"):
		return true
	case strings.HasPrefix(key, "CLAUDE_AUTO"):
		return true
	case key == "CLAUDE_CODE_ENTRYPOINT":
		return true
	case strings.HasPrefix(key, "WT_"):
		return true
	case key == "SESSIONNAME" || key == "WSLENV":
		return true
	case key == "PWD" || key == "OLDPWD" || key == "INIT_CWD":
		return true
	case strings.HasPrefix(key, "npm_lifecycle_"):
		return true
	case strings.HasPrefix(key, "npm_package_"):
		return true
	case key == "npm_execpath" || key == "npm_node_execpath" || key == "npm_command":
		return true
	case key == "TERM" || key == "TERM_PROGRAM" || key == "TERM_PROGRAM_VERSION":
		return true
	case key == "COLORTERM" || key == "LINES" || key == "COLUMNS":
		return true
	case key == "_" || key == "SHLVL" || key == "SHELL":
		return true
	case key == "SSH_CLIENT" || key == "SSH_CONNECTION" || key == "SSH_TTY":
		return true
	case key == "DISPLAY" || key == "XAUTHORITY" || key == "WAYLAND_DISPLAY":
		return true
	}
	return false
}
