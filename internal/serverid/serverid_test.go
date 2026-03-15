package serverid

import (
	"strings"
	"testing"
)

func TestGenerateContextKeyDeterministic(t *testing.T) {
	id1 := GenerateContextKey(ModeCwd, "node", []string{"server.js"}, []string{}, "/dev/app")
	id2 := GenerateContextKey(ModeCwd, "node", []string{"server.js"}, []string{}, "/dev/app")
	if id1 != id2 {
		t.Errorf("same args produced different IDs: %s vs %s", id1, id2)
	}
}

func TestGenerateContextKeyDifferentEnv(t *testing.T) {
	id1 := GenerateContextKey(ModeCwd, "node", []string{"server.js"}, []string{"API=1"}, "/dev/app")
	id2 := GenerateContextKey(ModeCwd, "node", []string{"server.js"}, []string{"API=2"}, "/dev/app")
	if id1 == id2 {
		t.Errorf("different env produced same ID: %s", id1)
	}
}

func TestGenerateContextKeyIsolated(t *testing.T) {
	id1 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, []string{}, "/dev/app")
	id2 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, []string{}, "/dev/app")
	if id1 == id2 {
		t.Errorf("isolated mode produced same ID: %s", id1)
	}
	if !strings.HasPrefix(id1, "isolated-") {
		t.Errorf("isolated mode missing prefix: %s", id1)
	}
}

func TestComputeEmptyFallback(t *testing.T) {
	// Should not panic
	id := Compute([]string{})
	if id == "" {
		t.Errorf("expected non-empty id")
	}
}

func TestIPCPath(t *testing.T) {
	path := IPCPath("abc123def456")
	if !strings.HasSuffix(path, ".sock") {
		t.Errorf("IPCPath = %q, want .sock suffix", path)
	}
	if !strings.Contains(path, "mcp-mux-abc123def456") {
		t.Errorf("IPCPath = %q, missing server ID", path)
	}
}

func TestControlPath(t *testing.T) {
	path := ControlPath("abc123def456")
	if !strings.HasSuffix(path, ".ctl.sock") {
		t.Errorf("ControlPath = %q, want .ctl.sock suffix", path)
	}
	if !strings.Contains(path, "mcp-mux-abc123def456") {
		t.Errorf("ControlPath = %q, missing server ID", path)
	}
}

func TestLockPath(t *testing.T) {
	path := LockPath("test123")
	if !strings.Contains(path, "mcp-mux-test123.lock") {
		t.Errorf("LockPath = %q, missing expected pattern", path)
	}
}

func TestDescribeArgs(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{}, "(empty)"},
		{[]string{"node", "server.js"}, "node server.js"},
		{[]string{"uvx", "--from", "pkg", "serena"}, "uvx --from pkg serena"},
	}

	for _, tt := range tests {
		got := DescribeArgs(tt.args)
		if got != tt.want {
			t.Errorf("DescribeArgs(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestPathNormalization(t *testing.T) {
	// This tests basic path normalization.
	// Cross-platform Windows tests are naturally implicit since we run on Windows.
	path1 := CanonicalizePath(`C:\dev\app`)
	path2 := CanonicalizePath(`c:\DEV\APP`)

	if path1 != path2 {
		t.Errorf("path normalization failed on Windows: %q != %q", path1, path2)
	}
}
