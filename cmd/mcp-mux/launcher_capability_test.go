package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
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

func allowLauncherAttestation(t *testing.T) {
	t.Helper()
	t.Setenv(envLauncherProtocol, "2:"+strconv.Itoa(os.Getppid()))
	t.Setenv(envLauncherAttestation, "test-attestation")
	orig := launcherAttestationProbe
	t.Cleanup(func() { launcherAttestationProbe = orig })
	launcherAttestationProbe = func(string, int) bool { return true }
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
	allowLauncherAttestation(t)

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

func TestLauncherLifecycleCapabilityAcceptsDifferentActiveEngineContents(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "stable launcher", "active versioned engine")
	allowLauncherAttestation(t)

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }

	if sameFile(launcherPath, enginePath) {
		t.Fatal("test fixture launcher and active engine unexpectedly match")
	}
	if !launcherLifecycleCapable() {
		t.Fatal("protocol-v2 direct launcher rejected its different active versioned engine")
	}
}

func TestLauncherLifecycleCapabilityRejectsWrongProtocolOrPID(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "stable launcher", "active versioned engine")
	allowLauncherAttestation(t)

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }
	if !launcherLifecycleCapable() {
		t.Fatal("valid protocol-v2 direct launcher fixture was not accepted")
	}

	for _, tc := range []struct {
		name     string
		protocol string
	}{
		{name: "wrong protocol version", protocol: "1:" + strconv.Itoa(os.Getppid())},
		{name: "wrong parent PID", protocol: "2:" + strconv.Itoa(os.Getppid()+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envLauncherProtocol, tc.protocol)
			if launcherLifecycleCapable() {
				t.Fatalf("launcher capability accepted %s", tc.name)
			}
		})
	}
}

func TestLauncherLifecycleCapabilityRejectsInactiveEngine(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "stable launcher", "active versioned engine")
	allowLauncherAttestation(t)

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }
	if !launcherLifecycleCapable() {
		t.Fatal("valid active engine fixture was not accepted")
	}

	otherEnginePath := filepath.Join(versionStoreDir(launcherPath), "other", engineFileName())
	writeTestFile(t, otherEnginePath, "other versioned engine")
	if err := writeActiveEngine(launcherPath, otherEnginePath); err != nil {
		t.Fatalf("write inactive engine pointer: %v", err)
	}
	if launcherLifecycleCapable() {
		t.Fatal("launcher capability accepted an inactive versioned engine")
	}
}

func TestLauncherLifecycleCapabilityRejectsCopiedDirectEngine(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "stable launcher", "active versioned engine")
	allowLauncherAttestation(t)

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }
	if !launcherLifecycleCapable() {
		t.Fatal("valid provider-derived layout fixture was not accepted")
	}

	copiedDir := t.TempDir()
	copiedLauncherPath := filepath.Join(copiedDir, launcherFileName())
	copiedEnginePath := filepath.Join(copiedDir, engineFileName())
	writeTestFile(t, copiedLauncherPath, "copied launcher")
	writeTestFile(t, copiedEnginePath, "copied engine")
	launcherCurrentExecutable = func() (string, error) { return copiedEnginePath, nil }
	launcherParentExecutable = func() (string, error) { return copiedLauncherPath, nil }
	if launcherLifecycleCapable() {
		t.Fatal("launcher capability accepted a direct engine outside the provider-derived version store")
	}
}

