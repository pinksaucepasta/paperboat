//go:build linux || darwin

package hostinstall

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func generationRequest(digit string) Request {
	return Request{Platform: runtime.GOOS, UID: 1001, GID: 1001, User: "tester", Group: "tester", Home: "/home/tester", Path: "/usr/bin", Shell: "/bin/sh", WorkspaceRoot: "/home/tester", ControlURL: "https://control.example.test", StateRoot: "/var/lib/paperboat-test", HelperListenAddress: "127.0.0.1:38080", UserMachineID: "machine_1",
		Source: installsource.Source{Version: "dev-" + digit, Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: strings.Repeat(digit, 64), Length: 1, Distribution: installsource.Custom}}
}

// pbvm4 2026-11-04: the update restarted the one carrier unit in place, so new
// SSH was refused ~40 s. Two generations must now be able to serve side by side:
// nothing they bind or lock may be shared.
func TestCarrierGenerationsShareNoUnitLockOrControlSocket(t *testing.T) {
	paths := platformPaths(1001)
	serving, err := deriveUnixCarrierConfig(generationRequest("a"), paths)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := deriveUnixCarrierConfig(generationRequest("b"), paths)
	if err != nil {
		t.Fatal(err)
	}
	if serving.Generation != strings.Repeat("a", 32) || candidate.Generation != strings.Repeat("b", 32) {
		t.Fatalf("generations %q %q", serving.Generation, candidate.Generation)
	}
	if serving.LockPath == candidate.LockPath || serving.ControlEndpoint == candidate.ControlEndpoint || carrierGenerationPath(serving.Instance, serving.Generation) == carrierGenerationPath(candidate.Instance, candidate.Generation) {
		t.Fatal("carrier generations share a lock, control socket or descriptor")
	}
	if serving.DispatchCapabilityFile != candidate.DispatchCapabilityFile || serving.HostdEndpoint != candidate.HostdEndpoint {
		t.Fatal("generations lost the account's shared capability or hostd endpoint")
	}
	units := map[string]bool{}
	for _, c := range []CarrierConfig{serving, candidate} {
		installer, err := carrierInstallerPending(c)
		if err != nil {
			t.Fatal(err)
		}
		units[installer.DefinitionPath()] = true
	}
	if len(units) != 2 {
		t.Fatal("carrier generations share one native unit")
	}
	tampered := candidate
	tampered.Generation = serving.Generation
	if tampered.Validate() == nil {
		t.Fatal("descriptor naming another source's generation accepted")
	}
	tampered = candidate
	tampered.LockPath = serving.LockPath
	if tampered.Validate() == nil {
		t.Fatal("descriptor sharing another generation's lock accepted")
	}
}

func TestHostdGenerationDefinitionsParseBack(t *testing.T) {
	generation := strings.Repeat("c", 32)
	for _, platform := range []string{"linux", "darwin"} {
		definition := hostdGenerationDefinition(platform, 1001, generation)
		if got := hostdDefinitionGeneration(platform, 1001, definition); got != generation {
			t.Fatalf("%s: %s parsed as %q", platform, definition, got)
		}
		if matched, _ := filepath.Match(hostdGenerationDefinition(platform, 1001, "*"), definition); !matched {
			t.Fatalf("%s: sweep pattern misses %s", platform, definition)
		}
		if matched, _ := filepath.Match(hostdGenerationDefinition(platform, 1001, "*"), hostdGenerationDefinition(platform, 10010, generation)); matched {
			t.Fatalf("%s: sweep pattern matches another account", platform)
		}
	}
}
