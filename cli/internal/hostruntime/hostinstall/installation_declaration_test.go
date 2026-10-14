package hostinstall

import (
	"bytes"
	"testing"
)

func TestPrivilegedRequestsRejectRetiredFields(t *testing.T) {
	for _, body := range []string{`{"setup_mode":"host"}`, `{"setup_roles":["host"]}`} {
		if _, err := Decode(bytes.NewBufferString(body)); err == nil {
			t.Fatal("removed product label accepted in privileged request")
		}
	}
}
