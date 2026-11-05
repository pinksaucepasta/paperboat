//go:build darwin || linux

package hostinstall

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// macOS sun_path holds 104 bytes including the terminating NUL (Linux 108); a
// longer socket path fails bind with EINVAL. pbmac: the per-generation carrier
// control socket was 115 bytes, so the first carrier never became ready.
const darwinSunPath = 104

func TestEveryGenerationSocketFitsDarwinSunPath(t *testing.T) {
	generation := strings.Repeat("f", 32)
	for _, platform := range []string{"darwin", "linux"} {
		layout, err := service.UserLayout(platform, 2147483647) // longest uid
		if err != nil {
			t.Fatal(err)
		}
		hostd := layout.HostdSocket
		for name, path := range map[string]string{
			"hostd lifecycle":        hostd,
			"carrier control":        carrierControlEndpoint(hostd, generation),
			"hostd handover":         hostdproto.HandoverPath(hostd),
			"worker private (pid 7)": hostd + ".4194304.workloads",
			"updater control":        layout.UpdaterSocket,
			"host service":           hostservice.SocketPath(platform, 2147483647),
		} {
			if len(path) >= darwinSunPath {
				t.Errorf("%s %s socket is %d bytes, over sun_path: %s", platform, name, len(path), path)
			}
		}
	}
}

// Boot clears /var/run on macOS; the host service socket shares the run root the installer makes and purge removes.
func TestDarwinHostServiceSocketIsUnderTheRunRoot(t *testing.T) {
	layout, err := service.UserLayout("darwin", 501)
	if err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Dir(filepath.Dir(layout.UpdaterSocket))
	if got := hostservice.SocketPath("darwin", 501); filepath.Dir(filepath.Dir(got)) != runRoot || filepath.Base(filepath.Dir(got)) != "paperboat-host-u501" {
		t.Fatalf("darwin host service socket %s is not the user's own under %s", got, runRoot)
	}
	// Each OS user's host service has its own socket (pbmac C032: a shared one was taken over by the last user).
	if hostservice.SocketPath("darwin", 501) == hostservice.SocketPath("darwin", 502) || hostservice.SocketPath("linux", 1000) == hostservice.SocketPath("linux", 1001) {
		t.Fatal("two OS users share a host service socket")
	}
	if got := hostservice.SocketPath("linux", 1000); got != "/var/run/paperboat-u1000/host-service.sock" {
		t.Fatalf("linux host service socket is not the unit's RuntimeDirectory: %s", got)
	}
}
