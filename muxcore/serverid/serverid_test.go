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

// TestGenerateContextKey_IsolatedDeterministic encodes the post-CR-001
// contract for isolated server IDs: identical (cmd, args, cwd) inputs MUST
// produce identical sids so per-(cwd, server) reconnects can rebind to the
// existing owner instead of spawning a fresh upstream every time. Replaces
// the pre-CR-001 behavior that minted a random UUID per call and never
// allowed reuse.
//
// Engram #244 Bug 2 reference: 23 random-UUID isolated owners coexisted on
// one workstation because every new session for the same (cmd, cwd) got a
// fresh upstream. Deterministic hashing eliminates that accumulation pattern.
func TestGenerateContextKey_IsolatedDeterministic(t *testing.T) {
	id1 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, []string{}, "/dev/app")
	id2 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, []string{}, "/dev/app")
	if id1 != id2 {
		t.Errorf("isolated mode must be deterministic for identical inputs, got %s vs %s", id1, id2)
	}
	if !strings.HasPrefix(id1, "isolated-") {
		t.Errorf("isolated mode missing prefix: %s", id1)
	}
}

// TestGenerateContextKey_IsolatedDistinctByCwd proves cwd participates in the
// hash so two cwds for the same (cmd, args) do NOT collapse onto the same
// owner. Without this, the split-on-isolation path in CR-002 could mistakenly
// share an isolated upstream across projects.
func TestGenerateContextKey_IsolatedDistinctByCwd(t *testing.T) {
	id1 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, nil, "/dev/app-1")
	id2 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, nil, "/dev/app-2")
	if id1 == id2 {
		t.Errorf("isolated mode must differ by cwd, got identical %s", id1)
	}
}

// TestGenerateContextKey_IsolatedDistinctByArgs proves args participate in
// the hash (mirrors the shared-mode behavior). Two upstreams with same cmd
// but different args are different upstreams.
func TestGenerateContextKey_IsolatedDistinctByArgs(t *testing.T) {
	id1 := GenerateContextKey(ModeIsolated, "node", []string{"server-a.js"}, nil, "/dev/app")
	id2 := GenerateContextKey(ModeIsolated, "node", []string{"server-b.js"}, nil, "/dev/app")
	if id1 == id2 {
		t.Errorf("isolated mode must differ by args, got identical %s", id1)
	}
}

// TestGenerateContextKey_IsolatedEnvIndependent proves env vars do NOT
// participate in the isolated hash. Transient session env (CLAUDE_CODE_*,
// TERM_PROGRAM, etc.) varies between MCP host invocations for the same
// project; if env were part of identity, every reconnect would create a
// new owner.
func TestGenerateContextKey_IsolatedEnvIndependent(t *testing.T) {
	id1 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, []string{"API=1"}, "/dev/app")
	id2 := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, []string{"API=2"}, "/dev/app")
	if id1 != id2 {
		t.Errorf("isolated mode must ignore env, got %s vs %s", id1, id2)
	}
}

