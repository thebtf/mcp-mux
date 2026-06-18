//go:build !windows && !linux

package mcpserver

func platformProcessSnapshot() ([]runtimeProcess, error) {
	return []runtimeProcess{}, errProcessSnapshotUnsupported
}
