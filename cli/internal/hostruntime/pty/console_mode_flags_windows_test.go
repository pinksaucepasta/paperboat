//go:build windows

package pty

import (
	"testing"

	"golang.org/x/sys/windows"
)

// The probe helper must ignore a Ctrl+C typed while it runs, or a real one makes the probe fail and the key read as
// unknown (d964a9877 review): it is started in its own process group.
func TestQueryHelperStartsInItsOwnProcessGroup(t *testing.T) {
	if queryHelperFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("the console mode helper does not start with CREATE_NEW_PROCESS_GROUP")
	}
	if queryHelperFlags&windows.EXTENDED_STARTUPINFO_PRESENT == 0 {
		t.Fatal("the console mode helper is not started with the pseudoconsole attribute list")
	}
}
