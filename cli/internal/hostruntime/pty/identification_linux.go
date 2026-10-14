//go:build linux

package pty

import (
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func identifyForeground(group int) Identification {
	base := "/proc/" + strconv.Itoa(group)
	var result Identification
	file, err := os.Open(base + "/comm")
	if err != nil {
		return result
	}
	// Linux comm is limited by TASK_COMM_LEN. Retain a small explicit bound
	// even when procfs is unavailable or replaced by another mount.
	name, err := io.ReadAll(io.LimitReader(file, 257))
	_ = file.Close()
	if err == nil && len(name) <= 256 {
		result.ForegroundProcess = strings.TrimSuffix(string(name), "\n")
	}
	var directory [4096]byte
	n, err := unix.Readlink(base+"/cwd", directory[:])
	if err == nil && n < len(directory) {
		result.CurrentDirectory = string(directory[:n])
	}
	return result
}
