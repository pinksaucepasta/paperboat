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
		err := Validate(request, 0)
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "server URL") {
			t.Fatalf("control URL %q: %v", raw, err)
		}
	}
}
