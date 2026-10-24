//go:build windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"golang.org/x/sys/windows"
)

func maintenanceAnchorWindowsGrant(t *testing.T, path, foreignGrant string) string {
	t.Helper()
	return maintenanceAnchorWindowsGrantACE(t, path, "(A;;"+foreignGrant+";;;WD)")
}

func maintenanceAnchorWindowsGrantACE(t *testing.T, path, foreignACE string) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")" + foreignACE)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	return maintenanceAnchorWindowsSecurity(t, path)
}

func maintenanceAnchorWindowsSecurity(t *testing.T, path string) string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sddl := descriptor.String()
	if sddl == "" {
		t.Fatal("could not encode the actual filesystem security descriptor")
	}
	return sddl
}

func TestMaintenanceAuthorityRejectsUnsafeWindowsACLs(t *testing.T) {
	for _, member := range []string{"parent", "ancestor", "anchor", "scope", "ledger", "transaction"} {
		t.Run(member, func(t *testing.T) {
			d, endpoint := maintenanceAnchorGuardFixture(t)
			path, grant := d.maintenancePath, "FA"
			switch member {
			case "parent":
				path, grant = filepath.Dir(endpoint), "DC"
			case "ancestor":
				path, grant = filepath.Dir(filepath.Dir(endpoint)), "DC"
			case "anchor":
				path = filepath.Dir(filepath.Dir(path))
			case "scope":
				path = filepath.Dir(path)
			case "transaction":
				path = maintenanceTransactionPath(path)
			}
			before := maintenanceAnchorWindowsGrant(t, path, grant)
			maintenanceAnchorGuardRefuses(t, d, endpoint)
			if after := maintenanceAnchorWindowsSecurity(t, path); after != before {
				t.Fatalf("rejected object ACL/owner was adopted or repaired: before=%s after=%s", before, after)
			}
		})
	}
}

func TestMaintenanceAuthorityColdReadRejectsUnsafeWindowsParentWithoutWrites(t *testing.T) {
	for _, grant := range []string{"DC", "WD", "WO"} {
		t.Run(grant, func(t *testing.T) {
			base := maintenanceAnchorTempDir(t, "mux-cold-")
			before := maintenanceAnchorWindowsGrant(t, base, grant)
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
			if after := maintenanceAnchorWindowsSecurity(t, base); after != before {
				t.Fatalf("read-only rejection changed parent ACL/owner: before=%s after=%s", before, after)
			}
		})
	}
}

