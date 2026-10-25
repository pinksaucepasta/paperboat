package machineguard

import (
	"errors"
	"testing"
)

func TestListenerRefusalCrossesTheSocketTyped(t *testing.T) {
	err := responseError(response{Error: "machine name or address belongs to another OS user", ErrorCode: "listener_refused"})
	if !errors.Is(err, ErrListenerRefused) || err.Error() != "machine name or address belongs to another OS user" {
		t.Fatalf("err=%v", err)
	}
}