// TestGenerateContextKey_IsolatedFormatPreserved keeps the wire-format
// contract: "isolated-" prefix + 16-hex suffix = 25 chars total. Snapshot
// files written by old code (random UUIDs in the same format) must remain
// parseable; we only change the chance of collision, not the shape.
func TestGenerateContextKey_IsolatedFormatPreserved(t *testing.T) {
	id := GenerateContextKey(ModeIsolated, "node", []string{"server.js"}, nil, "/dev/app")
	if !strings.HasPrefix(id, "isolated-") {
		t.Errorf("isolated id %q missing prefix", id)
	}
	const wantLen = len("isolated-") + 16
	if len(id) != wantLen {
		t.Errorf("isolated id %q has length %d, want %d", id, len(id), wantLen)
	}
	// Suffix must be hex.
	suffix := strings.TrimPrefix(id, "isolated-")
	for _, c := range suffix {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			t.Errorf("isolated id %q has non-hex suffix char %q", id, c)
		}
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
	customDir := t.TempDir()
	tests := []struct {
		name     string
		wantFile string
	}{
		{name: "mcp-mux", wantFile: "mcp-mux-abc123def456.sock"},
		{name: "aimux", wantFile: "aimux-abc123def456.sock"},
		{name: "aimux-dev", wantFile: "aimux-dev-abc123def456.sock"},
		{name: "", wantFile: "-abc123def456.sock"},
	}
	for _, tt := range tests {
		t.Run("name="+tt.name, func(t *testing.T) {
			path := IPCPath(customDir, tt.name, "abc123def456")
			if filepath.Base(path) != tt.wantFile {
				t.Errorf("IPCPath(%q, %q, %q) base = %q, want %q", customDir, tt.name, "abc123def456", filepath.Base(path), tt.wantFile)
			}
			if filepath.Dir(path) != customDir {
				t.Errorf("IPCPath dir = %q, want %q", filepath.Dir(path), customDir)
			}
		})
	}
}

func TestControlPath(t *testing.T) {
	customDir := t.TempDir()
	tests := []struct {
		name     string
		wantFile string
	}{
		{name: "mcp-mux", wantFile: "mcp-mux-abc123def456.ctl.sock"},
		{name: "aimux", wantFile: "aimux-abc123def456.ctl.sock"},
		{name: "aimux-dev", wantFile: "aimux-dev-abc123def456.ctl.sock"},
		{name: "", wantFile: "-abc123def456.ctl.sock"},
	}
	for _, tt := range tests {
		t.Run("name="+tt.name, func(t *testing.T) {
			path := ControlPath(customDir, tt.name, "abc123def456")
			if filepath.Base(path) != tt.wantFile {
				t.Errorf("ControlPath(%q, %q, %q) base = %q, want %q", customDir, tt.name, "abc123def456", filepath.Base(path), tt.wantFile)
			}
			if filepath.Dir(path) != customDir {
				t.Errorf("ControlPath dir = %q, want %q", filepath.Dir(path), customDir)
			}
		})
	}
}

func TestLockPath(t *testing.T) {
	customDir := t.TempDir()
	tests := []struct {
		name     string
		wantFile string
	}{
		{name: "mcp-mux", wantFile: "mcp-mux-test123.lock"},
		{name: "aimux", wantFile: "aimux-test123.lock"},
		{name: "aimux-dev", wantFile: "aimux-dev-test123.lock"},
		{name: "", wantFile: "-test123.lock"},
	}
	for _, tt := range tests {
		t.Run("name="+tt.name, func(t *testing.T) {
			path := LockPath(customDir, tt.name, "test123")
			if filepath.Base(path) != tt.wantFile {
				t.Errorf("LockPath(%q, %q, %q) base = %q, want %q", customDir, tt.name, "test123", filepath.Base(path), tt.wantFile)
			}
			if filepath.Dir(path) != customDir {
				t.Errorf("LockPath dir = %q, want %q", filepath.Dir(path), customDir)
			}
		})
	}
}

func TestIPCPath_EmptyBaseDir(t *testing.T) {
	path := IPCPath("", "mcp-mux", "abc123")
	tmpDir := os.TempDir()
	// macOS: os.TempDir() returns the /var/folders/... alias while the value
	// that reaches the socket path may be resolved through /private/... —
	// semantically the same directory, textually different. Normalise both
	// sides via EvalSymlinks before comparing so the test works on darwin
	// the same way it does on linux and windows.
	gotDir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(gotDir); err == nil {
		gotDir = resolved
	}
	if resolved, err := filepath.EvalSymlinks(tmpDir); err == nil {
		tmpDir = resolved
	}
	if gotDir != tmpDir {
		t.Errorf("IPCPath with empty baseDir: got dir %q, want os.TempDir() %q", gotDir, tmpDir)
	}
	if filepath.Base(path) != "mcp-mux-abc123.sock" {
		t.Errorf("IPCPath base = %q, want mcp-mux-abc123.sock", filepath.Base(path))
	}
}

