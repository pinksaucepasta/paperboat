//go:build darwin || linux

package hostinstall

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRequestRefusesMissingControlURL(t *testing.T) {
	request := validRequest(t)
	for _, raw := range []string{"", "not a url"} {
		changed := request
		changed.ControlURL = raw
		err := Validate(changed, request.UID)
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "server URL") {
			t.Fatalf("control URL %q: %v", raw, err)
		}
	}
}
