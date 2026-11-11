//go:build windows

package hostinstall

import (
	"errors"
	"testing"
)

func TestCarrierCallerVerifies(t *testing.T) {
	const owner, system, other = "S-1-5-21-1-2-3-1001", "S-1-5-18", "S-1-5-21-1-2-3-1002"
	for _, test := range []struct {
		name     string
		caller   string
		elevated bool
		verify   bool
		refused  bool
	}{
		{"the account itself has nothing it can verify", owner, false, false, false},
		{"the account itself elevated verifies like any administrator", owner, true, true, false},
		{"SYSTEM verifies", system, true, true, false},
		{"an elevated administrator is no longer refused", other, true, true, false},
		{"another account is refused", other, false, false, true},
	} {
		verify, err := carrierCallerVerifies(test.caller, owner, test.elevated)
		if verify != test.verify || errors.Is(err, ErrInvalidRequest) != test.refused {
			t.Errorf("%s: verify=%v err=%v", test.name, verify, err)
		}
	}
}
