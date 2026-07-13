//go:build !windows

package upstream

import (
	"fmt"
	"os"
	"time"
)

func prepareStreamReadCancel(file *os.File) (cancel, clear, cleanup func() error, err error) {
	cancel = func() error {
		if err := file.SetReadDeadline(time.Now()); err != nil {
			return fmt.Errorf("set stream handoff deadline: %w", err)
		}
		return nil
	}
	clear = func() error {
		if err := file.SetReadDeadline(time.Time{}); err != nil {
			return fmt.Errorf("clear stream handoff deadline: %w", err)
		}
		return nil
	}
	cleanup = func() error { return nil }
	return cancel, clear, cleanup, nil
}
