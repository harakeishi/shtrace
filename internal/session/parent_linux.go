//go:build linux

package session

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// starttimeField is the 1-based index of `starttime` in /proc/<pid>/stat as
// documented in proc(5).
const starttimeField = 22

// processStartTime reads field 22 (starttime, in clock ticks since boot) from
// /proc/<pid>/stat.
//
// The comm field (2) is wrapped in parentheses and may itself contain spaces
// and parentheses, so fields are counted after the final ')' rather than by
// splitting the whole line.
func processStartTime(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	line := string(b)
	close := strings.LastIndex(line, ")")
	if close < 0 || close+2 > len(line) {
		return "", fmt.Errorf("malformed stat line for pid %d", pid)
	}
	// Fields after comm start at index 3 (state), so field N maps to
	// rest[N-3] for N >= 3.
	rest := strings.Fields(line[close+1:])
	idx := starttimeField - 3
	if idx >= len(rest) {
		return "", fmt.Errorf("stat for pid %d has %d fields after comm, need %d", pid, len(rest), idx+1)
	}
	return rest[idx], nil
}
