//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		return errors.Is(err, os.ErrPermission)
	}
	return !processZombie(pid) // kill(pid, 0) also succeeds for an exited process its parent never reaps
}
