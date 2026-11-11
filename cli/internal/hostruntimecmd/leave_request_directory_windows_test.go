//go:build windows

package hostruntimecmd

import (
	"os"
	"path/filepath"
	"testing"
)

// The elevated helper starts in its request directory; while that is its working directory, the directory cannot
// be removed (a winget server it spawned held it open after uninstall).
func TestLeaveRequestDirectoryReleasesWorkingDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	request := filepath.Join(t.TempDir(), "request-test")
	if err := os.Mkdir(request, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(request); err != nil {
		t.Fatal(err)
	}
	if os.Remove(request) == nil {
		t.Fatal("the working directory was removable before the test left it")
	}
	if err := leaveRequestDirectory(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(request); err != nil {
		t.Fatalf("request directory still held after leaving it: %v", err)
	}
}
