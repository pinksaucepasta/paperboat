//go:build windows

package config

import (
	"time"

	"golang.org/x/sys/windows"
)

// processStartedAfter reports whether pid started after t (plus processStartSlack). Unknown is false: the
// caller then trusts the PID.
func processStartedAfter(pid int, t time.Time) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return false
	}
	return time.Unix(0, created.Nanoseconds()).After(t.Add(processStartSlack))
}
