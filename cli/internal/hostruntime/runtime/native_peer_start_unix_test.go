//go:build linux || darwin

package runtime

import (
	"errors"
	"fmt"
	"testing"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
)

// Every fresh pair on the shared account failed at "installing system services" once the server
// answered its peer network configuration with 409 peer_network_too_large: the carrier treated it as
// invalid and exited (2026-10-22). A refused peer network keeps the carrier serving and retries; an
// authorization refusal or any other conflict still fails start closed.
func TestRefusedPeerNetworkKeepsTheCarrierRunning(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{&clientapi.APIError{Status: 409, Code: "peer_network_too_large"}, true},
		{fmt.Errorf("register: %w", &clientapi.APIError{Status: 409, Code: "peer_network_conflict"}), true},
		{&clientapi.APIError{Status: 503, Code: "peer_network_unavailable"}, true},
		{&clientapi.APIError{Status: 409, Code: "operation_conflict"}, false},
		{&clientapi.APIError{Status: 403, Code: "peer_network_denied"}, false},
		{errors.Join(ErrProductionInvalid, errors.New("invalid certificate")), false},
	} {
		if got := transientNativePeerStart(test.err); got != test.want {
			t.Errorf("%v: keeps running=%t, want %t", test.err, got, test.want)
		}
	}
	if message := (&clientapi.APIError{Status: 409, Code: "peer_network_too_large"}).Error(); message == (&clientapi.APIError{Status: 409}).Error() {
		t.Fatalf("too-large refusal has the generic conflict message %q; status and doctor must say what is wrong", message)
	}
}
