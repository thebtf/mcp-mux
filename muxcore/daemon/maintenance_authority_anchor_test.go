package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestMaintenanceAuthoritySurvivesConfigRootChanges(t *testing.T) {
	for _, state := range []control.MaintenanceState{control.MaintenanceHeld, control.MaintenanceRetirementBlocked} {
		for _, change := range []string{"relocated", "unset"} {
			t.Run(string(state)+"/"+change, func(t *testing.T) {
				oldConfig, newConfig := t.TempDir(), t.TempDir()
				vars := []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"}
				env := make(map[string]string, len(vars))
				for _, name := range vars {
					t.Setenv(name, oldConfig)
					env[name] = oldConfig
				}
				// A short, private endpoint directory avoids Unix socket length
				// limits and keeps every authority write out of host user config.
				base := maintenanceAnchorTempDir(t, "mux-ledger-")
				endpoint := filepath.Join(base, "control.sock")
				cfg := Config{ControlPath: endpoint, Namespace: "stable-ledger", SkipSnapshot: true, Logger: testLogger(t)}
				d, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { d.shutdown(nil) })
				// Pin the owner's environment: only the daemon/caller's config
				// roots change, not the held request's execution context.
				req := control.Request{Command: "maintenance-anchor-cache-only", Mode: "global", Cwd: base, Env: env}
				d.updateTemplate(req.Command, req.Args, daemonMaterializationSnapshot(false))
				_, sid, _, err := d.Spawn(req)
				if err != nil {
					t.Fatal(err)
				}
				held, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(600000)})
				if err != nil || held.State != control.MaintenanceHeld || !held.TreesRetired {
					t.Fatalf("persist retired HELD authority: %+v %v", held, err)
				}
				d.shutdown(nil)
				lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if state == control.MaintenanceRetirementBlocked {
					// Model durable uncertain retirement, without converting it
					// back to safe HELD during recovery or granting a release.
					d.maintenanceGate.Lock()
					lease := d.maintenanceLeases[held.HoldID]
					updated := *lease
					updated.result.State = state
					updated.result.TreesRetired = false
					err = d.commitMaintenanceLeaseLocked(lease, &updated)
					d.maintenanceGate.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
				ledgerBefore, err := os.ReadFile(d.maintenancePath)
				if err != nil {
					t.Fatal(err)
				}
				transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
				if err != nil {
					t.Fatal(err)
				}
				root := newConfig
				if change == "unset" {
					root = ""
				}
				for _, name := range vars {
					t.Setenv(name, root)
				}
				wantActivation := control.ErrMaintenanceHeld
				if state == control.MaintenanceRetirementBlocked {
					wantActivation = control.ErrMaintenanceRetirementBlocked
				}
				if err := CheckMaintenanceForActivation(cfg.Namespace, endpoint); !errors.Is(err, wantActivation) {
					t.Errorf("same-endpoint activation lost %s fence after config roots %s: %v", state, change, err)
				}
				recovered, err := New(cfg)
				if err != nil {
					t.Fatalf("same-endpoint recovery after config roots %s: %v", change, err)
				}
				t.Cleanup(func() { recovered.shutdown(nil) })
				if recovered.maintenancePath != d.maintenancePath || recovered.maintenanceScope != d.maintenanceScope || recovered.maintenanceLockPath != d.maintenanceLockPath {
					t.Error("config roots changed durable authority or namespace lock identity")
				}
				if _, _, _, err := recovered.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
					t.Errorf("same-endpoint daemon lost matching admission fence: %v", err)
				}
				states := recovered.maintenanceResults()
				if len(states) != 1 || states[0].HoldID != held.HoldID || states[0].State != state {
					t.Errorf("recovered durable authority: %+v", states)
				}
				recovered.HandleStatus()
				ledgerAfter, err := os.ReadFile(d.maintenancePath)
				if err != nil || !bytes.Equal(ledgerBefore, ledgerAfter) {
					t.Fatalf("startup/status/activation changed ledger: %v", err)
				}
				transactionAfter, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
				if err != nil || !bytes.Equal(transactionBefore, transactionAfter) {
					t.Fatalf("startup/status/activation changed transaction certificate: %v", err)
				}
				wantPath := filepath.Join(serverid.DaemonLockPath(filepath.Dir(canonicalMaintenancePath(endpoint)), cfg.Namespace)+".maintenance", d.maintenanceScope[3:], "ledger.json")
				if d.maintenancePath != wantPath {
					t.Fatalf("authority is not beside the canonical namespace lock: %s", d.maintenancePath)
				}
				for _, config := range []string{oldConfig, newConfig} {
					entries, err := os.ReadDir(config)
					if err != nil || len(entries) != 0 {
						t.Fatalf("authority touched caller config root %s: entries=%d err=%v", config, len(entries), err)
					}
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				if state == control.MaintenanceRetirementBlocked {
					if _, err := recovered.HandleMaintenance(control.Request{Cmd: "resume", HoldID: held.HoldID}); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
						t.Fatalf("uncertain durable authority granted release: %v", err)
					}
				} else {
					if released, err := recovered.HandleMaintenance(control.Request{Cmd: "resume", HoldID: held.HoldID}); err != nil || released.State != control.MaintenanceReleased {
						t.Fatalf("safe durable release: %+v %v", released, err)
					}
					recovered.updateTemplate(req.Command, req.Args, daemonMaterializationSnapshot(false))
					if _, _, _, err := recovered.Spawn(req); err != nil {
						t.Fatalf("safe durable clear did not admit matching request: %v", err)
					}
				}
				lock, err = ipc.AcquireFileLock(d.maintenanceLockPath)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				activationErr := CheckMaintenanceForActivation(cfg.Namespace, endpoint)
				if state == control.MaintenanceHeld && activationErr != nil || state == control.MaintenanceRetirementBlocked && !errors.Is(activationErr, control.ErrMaintenanceRetirementBlocked) {
					t.Fatalf("activation did not reflect only safe durable clear: %v", activationErr)
				}
			})
		}
	}
}

