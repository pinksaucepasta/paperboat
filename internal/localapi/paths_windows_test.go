//go:build windows

package localapi

import "testing"

func TestWindowsPathsIsolatesNamedPipeByOwnerSID(t *testing.T) {
	root := `C:\Users\owner\AppData\Local\Paperboat\state`
	first, err := WindowsPaths(root, "S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	second, err := WindowsPaths(root, "S-1-5-21-1-2-3-1002")
	if err != nil {
		t.Fatal(err)
	}
	if first.SocketPath == second.SocketPath {
		t.Fatalf("different OS users share local API pipe %q", first.SocketPath)
	}
	repeat, err := WindowsPaths(root, "S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	if repeat.SocketPath != first.SocketPath {
		t.Fatalf("owner pipe is unstable: %q != %q", repeat.SocketPath, first.SocketPath)
	}
}

func TestWindowsPathsRejectsInvalidOwner(t *testing.T) {
	if _, err := WindowsPaths(`C:\state`, "not-a-sid"); err == nil {
		t.Fatal("invalid owner SID accepted")
	}
}
