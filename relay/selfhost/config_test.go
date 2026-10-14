package selfhost

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadConfigPreservesProtectedInstallationAndHonorsLock(t *testing.T) {
	dir, _, _, server := testInstallation(t, true)
	server.Close()
	path := filepath.Join(dir, "selfhost.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original.Config) {
		t.Fatal("existing settings changed")
	}
	got.Components[0].TCPPort++
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("configuration read changed protected state")
	}
	lock, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(dir); err == nil {
		lock.Close()
		t.Fatal("read bypassed active state owner")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := ReadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, original.Config) {
		t.Fatal("returned configuration mutation persisted")
	}
	if _, err := ReadConfig(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Fatalf("missing installation: %v", err)
	}
}
