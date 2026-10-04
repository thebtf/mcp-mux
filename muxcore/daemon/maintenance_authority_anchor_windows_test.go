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
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;;" + foreignGrant + ";;;WD)")
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
