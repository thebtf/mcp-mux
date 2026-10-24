//go:build !windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func createMaintenanceDirectory(path string) error {
	err := os.Mkdir(path, 0o700)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Only the directory just created by this writer may be normalized.
	if err := secureMaintenancePath(path, true); err != nil {
		return err
	}
	parent, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func validateMaintenanceParent(path string) error {
	return validateMaintenanceTraversal(path, 40, func(path string, info os.FileInfo, symbolic bool) error {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			return control.ErrMaintenancePersistenceFailed
		}
		if !symbolic && (!info.IsDir() || info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return control.ErrMaintenancePersistenceFailed
		}
		return nil
	})
}

func validateMaintenancePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != mode || directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return control.ErrMaintenancePersistenceFailed
	}
	return nil
}

func secureMaintenancePath(path string, directory bool) error {
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	return os.Chmod(path, mode)
}

func replaceMaintenanceLedger(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	dir, err := os.OpenFile(filepath.Dir(dst), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	return errors.Join(syncErr, dir.Close())
}

type maintenancePathGuard struct{ lockPath string }

func guardMaintenanceParent(path, lockName string) (*maintenancePathGuard, error) {
	if err := validateMaintenanceParent(path); err != nil {
		return nil, err
	}
	return &maintenancePathGuard{lockPath: filepath.Join(path, lockName)}, nil
}

func (g *maintenancePathGuard) Close() error { return nil }

func (g *maintenancePathGuard) openNamespaceLock() (*os.File, error) {
	return os.Open(g.lockPath)
}

func (g *maintenancePathGuard) checkStorePaths(path string, create bool) (bool, error) {
	if err := checkMaintenancePrivatePaths(path, create); err != nil {
		return false, err
	}
	_, err := os.Stat(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (g *maintenancePathGuard) readStoreMember(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (g *maintenancePathGuard) validateCanonicalParent(path string) error { return nil }
