//go:build windows

package daemon

import (
	"log"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/internal/envidentity"
)

func TestDetachedDevNullDaemonAcceptsParentStatus(t *testing.T) {
	ctlPath := shortSocketPath(t, "detached-daemon.ctl.sock")
	readyPath := ctlPath + ".ready"
	resultPath := ctlPath + ".result"

	cmd := exec.Command(os.Args[0], "-test.run=TestDetachedDaemonControlHelper")
	cmd.Env = append(os.Environ(),
		"MUXCORE_DAEMON_TEST_HELPER=1",
		"MUXCORE_DAEMON_TEST_CONTROL="+ctlPath,
		"MUXCORE_DAEMON_TEST_READY="+readyPath,
		"MUXCORE_DAEMON_TEST_RESULT="+resultPath,
	)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	if err := cmd.Process.Release(); err != nil {
		t.Fatalf("release helper: %v", err)
	}

	waitForDaemonFile(t, readyPath, 5*time.Second)
	resp, err := control.Send(ctlPath, control.Request{Cmd: "status"})
	if err != nil {
		t.Fatalf("control.Send(status) error: %v", err)
	}
	if !resp.OK {
		t.Fatalf("control.Send(status) OK=false: %+v", resp)
	}
	waitForDaemonFile(t, resultPath, 5*time.Second)
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "ok" {
		t.Fatalf("helper result = %q, want ok", got)
	}
}

func TestDetachedDaemonControlHelper(t *testing.T) {
	if os.Getenv("MUXCORE_DAEMON_TEST_HELPER") != "1" {
		return
	}
	ctlPath := os.Getenv("MUXCORE_DAEMON_TEST_CONTROL")
	readyPath := os.Getenv("MUXCORE_DAEMON_TEST_READY")
	resultPath := os.Getenv("MUXCORE_DAEMON_TEST_RESULT")
	if ctlPath == "" || readyPath == "" || resultPath == "" {
		os.Exit(2)
	}
	d, err := New(Config{
		Name:         "detached-daemon-test",
		Namespace:    "detached-daemon-test",
		ControlPath:  ctlPath,
		IdleTimeout:  5 * time.Second,
		SkipSnapshot: true,
		Logger:       log.New(os.Stderr, "[daemon-helper] ", log.LstdFlags),
	})
	if err != nil {
		_ = os.WriteFile(resultPath, []byte("daemon: "+err.Error()), 0600)
		os.Exit(0)
	}
	defer d.Shutdown()
	if err := os.WriteFile(readyPath, []byte("ready"), 0600); err != nil {
		_ = os.WriteFile(resultPath, []byte("ready: "+err.Error()), 0600)
		os.Exit(0)
	}
	time.Sleep(500 * time.Millisecond)
	_ = os.WriteFile(resultPath, []byte("ok"), 0600)
	os.Exit(0)
}

func TestWindowsEffectiveEnvUsesShimOverrideForIdentityTemplateAndLaunch(t *testing.T) {
	t.Setenv("CONFIG_PATH", "A")
	shimEnv := map[string]string{"config_path": "B"}

	assertNormalized := func(label string, env map[string]string) {
		t.Helper()
		matches := 0
		for key, value := range env {
			if strings.EqualFold(key, "CONFIG_PATH") {
				matches++
				if value != "B" {
					t.Fatalf("%s %q=%q, want shim value B", label, key, value)
				}
			}
		}
		if matches != 1 {
			t.Fatalf("%s has %d case-insensitive CONFIG_PATH keys, want 1", label, matches)
		}
	}

	effectiveEnv := mergeEnv(shimEnv)
	assertNormalized("effective env", effectiveEnv)
	identity := envidentity.Build(effectiveEnv)
	secondEffectiveEnv := mergeEnv(shimEnv)
	assertNormalized("second effective env", secondEffectiveEnv)
	if second := envidentity.Build(secondEffectiveEnv); second != identity {
		t.Fatalf("effective env identity changed across identical merges: first=%q second=%q", identity.Fingerprint, second.Fingerprint)
	}

	d := testDaemon(t)
	command := "windows-case-insensitive-effective-env"
	cwd := t.TempDir()
	template := daemonMaterializationSnapshot(false)
	template.Cwd = cwd
	template.Env = shimEnv
	d.updateTemplate(command, nil, template)

	match, ok := d.getCompatibleTemplate(command, nil, cwd, effectiveEnv)
	if !ok {
		t.Fatal("normalized effective env did not authorize its template")
	}
	assertNormalized("template env", match.snapshot.Env)
	if !reflect.DeepEqual(match.snapshot.Env, effectiveEnv) {
		t.Fatal("template authorization did not retain the normalized effective env")
	}

	_, sid, _, err := d.Spawn(control.Request{Command: command, Mode: "global", Cwd: cwd, Env: shimEnv})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	entry := d.Entry(sid)
	if entry == nil || entry.Owner == nil {
		t.Fatalf("spawned owner %q missing", sid)
	}
	assertNormalized("owner env", entry.Env)
	if !reflect.DeepEqual(entry.Env, effectiveEnv) {
		t.Fatal("owner launch context did not receive the normalized effective env")
	}
	launchSnapshot := entry.Owner.ExportSnapshot()
	assertNormalized("owner snapshot env", launchSnapshot.Env)
	if !reflect.DeepEqual(launchSnapshot.Env, effectiveEnv) {
		t.Fatal("owner snapshot did not preserve the normalized launch env")
	}
}

func waitForDaemonFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", path)
}
