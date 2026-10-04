//go:build windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"golang.org/x/sys/windows"
)

func createMaintenanceDirectory(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	err = windows.CreateDirectory(name, &attributes)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil
	}
	return err
}

func validateMaintenanceParent(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return validateMaintenanceTraversal(path, 40, func(path string, info os.FileInfo, symbolic bool) error {
		return validateMaintenanceWindowsPath(path, user.User.Sid, info, true, false, symbolic)
	})
}

func validateMaintenancePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return validateMaintenanceWindowsPath(path, user.User.Sid, info, directory, true, false)
}

func validateMaintenanceWindowsPath(path string, user *windows.SID, info os.FileInfo, directory, private, symbolic bool) error {
	if symbolic && info.Mode()&os.ModeSymlink == 0 || !symbolic && (directory && !info.IsDir() || !directory && !info.Mode().IsRegular()) {
		return control.ErrMaintenancePersistenceFailed
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && !symbolic {
		return control.ErrMaintenancePersistenceFailed
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return control.ErrMaintenancePersistenceFailed
	}
	if !trustedMaintenanceSID(owner, user) {
		if private || symbolic {
			return control.ErrMaintenancePersistenceFailed
		}
		// The Windows servicing authority owns default OS ancestors such as
		// the volume root. Resolve that exact OS identity, not any service SID.
		installer, _, _, err := windows.LookupSID("", `NT SERVICE\TrustedInstaller`)
		if err != nil || !owner.Equals(installer) {
			return control.ErrMaintenancePersistenceFailed
		}
	}
	flags, _, err := descriptor.Control()
	if err != nil || private && flags&windows.SE_DACL_PROTECTED == 0 {
		return control.ErrMaintenancePersistenceFailed
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return control.ErrMaintenancePersistenceFailed
	}
	// FILE_DELETE_CHILD (0x40) bypasses the child's protected DACL. Ancestor
	// ownership and control rights must therefore be checked as well.
	dangerous := windows.ACCESS_MASK(windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.FILE_WRITE_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | 0x40)
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if !private && ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Header.AceSize < uint16(unsafe.Sizeof(windows.ACCESS_ALLOWED_ACE{})) {
				return control.ErrMaintenancePersistenceFailed
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			trusted := trustedMaintenanceSID(sid, user) || !private && !symbolic && sid.Equals(owner)
			if !sid.IsValid() || !trusted && (private && ace.Mask != 0 || !private && ace.Mask&dangerous != 0) {
				return control.ErrMaintenancePersistenceFailed
			}
		default:
			// Unknown/object/conditional ACEs are not proof of private authority.
			return control.ErrMaintenancePersistenceFailed
		}
	}
	return nil
}

func trustedMaintenanceSID(sid, user *windows.SID) bool {
	return sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

func secureMaintenancePath(path string, directory bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func replaceMaintenanceLedger(src, dst string) error {
	source, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
