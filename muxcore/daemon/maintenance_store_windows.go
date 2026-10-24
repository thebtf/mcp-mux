//go:build windows

package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func validateMaintenancePath(path string, directory bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	handle, err := openMaintenanceWindowsPath(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	_, err = maintenanceWindowsAccess(handle, user.User.Sid, directory, true, false, false)
	return err
}

func maintenanceWindowsAccess(handle windows.Handle, user *windows.SID, directory, private, symbolic, endpoint bool) (uint32, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, err
	}
	reparse := info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	if reparse && !symbolic {
		return 0, windows.STATUS_REPARSE_POINT_ENCOUNTERED.Errno()
	}
	if symbolic != reparse || !symbolic && (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return 0, control.ErrMaintenancePersistenceFailed
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return 0, err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return 0, control.ErrMaintenancePersistenceFailed
	}
	if !trustedMaintenanceSID(owner, user) {
		if private || symbolic || endpoint {
			return 0, control.ErrMaintenancePersistenceFailed
		}
		// Exact Windows servicing authority is trusted only for strict ancestors.
		installer, _, _, err := windows.LookupSID("", `NT SERVICE\TrustedInstaller`)
		if err != nil || !owner.Equals(installer) {
			return 0, control.ErrMaintenancePersistenceFailed
		}
	}
	flags, _, err := descriptor.Control()
	if err != nil || private && flags&windows.SE_DACL_PROTECTED == 0 {
		return 0, control.ErrMaintenancePersistenceFailed
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return 0, control.ErrMaintenancePersistenceFailed
	}
	dangerous := windows.ACCESS_MASK(windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL | windows.GENERIC_WRITE | 0x40) // FILE_DELETE_CHILD
	if !endpoint {
		dangerous |= windows.DELETE | windows.FILE_WRITE_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES
	}
	var untrusted uint32
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return 0, err
		}
		if !private && ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Header.AceSize < uint16(unsafe.Sizeof(windows.ACCESS_ALLOWED_ACE{})) {
				return 0, control.ErrMaintenancePersistenceFailed
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsValid() {
				return 0, control.ErrMaintenancePersistenceFailed
			}
			trusted := trustedMaintenanceSID(sid, user) || !private && !symbolic && sid.Equals(owner)
			if !trusted {
				if private && ace.Mask != 0 || !private && ace.Mask&dangerous != 0 {
					return 0, control.ErrMaintenancePersistenceFailed
				}
				untrusted |= uint32(ace.Mask)
			}
		default:
			return 0, control.ErrMaintenancePersistenceFailed
		}
	}
	return untrusted, nil
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

type maintenancePathGuard struct {
	directories []windows.Handle
	endpoint    windows.Handle
	scope       windows.Handle
	user        *windows.SID
	lock        *os.File
	permissive  bool
}

