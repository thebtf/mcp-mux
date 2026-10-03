//go:build !windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
)

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
