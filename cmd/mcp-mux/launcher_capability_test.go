package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func launcherCapabilityTestLayout(t *testing.T, launcherContents, engineContents string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherFileName())
	enginePath := filepath.Join(versionStoreDir(launcherPath), "candidate", engineFileName())
	writeTestFile(t, launcherPath, launcherContents)
	writeTestFile(t, enginePath, engineContents)
	if err := writeActiveEngine(launcherPath, enginePath); err != nil {
		t.Fatalf("write active engine: %v", err)
	}
	return launcherPath, enginePath, activeEngineFile(launcherPath)
}

func TestLauncherFileNamePreservesPlatformExtension(t *testing.T) {
	want := "mcp-mux"
	if runtime.GOOS == "windows" {
		want = "mcp-mux.exe"
	}
	if got := launcherFileName(); got != want {
		t.Fatalf("launcherFileName() = %q, want %q", got, want)
	}
}

func TestLauncherLifecycleCapabilityRejectsForgedEnvironment(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "capable launcher", "capable launcher")
	forgedDir := t.TempDir()
	forgedLauncher := filepath.Join(forgedDir, "forged-launcher")
	forgedActive := filepath.Join(forgedDir, "active.txt")
	writeTestFile(t, forgedLauncher, "capable launcher")
	writeTestFile(t, forgedActive, enginePath+"\n")
	t.Setenv(envLauncherExe, forgedLauncher)
	t.Setenv(envActiveEngineFile, forgedActive)
	t.Setenv(envLauncherProtocol, "1:"+strconv.Itoa(os.Getppid()))

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return forgedLauncher, nil }
	if launcherLifecycleCapable() {
		t.Fatal("fully forged launcher environment accepted a non-provider parent")
	}

	launcherParentExecutable = func() (string, error) { return launcherPath, nil }
	if !launcherLifecycleCapable() {
		t.Fatal("provider-derived installed launcher was not accepted")
	}
}

func TestBootstrapStableLauncherUsesInstalledLayout(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "old launcher", "new launcher")
	redirectedLauncher := filepath.Join(t.TempDir(), "redirected-launcher")
	writeTestFile(t, redirectedLauncher, "unrelated")
	t.Setenv(envLauncherExe, redirectedLauncher)
	t.Setenv(envActiveEngineFile, filepath.Join(t.TempDir(), "redirected-active.txt"))

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }

	updated, err := bootstrapStableLauncher()
	if err != nil || !updated {
		t.Fatalf("bootstrapStableLauncher() = (%v, %v), want updated", updated, err)
	}
	if !sameFile(launcherPath, enginePath) {
		t.Fatal("bootstrap did not update the provider-derived launcher")
	}
	if got, err := os.ReadFile(redirectedLauncher); err != nil || string(got) != "unrelated" {
		t.Fatalf("bootstrap redirected by environment: %q, %v", got, err)
	}
	if _, err := os.Stat(launcherPath + ".old." + strconv.Itoa(os.Getpid())); err != nil {
		t.Fatalf("old running-launcher path not retained: %v", err)
	}

	launcherParentExecutable = func() (string, error) { return "", errors.New("gone") }
	if _, err := bootstrapStableLauncher(); err == nil {
		t.Fatal("bootstrap accepted unavailable direct parent")
	}
}

func TestLauncherMigrationRequiresOneFutureInvocation(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "old launcher", "capable launcher")
	t.Setenv(envLauncherProtocol, "") // Current old launcher cannot send private frames.

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }

	if launcherLifecycleCapable() {
		t.Fatal("old launcher generation was allowed to receive private dormant frames")
	}
	updated, err := bootstrapStableLauncher()
	if err != nil || !updated {
		t.Fatalf("bootstrapStableLauncher() = (%v, %v), want future-launcher update", updated, err)
	}
	if launcherLifecycleCapable() {
		t.Fatal("current old launcher generation became capable without a host restart")
	}
	t.Setenv(envLauncherProtocol, "1:"+strconv.Itoa(os.Getppid()))
	if !launcherLifecycleCapable() {
		t.Fatal("future invocation from the bootstrapped capable launcher was not accepted")
	}
}
