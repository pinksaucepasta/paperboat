//go:build linux

package machineguard

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestMachineHostsPreservesUnixSecurityMetadata(t *testing.T) {
	path := testHostsFile(t)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, "user.paperboat-test", []byte("preserve me"), 0); err != nil {
		t.Fatal(err)
	}
	owned := []byte(hostsStart + "\n127.100.0.4 hp.pprbt\n" + hostsEnd + "\n127.0.0.1 localhost\n")
	if err := os.WriteFile(path, owned, 0640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := removeOwnedMachineHosts(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if before.Mode() != after.Mode() {
		t.Fatal("hosts mode changed")
	}
	attrs, err := hostsAttributes(path)
	if err != nil || attrs["user.paperboat-test"] != "preserve me" {
		t.Fatalf("attributes lost: %v %v", attrs, err)
	}
	if got, err := os.ReadFile(filepath.Clean(path)); err != nil || string(got) != "127.0.0.1 localhost\n" {
		t.Fatalf("hosts cleanup result %q %v", got, err)
	}
}
