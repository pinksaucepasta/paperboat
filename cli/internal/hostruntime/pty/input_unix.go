//go:build darwin || linux

package pty

import (
	"os"
	"time"
)

// WriteInput bounds a stalled terminal/exec input write without terminating its
// process. Callers serialize writes to the same descriptor.
func WriteInput(file *os.File, data []byte) (int, error) {
	return writeInput(file, data, 20*time.Second)
}
func writeInput(file *os.File, data []byte, timeout time.Duration) (int, error) {
	if err := file.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	defer file.SetWriteDeadline(time.Time{})
	return file.Write(data)
}
