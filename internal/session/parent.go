package session

import (
	"fmt"
	"os"
	"strconv"
)

// ParentKey identifies the parent process of this invocation, so that repeated
// shtrace runs launched by one long-lived process (an AI agent that spawns a
// throwaway shell per command) can be grouped into a single session.
//
// The key pairs the pid with the parent's start time. A pid alone is not
// enough: the OS recycles pids, and a recycled one would silently merge an
// unrelated later process into an old session.
func ParentKey() (string, error) {
	return processKey(os.Getppid())
}

// processKey builds the "pid:starttime" key for pid. It returns an error when
// the start time cannot be read, which callers treat as "do not group".
func processKey(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	started, err := processStartTime(pid)
	if err != nil {
		return "", fmt.Errorf("process %d start time: %w", pid, err)
	}
	return strconv.Itoa(pid) + ":" + started, nil
}