func TestMaintenanceActivationWindowsEndpointBinding(t *testing.T) {
	for _, test := range []struct {
		name          string
		endpointGrant string
		parentGrant   string
		wantErr       error
	}{
		{name: "protected_deletable_endpoint", endpointGrant: "0x1301bf"},
		{name: "protected_non_deletable_endpoint", endpointGrant: "0x1201bf"},
		{name: "create_only_parent_non_deletable_endpoint", endpointGrant: "0x1201bf", parentGrant: "0x4"},
		{name: "replaceable_deletable_endpoint", endpointGrant: "0x1301bf", parentGrant: "0x4", wantErr: control.ErrMaintenancePersistenceFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := maintenanceAnchorTempDir(t, "mux-binding-")
			if err := secureMaintenancePath(base, true); err != nil {
				t.Fatal(err)
			}
			parentSecurity := maintenanceAnchorWindowsSecurity(t, base)
			if test.parentGrant != "" {
				parentSecurity = maintenanceAnchorWindowsGrantACE(t, base, "(A;;"+test.parentGrant+";;;BU)")
			}
			directory := filepath.Join(base, "endpoint")
			if err := createMaintenanceDirectory(directory); err != nil {
				t.Fatal(err)
			}
			endpointSecurity := maintenanceAnchorWindowsGrantACE(t, directory, "(A;OICI;"+test.endpointGrant+";;;BU)")
			const namespace = "activation-binding"
			lockPath := serverid.DaemonLockPath(directory, namespace)
			const lockContent = "existing namespace lock\n"
			if err := os.WriteFile(lockPath, []byte(lockContent), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(lockPath)
			if err != nil || !before.Mode().IsRegular() {
				t.Fatalf("fixture namespace lock is not an existing regular file: %v", err)
			}
			lock, err := ipc.AcquireFileLock(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := CheckMaintenanceForActivation(namespace, filepath.Join(directory, "control.sock")); !errors.Is(err, test.wantErr) {
				t.Errorf("activation with endpoint grant %s and incoming-parent grant %q: got %v, want %v", test.endpointGrant, test.parentGrant, err, test.wantErr)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			activation, err := AcquireMaintenanceForActivation(namespace, filepath.Join(directory, "control.sock"))
			if !errors.Is(err, test.wantErr) {
				t.Errorf("guarded activation with endpoint grant %s and incoming-parent grant %q: got %v, want %v", test.endpointGrant, test.parentGrant, err, test.wantErr)
			}
			if activation != nil {
				if err := activation.Close(); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(lockPath) {
				t.Fatalf("read-only activation created authority: entries=%d err=%v", len(entries), err)
			}
			after, err := os.Lstat(lockPath)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("read-only activation replaced the namespace lock: %v", err)
			}
			data, err := os.ReadFile(lockPath)
			if err != nil || string(data) != lockContent {
				t.Fatalf("read-only activation changed namespace lock bytes: %v", err)
			}
			if after := maintenanceAnchorWindowsSecurity(t, directory); after != endpointSecurity {
				t.Fatal("activation changed endpoint ACL/owner")
			}
			if after := maintenanceAnchorWindowsSecurity(t, base); after != parentSecurity {
				t.Fatal("activation changed incoming-parent ACL/owner")
			}
		})
	}
}

func TestMaintenanceActivationWindowsMissingLockDoesNotCreateAuthority(t *testing.T) {
	base := maintenanceAnchorTempDir(t, "mux-no-lock-")
	if err := secureMaintenancePath(base, true); err != nil {
		t.Fatal(err)
	}
	before := maintenanceAnchorWindowsSecurity(t, base)
	endpoint := filepath.Join(base, "control.sock")
	if err := CheckMaintenanceForActivation("missing-lock", endpoint); err != nil {
		t.Fatalf("compatible read-only check rejected private absent authority: %v", err)
	}
	activation, err := AcquireMaintenanceForActivation("missing-lock", endpoint)
	if activation != nil {
		_ = activation.Close()
		t.Fatal("activation admitted a missing namespace lock")
	}
	if err == nil {
		t.Fatal("activation did not reject the missing namespace lock")
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing-lock refusal created namespace state: entries=%d err=%v", len(entries), err)
	}
	if maintenanceAnchorWindowsSecurity(t, base) != before {
		t.Fatal("missing-lock refusal changed directory security")
	}
}

func TestMaintenanceActivationWindowsRetainsBindingAndOldWriterContention(t *testing.T) {
	d, endpoint := maintenanceAnchorGuardFixture(t)
	directory := filepath.Dir(endpoint)
	maintenanceAnchorWindowsGrantACE(t, directory, "(A;OICI;0x1301bf;;;BU)")
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		t.Fatal(err)
	}
	err = d.persistMaintenanceLocked(map[string]*maintenanceLease{})
	closeErr := lock.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("writer failed under protected writable endpoint: %v %v", err, closeErr)
	}
	recovered := &Daemon{namespace: d.namespace}
	if err := recovered.loadMaintenance(endpoint); err != nil || len(recovered.maintenanceLeases) != 0 {
		t.Fatalf("cold recovery rejected the same clear authority: %v", err)
	}
	oldWriter, err := os.OpenFile(d.maintenanceLockPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer oldWriter.Close()
	oldHandle := windows.Handle(oldWriter.Fd())
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(oldHandle, flags, 0, 1, 0, &windows.Overlapped{}); err != nil {
		t.Fatal(err)
	}
	activation, err := AcquireMaintenanceForActivation(d.namespace, endpoint)
	if activation != nil {
		_ = activation.Close()
	}
	if !errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("old writer produced sharing failure or lost contention: %v", err)
	}
	if err := windows.UnlockFileEx(oldHandle, 0, 1, 0, &windows.Overlapped{}); err != nil {
		t.Fatal(err)
	}
	activation, err = AcquireMaintenanceForActivation(d.namespace, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer activation.Close()
	if err := os.Rename(directory, directory+".moved"); err == nil {
		t.Fatal("active guard allowed endpoint replacement")
	}
	if err := os.Rename(d.maintenanceLockPath, d.maintenanceLockPath+".moved"); err == nil {
		t.Fatal("active guard allowed namespace-lock replacement")
	}
	if err := windows.LockFileEx(oldHandle, flags, 0, 1, 0, &windows.Overlapped{}); !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		if err == nil {
			_ = windows.UnlockFileEx(oldHandle, 0, 1, 0, &windows.Overlapped{})
		}
		t.Fatalf("activation did not contend with the existing writable handle: %v", err)
	}
	if err := oldWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := activation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+".moved"); err != nil {
		t.Fatalf("Close retained a directory guard: %v", err)
	}
}

