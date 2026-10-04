package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const engineShutdownAuthorityCase = "MCPMUX_ENGINE_SHUTDOWN_AUTHORITY_CASE"

func TestEngineRunShutdownAuthority(t *testing.T) {
	for _, scenario := range []string{"held", "failed-admission", "context-unfenced", "done-unfenced"} {
		t.Run(scenario, func(t *testing.T) {
			scratch := os.Getenv("GOTMPDIR")
			canonical := strings.ToLower(filepath.ToSlash(filepath.Clean(scratch)))
			if !filepath.IsAbs(scratch) || !strings.Contains(canonical, "/.agent/") || strings.Contains(canonical, "/.agent/worktrees/") {
				t.Skip("requires parent-supplied GOTMPDIR beneath primary .agent")
			}
			root, err := os.MkdirTemp(scratch, "ec-")
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "config")
			if err := os.Mkdir(config, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Logf("caller authority fixture: %s", root)
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEngineRunShutdownAuthorityHelper$")
			command.Env = append(os.Environ(), engineShutdownAuthorityCase+"="+scenario,
				"MCPMUX_ENGINE_SHUTDOWN_AUTHORITY_ROOT="+root,
				"APPDATA="+config, "XDG_CONFIG_HOME="+config, "HOME="+config, "USERPROFILE="+config,
				"TMP="+root, "TEMP="+root, "TMPDIR="+root)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("actual Run caller %s: %v\n%s", scenario, err, output)
			}
			t.Logf("%s", output)
		})
	}
}

type engineShutdownAuthorityLog struct {
	refused chan struct{}
	count   atomic.Int32
}

func (w *engineShutdownAuthorityLog) Write(data []byte) (int, error) {
	n, err := os.Stderr.Write(data)
	if strings.Contains(string(data), "daemon shutdown refused:") {
		w.count.Add(1)
		select {
		case w.refused <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestEngineRunShutdownAuthorityHelper(t *testing.T) {
	scenario := os.Getenv(engineShutdownAuthorityCase)
	if scenario == "" {
		return
	}
	root := os.Getenv("MCPMUX_ENGINE_SHUTDOWN_AUTHORITY_ROOT")
	const daemonFlag = "--test-caller-authority-daemon"
	os.Args = append(os.Args, daemonFlag)
	observed := &engineShutdownAuthorityLog{refused: make(chan struct{}, 1)}
	eng, err := New(Config{
		Name: "caller-authority", Namespace: "ec", BaseDir: root, DaemonFlag: daemonFlag,
		SessionHandler: noopSessionHandler{}, SkipSnapshot: true, IdleTimeout: time.Minute,
		ZeroSessionCleanupDelay: -1, Logger: log.New(observed, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- eng.Run(ctx) }()
	select {
	case <-eng.Ready():
	case err := <-runErr:
		t.Fatalf("Run returned before readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not become ready")
	}
	d := eng.Daemon()
	if d == nil || eng.Mode() != ModeDaemon {
		t.Fatal("actual Run did not enter daemon mode")
	}
	request := control.Request{Command: "caller-authority-native", Mode: "global", Cwd: root}
	var hold control.MaintenanceResult
	if scenario == "held" || scenario == "failed-admission" {
		_, sid, _, err := d.Spawn(request)
		if err != nil {
			t.Fatal(err)
		}
		entry := d.Entry(sid)
		if scenario == "failed-admission" {
			// Fail the first real store write at this daemon's actual endpoint-bound anchor.
			lockPath := serverid.DaemonLockPath(filepath.Dir(eng.ControlSocketPath()), eng.cfg.Namespace)
			if err := os.WriteFile(lockPath+".maintenance", []byte("unavailable store directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		hold, err = d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid})
		if scenario == "held" {
			if err != nil || hold.State != control.MaintenanceHeld || !hold.TreesRetired {
				t.Fatalf("hold: %+v %v", hold, err)
			}
		} else if !errors.Is(err, control.ErrMaintenancePersistenceFailed) || d.Entry(sid) != entry || entry.Owner.MaintenanceRetired() {
			t.Fatalf("failed write did not retain unretired authority: %+v %v", hold, err)
		}
	}

	if scenario == "done-unfenced" {
		if _, err := d.HandleShutdownWithError(0); err != nil {
			t.Fatal(err)
		}
	} else {
		cancel()
	}
	if scenario == "held" || scenario == "failed-admission" {
		select {
		case <-observed.refused:
		case err := <-runErr:
			t.Fatalf("Run exited before refused shutdown was observed: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("context cancellation did not reach the actual Shutdown caller")
		}
		select {
		case err := <-runErr:
			t.Fatalf("Run relinquished refused shutdown authority: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if eng.Daemon() != d || observed.count.Load() != 1 {
			t.Fatal("refused cancellation cleared the daemon reference or retried shutdown")
		}
		select {
		case <-d.Done():
			t.Fatal("refused cancellation falsely completed daemon retirement")
		default:
		}
		for _, cmd := range []string{"ping", "status"} {
			response, err := control.SendWithTimeout(eng.ControlSocketPath(), control.Request{Cmd: cmd}, time.Second)
			if err != nil || !response.OK {
				t.Fatalf("canceled Run stopped real control serving (%s): %+v %v", cmd, response, err)
			}
		}
		want := control.ErrMaintenanceHeld
		if scenario == "failed-admission" {
			want = control.ErrMaintenancePersistenceFailed
		}
		if _, _, _, err := d.Spawn(request); !errors.Is(err, want) {
			t.Fatalf("canceled Run lost fail-closed admission: %v", err)
		}
		if _, err := d.HandleShutdownWithError(0); !errors.Is(err, want) {
			t.Fatalf("canceled Run lost lifecycle refusal: %v", err)
		}
		if scenario == "failed-admission" {
			// This isolated helper has native owners only, no external process tree.
			// Its test process may finish; production Run must still own the daemon.
			fmt.Fprintln(os.Stderr, "verified failed-admission caller and service retention")
			return
		}
		response, err := control.SendWithTimeout(eng.ControlSocketPath(), control.Request{Cmd: "resume", HoldID: hold.HoldID}, time.Second)
		if err != nil || !response.OK {
			t.Fatalf("safe exact-lease release: %+v %v", response, err)
		}
		response, err = control.SendWithTimeout(eng.ControlSocketPath(), control.Request{Cmd: "shutdown"}, time.Second)
		if err != nil || !response.OK {
			t.Fatalf("explicit admitted shutdown: %+v %v", response, err)
		}
	}
	select {
	case err := <-runErr:
		if scenario == "done-unfenced" && err != nil || scenario != "done-unfenced" && !errors.Is(err, context.Canceled) {
			t.Fatalf("ordinary exit result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("actual Run did not exit after legitimate daemon completion")
	}
	select {
	case <-d.Done():
	default:
		t.Fatal("Run returned before actual daemon completion")
	}
	if eng.Daemon() != nil {
		t.Fatal("completed Run retained its retired daemon reference")
	}
	fmt.Fprintln(os.Stderr, "verified caller completion:", scenario)
}
