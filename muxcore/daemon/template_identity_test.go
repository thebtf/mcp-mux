package daemon

import (
	"io"
	"log"
	"testing"
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
	snap.Env = env
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
