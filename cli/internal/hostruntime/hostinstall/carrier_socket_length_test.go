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
			"host service":           hostservice.SocketPath(platform),
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
	if got := hostservice.SocketPath("darwin"); filepath.Dir(filepath.Dir(got)) != runRoot {
		t.Fatalf("darwin host service socket %s is not under %s", got, runRoot)
	}
	if got := hostservice.SocketPath("linux"); got != "/var/run/paperboat/host-service.sock" {
		t.Fatalf("linux host service socket changed: %s", got)
	}
}
