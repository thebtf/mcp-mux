package serverid

import (
	"os"
	"path/filepath"
	"runtime"
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
	path := IPCPath("abc123def456", "")
	if !strings.HasSuffix(path, ".sock") {
		t.Errorf("IPCPath = %q, want .sock suffix", path)
	}
	if !strings.Contains(path, "mcp-mux-abc123def456") {
		t.Errorf("IPCPath = %q, missing server ID", path)
	}
}

func TestControlPath(t *testing.T) {
	path := ControlPath("abc123def456", "")
	if !strings.HasSuffix(path, ".ctl.sock") {
		t.Errorf("ControlPath = %q, want .ctl.sock suffix", path)
	}
	if !strings.Contains(path, "mcp-mux-abc123def456") {
		t.Errorf("ControlPath = %q, missing server ID", path)
	}
}

func TestLockPath(t *testing.T) {
	path := LockPath("test123", "")
	if !strings.Contains(path, "mcp-mux-test123.lock") {
		t.Errorf("LockPath = %q, missing expected pattern", path)
	}
}

func TestIPCPath_EmptyBaseDir(t *testing.T) {
	path := IPCPath("abc123", "")
	tmpDir := os.TempDir()
	if filepath.Dir(path) != tmpDir {
		t.Errorf("IPCPath with empty baseDir: got dir %q, want os.TempDir() %q", filepath.Dir(path), tmpDir)
	}
}

func TestIPCPath_CustomBaseDir(t *testing.T) {
	customDir := t.TempDir()
	path := IPCPath("abc123", customDir)
	if filepath.Dir(path) != customDir {
		t.Errorf("IPCPath with custom baseDir: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if !strings.Contains(path, "mcp-mux-abc123.sock") {
		t.Errorf("IPCPath = %q, missing expected filename", path)
	}
}

func TestDaemonControlPath_CustomBaseDir(t *testing.T) {
	customDir := t.TempDir()
	path := DaemonControlPath(customDir)
	if filepath.Dir(path) != customDir {
		t.Errorf("DaemonControlPath with custom baseDir: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if filepath.Base(path) != "mcp-muxd.ctl.sock" {
		t.Errorf("DaemonControlPath = %q, unexpected filename", path)
	}
}

func TestDaemonLockPath_CustomBaseDir(t *testing.T) {
	customDir := t.TempDir()
	path := DaemonLockPath(customDir)
	if filepath.Dir(path) != customDir {
		t.Errorf("DaemonLockPath with custom baseDir: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if filepath.Base(path) != "mcp-muxd.lock" {
		t.Errorf("DaemonLockPath = %q, unexpected filename", path)
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

func TestFindGitRootRegularRepo(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got := findGitRoot(sub)
	if got != dir {
		t.Errorf("findGitRoot(%q) = %q, want %q", sub, got, dir)
	}
}

func TestFindGitRootWorktree(t *testing.T) {
	// Simulate a git worktree layout:
	// mainRepo/.git/worktrees/wt1/  (directory)
	// worktreeDir/.git              (file: "gitdir: mainRepo/.git/worktrees/wt1")
	mainRepo := t.TempDir()
	mainGit := filepath.Join(mainRepo, ".git")
	if err := os.Mkdir(mainGit, 0o755); err != nil {
		t.Fatal(err)
	}
	wtGitDir := filepath.Join(mainGit, "worktrees", "wt1")
	if err := os.MkdirAll(wtGitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	worktreeDir := t.TempDir()
	gitFile := filepath.Join(worktreeDir, ".git")
	content := "gitdir: " + wtGitDir + "\n"
	if err := os.WriteFile(gitFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := findGitRoot(worktreeDir)
	// Should resolve to mainRepo, not worktreeDir.
	// Use CanonicalizePath on both sides: macOS /var → /private/var symlink.
	wantCanon := CanonicalizePath(mainRepo)
	gotCanon := CanonicalizePath(got)
	if gotCanon != wantCanon {
		t.Errorf("findGitRoot(worktree) = %q (canon: %q), want %q (canon: %q)", got, gotCanon, mainRepo, wantCanon)
	}
}

func TestFindGitRootWorktreeSubdir(t *testing.T) {
	mainRepo := t.TempDir()
	mainGit := filepath.Join(mainRepo, ".git")
	if err := os.Mkdir(mainGit, 0o755); err != nil {
		t.Fatal(err)
	}
	wtGitDir := filepath.Join(mainGit, "worktrees", "feature")
	if err := os.MkdirAll(wtGitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	worktreeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"),
		[]byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(worktreeDir, "src", "main")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// Searching from a subdirectory of a worktree should still find main repo
	got := findGitRoot(sub)
	wantCanon := CanonicalizePath(mainRepo)
	gotCanon := CanonicalizePath(got)
	if gotCanon != wantCanon {
		t.Errorf("findGitRoot(worktree/sub) = %q (canon: %q), want %q (canon: %q)", got, gotCanon, mainRepo, wantCanon)
	}
}

func TestWorktreeAndMainShareServerID(t *testing.T) {
	mainRepo := t.TempDir()
	mainGit := filepath.Join(mainRepo, ".git")
	if err := os.Mkdir(mainGit, 0o755); err != nil {
		t.Fatal(err)
	}
	wtGitDir := filepath.Join(mainGit, "worktrees", "wt1")
	if err := os.MkdirAll(wtGitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	worktreeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"),
		[]byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	idMain := GenerateContextKey(ModeGit, "node", []string{"server.js"}, nil, mainRepo)
	idWT := GenerateContextKey(ModeGit, "node", []string{"server.js"}, nil, worktreeDir)
	if idMain != idWT {
		t.Errorf("worktree and main produce different server IDs:\n  main: %s\n  wt:   %s", idMain, idWT)
	}
}

func TestPathNormalization(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific path normalization test")
	}
	path1 := CanonicalizePath(`C:\dev\app`)
	path2 := CanonicalizePath(`c:\DEV\APP`)

	if path1 != path2 {
		t.Errorf("path normalization failed on Windows: %q != %q", path1, path2)
	}
}
