package selfhost

import "testing"

func TestInstallationInventoryOptionsValidateBeforeDatabase(t *testing.T) {
	for _, options := range []InventoryOptions{{Limit: -1}, {Limit: 201}, {Offset: -1}, {Offset: 2147483648}, {Q: "\x00"}, {Capability: "exec"}, {EnrollmentState: "ready"}} {
		if _, err := options.normalized(); err != ErrInvalid {
			t.Fatalf("options accepted: %+v %v", options, err)
		}
	}
	if options, err := (InventoryOptions{}).normalized(); err != nil || options.Limit != 50 {
		t.Fatalf("default options: %+v %v", options, err)
	}
}
