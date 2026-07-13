//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package upstream

import "fmt"

func watchAttachedLeader(_ *Process, pid int) (<-chan error, error) {
	return nil, fmt.Errorf("upstream: immutable leader watch is unsupported for pid %d on this platform", pid)
}
