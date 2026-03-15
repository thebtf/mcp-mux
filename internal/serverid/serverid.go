// Package serverid computes deterministic server identities from command + args.
//
// Two mcp-mux instances wrapping the same command will compute the same server ID,
// allowing them to find each other via IPC.
package serverid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Compute returns a 16-char hex hash identifying a server by its command + args.
// Uses null byte separator between args to prevent collisions
// (e.g., ["a", "bc"] vs ["ab", "c"]).
func Compute(args []string) string {
	h := sha256.New()
	for i, arg := range args {
		if i > 0 {
			h.Write([]byte{0}) // null separator
		}
		h.Write([]byte(arg))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// IPCPath returns the platform-specific IPC endpoint path for a given server ID.
func IPCPath(id string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`\\.\pipe\mcp-mux-%s`, id)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.sock", id))
}

// LockPath returns the lock file path used for owner election.
func LockPath(id string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("mcp-mux-%s.lock", id))
}

// DescribeArgs returns a human-readable summary of the command + args
// for display in status output.
func DescribeArgs(args []string) string {
	if len(args) == 0 {
		return "(empty)"
	}
	return strings.Join(args, " ")
}
