//go:build !darwin && !linux

package session

import "fmt"

// processStartTime is unavailable on platforms shtrace does not target, so
// callers fall back to starting a fresh session.
func processStartTime(pid int) (string, error) {
	return "", fmt.Errorf("process start time unsupported on this platform")
}
