package ipc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMaintenanceNamespaceLockHelperProcess(t *testing.T) {
	path := os.Getenv("MCPMUX_MAINTENANCE_LOCK_HELPER")
	if path == "" {
		return
	}
	lock, err := AcquireFileLock(path)
	if err != nil {
		fmt.Fprintln(os.Stdout, "refused")
		os.Exit(0)
	}
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	_ = lock.Close()
	os.Exit(0)
}

func TestMaintenanceNamespaceLockSerializesProcessesAndRetainsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namespace.lock")
	if err := os.WriteFile(path, []byte("persistent lock identity"), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestMaintenanceNamespaceLockHelperProcess$")
	child.Env = append(os.Environ(), "MCPMUX_MAINTENANCE_LOCK_HELPER="+path)
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = child.Wait() })
	scanner := bufio.NewScanner(out)
	if !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatal("helper did not acquire namespace lock")
	}
	lock, err := AcquireFileLock(path)
	if lock != nil {
		_ = lock.Close()
	}
	if !errors.Is(err, ErrFileLocked) {
		t.Fatalf("concurrent lock: %v", err)
	}
	_ = in.Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	lock, err = AcquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "persistent lock identity" {
		t.Fatalf("namespace lock identity changed: %q %v", data, err)
	}
}
