//go:build darwin

package session

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// processStartTime reads the process start time via sysctl(kern.proc.pid).
func processStartTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	tv := kp.Proc.P_starttime
	return strconv.FormatInt(tv.Sec, 10) + "." + strconv.FormatInt(int64(tv.Usec), 10), nil
}
