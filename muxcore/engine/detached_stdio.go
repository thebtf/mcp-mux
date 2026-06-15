package engine

import (
	"fmt"
	"os"
	"os/exec"
)

func attachDetachedStdio(cmd *exec.Cmd) (func(), error) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s for detached daemon stdio: %w", os.DevNull, err)
	}
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	return func() { _ = devNull.Close() }, nil
}
