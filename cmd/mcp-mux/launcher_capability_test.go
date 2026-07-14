package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLauncherLifecycleCapabilityRequiresDirectCurrentLauncher(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(dir, "engine.exe")
	activeFile := filepath.Join(dir, "active.txt")
	writeTestFile(t, launcherPath, "launcher")
	writeTestFile(t, enginePath, "engine")
	writeTestFile(t, activeFile, enginePath+"\n")
	t.Setenv(envLauncherExe, launcherPath)
	t.Setenv(envActiveEngineFile, activeFile)
	t.Setenv(envLauncherProtocol, "1:"+strconv.Itoa(os.Getppid()))

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() {
		launcherCurrentExecutable, launcherParentExecutable = origExe, origParent
	})
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }
	if !launcherLifecycleCapable() {
		t.Fatal("verified direct launcher was not accepted")
	}
	launcherParentExecutable = func() (string, error) { return filepath.Join(dir, "spoofing-parent.exe"), nil }
	if launcherLifecycleCapable() {
		t.Fatal("forged environment and active pointer accepted a non-launcher direct parent")
	}
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }

	t.Setenv(envLauncherProtocol, "1:1")
	if launcherLifecycleCapable() {
		t.Fatal("stale inherited advertisement accepted lifecycle capability")
	}
	t.Setenv(envLauncherProtocol, "1:"+strconv.Itoa(os.Getppid()))
	writeTestFile(t, activeFile, filepath.Join(dir, "stale.exe")+"\n")
	if launcherLifecycleCapable() {
		t.Fatal("stale active pointer accepted lifecycle capability")
	}
	if launcherBootstrapEligible() {
		t.Fatal("stale active pointer accepted launcher bootstrap")
	}
}

func TestBootstrapStableLauncherUsesSwapAfterIdentityProof(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(dir, "engine.exe")
	activeFile := filepath.Join(dir, "active.txt")
	writeTestFile(t, launcherPath, "old launcher")
	writeTestFile(t, enginePath, "new launcher")
	writeTestFile(t, activeFile, enginePath+"\n")
	t.Setenv(envLauncherExe, launcherPath)
	t.Setenv(envActiveEngineFile, activeFile)

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() {
		launcherCurrentExecutable, launcherParentExecutable = origExe, origParent
	})
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }

	updated, err := bootstrapStableLauncher()
	if err != nil || !updated {
		t.Fatalf("bootstrapStableLauncher() = (%v, %v), want updated", updated, err)
	}
	got, err := os.ReadFile(launcherPath)
	if err != nil || string(got) != "new launcher" {
		t.Fatalf("launcher after bootstrap = %q, %v", got, err)
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
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "mcp-mux.exe")
	enginePath := filepath.Join(dir, "engine.exe")
	activeFile := filepath.Join(dir, "active.txt")
	writeTestFile(t, launcherPath, "old launcher")
	writeTestFile(t, enginePath, "capable launcher")
	writeTestFile(t, activeFile, enginePath+"\n")
	t.Setenv(envLauncherExe, launcherPath)
	t.Setenv(envActiveEngineFile, activeFile)
	t.Setenv(envLauncherProtocol, "") // Current old launcher cannot send private frames.

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() {
		launcherCurrentExecutable, launcherParentExecutable = origExe, origParent
	})
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
