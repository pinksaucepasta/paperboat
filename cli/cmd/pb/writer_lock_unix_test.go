//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// holdWriterLock takes the diagnostic ring's lock as another pb process would.
func holdWriterLock(t *testing.T, directory string) func() {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(directory, "writer.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { _ = file.Close() }
}
