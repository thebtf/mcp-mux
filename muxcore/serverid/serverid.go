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

// findGitRoot recursively searches upwards for a .git directory or file.
// In a regular checkout, .git is a directory. In a git worktree, .git is a
// file containing "gitdir: <path>". Both indicate a git repository root.
func findGitRoot(startPath string) string {
	current := startPath
	for {
		gitPath := filepath.Join(current, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			// .git exists — either directory (normal) or file (worktree).
			// In both cases, this is the repo/worktree root.
			return resolveWorktreeRoot(current, gitPath)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return startPath
		}
		current = parent
	}
}

// resolveWorktreeRoot resolves a worktree .git file to the main repo root.
// For a regular .git directory, returns dir unchanged.
// For a worktree .git file (contains "gitdir: ..."), follows the pointer
// back to the main repo so all worktrees share the same server ID.
func resolveWorktreeRoot(dir, gitPath string) string {
	info, err := os.Stat(gitPath)
	if err != nil || info.IsDir() {
		return dir // regular .git directory
	}
	// .git file — read "gitdir: <path>" pointer
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return dir
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir: ") {
		return dir
	}
	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	// gitdir points to .git/worktrees/<name> — go up two levels to get repo root
	// e.g., /repo/.git/worktrees/my-branch → /repo
	repoGitDir := filepath.Dir(filepath.Dir(gitdir))
	repoRoot := filepath.Dir(repoGitDir)
	// Verify it's actually a git directory
	if info, err := os.Stat(repoGitDir); err == nil && info.IsDir() {
		return CanonicalizePath(repoRoot)
	}
	return dir
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

// resolveBaseDir returns baseDir if non-empty, otherwise os.TempDir().
func resolveBaseDir(baseDir string) string {
	if baseDir == "" {
		return os.TempDir()
	}
	return baseDir
}

// IPCPath returns the platform-specific IPC endpoint path for a given server ID.
// When baseDir is empty, os.TempDir() is used.
func IPCPath(id string, baseDir string) string {
	return filepath.Join(resolveBaseDir(baseDir), fmt.Sprintf("mcp-mux-%s.sock", id))
}

// ControlPath returns the control socket path for a given server ID.
// When baseDir is empty, os.TempDir() is used.
func ControlPath(id string, baseDir string) string {
	return filepath.Join(resolveBaseDir(baseDir), fmt.Sprintf("mcp-mux-%s.ctl.sock", id))
}

// LockPath returns the lock file path used for owner election.
// When baseDir is empty, os.TempDir() is used.
func LockPath(id string, baseDir string) string {
	return filepath.Join(resolveBaseDir(baseDir), fmt.Sprintf("mcp-mux-%s.lock", id))
}

// DaemonControlPath returns the control socket path for a named daemon.
// The name parameter identifies the engine instance (e.g. "mcp-mux", "aimux").
// When name is empty, defaults to "mcp-mux" for backward compatibility.
// When baseDir is empty, os.TempDir() is used.
func DaemonControlPath(baseDir, name string) string {
	if name == "" {
		name = "mcp-mux"
	}
	return filepath.Join(resolveBaseDir(baseDir), fmt.Sprintf("%s-muxd.ctl.sock", name))
}

// DaemonLockPath returns the lock file path for daemon startup coordination.
// The name parameter identifies the engine instance (e.g. "mcp-mux", "aimux").
// When name is empty, defaults to "mcp-mux" for backward compatibility.
// When baseDir is empty, os.TempDir() is used.
func DaemonLockPath(baseDir, name string) string {
	if name == "" {
		name = "mcp-mux"
	}
	return filepath.Join(resolveBaseDir(baseDir), fmt.Sprintf("%s-muxd.lock", name))
}

// DescribeArgs returns a human-readable summary of the command + args
func DescribeArgs(args []string) string {
	if len(args) == 0 {
		return "(empty)"
	}
	return strings.Join(args, " ")
}