func (g *maintenancePathGuard) Close() error {
	var err error
	if g.lock != nil {
		err = g.lock.Close()
		g.lock = nil
	}
	for index := len(g.directories) - 1; index >= 0; index-- {
		if closeErr := windows.CloseHandle(g.directories[index]); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	g.directories = nil
	g.endpoint, g.scope = 0, 0
	return err
}

func openMaintenanceWindowsPath(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE|windows.SYNCHRONIZE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// Every child name is resolved against the pinned parent, never a second full-path walk.
func openMaintenanceWindowsChild(parent windows.Handle, name string, directory, create bool, security *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/:`) {
		return 0, control.ErrMaintenancePersistenceFailed
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE, SecurityDescriptor: security}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	access := uint32(windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
		access |= windows.FILE_TRAVERSE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
		access |= windows.FILE_READ_DATA
	}
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_OPEN_IF
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access, &attributes, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition, options, 0, 0)
	if statusErr, ok := err.(windows.NTStatus); ok {
		err = statusErr.Errno()
	}
	return handle, err
}

func guardMaintenanceParent(path, lockName string) (*maintenancePathGuard, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	g := &maintenancePathGuard{user: user.User.Sid}
	if filepath.Clean(path) != filepath.FromSlash(path) {
		err = g.pinStrict(path)
	} else {
		err = g.pinOrdinary(path)
		if errors.Is(err, windows.STATUS_REPARSE_POINT_ENCOUNTERED.Errno()) {
			_ = g.Close()
			g.permissive = false
			err = g.pinStrict(path)
		}
	}
	if err != nil {
		_ = g.Close()
		return nil, err
	}
	handle, err := openMaintenanceWindowsChild(g.endpoint, lockName, false, false, nil)
	if err != nil {
		if !g.permissive && errors.Is(err, os.ErrNotExist) {
			return g, nil
		}
		_ = g.Close()
		return nil, err
	}
	g.lock = os.NewFile(uintptr(handle), filepath.Join(path, lockName))
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &info); err == nil && info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		err = control.ErrMaintenancePersistenceFailed
	}
	if err == nil {
		_, err = maintenanceWindowsAccess(g.endpoint, g.user, true, false, false, g.permissive)
	}
	if err == nil && g.permissive {
		// The nonempty-directory reparse exclusion is a local NTFS guarantee.
		var filesystem [16]uint16
		err = windows.GetVolumeInformationByHandle(g.endpoint, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem)))
		root, rootErr := windows.UTF16PtrFromString(filepath.VolumeName(path) + `\`)
		if rootErr != nil {
			err = rootErr
		} else if drive := windows.GetDriveType(root); drive != windows.DRIVE_FIXED && drive != windows.DRIVE_REMOVABLE || !strings.EqualFold(windows.UTF16ToString(filesystem[:]), "NTFS") {
			err = control.ErrMaintenancePersistenceFailed
		}
	}
	if err != nil {
		_ = g.Close()
		return nil, err
	}
	return g, nil
}

func (g *maintenancePathGuard) pinOrdinary(path string) error {
	if !filepath.IsAbs(path) {
		return control.ErrMaintenancePersistenceFailed
	}
	start := len(filepath.VolumeName(path)) + 1
	if start > len(path) {
		return control.ErrMaintenancePersistenceFailed
	}
	handle, err := openMaintenanceWindowsPath(path[:start])
	if err != nil {
		return err
	}
	g.directories = append(g.directories, handle)
	terminal := start == len(path)
	parentMask, err := maintenanceWindowsAccess(handle, g.user, true, false, false, terminal)
	if err != nil {
		return err
	}
	g.endpoint = handle
	if terminal {
		g.permissive = parentMask&(windows.DELETE|windows.FILE_WRITE_DATA|windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES) != 0
	}
	for start < len(path) {
		end := start
		for end < len(path) && !os.IsPathSeparator(path[end]) {
			end++
		}
		handle, err = openMaintenanceWindowsChild(g.endpoint, path[start:end], true, false, nil)
		if err != nil {
			return err
		}
		g.directories = append(g.directories, handle)
		terminal := end == len(path)
		mask, err := maintenanceWindowsAccess(handle, g.user, true, false, false, terminal)
		if err != nil {
			return err
		}
		// Colluding untrusted SIDs may remove the child and insert its replacement.
		if parentMask&0x4 != 0 && mask&windows.DELETE != 0 { // FILE_ADD_SUBDIRECTORY
			return control.ErrMaintenancePersistenceFailed
		}
		g.endpoint = handle
		parentMask = mask
		if terminal {
			g.permissive = mask&(windows.DELETE|windows.FILE_WRITE_DATA|windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES) != 0
		}
		start = end + 1
	}
	return nil
}

func (g *maintenancePathGuard) pinStrict(path string) error {
	// Existing trusted aliases retain the stricter, original traversal policy.
	err := validateMaintenanceTraversal(path, 40, func(path string, _ os.FileInfo, symbolic bool) error {
		handle, err := openMaintenanceWindowsPath(path)
		if err != nil {
			return err
		}
		g.directories = append(g.directories, handle)
		_, err = maintenanceWindowsAccess(handle, g.user, true, false, symbolic, false)
		return err
	})
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	handle, err := openMaintenanceWindowsPath(canonical)
	if err != nil {
		return err
	}
	g.directories = append(g.directories, handle)
	mask, err := maintenanceWindowsAccess(handle, g.user, true, false, false, true)
	if err != nil {
		return err
	}
	if mask&(windows.DELETE|windows.FILE_WRITE_DATA|windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES) != 0 {
		return control.ErrMaintenancePersistenceFailed
	}
	g.endpoint = handle
	return nil
}

func (g *maintenancePathGuard) openNamespaceLock() (*os.File, error) {
	if g.lock == nil {
		return nil, control.ErrMaintenancePersistenceFailed
	}
	file := g.lock
	g.lock = nil
	return file, nil
}

func (g *maintenancePathGuard) checkStorePaths(path string, create bool) (bool, error) {
	var security *windows.SECURITY_DESCRIPTOR
	if create {
		var err error
		security, err = windows.SecurityDescriptorFromString("O:" + g.user.String() + "D:P(A;OICI;FA;;;" + g.user.String() + ")")
		if err != nil {
			return false, err
		}
	}
	dir := filepath.Dir(path)
	parent := g.endpoint
	for _, name := range [2]string{filepath.Base(filepath.Dir(dir)), filepath.Base(dir)} {
		handle, err := openMaintenanceWindowsChild(parent, name, true, create, security)
		if err != nil {
			if !create && errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		g.directories = append(g.directories, handle)
		if _, err := maintenanceWindowsAccess(handle, g.user, true, true, false, false); err != nil {
			return false, err
		}
		parent = handle
	}
	g.scope = parent
	// Member handles close before publication so atomic ledger replacement still works.
	for _, name := range [3]string{filepath.Base(path), "ledger.json", "transaction.json"} {
		handle, err := openMaintenanceWindowsChild(parent, name, false, false, nil)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		_, checkErr := maintenanceWindowsAccess(handle, g.user, false, true, false, false)
		closeErr := windows.CloseHandle(handle)
		if err := errors.Join(checkErr, closeErr); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (g *maintenancePathGuard) readStoreMember(path string) ([]byte, error) {
	handle, err := openMaintenanceWindowsChild(g.scope, filepath.Base(path), false, false, nil)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	_, err = maintenanceWindowsAccess(handle, g.user, false, true, false, false)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(file)
	}
	return data, errors.Join(err, file.Close())
}

func (g *maintenancePathGuard) validateCanonicalParent(path string) error {
	handle, err := openMaintenanceWindowsPath(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var original, canonical windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(g.endpoint, &original); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(handle, &canonical); err != nil {
		return err
	}
	if canonical.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || original.VolumeSerialNumber != canonical.VolumeSerialNumber || original.FileIndexHigh != canonical.FileIndexHigh || original.FileIndexLow != canonical.FileIndexLow {
		return control.ErrMaintenancePersistenceFailed
	}
	return nil
}
