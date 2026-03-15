package serverid

import (
	"runtime"
	"strings"
	"testing"
)

func TestComputeDeterministic(t *testing.T) {
	args := []string{"uvx", "--refresh", "--from", "git+https://example.com", "serena"}
	id1 := Compute(args)
	id2 := Compute(args)
	if id1 != id2 {
		t.Errorf("same args produced different IDs: %s vs %s", id1, id2)
	}
}

func TestComputeDifferentArgs(t *testing.T) {
	id1 := Compute([]string{"node", "server1.js"})
	id2 := Compute([]string{"node", "server2.js"})
	if id1 == id2 {
		t.Errorf("different args produced same ID: %s", id1)
	}
}

func TestComputeOrderMatters(t *testing.T) {
	id1 := Compute([]string{"a", "b"})
	id2 := Compute([]string{"b", "a"})
	if id1 == id2 {
		t.Errorf("different order produced same ID: %s", id1)
	}
}

func TestComputeSeparatorPreventsCollision(t *testing.T) {
	id1 := Compute([]string{"ab", "c"})
	id2 := Compute([]string{"a", "bc"})
	if id1 == id2 {
		t.Errorf("colliding args produced same ID: %s", id1)
	}
}

func TestComputeEmpty(t *testing.T) {
	id := Compute([]string{})
	if len(id) != 16 {
		t.Errorf("empty args ID length = %d, want 16", len(id))
	}
}

func TestComputeLength(t *testing.T) {
	id := Compute([]string{"node", "some/path/server.js"})
	if len(id) != 16 {
		t.Errorf("ID length = %d, want 16", len(id))
	}
}

func TestIPCPathWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only test")
	}
	path := IPCPath("abc123def456")
	if !strings.HasPrefix(path, `\\.\pipe\mcp-mux-`) {
		t.Errorf("IPCPath = %q, want prefix \\\\?\\pipe\\mcp-mux-", path)
	}
	if !strings.Contains(path, "abc123def456") {
		t.Errorf("IPCPath = %q, missing server ID", path)
	}
}

func TestIPCPathUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}
	path := IPCPath("abc123def456")
	if !strings.HasSuffix(path, ".sock") {
		t.Errorf("IPCPath = %q, want .sock suffix", path)
	}
	if !strings.Contains(path, "mcp-mux-abc123def456") {
		t.Errorf("IPCPath = %q, missing server ID", path)
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
