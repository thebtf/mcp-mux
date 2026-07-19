package supervisor

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRealBinaryRollingCompatibilityMatrix(t *testing.T) {
	launcher := buildCompatibilityFixture(t, "compat-launcher")
	engine := buildCompatibilityFixture(t, "compat-engine")
	cases := []struct {
		name         string
		launcherMode string
		engineMode   string
	}{
		{name: "new_launcher_old_engine", launcherMode: "new", engineMode: "old"},
		{name: "new_launcher_pre_attestation_collision", launcherMode: "new", engineMode: "old-colliding"},
		{name: "old_launcher_new_engine", launcherMode: "old", engineMode: "new"},
		{name: "new_launcher_new_engine", launcherMode: "new", engineMode: "new"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "children.txt")
			attestationFile := filepath.Join(t.TempDir(), "attestation.txt")
			if runtime.GOOS != "windows" {
				if err := os.WriteFile(attestationFile, []byte("stale"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(attestationFile, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(launcher,
				"--mode", testCase.launcherMode,
				"--engine", engine,
				"--engine-mode", testCase.engineMode,
				"--pid-file", pidFile,
				"--attestation-file", attestationFile,
			)
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			finished := false
			t.Cleanup(func() {
				_ = stdin.Close()
				if !finished {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			})
			lines := make(chan string, 16)
			go scanCompatibilityLines(stdout, lines)

			writeCompatibilityLine(t, stdin, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`)
			initialize := readCompatibilityResponse(t, lines, `"id":"init"`)
			if !strings.Contains(initialize, `"version":"`+testCase.engineMode+`"`) {
				t.Fatalf("initialize response = %s stderr=%s", initialize, stderr.String())
			}
			writeCompatibilityLine(t, stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			writeCompatibilityLine(t, stdin, `{"jsonrpc":"2.0","id":"tools","method":"tools/call","params":{"name":"compat"}}`)
			tools := readCompatibilityResponse(t, lines, `"id":"tools"`)
			if !strings.Contains(tools, `"text":"`+testCase.engineMode+`"`) {
				t.Fatalf("tools response = %s stderr=%s", tools, stderr.String())
			}

			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			wait := make(chan error, 1)
			go func() { wait <- command.Wait() }()
			select {
			case err := <-wait:
				if err != nil {
					t.Fatalf("launcher exit: %v stderr=%s", err, stderr.String())
				}
				finished = true
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				select {
				case <-wait:
					finished = true
				case <-time.After(time.Second):
				}
				t.Fatalf("launcher termination exceeded bound stderr=%s", stderr.String())
			}

			attestation, err := os.ReadFile(attestationFile)
			if err != nil {
				t.Fatal(err)
			}
			wantAttestation := testCase.launcherMode == "new" && testCase.engineMode == "new"
			if got := strings.TrimSpace(string(attestation)) == "true"; got != wantAttestation {
				t.Fatalf("private attestation = %t, want %t; raw=%q stderr=%s", got, wantAttestation, attestation, stderr.String())
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(attestationFile)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != 0o600 {
					t.Fatalf("attestation file mode = %o, want 600", got)
				}
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			entries := strings.Fields(string(data))
			if len(entries) != 1 {
				t.Fatalf("child authority ledger = %q, want exactly one PID", data)
			}
			var childPID int
			if _, err := fmt.Sscan(entries[0], &childPID); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for !processGone(childPID) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !processGone(childPID) {
				t.Fatalf("child PID %d survived launcher termination", childPID)
			}
		})
	}
}

func buildCompatibilityFixture(t *testing.T, name string) string {
	t.Helper()
	executable := name
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	output := filepath.Join(t.TempDir(), executable)
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "build", "-o", output, "./supervisor/testdata/"+name)
	command.Dir = moduleRoot
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, result)
	}
	return output
}

func scanCompatibilityLines(reader io.Reader, lines chan<- string) {
	defer close(lines)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		lines <- scanner.Text()
	}
}

func writeCompatibilityLine(t *testing.T, writer io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(writer, line); err != nil {
		t.Fatal(err)
	}
}

func readCompatibilityResponse(t *testing.T, lines <-chan string, match string) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("launcher stdout closed before %s", match)
			}
			if strings.Contains(line, "$/mcp-mux/launcher/") {
				t.Fatalf("private lifecycle frame leaked to host stdout: %s", line)
			}
			if strings.Contains(line, match) {
				return line
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for %s", match)
		}
	}
}
