//go:build !windows

package daemon

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestMaintenanceAuthorityRejectsInsecureUnixPaths(t *testing.T) {
	for _, member := range []string{"parent", "ancestor", "anchor", "scope", "ledger", "transaction"} {
		t.Run(member, func(t *testing.T) {
			d, endpoint := maintenanceAnchorGuardFixture(t)
			path := d.maintenancePath
			mode := os.FileMode(0o777)
			switch member {
			case "parent":
				path = filepath.Dir(endpoint)
			case "ancestor":
				path = filepath.Dir(filepath.Dir(endpoint))
			case "anchor":
				path = filepath.Dir(filepath.Dir(path))
			case "scope":
				path = filepath.Dir(path)
			case "ledger":
				mode = 0o666
			case "transaction":
				path, mode = maintenanceTransactionPath(path), 0o666
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			maintenanceAnchorGuardRefuses(t, d, endpoint)
			if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != mode {
				t.Fatalf("insecure existing object was repaired/adopted: %v", err)
			}
		})
	}
}

func TestMaintenanceAuthorityRejectsForeignUnixOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a fixture to a foreign UID requires root; non-root mode/symlink tests still run")
	}
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
			if err := os.Chown(path, 65534, -1); err != nil {
				t.Fatal(err)
			}
			maintenanceAnchorGuardRefuses(t, d, endpoint)
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Sys().(*syscall.Stat_t).Uid != 65534 {
				t.Fatal("foreign object ownership changed")
			}
		})
	}
}

func TestMaintenanceAuthorityColdReadRejectsUnsafeUnixParentWithoutWrites(t *testing.T) {
	base := maintenanceAnchorTempDir(t, "mux-cold-")
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(base, "control.sock")
	lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath(base, "cold-guard"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := CheckMaintenanceForActivation("cold-guard", endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("absent authority admitted under replaceable parent: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 || entries[0].Name() != "cold-guard-muxd.lock" {
		t.Fatalf("read-only rejection created authority: entries=%d err=%v", len(entries), err)
	}
	if info, err := os.Stat(base); err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("read-only rejection changed parent permissions: %v", err)
	}
}

func TestMaintenanceAuthorityRejectsForeignAliasRetarget(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("genuine foreign-UID alias retarget requires Linux root credentials and /proc/self/fd")
	}
	for _, variant := range []string{"direct", "trusted_outer_pre_dotdot"} {
		t.Run(variant, func(t *testing.T) {
			d, endpoint := maintenanceAnchorGuardFixture(t)
			ledgerBefore, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			transactionBefore, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			base := maintenanceAnchorTempDir(t, "mux-foreign-alias-")
			if err := os.Chmod(base, 0o777|os.ModeSticky); err != nil {
				t.Fatal(err)
			}
			directory, err := os.Open(base)
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			inner := filepath.Join(base, "foreign")
			alias := inner
			cold := maintenanceAnchorTempDir(t, "mux-cold-target-")
			targets := []string{filepath.Dir(endpoint), cold}
			resolvedTargets := targets
			if variant == "trusted_outer_pre_dotdot" {
				targets = []string{filepath.Dir(filepath.Dir(endpoint)), cold}
				resolvedTargets = []string{filepath.Dir(endpoint), filepath.Join(cold, "endpoint")}
				for _, path := range []string{filepath.Join(base, "endpoint"), filepath.Join(targets[0], "child"), filepath.Join(cold, "child"), resolvedTargets[1]} {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				outerBase := maintenanceAnchorTempDir(t, "mux-trusted-outer-")
				alias = filepath.Join(outerBase, "trusted")
				// Do not Join/Clean this payload: the foreign component must be
				// traversed before .. selects A/endpoint or B/endpoint.
				if err := os.Symlink(inner+"/../endpoint", alias); err != nil {
					t.Fatal(err)
				}
			}
			for index, target := range targets {
				if variant == "trusted_outer_pre_dotdot" {
					target = filepath.Join(target, "child")
				}
				// An actual foreign UID owns and retargets its entry. The
				// inherited directory FD confines it to this fixture even when
				// the surrounding proof root is private.
				command := exec.Command("/bin/ln", "-sfn", target, "/proc/self/fd/3/foreign")
				command.ExtraFiles = []*os.File{directory}
				command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("foreign UID failed its actual retarget operation: %v %s", err, output)
				}
				info, err := os.Lstat(inner)
				if err != nil || info.Sys().(*syscall.Stat_t).Uid != 65534 {
					t.Fatalf("inner alias was not actually owned by foreign UID: %v", err)
				}
				aliasedEndpoint := filepath.Join(alias, filepath.Base(endpoint))
				if canonicalMaintenancePath(aliasedEndpoint) != canonicalMaintenancePath(filepath.Join(resolvedTargets[index], filepath.Base(endpoint))) {
					t.Fatal("fixture did not reach the intended trusted A/B target")
				}
				if _, _, _, err := readMaintenanceAuthority(d.namespace, aliasedEndpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
					t.Errorf("foreign original traversal admitted A/B authority/absence: %v", err)
				}
				lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath(alias, d.namespace))
				if err != nil {
					t.Fatal(err)
				}
				activationErr := CheckMaintenanceForActivation(d.namespace, aliasedEndpoint)
				lock.Close()
				if !errors.Is(activationErr, control.ErrMaintenancePersistenceFailed) {
					t.Errorf("foreign A/B retarget admitted offline activation: %v", activationErr)
				}
				recovered := &Daemon{namespace: d.namespace}
				if err := recovered.loadMaintenance(aliasedEndpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) || recovered.maintenanceLockPath != "" {
					t.Errorf("foreign alias exposed daemon lock or recovery authority before validation: lock=%s err=%v", recovered.maintenanceLockPath, err)
				}
			}
			ledgerAfter, err := os.ReadFile(d.maintenancePath)
			if err != nil || !bytes.Equal(ledgerBefore, ledgerAfter) {
				t.Fatalf("foreign alias rejection mutated original held ledger: %v", err)
			}
			transactionAfter, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil || !bytes.Equal(transactionBefore, transactionAfter) {
				t.Fatalf("foreign alias rejection mutated original certificate: %v", err)
			}
		})
	}
}

func TestMaintenanceAuthorityPreservesTrustedEndpointAliasUnix(t *testing.T) {
	t.Run("owned_inner_pre_dotdot", func(t *testing.T) {
		d, endpoint := maintenanceAnchorGuardFixture(t)
		base := maintenanceAnchorTempDir(t, "mux-alias-")
		child := filepath.Join(filepath.Dir(filepath.Dir(endpoint)), "child")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatal(err)
		}
		inner := filepath.Join(base, "owned-inner")
		if err := os.Symlink(child, inner); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(base, "trusted")
		// Unix traverses the inner symlink before ..; Windows normalizes this
		// payload lexically, so it does not name the same endpoint there.
		if err := os.Symlink(inner+"/../endpoint", alias); err != nil {
			t.Fatal(err)
		}
		maintenanceAnchorTrustedAliasPreserves(t, d, endpoint, filepath.Join(alias, filepath.Base(endpoint)))
	})
}
