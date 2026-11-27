//go:build windows

package hostinstall

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestValidateRequestRefusesMissingControlURL(t *testing.T) {
	for _, raw := range []string{"", "not a url"} {
		request := Request{Schema: SchemaV1, Platform: runtime.GOOS, User: "user", OwnerSID: "S-1-5-18", UserMachineID: "machine", ControlURL: raw}
		err := ValidateFresh(request)
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "server URL") {
			t.Fatalf("control URL %q: %v", raw, err)
		}
	}
}

// Uninstall, Commit, Repair (windowsCarrierInstallRequest) and the prepared update validate a request built
// from the persisted installation; one whose stored control URL is empty or not normalized must still pass
// the control URL step (the request below fails only later, at the listen address it leaves out).
func TestValidatePersistedRequestDoesNotRequireControlURL(t *testing.T) {
	for _, raw := range []string{"", "not a url", "HTTPS://API.EXAMPLE.TEST/"} {
		request := Request{Schema: SchemaV1, Platform: runtime.GOOS, User: "user", OwnerSID: "S-1-5-18", UserMachineID: "machine", ControlURL: raw}
		err := Validate(request, 0)
		if err == nil || strings.Contains(err.Error(), "server URL") || !strings.Contains(err.Error(), "helper listen address") {
			t.Fatalf("persisted control URL %q: %v", raw, err)
		}
	}
}
