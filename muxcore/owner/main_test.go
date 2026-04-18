package owner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// mockServerBin is the path to the pre-compiled mock_server binary.
// Set by TestMain before any test runs. Falls back to "go run" if build fails.
var mockServerBin string

// mockServerArgs returns the command + args for running the mock server.
// Uses pre-built binary when available (eliminates per-test compile time on Ubuntu CI).
func mockServerArgs() (command string, args []string) {
	if mockServerBin != "" {
		return mockServerBin, nil
	}
	// Fallback: go run (slower, compiles each time)
	return "go", []string{"run", "../../testdata/mock_server.go"}
}

// TestMain pre-builds the mock_server binary once so every test in this package
// can exec it directly, avoiding the per-test "go run" compile latency.
//
// On Ubuntu CI, "go run" cold-starts (compile + start) take 20-30s per test
// because the Go build cache is cold and multiple tests run in parallel. This
// consistently triggers the 30s readResp timeout in TestOwnerMultipleSessions.
//
// Pre-building once amortises the compile cost across all tests and reduces
// per-exec startup from ~20s to <100ms.
func TestMain(m *testing.M) {
	// mock_server.go lives in <repo_root>/testdata/ — two levels up from
	// muxcore/owner/ (the test working directory). We set cmd.Dir to that
	// directory so "go build" resolves in the root module context, not the
	// muxcore sub-module, avoiding cross-module file-path errors.
	testdataDir := filepath.Join("..", "..", "testdata")
	testdataDirAbs, absErr := filepath.Abs(testdataDir)
	if absErr != nil {
		testdataDirAbs = testdataDir // best-effort fallback
	}

	// Build into a temp directory (separate from any package cache).
	tmpDir, err := os.MkdirTemp("", "mcp-mux-mock-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[TestMain] warning: cannot create temp dir: %v — falling back to go run\n", err)
		os.Exit(m.Run())
		return
	}
	defer os.RemoveAll(tmpDir)

	binPath := filepath.Join(tmpDir, "mock_server")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}

	// Run "go build -o <binPath> mock_server.go" from the testdata directory.
	// This keeps the build in the root module context (where mock_server.go lives)
	// rather than the muxcore sub-module, avoiding cross-module file-path issues.
	cmd := exec.Command("go", "build", "-o", binPath, "mock_server.go")
	cmd.Dir = testdataDirAbs
	cmd.Stdout = os.Stderr // route build output to stderr so it appears in test logs
	cmd.Stderr = os.Stderr
	if buildErr := cmd.Run(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "[TestMain] warning: mock_server build failed: %v — falling back to go run\n", buildErr)
		// Do not set mockServerBin; tests will use go run fallback.
	} else {
		mockServerBin = binPath
		fmt.Fprintf(os.Stderr, "[TestMain] mock_server pre-built at %s\n", binPath)
	}

	os.Exit(m.Run())
}
