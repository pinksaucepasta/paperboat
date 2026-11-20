package selfhost

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
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

// The runtime listens on both address families unless the installation names a host: "::" accepts IPv4 too.
func TestListenAddressDefaultsToBothAddressFamilies(t *testing.T) {
	if got := listenAddress("", 443); got != "[::]:443" {
		t.Fatalf("default listen address = %q, want every address of both families", got)
	}
	if got := listenAddress("192.0.2.1", 8443); got != "192.0.2.1:8443" {
		t.Fatalf("named listen host = %q", got)
	}
	listener, err := net.Listen("tcp", listenAddress("", 0))
	if err != nil {
		t.Skipf("no IPv6 wildcard listener on this host: %v", err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	for _, host := range []string{"127.0.0.1", "::1"} {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
		if err != nil {
			if host == "::1" {
				t.Logf("no IPv6 loopback here: %v", err)
				continue
			}
			t.Fatalf("dial %s: %v", host, err)
		}
		connection.Close()
	}
}
