//go:build !windows

package attest

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRandomEndpointFitsDarwinPathCeiling is the regression guard for
// https://github.com/thebtf/mcp-mux/pull/145 CI run 29645121822.
func TestRandomEndpointFitsDarwinPathCeiling(t *testing.T) {
	// Reproduce the exact 48-byte macOS CI temp directory from run 29645121822.
	darwinRunnerTempDir := "/var/folders/8j/sfr9qqcj73j4p6nhwcfpr0th0000gn/T"
	if len(darwinRunnerTempDir) != 48 {
		t.Fatalf("test setup: expected 48-byte dir, got %d", len(darwinRunnerTempDir))
	}

	endpoint, err := randomEndpointIn(darwinRunnerTempDir)
	if err != nil {
		t.Fatalf("randomEndpointIn failed for Darwin runner temp dir: %v", err)
	}

	const usable = unixPathMax - 1
	if got := len(endpoint); got >= unixPathMax {
		t.Errorf("endpoint length %d >= unixPathMax %d (would fail bind on Darwin): %q", got, unixPathMax, endpoint)
	} else {
		t.Logf("endpoint length %d / %d usable: %q", got, usable, endpoint)
	}
}

func TestRandomEndpointRetainsAtLeast64RandomBits(t *testing.T) {
	longestDir := strings.Repeat("x", unixPathMax-1-1-attestBasenameOverhead-minHexLen)
	endpoint, err := randomEndpointIn(longestDir)
	if err != nil {
		t.Fatalf("randomEndpointIn at 64-bit boundary: %v", err)
	}
	if got := len(endpoint); got >= unixPathMax {
		t.Fatalf("endpoint length %d >= unixPathMax %d", got, unixPathMax)
	}
	base := filepath.Base(endpoint)
	randomHex := strings.TrimSuffix(strings.TrimPrefix(base, "mcp-supervisor-attest-"), ".sock")
	if len(randomHex) != minHexLen {
		t.Fatalf("random suffix length = %d, want at least %d hex chars", len(randomHex), minHexLen)
	}
}

// TestRandomEndpointPreFixWouldExceedDarwinCeiling confirms that the pre-fix
// 16-byte random suffix exceeded Darwin's sockaddr storage on the CI temp path.
func TestRandomEndpointPreFixWouldExceedDarwinCeiling(t *testing.T) {
	darwinRunnerTempDir := "/var/folders/8j/sfr9qqcj73j4p6nhwcfpr0th0000gn/T"
	preFix32HexChars := "00000000000000000000000000000000"
	prePath := darwinRunnerTempDir + "/" + "mcp-supervisor-attest-" + preFix32HexChars + ".sock"
	if got := len(prePath); got < unixPathMax {
		t.Errorf("pre-fix path length %d < unixPathMax %d; test assumption wrong", got, unixPathMax)
	}
}
