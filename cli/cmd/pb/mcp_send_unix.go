//go:build darwin || linux

package main

import (
	"os"
	"syscall"
)

func openMCPSendSource(root *os.Root, path string) (*os.File, error) {
	// A FIFO must not block open before the caller can reject non-regular files.
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
