//go:build unix

package sockperm_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/sockperm"
)

func TestSockperm_SingleListen_Mode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")
	ln, err := sockperm.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := info.Mode() & 0777
	if got != 0600 {
		t.Errorf("socket mode = %04o, want 0600", got)
	}
}

func TestSockperm_Concurrent50_AllMode0600(t *testing.T) {
	const n = 50
	// Pre-create all 50 paths before launching goroutines. If a goroutine
	// called t.TempDir() concurrently with another goroutine's sockperm.Listen,
	// the umask=0177 window would affect MkdirTemp — the new directory would
	// land with mode 0700 & ~0177 = 0600 (no exec bit), and the subsequent
	// bind would fail with "permission denied". Pre-creating all dirs under
	// a single t.TempDir() (which itself ran before any umask manipulation)
	// avoids the race: the sockperm mutex only covers Listen, not MkdirTemp.
	baseDir := t.TempDir()
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		paths[i] = filepath.Join(baseDir, fmt.Sprintf("sock-%02d.sock", i))
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := paths[i]
			ln, err := sockperm.Listen("unix", path)
			if err != nil {
				errs <- err
				return
			}
			defer ln.Close()
			defer os.Remove(path)

			info, err := os.Stat(path)
			if err != nil {
				errs <- err
				return
			}
			if got := info.Mode() & 0777; got != 0600 {
				errs <- fmt.Errorf("goroutine %d: socket mode = %04o, want 0600", i, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSockperm_UmaskRestored(t *testing.T) {
	// Probe the current umask. Restore it immediately — this is the best
	// portable way to read it. If the runner already has umask 0177 (which
	// would produce 0600 sockets via plain net.Listen), this test cannot
	// distinguish "umask restored" from "umask was 0177 all along".
	currentUmask := syscall.Umask(0)
	syscall.Umask(currentUmask)
	if currentUmask == 0177 {
		t.Skip("process umask is already 0177; test cannot distinguish restored from not-restored")
	}

	dir := t.TempDir()
	path1 := filepath.Join(dir, "sock1.sock")
	path2 := filepath.Join(dir, "sock2.sock")

	ln1, err := sockperm.Listen("unix", path1)
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	defer os.Remove(path1)

	// After sockperm.Listen, the umask should be restored to original.
	// A plain net.Listen should produce a non-0600 mode (typically 0755 with umask 022).
	ln2, err := net.Listen("unix", path2)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	defer os.Remove(path2)

	info, err := os.Stat(path2)
	if err != nil {
		t.Fatal(err)
	}
	got := info.Mode() & 0777
	// Should NOT be 0600 if umask was properly restored.
	// Note: this test depends on the process umask being != 0177.
	// Typical umask is 022 → socket mode would be 0755.
	if got == 0600 {
		t.Error("plain net.Listen produced 0600 — umask may not have been restored by sockperm.Listen")
	}
}