func TestIPCPath_CustomBaseDir(t *testing.T) {
	customDir := t.TempDir()
	path := IPCPath(customDir, "mcp-mux", "abc123")
	if filepath.Dir(path) != customDir {
		t.Errorf("IPCPath with custom baseDir: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if filepath.Base(path) != "mcp-mux-abc123.sock" {
		t.Errorf("IPCPath = %q, want filename mcp-mux-abc123.sock", path)
	}
}

func TestDaemonControlPath_CustomBaseDir(t *testing.T) {
	customDir := t.TempDir()
	path := DaemonControlPath(customDir, "")
	if filepath.Dir(path) != customDir {
		t.Errorf("DaemonControlPath with custom baseDir: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if filepath.Base(path) != "mcp-mux-muxd.ctl.sock" {
		t.Errorf("DaemonControlPath = %q, unexpected filename", path)
	}
}

func TestDaemonControlPath_CustomName(t *testing.T) {
	customDir := t.TempDir()
	path := DaemonControlPath(customDir, "aimux")
	if filepath.Dir(path) != customDir {
		t.Errorf("DaemonControlPath with custom name: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if filepath.Base(path) != "aimux-muxd.ctl.sock" {
		t.Errorf("DaemonControlPath = %q, want aimux-muxd.ctl.sock", filepath.Base(path))
	}
}

func TestDaemonLockPath_CustomBaseDir(t *testing.T) {
	customDir := t.TempDir()
	path := DaemonLockPath(customDir, "")
	if filepath.Dir(path) != customDir {
		t.Errorf("DaemonLockPath with custom baseDir: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if filepath.Base(path) != "mcp-mux-muxd.lock" {
		t.Errorf("DaemonLockPath = %q, unexpected filename", path)
	}
}

func TestDaemonLockPath_CustomName(t *testing.T) {
	customDir := t.TempDir()
	path := DaemonLockPath(customDir, "aimux")
	if filepath.Dir(path) != customDir {
		t.Errorf("DaemonLockPath with custom name: got dir %q, want %q", filepath.Dir(path), customDir)
	}
	if filepath.Base(path) != "aimux-muxd.lock" {
		t.Errorf("DaemonLockPath = %q, want aimux-muxd.lock", filepath.Base(path))
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

func TestWorktreeRoot_MainCheckout(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	got := WorktreeRoot(dir)
	if CanonicalizePath(got) != CanonicalizePath(dir) {
		t.Errorf("WorktreeRoot(main checkout) = %q, want %q", got, dir)
	}
}

func TestWorktreeRoot_LinkedWorktree(t *testing.T) {
	// Create a fake worktree: .git is a file (not a directory).
	// WorktreeRoot should return the worktree dir, NOT resolve back to main repo.
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

	got := WorktreeRoot(worktreeDir)
	// Should return the worktree dir itself, NOT the main repo.
	if CanonicalizePath(got) != CanonicalizePath(worktreeDir) {
		t.Errorf("WorktreeRoot(linked worktree) = %q, want %q (NOT resolved to main repo)", got, worktreeDir)
	}
	if CanonicalizePath(got) == CanonicalizePath(mainRepo) {
		t.Errorf("WorktreeRoot(linked worktree) incorrectly resolved to main repo %q", mainRepo)
	}
}

func TestWorktreeRoot_NoGit(t *testing.T) {
	dir := t.TempDir()
	got := WorktreeRoot(dir)
	if CanonicalizePath(got) != CanonicalizePath(dir) {
		t.Errorf("WorktreeRoot(no .git) = %q, want canonical cwd %q", got, dir)
	}
}

func TestWorktreeRoot_Subdirectory(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got := WorktreeRoot(sub)
	if CanonicalizePath(got) != CanonicalizePath(dir) {
		t.Errorf("WorktreeRoot(subdir) = %q, want parent with .git %q", got, dir)
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
