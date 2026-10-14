package containers

import (
	"strings"
	"testing"
)

func TestSupportedSelfhostOperatingSystemsAndArchitectures(t *testing.T) {
	for _, host := range []struct {
		platform, arch, id, version string
		allowed                     bool
	}{
		{"linux", "amd64", "ubuntu", "22.04", true}, {"linux", "arm64", "ubuntu", "24.04", true}, {"linux", "arm64", "debian", "12", true},
		{"linux", "amd64", "ubuntu", "26.04", false}, {"linux", "amd64", "debian", "13", false}, {"linux", "386", "ubuntu", "24.04", false}, {"darwin", "arm64", "ubuntu", "24.04", false}, {"linux", "amd64", "alpine", "3.20", false},
	} {
		err := supportedHost(host.platform, host.arch, strings.NewReader("ID="+host.id+"\nVERSION_ID=\""+host.version+"\"\n"))
		if (err == nil) != host.allowed {
			t.Fatalf("supported-host decision wrong for %s/%s %s%s", host.platform, host.arch, host.id, host.version)
		}
	}
}
