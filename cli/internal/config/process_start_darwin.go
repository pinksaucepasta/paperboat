//go:build darwin

package config

import (
	"time"

	"golang.org/x/sys/unix"
)

// processStartedAfter reports whether pid started after t (plus processStartSlack). Unknown is false: the
// caller then trusts the PID.
func processStartedAfter(pid int, t time.Time) bool {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	started := time.Unix(int64(process.Proc.P_starttime.Sec), int64(process.Proc.P_starttime.Usec)*1000)
	return started.After(t.Add(processStartSlack))
}
