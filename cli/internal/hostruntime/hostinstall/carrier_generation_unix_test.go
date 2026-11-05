//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
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

// pb install of dev-ee7b48ac8 over dev-232279b79 failed with "invalid privileged
// installation request": the retained generation's descriptor named its control
// socket the way its own build did (.carrier-<32 hex>), and the newer build
// rejected every name but its own rule's.
func TestRetainedGenerationKeepsTheControlSocketItsBuildChose(t *testing.T) {
	paths := platformPaths(1001)
	retained, err := deriveUnixCarrierConfig(generationRequest("a"), paths)
	if err != nil {
		t.Fatal(err)
	}
	retained.ControlEndpoint = retained.HostdEndpoint + ".carrier-" + retained.Generation
	if err := retained.Validate(); err != nil {
		t.Fatalf("descriptor from an earlier build refused: %v", err)
	}
	other, err := deriveUnixCarrierConfig(generationRequest("b"), paths)
	if err != nil {
		t.Fatal(err)
	}
	for name, endpoint := range map[string]string{
		"another directory":         "/tmp/x" + retained.Generation,
		"another generation's name": other.ControlEndpoint,
		"over sun_path":             retained.HostdEndpoint + "." + retained.Generation + strings.Repeat("x", 120),
		"nested path":               retained.HostdEndpoint + ".d/" + retained.Generation,
	} {
		tampered := retained
		tampered.ControlEndpoint = endpoint
		if tampered.Validate() == nil {
			t.Fatalf("control socket in %s accepted", name)
		}
	}
}

// The root helper slices a source's SHA-256 into generation names: a request with
// an empty or short digest or generation must be refused, never panic.
func TestMalformedSourceOrGenerationRefusedWithoutPanic(t *testing.T) {
	for _, digest := range []string{"", "abc", strings.Repeat("a", 31)} {
		request := generationRequest("a")
		request.Source.SHA256 = digest
		paths := platformPaths(request.UID)
		root := t.TempDir()
		paths.metadata, paths.journal = filepath.Join(root, "metadata"), filepath.Join(root, "journal")
		if err := commitPreparedInstallationLocked(context.Background(), request, paths, true); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("commit with SHA-256 %q: %v", digest, err)
		}
		if _, err := deriveUnixCarrierConfig(request, paths); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("carrier derivation with SHA-256 %q: %v", digest, err)
		}
		if _, _, err := installersWithMaintenance(request, paths, true, false); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("installers with SHA-256 %q: %v", digest, err)
		}
		if err := startBaselineHostd(context.Background(), generationRequest("b"), &request.Source); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("baseline hostd with SHA-256 %q: %v", digest, err)
		}
	}
	c, err := deriveUnixCarrierConfig(generationRequest("a"), platformPaths(1001))
	if err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"a", strings.Repeat("a", 15), strings.Repeat("a", 31)} {
		short := c
		short.Generation = generation
		if !errors.Is(short.Validate(), ErrInvalidRequest) || validCarrierControlEndpoint(short) {
			t.Errorf("carrier descriptor with generation %q accepted", generation)
		}
	}
}
