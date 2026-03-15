// Package serverid computes deterministic server identities from command + args + context.
//
// Two mcp-mux instances wrapping the same command with the same context will compute the same server ID,
// allowing them to find each other via IPC.
package serverid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/google/uuid"
)

type SharingMode string

const (
	ModeCwd      SharingMode = "cwd"
	ModeGit      SharingMode = "git"      // requires traversing up to find .git
	ModeGlobal   SharingMode = "global"   // ignores cwd entirely
	ModeIsolated SharingMode = "isolated" // never shares
	MuxVersion               = "v1.0.0"
)

// CanonicalizePath aggressively normalizes a path for consistent hashing
func CanonicalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p // Fallback
	}
	eval, err := filepath.EvalSymlinks(abs)
	if err == nil {
		abs = eval
	}
	clean := filepath.Clean(abs)

	// Windows paths are case-insensitive. Normalize to lower for hash stability.
	if runtime.GOOS == "windows" {
		clean = strings.ToLower(clean)
	}
	return clean
}

// findGitRoot recursively searches upwards for a .git directory
func findGitRoot(startPath string) string {
	current := startPath
	for {
		if info, err := os.Stat(filepath.Join(current, ".git")); err == nil && info.IsDir() {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached root, fallback to original path
			return startPath
		}
		current = parent
	}
}

// Compute (deprecated) is kept for backward compatibility if needed temporarily.
// Use GenerateContextKey instead.
func Compute(args []string) string {
	if len(args) == 0 {
		return GenerateContextKey(ModeCwd, "", nil, nil, ".")
	}
	return GenerateContextKey(ModeCwd, args[0], args[1:], nil, ".")
}

// GenerateContextKey creates the deterministic ID for the upstream server
func GenerateContextKey(mode SharingMode, cmd string, args, env []string, rawCwd string) string {
	if mode == ModeIsolated {
		// Return a random/unique string to ensure no sharing
		return "isolated-" + uuid.New().String()[:16]
	}

	hash := sha256.New()
	hash.Write([]byte(MuxVersion))
	hash.Write([]byte{0})
	hash.Write([]byte(cmd))
	for _, arg := range args {
		hash.Write([]byte{0})
		hash.Write([]byte(arg))
	}

	// 1. Determine Scope Path based on mode
	scopePath := ""
	switch mode {
	case ModeCwd:
		scopePath = CanonicalizePath(rawCwd)
	case ModeGit:
		scopePath = findGitRoot(CanonicalizePath(rawCwd))
	case ModeGlobal:
		scopePath = "global"
	}
	hash.Write([]byte{0})
	hash.Write([]byte(scopePath))

	// 2. Hash deterministic environment fingerprint
	// Only hash explicitly provided env vars, not the whole os.Environ()
	if len(env) > 0 {
		envCopy := make([]string, len(env))
		copy(envCopy, env)
		sort.Strings(envCopy)
		for _, e := range envCopy {
			hash.Write([]byte{0})
			hash.Write([]byte(e))
		}
	}

	return hex.EncodeToString(hash.Sum(nil))[:16]
}

// IPCPath returns the platform-specific IPC endpoint path for a given server ID.
func IPCPath(id string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.sock", id))
}

// LockPath returns the lock file path used for owner election.
func LockPath(id string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.lock", id))
}

// DescribeArgs returns a human-readable summary of the command + args
func DescribeArgs(args []string) string {
	if len(args) == 0 {
		return "(empty)"
	}
	return strings.Join(args, " ")
}
