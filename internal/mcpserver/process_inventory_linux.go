//go:build linux

package mcpserver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func platformProcessSnapshot() ([]runtimeProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	processes := make([]runtimeProcess, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		name, parentPID, err := linuxProcessStat(pid)
		if err != nil {
			continue
		}
		exePath, _ := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if proc, ok := newRuntimeProcess(pid, parentPID, name, exePath); ok {
			processes = append(processes, proc)
		}
	}
	return processes, nil
}

func linuxProcessStat(pid int) (string, int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	line := string(data)
	left := strings.Index(line, "(")
	right := strings.LastIndex(line, ")")
	if left < 0 || right <= left {
		return "", 0, fmt.Errorf("invalid proc stat for pid %d", pid)
	}
	name := line[left+1 : right]
	fields := strings.Fields(line[right+1:])
	if len(fields) < 2 {
		return "", 0, fmt.Errorf("invalid proc stat fields for pid %d", pid)
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, err
	}
	return name, parentPID, nil
}
