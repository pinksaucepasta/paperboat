//go:build windows

package hostinstall

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Review of 083679b72: a carrier service registered without its generation descriptor is removed by the sweep;
// the active generation, the generations the descriptors named and other accounts' or kinds' services are not.
func TestWindowsCarrierSweepRemovesOrphanServices(t *testing.T) {
	instance := "u" + strings.Repeat("a", 24)
	active, known, orphan := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	inventory, remove := windowsCarrierServiceInventory, removeWindowsServiceDeclaration
	t.Cleanup(func() { windowsCarrierServiceInventory, removeWindowsServiceDeclaration = inventory, remove })
	windowsCarrierServiceInventory = func(context.Context, string) ([]string, error) {
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
