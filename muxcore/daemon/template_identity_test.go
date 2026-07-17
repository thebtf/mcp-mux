package daemon

import (
	"io"
	"log"
	"os"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/classify"
)

func TestTemplateCompatibilityUsesExactEnvAndIsolatedCwd(t *testing.T) {
	d := &Daemon{
		templateCache: make(map[string]*templateFamily),
		logger:        log.New(io.Discard, "", 0),
	}
	command := "identity-template"
	args := []string{"--serve"}
	env := map[string]string{
		"GITHUB_TOKEN":        "token-a",
		"SERVICE_CONFIG_PATH": "/cfg/a",
	}
	snap := daemonMaterializationSnapshot(false)
	snap.Classification = "shared"
	snap.Cwd = "/project/a"
	snap.Env = mergeEnv(env)
	d.updateTemplate(command, args, snap)

	compatible := mergeEnv(map[string]string{
		"GITHUB_TOKEN":        "token-a",
		"SERVICE_CONFIG_PATH": "/cfg/a",
		"PWD":                 "/other/transient",
	})
	if _, ok := d.getCompatibleTemplate(command, args, "/project/elsewhere", compatible); !ok {
		t.Fatal("shared template rejected exact env with transient differences")
	}
	changedCredential := mergeEnv(map[string]string{"GITHUB_TOKEN": "token-b", "SERVICE_CONFIG_PATH": "/cfg/a"})
	if _, ok := d.getCompatibleTemplate(command, args, "/project/a", changedCredential); ok {
		t.Fatal("template crossed changed credential boundary")
	}
	changedConfig := mergeEnv(map[string]string{"GITHUB_TOKEN": "token-a", "SERVICE_CONFIG_PATH": "/cfg/b"})
	if _, ok := d.getCompatibleTemplate(command, args, "/project/a", changedConfig); ok {
		t.Fatal("template crossed changed config boundary")
	}
	missingConfig := mergeEnv(map[string]string{"GITHUB_TOKEN": "token-a"})
	if _, ok := d.getCompatibleTemplate(command, args, "/project/a", missingConfig); ok {
		t.Fatal("template treated an incomplete exact projection as compatible")
	}

	snap.Classification = "isolated"
	d.updateTemplate(command, args, snap)
	if match, ok := d.getCompatibleTemplate(command, args, "/project/a", compatible); !ok || match.snapshot.Classification != "isolated" {
		t.Fatalf("isolated template exact context did not match: match=%+v ok=%v", match, ok)
	}
	if _, ok := d.getCompatibleTemplate(command, args, "/project/b", compatible); ok {
		t.Fatal("isolated template crossed canonical cwd boundary")
	}

	snap.Cwd = "/project/b"
	d.updateTemplate(command, args, snap)
	if _, ok := d.getCompatibleTemplate(command, args, "/project/a", compatible); !ok {
		t.Fatal("second isolated template replaced the first CWD entry")
	}
	if _, ok := d.getCompatibleTemplate(command, args, "/project/b", compatible); !ok {
		t.Fatal("second isolated template was not retained for its exact CWD")
	}
}

func TestTemplateIsolationBoundarySurvivesRelaxedFamilyPublish(t *testing.T) {
	for _, relaxed := range []classify.SharingMode{classify.ModeShared, classify.ModeSessionAware} {
		t.Run(string(relaxed), func(t *testing.T) {
			d := &Daemon{
				templateCache: make(map[string]*templateFamily),
				logger:        log.New(io.Discard, "", 0),
			}
			command := "template-isolation-boundary-" + string(relaxed)
			args := []string{"--serve"}
			cwdA, cwdB, cwdC := t.TempDir(), t.TempDir(), t.TempDir()
			env := map[string]string{"SERVICE_CONFIG_PATH": "/cfg/same"}
			effectiveEnv := mergeEnv(env)

			isolatedA := daemonMaterializationSnapshot(false)
			isolatedA.Classification = "isolated"
			isolatedA.Cwd = cwdA
			isolatedA.Env = mergeEnv(env)
			d.updateTemplate(command, args, isolatedA)

			relaxedB := isolatedA
			relaxedB.Classification = relaxed
			relaxedB.Cwd = cwdB
			d.updateTemplate(command, args, relaxedB)

			assertTemplate := func(cwd, want string) {
				t.Helper()
				match, ok := d.getCompatibleTemplate(command, args, cwd, effectiveEnv)
				if want == "" {
					if ok {
						t.Fatalf("template for %q = %q, want no compatible template", cwd, match.snapshot.Classification)
					}
					return
				}
				if !ok {
					t.Fatalf("template for %q missing, want classification %q", cwd, want)
				}
				if got := string(match.snapshot.Classification); got != want {
					t.Fatalf("template for %q classification=%q, want %q", cwd, got, want)
				}
			}

			assertTemplate(cwdA, "isolated")
			assertTemplate(cwdB, string(relaxed))
			assertTemplate(cwdC, string(relaxed))

			isolatedB := relaxedB
			isolatedB.Classification = "isolated"
			d.updateTemplate(command, args, isolatedB)

			assertTemplate(cwdA, "isolated")
			assertTemplate(cwdB, "isolated")
			assertTemplate(cwdC, "")
		})
	}
}

func TestSnapshotTemplatePublicationDoesNotMergeSuccessorEnvironment(t *testing.T) {
	const successorOnlyKey = "MCPMUX_RESTORE_CONFIG_PATH"
	original, existed := os.LookupEnv(successorOnlyKey)
	if err := os.Unsetenv(successorOnlyKey); err != nil {
		t.Fatalf("unset %s: %v", successorOnlyKey, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(successorOnlyKey, original)
		} else {
			_ = os.Unsetenv(successorOnlyKey)
		}
	})

	predecessorEnv := mergeEnv(nil)
	if _, leaked := predecessorEnv[successorOnlyKey]; leaked {
		t.Fatalf("predecessor fixture unexpectedly contains %s", successorOnlyKey)
	}
	if err := os.Setenv(successorOnlyKey, "successor-only"); err != nil {
		t.Fatalf("set %s: %v", successorOnlyKey, err)
	}

	d := &Daemon{
		templateCache: make(map[string]*templateFamily),
		logger:        log.New(io.Discard, "", 0),
	}
	snap := daemonMaterializationSnapshot(false)
	snap.Env = predecessorEnv
	d.updateTemplate("snapshot-template-env", nil, snap)

	if _, ok := d.getCompatibleTemplate("snapshot-template-env", nil, "/project", predecessorEnv); !ok {
		t.Fatal("snapshot template lost its captured predecessor environment")
	}
	successorEnv := mergeEnv(nil)
	if _, ok := d.getCompatibleTemplate("snapshot-template-env", nil, "/project", successorEnv); ok {
		t.Fatal("snapshot template inherited successor-only environment identity")
	}
}
