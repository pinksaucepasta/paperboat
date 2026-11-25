//go:build darwin || linux

package dnsfile

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/dnscap"
)

func TestAddressFileIsOwnerOnlyAndAForeignOneIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns.json")
	Open(path)
	dnscap.Verified("file.example.test", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 443})
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v, want 0600", info, err)
	}
	body := `{"HOST":{"address":"203.0.113.7","verified":"` + time.Now().UTC().Format(time.RFC3339) + `"}}`
	plant := func(host string, mode os.FileMode, link bool) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "dns.json")
		if err := os.WriteFile(file, []byte(strings.Replace(body, "HOST", host, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		if link {
			name := filepath.Join(t.TempDir(), "dns.json")
			if err := os.Symlink(file, name); err != nil {
				t.Fatal(err)
			}
			return name
		}
		return file
	}
	Open(plant("ours.example.test", 0o600, false))
	if !dnscap.Cached("ours.example.test").IsValid() {
		t.Fatal("an owner-only file was not loaded")
	}
	Open(plant("writable.example.test", 0o666, false))
	Open(plant("linked.example.test", 0o600, true))
	for _, host := range []string{"writable.example.test", "linked.example.test"} {
		if dnscap.Cached(host).IsValid() {
			t.Fatalf("%s was loaded", host)
		}
	}
	link := plant("through.example.test", 0o600, true)
	before, _ := os.ReadFile(link)
	Open(link)
	dnscap.Verified("new.example.test", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 8), Port: 443})
	if after, _ := os.ReadFile(link); string(after) != string(before) {
		t.Fatal("written through a link")
	}
}
