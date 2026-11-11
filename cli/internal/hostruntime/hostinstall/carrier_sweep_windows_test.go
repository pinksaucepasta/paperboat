//go:build windows

package hostinstall

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// Review of 083679b72: a carrier service registered without its generation descriptor is removed by the sweep;
// the active generation, the generations the descriptors named and other accounts' or kinds' services are not.
func TestWindowsCarrierSweepRemovesOrphanServices(t *testing.T) {
	instance := "u" + strings.Repeat("a", 24)
	active, known, orphan := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	inventory, remove := windowsGenerationServiceInventory, removeWindowsServiceDeclaration
	t.Cleanup(func() { windowsGenerationServiceInventory, removeWindowsServiceDeclaration = inventory, remove })
	windowsGenerationServiceInventory = func(context.Context, string) ([]string, error) {
		return []string{
			"PaperboatCarrier-" + instance + "-" + active,
			"PaperboatCarrier-" + instance + "-" + known,
			"PaperboatCarrier-" + instance + "-" + orphan,
			"PaperboatHostd-" + instance + "-" + orphan,
			"PaperboatSessionOwner-" + instance + "-" + orphan,
		}, nil
	}
	var removed []string
	removeWindowsServiceDeclaration = func(_ context.Context, definition string) error {
		removed = append(removed, strings.TrimSuffix(filepath.Base(definition), ".json"))
		return nil
	}
	if err := removeOrphanWindowsCarrierServices(context.Background(), instance, []string{active, known}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "PaperboatCarrier-"+instance+"-"+orphan {
		t.Fatalf("removed %v, want only the orphan carrier", removed)
	}
}

// Condition 4 of the per-generation hostd design: a replaced hostd generation still finishing its work is disabled
// (it must not start at boot beside the installed one) and kept; once it has exited the sweep removes it; the
// installed generation is never touched.
func TestWindowsHostdSweepDisablesARunningReplacedGeneration(t *testing.T) {
	instance := "u" + strings.Repeat("b", 24)
	installed, draining, exited := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	name := func(generation string) string { return "PaperboatHostd-" + instance + "-" + generation }
	inventory, inspect, remove, disable := windowsGenerationServiceInventory, inspectWindowsServiceDeclaration, removeWindowsServiceDeclaration, disableWindowsServiceDeclaration
	t.Cleanup(func() {
		windowsGenerationServiceInventory, inspectWindowsServiceDeclaration, removeWindowsServiceDeclaration, disableWindowsServiceDeclaration = inventory, inspect, remove, disable
	})
	windowsGenerationServiceInventory = func(context.Context, string) ([]string, error) {
		return []string{name(installed), name(draining), name(exited), "PaperboatCarrier-" + instance + "-" + exited}, nil
	}
	base := func(definition string) string { return strings.TrimSuffix(filepath.Base(definition), ".json") }
	inspectWindowsServiceDeclaration = func(_ context.Context, definition string) (service.NativeControllerStatus, error) {
		return service.NativeControllerStatus{Registered: true, Running: base(definition) != name(exited)}, nil
	}
	var removed, disabled []string
	removeWindowsServiceDeclaration = func(_ context.Context, definition string) error {
		removed = append(removed, base(definition))
		return nil
	}
	disableWindowsServiceDeclaration = func(_ context.Context, definition string) error {
		disabled = append(disabled, base(definition))
		return nil
	}
	if err := removeWindowsHostdGenerations(context.Background(), instance, installed, true); err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 || disabled[0] != name(draining) || len(removed) != 1 || removed[0] != name(exited) {
		t.Fatalf("disabled %v, removed %v; want the draining generation disabled and the exited one removed", disabled, removed)
	}
}
