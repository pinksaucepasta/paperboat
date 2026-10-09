package tunnel

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
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

func TestLocalPeerPendingRequestPreservesWorkspace(t *testing.T) {
	for _, selector := range []string{"personal", "team-audit", "INVALID/team"} {
		info := resolver.ConnectInfo{Workspace: selector, MachineID: "machine_1", MachineGeneration: 1, Terminal: &resolver.TerminalTarget{EnvironmentID: "environment_1"}}
		request, err := (LocalPeerTunnel{Client: &localapi.Client{}}).request(info, "exec", "operation_1", nil)
		if selector == "INVALID/team" {
			if err == nil {
				t.Fatal("invalid workspace accepted")
			}
			continue
		}
		if err != nil || request.Workspace != selector {
			t.Fatalf("workspace=%s request=%s err=%v", selector, request.Workspace, err)
		}
	}
}

func TestHelperRemoteErrorExposesOnlySafeEnvironmentCode(t *testing.T) {
	for _, code := range []string{"environment_unavailable", "invalid_request", "secret_error_detail"} {
		exposed := (&helperRemoteError{Code: code, Message: "private value"}).LocalAPICode()
		if code == "environment_unavailable" {
			if exposed != code {
				t.Fatal("ENV denial lost")
			}
		} else if exposed != "" {
			t.Fatal("unapproved remote code escaped")
		}
	}
}