func maintenanceAnchorGuardFixture(t *testing.T) (*Daemon, string) {
	t.Helper()
	config := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(name, config)
	}
	base := maintenanceAnchorTempDir(t, "mux-guard-")
	parent := filepath.Join(base, "endpoint")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := secureMaintenancePath(parent, true); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(parent, "control.sock")
	d := &Daemon{namespace: "anchor-guard"}
	if err := d.loadMaintenance(endpoint); err != nil {
		t.Fatal(err)
	}
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	now := time.Now().UTC()
	lease := &maintenanceLease{record: maintenanceRecord{HoldID: "guard-fixture", Keys: []string{maintenanceDigest("guard-context")}}, result: control.MaintenanceResult{HoldID: "guard-fixture", State: control.MaintenanceHeld, ExpiresAt: now.Add(10 * time.Minute), DrainDeadline: now, TreesRetired: true}}
	if err := d.persistMaintenanceLocked(map[string]*maintenanceLease{lease.result.HoldID: lease}); err != nil {
		t.Fatal(err)
	}
	return d, endpoint
}

func maintenanceAnchorGuardRefuses(t *testing.T, d *Daemon, endpoint string) {
	t.Helper()
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, _, _, err := readMaintenanceAuthority(d.namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Errorf("unsafe filesystem authority was read: %v", err)
	}
	recovered, err := New(Config{ControlPath: endpoint, Namespace: d.namespace, SkipSnapshot: true, Logger: testLogger(t)})
	if recovered != nil {
		t.Cleanup(func() { recovered.shutdown(nil) })
	}
	if !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Errorf("unsafe filesystem authority admitted daemon startup: %v", err)
	}
	if err := CheckMaintenanceForActivation(d.namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Errorf("unsafe filesystem authority admitted activation: %v", err)
	}
	if err := writeMaintenanceLedger(d.maintenancePath, []byte("must-not-publish")); err == nil {
		t.Error("writer adopted unsafe filesystem authority")
	}
}

