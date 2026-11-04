package service

import (
	"strings"
	"testing"
)

// hostd runs as the enrolled account; /var/run is cleared at boot and that account cannot recreate a directory
// there, so on macOS its socket must live under a parent root created once and boot does not clear.
func TestDarwinHostdSocketIsNotUnderVarRun(t *testing.T) {
	layout, err := UserLayout("darwin", 501)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(layout.HostdSocket, "/var/run/") || strings.HasPrefix(layout.HostdSocket, "/run/") {
		t.Fatalf("darwin hostd socket %q is under a boot-cleared directory", layout.HostdSocket)
	}
	if !strings.HasPrefix(layout.HostdSocket, "/Library/Application Support/Paperboat/run/paperboat-hostd-u501/") {
		t.Fatalf("darwin hostd socket = %q", layout.HostdSocket)
	}
	if !strings.HasPrefix(layout.UpdaterSocket, "/Library/Application Support/Paperboat/run/paperboat-updated-u501/") {
		t.Fatalf("darwin updater socket = %q: boot clears /var/run and nothing recreates the directory", layout.UpdaterSocket)
	}
	if len(layout.HostdSocket+".carrier") >= 104 {
		t.Fatalf("socket path %q exceeds the macOS sockaddr_un limit", layout.HostdSocket+".carrier")
	}
	linux, err := UserLayout("linux", 501)
	if err != nil || linux.HostdSocket != "/run/paperboat-hostd-u501/hostd.sock" || linux.UpdaterSocket != "/run/paperboat-updated-u501/control.sock" {
		t.Fatalf("linux sockets changed: %q %q, %v", linux.HostdSocket, linux.UpdaterSocket, err)
	}
}
