//go:build windows

package hostinstall

import (
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func testWindowsHostdSocket(t *testing.T, instance string, source installsource.Source) string {
	t.Helper()
	socket, _, err := windowsHostdSockets(instance, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	return socket
}

// Each hostd generation has its own pipe, named in the installed configuration (the one place clients read it):
// a new installation names its generation's pipe and the one it takes over from; a reinstall of the same
// generation takes over from nothing; a configuration naming another generation's pipe, or a handover from
// itself or from another account's pipe, is refused.
func TestWindowsConfigNamesItsHostdGenerationPipes(t *testing.T) {
	ownerSID := "S-1-5-21-1-2-3-1001"
	instance, err := WindowsInstanceForSID(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := WindowsInstanceTokenPath(instance)
	source := func(digit string) installsource.Source {
		return installsource.Source{Version: "dev", Platform: "windows", Architecture: runtime.GOARCH, SHA256: strings.Repeat(digit, 64), Length: 1, Distribution: installsource.Custom}
	}
	oldSocket, from, err := windowsHostdSockets(instance, source("1"), nil)
	if err != nil || from != "" {
		t.Fatalf("first installation: %q, from %q, %v", oldSocket, from, err)
	}
	previous := WindowsRuntimeConfig{Instance: instance, HostdSocket: oldSocket}
	newSocket, from, err := windowsHostdSockets(instance, source("2"), &previous)
	if err != nil || newSocket == oldSocket || from != oldSocket {
		t.Fatalf("update: %q from %q (%v); want a new pipe handed over from %q", newSocket, from, err, oldSocket)
	}
	if again, from, err := windowsHostdSockets(instance, source("1"), &previous); err != nil || again != oldSocket || from != "" {
		t.Fatalf("reinstall of the same generation: %q from %q, %v", again, from, err)
	}
	layout, err := WindowsLayoutForInstance(instance)
	if err != nil || !strings.HasPrefix(newSocket, layout.HostdSocket+"-") {
		t.Fatalf("pipe %q is not under the account's hostd pipe %q (%v)", newSocket, layout.HostdSocket, err)
	}
	config := WindowsRuntimeConfig{Schema: windowsConfigSchema, Instance: instance, OwnerSID: ownerSID, User: "tester", StateRoot: `C:\State`, Workspace: `C:\Users\tester`, ListenAddress: "127.0.0.1:8080", EnrollmentPending: true, TokenFile: token, Source: source("2"), HostdSocket: newSocket, HandoverFrom: oldSocket}
	if !validWindowsConfig(config) {
		t.Fatal("an update's configuration was refused")
	}
	other, err := WindowsInstanceForSID("S-1-5-21-1-2-3-1002")
	if err != nil {
		t.Fatal(err)
	}
	foreign := testWindowsHostdSocket(t, other, source("1"))
	for name, mutate := range map[string]func(*WindowsRuntimeConfig){
		"another generation's pipe": func(c *WindowsRuntimeConfig) { c.HostdSocket = oldSocket },
		"no pipe":                   func(c *WindowsRuntimeConfig) { c.HostdSocket = "" },
		"handover from itself":      func(c *WindowsRuntimeConfig) { c.HandoverFrom = newSocket },
		"handover from an account":  func(c *WindowsRuntimeConfig) { c.HandoverFrom = foreign },
		"handover from a name":      func(c *WindowsRuntimeConfig) { c.HandoverFrom = layout.HostdSocket },
	} {
		candidate := config
		mutate(&candidate)
		if validWindowsConfig(candidate) {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A carrier descriptor pairs with the hostd generation of its source: its hostd endpoint is that generation's pipe,
// and it validates (its control endpoint derives from that pipe). The first per-generation build failed every
// install on winpool-13 because the control endpoint still derived from the account pipe.
func TestWindowsCarrierDescriptorPairsWithItsHostdGeneration(t *testing.T) {
	ownerSID := "S-1-5-21-1-2-3-1001"
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	source := installsource.Source{Version: "dev", Platform: "windows", Architecture: runtime.GOARCH, SHA256: strings.Repeat("4", 64), Length: 1, Distribution: installsource.Custom}
	pin, err := WindowsPinnedSourcePath(layout, source)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{OwnerSID: ownerSID, Source: source, StateRoot: `C:\Users\tester\AppData\Local\Paperboat\runtime`, WorkspaceRoot: `C:\Users\tester`, ControlURL: "https://api.example.test", UserMachineID: "machine", HelperListenAddress: "127.0.0.1:38080"}
	c, err := deriveWindowsCarrierConfig(request, layout, pin)
	if err != nil {
		t.Fatalf("the derived carrier descriptor is invalid: %v", err)
	}
	if want := testWindowsHostdSocket(t, layout.Instance, source); c.HostdEndpoint != want {
		t.Fatalf("carrier hostd endpoint %q, want its generation's pipe %q", c.HostdEndpoint, want)
	}
}

// An upgrade's standby carrier may differ from the serving one only in what belongs to its generation, now
// including its hostd pipe; account custody and endpoints stay. winpool-13: every per-generation upgrade failed
// staging the standby because the hostd endpoint counted as custody.
func TestWindowsCarrierUpgradeKeepsCustodyButNotTheHostdPipe(t *testing.T) {
	ownerSID := "S-1-5-21-1-2-3-1001"
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	derive := func(digit string) CarrierConfig {
		source := installsource.Source{Version: "dev", Platform: "windows", Architecture: runtime.GOARCH, SHA256: strings.Repeat(digit, 64), Length: 1, Distribution: installsource.Custom}
		pin, err := WindowsPinnedSourcePath(layout, source)
		if err != nil {
			t.Fatal(err)
		}
		c, err := deriveWindowsCarrierConfig(Request{OwnerSID: ownerSID, Source: source, StateRoot: `C:\Users\tester\AppData\Local\Paperboat\runtime`, WorkspaceRoot: `C:\Users\tester`, ControlURL: "https://api.example.test", UserMachineID: "machine", HelperListenAddress: "127.0.0.1:38080"}, layout, pin)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	retained, candidate := derive("5"), derive("6")
	if candidate.HostdEndpoint == retained.HostdEndpoint || !carrierCustodyUnchanged(candidate, retained) {
		t.Fatalf("an upgrade to another hostd generation was refused (hostd %q -> %q)", retained.HostdEndpoint, candidate.HostdEndpoint)
	}
	moved := candidate
	moved.WorkerAddress = "127.0.0.1:48080"
	if carrierCustodyUnchanged(moved, retained) {
		t.Fatal("an upgrade that moves the worker address was accepted")
	}
}
