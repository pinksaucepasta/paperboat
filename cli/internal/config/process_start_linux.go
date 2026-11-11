//go:build linux

package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// linuxUserHZ is the kernel's fixed USER_HZ for the tick fields of /proc/<pid>/stat.
const linuxUserHZ = 100

// processStartedAfter reports whether pid started after t (plus processStartSlack). Unknown is false: the
// caller then trusts the PID, as it did before start times were compared.
func processStartedAfter(pid int, t time.Time) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	boot := linuxBootTime()
	started := linuxProcessStart(string(stat), boot)
	return !started.IsZero() && started.After(t.Add(processStartSlack))
}

func linuxBootTime() time.Time {
	data, _ := os.ReadFile("/proc/stat")
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			seconds, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			return time.Unix(seconds, 0)
		}
	}
	return time.Time{}
}

// linuxProcessStart reads field 22 of /proc/<pid>/stat (ticks since boot); the command name may contain
// spaces and parentheses, so parsing starts after the last ')'.
func linuxProcessStart(stat string, boot time.Time) time.Time {
	index := strings.LastIndexByte(stat, ')')
	if index < 0 || boot.IsZero() {
		return time.Time{}
	}
	fields := strings.Fields(stat[index+1:])
	if len(fields) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return boot.Add(time.Duration(ticks) * time.Second / linuxUserHZ)
}
