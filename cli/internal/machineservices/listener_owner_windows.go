//go:build windows

package machineservices

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// listenerOwner scans both TCP tables with the existing readTCPTable helper
// (discovery_windows.go) and names the smallest LISTEN pid found, so shared
// listeners report deterministically. QueryFullProcessImageName follows the
// pid-to-path pattern of machineguard/purge_windows.go; only the base name
// leaves this function.
func listenerOwner(port uint16) (string, int, bool) {
	var pids []uint32
	for _, family := range []uint32{windows.AF_INET, windows.AF_INET6} {
		data, err := readTCPTable(family)
		if err != nil {
			continue
		}
		pids = append(pids, listenPIDsOwnerPIDTable(data, family == windows.AF_INET6, port)...)
		if len(pids) >= maxListenerOwners {
			break
		}
	}
	if len(pids) == 0 {
		return "", 0, false
	}
	pid := pids[0]
	for _, p := range pids[1:] {
		if p < pid {
			pid = p
		}
	}
	// OpenProcess fails for another user's process or a pid that just exited:
	// the pid is still the answer, only the name is unknown.
	return processBaseName(pid), int(pid), true
}

func processBaseName(pid uint32) string {
	if pid == 4 { // the System process: HTTP.sys listens for IIS and other http.sys servers, and cannot be opened
		return "Windows HTTP service (HTTP.sys, for example IIS)"
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(process)
	buffer := make([]uint16, 1024)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil || size == 0 {
		return ""
	}
	base := filepath.Base(windows.UTF16ToString(buffer[:size]))
	if base == "." || base == string(filepath.Separator) || strings.ContainsRune(base, '\x00') {
		return ""
	}
	return base
}