func TestMaintenanceActivationWindowsRejectsNamespaceLockReparse(t *testing.T) {
	d, endpoint := maintenanceAnchorGuardFixture(t)
	parent, err := openMaintenanceWindowsPath(filepath.Dir(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(parent)
	target := d.maintenanceLockPath + ".original"
	if err := os.Rename(d.maintenanceLockPath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, d.maintenanceLockPath); err != nil {
		t.Fatal(err)
	}
	targetLock, err := ipc.AcquireFileLock(target)
	if err != nil {
		t.Fatal(err)
	}
	defer targetLock.Close()
	// Replace the child after its parent is pinned, before the relative open.
	child, childErr := openMaintenanceWindowsChild(parent, filepath.Base(d.maintenanceLockPath), false, false, nil)
	if childErr == nil {
		var info windows.ByHandleFileInformation
		infoErr := windows.GetFileInformationByHandle(child, &info)
		closeErr := windows.CloseHandle(child)
		if infoErr != nil || closeErr != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			t.Fatalf("relative child open followed the replacement reparse target: %v %v", infoErr, closeErr)
		}
	}
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if lock != nil {
		_ = lock.Close()
		t.Fatal("writer followed a namespace-lock reparse point")
	}
	if err == nil || errors.Is(err, ipc.ErrFileLocked) {
		t.Fatalf("writer reached the reparse target instead of rejecting the link: %v", err)
	}
	activation, err := AcquireMaintenanceForActivation(d.namespace, endpoint)
	if activation != nil {
		_ = activation.Close()
		t.Fatal("activation followed a namespace-lock reparse point")
	}
	if !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("activation reached the reparse target instead of rejecting the link: %v", err)
	}
	if err := targetLock.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err = ipc.AcquireFileLock(target)
	if err != nil {
		t.Fatalf("refused opening retained a lock on the reparse target: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceActivationWindowsWritableEndpointKeepsFences(t *testing.T) {
	for _, state := range []string{"held", "malformed-pair", "missing-transaction", "DC", "WD", "WO", "replaceable-parent"} {
		t.Run(state, func(t *testing.T) {
			d, endpoint := maintenanceAnchorGuardFixture(t)
			grant := "(A;OICI;0x1301bf;;;BU)"
			want := error(control.ErrMaintenancePersistenceFailed)
			switch state {
			case "held":
				want = control.ErrMaintenanceHeld
			case "malformed-pair":
				if err := os.WriteFile(maintenanceTransactionPath(d.maintenancePath), []byte("not-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-transaction":
				if err := os.Remove(maintenanceTransactionPath(d.maintenancePath)); err != nil {
					t.Fatal(err)
				}
			case "replaceable-parent":
				maintenanceAnchorWindowsGrantACE(t, filepath.Dir(filepath.Dir(endpoint)), "(A;;0x4;;;WD)")
			default:
				grant += "(A;;" + state + ";;;BU)"
			}
			maintenanceAnchorWindowsGrantACE(t, filepath.Dir(endpoint), grant)
			activation, err := AcquireMaintenanceForActivation(d.namespace, endpoint)
			if activation != nil {
				_ = activation.Close()
				t.Fatal("unsafe or held authority admitted activation")
			}
			if !errors.Is(err, want) {
				t.Fatalf("activation refusal=%v, want %v", err, want)
			}
			lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
			if err != nil {
				t.Fatalf("refused activation retained the namespace lock: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMaintenanceActivationWindowsMemberReparseAfterValidation(t *testing.T) {
	for _, member := range []string{"ledger.json", "transaction.json"} {
		t.Run(member, func(t *testing.T) {
			d, endpoint := maintenanceAnchorGuardFixture(t)
			guard, _, err := openMaintenanceAuthority(d.namespace, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if present, err := guard.checkStorePaths(d.maintenancePath, false); err != nil || !present {
				t.Fatalf("fixture authority validation failed: present=%t err=%v", present, err)
			}
			path := filepath.Join(filepath.Dir(d.maintenancePath), member)
			if err := os.Rename(path, filepath.Join(filepath.Dir(path), ".ledger-preserved-"+member)); err != nil {
				t.Fatalf("validation retained an old member across replacement: %v", err)
			}
			target := d.maintenanceLockPath + ".member-target"
			if err := os.WriteFile(target, []byte("must-not-read"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := secureMaintenancePath(target, false); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if data, err := guard.readStoreMember(path); err == nil || len(data) != 0 {
				t.Fatalf("guarded read followed a member replaced after validation: bytes=%d err=%v", len(data), err)
			}
		})
	}
}
