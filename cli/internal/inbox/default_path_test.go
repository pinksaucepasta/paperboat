package inbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrg/xdg"
)

func TestDefaultInboxReceivesOutsideProtectedUserFolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	previousDownloads := xdg.UserDirs.Download
	xdg.UserDirs.Download = filepath.Join(home, "Downloads")
	t.Cleanup(func() { xdg.UserDirs.Download = previousDownloads })
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(home, "Paperboat", "Inbox")
	if path != expected {
		t.Fatalf("default Inbox=%q; expected unprotected application folder %q", path, expected)
	}
	data := []byte("verified default-inbox delivery")
	receiver, err := New(Config{Client: &fakeClient{data: data}, MachineID: "machine_local", SessionID: "session_1", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Deliver(context.Background(), manifest("ft_default", "result.bin", data)); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(expected, "result.bin"))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("default Inbox did not receive verified file")
	}
	for _, protected := range []string{"Downloads", "Documents", "Desktop"} {
		if _, err := os.Lstat(filepath.Join(home, protected)); !os.IsNotExist(err) {
			t.Fatalf("default delivery touched protected %s folder", protected)
		}
	}
}
