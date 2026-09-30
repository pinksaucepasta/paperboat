//go:build linux

package deviceguard

import (
	"golang.org/x/sys/unix"
	"os"
	"testing"
)

func TestDeviceHostsPreservesUnixSecurityMetadata(t *testing.T) {
	path := testHostsFile(t)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, "user.paperboat-test", []byte("preserve me"), 0); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := updateDeviceHosts(t.Context(), path, map[string]string{"hp.pprbt": "127.100.0.4"}); err != nil {
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
}