func TestLauncherLifecycleCapabilityRejectsUnverifiedAttestation(t *testing.T) {
	launcherPath, enginePath, _ := launcherCapabilityTestLayout(t, "stable launcher", "active versioned engine")
	allowLauncherAttestation(t)
	launcherAttestationProbe = func(string, int) bool { return false }

	origExe, origParent := launcherCurrentExecutable, launcherParentExecutable
	t.Cleanup(func() { launcherCurrentExecutable, launcherParentExecutable = origExe, origParent })
	launcherCurrentExecutable = func() (string, error) { return enginePath, nil }
	launcherParentExecutable = func() (string, error) { return launcherPath, nil }

	if launcherLifecycleCapable() {
		t.Fatal("launcher capability accepted an unverified parent attestation")
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
	t.Setenv(envLauncherProtocol, "") // Current old launcher cannot serve target-bound attestation.
	t.Setenv(envLauncherAttestation, "")
	origProbe := launcherAttestationProbe
	t.Cleanup(func() { launcherAttestationProbe = origProbe })
	launcherAttestationProbe = func(string, int) bool { return true }

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
	t.Setenv(envLauncherProtocol, "2:"+strconv.Itoa(os.Getppid()))
	t.Setenv(envLauncherAttestation, "future-attestation")
	if !launcherLifecycleCapable() {
		t.Fatal("future invocation from the bootstrapped capable launcher was not accepted")
	}
}

const launcherAttestationHelperMode = "MCPMUX_LAUNCHER_ATTESTATION_TEST_MODE"

func TestLauncherAttestationHelper(t *testing.T) {
	switch os.Getenv(launcherAttestationHelperMode) {
	case "direct":
		if !launcherAttestationCapable() {
			os.Exit(20)
		}
		fmt.Fprintln(os.Stdout, "host-output")
		os.Exit(0)
	case "forward":
		cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherAttestationHelper$")
		cmd.Env = setEnv(os.Environ(), launcherAttestationHelperMode, "forwarded-child")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(21)
		}
		os.Exit(0)
	case "forwarded-child":
		if launcherAttestationCapable() {
			os.Exit(22)
		}
		fmt.Fprintln(os.Stdout, "rejected")
		os.Exit(0)
	}
}

func TestLauncherAttestationBindsServerToDirectParent(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want string
	}{
		{name: "direct child", mode: "direct", want: "host-output\n"},
		{name: "forwarded through old launcher", mode: "forward", want: "rejected\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newLauncherEnvCommand(os.Args[0], os.Args[0], []string{"-test.run=^TestLauncherAttestationHelper$"})
			cmd.Env = setEnv(cmd.Env, launcherAttestationHelperMode, tc.mode)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := startLauncherEnvCommand(cmd); err != nil {
				t.Fatalf("helper start failed: %v; stderr=%s", err, stderr.String())
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("helper failed: %v; stderr=%s", err, stderr.String())
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q; private attestation bytes must not enter host stdout", got, tc.want)
			}
		})
	}
}

func TestLauncherAttestationSuccessRemovesUnixEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows named pipes do not leave filesystem socket paths")
	}
	path, cancel, err := startLauncherAttestation()
	if err != nil {
		t.Fatalf("startLauncherAttestation: %v", err)
	}
	t.Cleanup(cancel)
	if !verifyLauncherAttestation(path, os.Getpid()) {
		t.Fatal("launcher attestation failed")
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("attestation socket path still exists after successful exchange: %s (stat error: %v)", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLauncherCommandStartFailureCancelsAttestation(t *testing.T) {
	originalStart := launcherAttestationStart
	t.Cleanup(func() { launcherAttestationStart = originalStart })
	starts, cancels := 0, 0
	launcherAttestationStart = func() (string, func(), error) {
		starts++
		return "test-attestation", func() { cancels++ }, nil
	}

	missing := filepath.Join(t.TempDir(), "missing-engine")
	cmd := newLauncherEnvCommand(filepath.Join(t.TempDir(), "missing-launcher"), missing, nil)
	if err := startLauncherEnvCommand(cmd); err == nil {
		t.Fatal("missing engine unexpectedly started")
	}
	if starts != 1 || cancels != 1 {
		t.Fatalf("attestation lifecycle = %d starts, %d cancels; want 1, 1", starts, cancels)
	}
}

func TestSupervisedStartFailuresCancelEachAttestation(t *testing.T) {
	originalStart := launcherAttestationStart
	t.Cleanup(func() { launcherAttestationStart = originalStart })
	starts, cancels := 0, 0
	launcherAttestationStart = func() (string, func(), error) {
		starts++
		return "test-attestation", func() { cancels++ }, nil
	}

	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "missing-launcher")
	enginePath := filepath.Join(dir, "missing-engine")
	if _, err := startDefaultSupervisedEngineChild(launcherPath, enginePath, nil, io.Discard); err == nil {
		t.Fatal("missing active and fallback engines unexpectedly started")
	}
	if starts != 2 || cancels != 2 {
		t.Fatalf("attestation lifecycle = %d starts, %d cancels; want 2, 2", starts, cancels)
	}
}