func TestMaintenanceAuthorityRejectsSymlinks(t *testing.T) {
	for _, member := range []string{"anchor", "scope", "ledger", "transaction"} {
		t.Run(member, func(t *testing.T) {
			d, endpoint := maintenanceAnchorGuardFixture(t)
			path := d.maintenancePath
			switch member {
			case "anchor":
				path = filepath.Dir(filepath.Dir(path))
			case "scope":
				path = filepath.Dir(path)
			case "transaction":
				path = maintenanceTransactionPath(path)
			}
			preserved := path + ".preserved"
			if member == "ledger" || member == "transaction" {
				preserved = filepath.Join(filepath.Dir(path), ".ledger-preserved-"+member)
			}
			if err := os.Rename(path, preserved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(preserved, path); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("Windows symlink creation unavailable: %v", err)
				}
				t.Fatal(err)
			}
			maintenanceAnchorGuardRefuses(t, d, endpoint)
			if target, err := os.Readlink(path); err != nil || target != preserved {
				t.Fatalf("validation/writer replaced or changed rejected link: target=%s err=%v", target, err)
			}
		})
	}
}

func maintenanceAnchorTempDir(t *testing.T, prefix string) string {
	t.Helper()
	// Only verification fixtures have this explicit endpoint-root override.
	// Production authority discovery has no environment override or fallback.
	base, err := os.MkdirTemp(os.Getenv("MCP_MUX_MAINTENANCE_TEST_ROOT"), prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	return base
}

func TestMaintenanceAuthorityPreservesTrustedEndpointAlias(t *testing.T) {
	for _, variant := range []string{"absolute", "relative_dotdot"} {
		t.Run(variant, func(t *testing.T) {
			d, endpoint := maintenanceAnchorGuardFixture(t)
			base := maintenanceAnchorTempDir(t, "mux-alias-")
			alias := filepath.Join(base, "trusted")
			target := filepath.Dir(endpoint)
			if variant == "relative_dotdot" {
				var err error
				target, err = filepath.Rel(base, target)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			maintenanceAnchorTrustedAliasPreserves(t, d, endpoint, filepath.Join(alias, filepath.Base(endpoint)))
		})
	}
}

func maintenanceAnchorTrustedAliasPreserves(t *testing.T, d *Daemon, endpoint, aliasedEndpoint string) {
	t.Helper()
	aliasParent, err := os.Stat(filepath.Dir(aliasedEndpoint))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.Stat(filepath.Dir(endpoint))
	if err != nil || !os.SameFile(aliasParent, parent) {
		t.Fatalf("alias did not reach the actual endpoint directory: %v", err)
	}
	if canonicalMaintenancePath(aliasedEndpoint) != canonicalMaintenancePath(endpoint) {
		t.Fatal("alias did not preserve canonical endpoint identity")
	}
	ledgerBefore, err := os.ReadFile(d.maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath(filepath.Dir(aliasedEndpoint), d.namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	path, scope, ledger, err := readMaintenanceAuthority(d.namespace, aliasedEndpoint)
	if err != nil || path != d.maintenancePath || scope != d.maintenanceScope || ledger == nil || len(ledger.Leases) != 1 {
		t.Fatalf("trusted original traversal changed authority: path=%s scope=%s ledger=%+v err=%v", path, scope, ledger, err)
	}
	if lease := ledger.Leases[0]; lease.HoldID != "guard-fixture" || lease.State != control.MaintenanceHeld || len(lease.Keys) != 1 || lease.Keys[0] != maintenanceDigest("guard-context") {
		t.Fatalf("trusted alias did not recover the known held authority: %+v", lease)
	}
	if err := CheckMaintenanceForActivation(d.namespace, aliasedEndpoint); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("trusted alias lost durable fence: %v", err)
	}
	ledgerAfter, err := os.ReadFile(d.maintenancePath)
	if err != nil || !bytes.Equal(ledgerBefore, ledgerAfter) {
		t.Fatalf("trusted alias read/activation changed ledger: %v", err)
	}
	transactionAfter, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
	if err != nil || !bytes.Equal(transactionBefore, transactionAfter) {
		t.Fatalf("trusted alias read/activation changed transaction certificate: %v", err)
	}
}
