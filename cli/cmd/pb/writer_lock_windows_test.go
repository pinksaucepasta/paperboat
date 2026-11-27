//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// holdWriterLock takes the diagnostic ring's lock as another pb process would.
func holdWriterLock(t *testing.T, directory string) func() {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(directory, "writer.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		t.Fatal(err)
	}
	return func() { _ = file.Close() }
}
