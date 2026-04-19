//go:build linux

package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readPIDUID parses /proc/{pid}/status for the real UID of the process.
// Returns the real UID (field 1 of the "Uid:" line: real effUid saved fsUid).
func readPIDUID(pid int) (int, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line) // ["Uid:", realUid, effUid, savedUid, fsUid]
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected Uid line: %q", line)
		}
		uid, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, fmt.Errorf("parse uid: %w", err)
		}
		return uid, nil
	}
	return 0, fmt.Errorf("no Uid: line in %s", path)
}
