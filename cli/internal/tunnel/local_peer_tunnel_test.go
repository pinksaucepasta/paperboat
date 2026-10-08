package tunnel

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestLocalPeerTunnelInvalidTargetDoesNotEchoTargetIdentifiers(t *testing.T) {
	_, err := (LocalPeerTunnel{}).request(resolver.ConnectInfo{
		MachineID:         "machine_private_identifier",
		MachineGeneration: 7,
		Terminal:          &resolver.TerminalTarget{EnvironmentID: "environment_private_identifier"},
	}, "terminal", "operation_private_identifier", nil)
	if !errors.Is(err, ErrPeerTerminalInvalid) {
		t.Fatalf("invalid target error=%v", err)
	}
	for _, private := range []string{"machine_private_identifier", "environment_private_identifier", "operation_private_identifier"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("invalid target error echoed private identifier %q: %q", private, err)
		}
	}
	if err.Error() != ErrPeerTerminalInvalid.Error() {
		t.Fatalf("invalid target error=%q", err)
	}
}
