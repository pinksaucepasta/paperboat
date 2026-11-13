//go:build windows

package main

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The workspace preference directory gets its own protected DACL naming the user, so an elevated session that created it
// cannot lock a normal console out of `pb connect` (X-24).
func TestWorkspaceRecordDirectoryHasProtectedUserACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json.workspaces", "record.json")
	if err := writeWorkspaceRecord(path, workspaceEnvironmentFile{Version: workspacePreferenceVersion, MachineID: "m1"}); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(filepath.Dir(path), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("workspace preference directory inherits its DACL (control %#x): the creating token decides who can write", control)
	}
}
