package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestManagedSSHAuthorityFailurePreservesCauseWithoutErrorText(t *testing.T) {
	cause := errors.New("private grant token detail")
	err := managedSSHAuthorityError(cause)
	if !errors.Is(err, cause) {
		t.Fatal("authority failure lost the original cause")
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "grant token") {
		t.Fatalf("authority error exposed private text: %q", err)
	}
	staged, ok := err.(interface{ DiagnosticStage() string })
	if !ok || staged.DiagnosticStage() != "peer_authority" {
		t.Fatalf("authority stage = %v", err)
	}
	coded, ok := err.(interface{ DiagnosticCode() string })
	if !ok || coded.DiagnosticCode() != "managed_ssh_failed" {
		t.Fatalf("authority code = %v", err)
	}
}

func TestManagedSSHHealthCodeUsesOnlyGenuineAuthenticationRejections(t *testing.T) {
	operational := errors.New("private storage detail")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "unauthenticated sentinel", err: api.ErrUnauthenticated, want: "ssh_key_rejected"},
		{name: "wrapped unauthenticated sentinel", err: fmt.Errorf("registration: %w", api.ErrUnauthenticated), want: "ssh_key_rejected"},
		{name: "unauthorized response", err: &api.APIError{Status: http.StatusUnauthorized}, want: "ssh_key_rejected"},
		{name: "forbidden response", err: &api.APIError{Status: http.StatusForbidden}, want: "ssh_key_rejected"},
		{name: "server failure", err: &api.APIError{Status: http.StatusBadGateway}, want: "ssh_target_not_ready"},
		{name: "filesystem failure", err: fs.ErrPermission, want: "ssh_target_not_ready"},
		{name: "unknown failure", err: operational, want: "ssh_target_not_ready"},
		{name: "rejection joined with operation failure", err: errors.Join(&api.APIError{Status: http.StatusUnauthorized}, operational), want: "ssh_target_not_ready"},
		{name: "sentinel joined with operation failure", err: errors.Join(api.ErrUnauthenticated, operational), want: "ssh_target_not_ready"},
		{name: "rejection joined with caller cancellation", err: errors.Join(&api.APIError{Status: http.StatusForbidden}, context.Canceled), want: "ssh_key_rejected"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := ManagedSSHHealthCode(test.err); got != test.want {
				t.Fatalf("health code = %q, want %q", got, test.want)
			}
		})
	}
}

type managedSSHCyclicError struct{}

func (*managedSSHCyclicError) Error() string { return "cycle" }

func (e *managedSSHCyclicError) Unwrap() error { return e }

func TestManagedSSHHealthCodeBoundsCyclicCause(t *testing.T) {
	if got := ManagedSSHHealthCode(&managedSSHCyclicError{}); got != "ssh_target_not_ready" {
		t.Fatalf("cyclic health code = %q", got)
	}
}

// The server's refusal at the account's client-key limit (managed_ssh_client_key_capacity) is its own health item
// with the way out, not "SSH target is not ready" retried forever (hp 2026-10-27, a full test account).
func TestManagedSSHClientKeyCapacityIsNamed(t *testing.T) {
	refused := managedSSHAuthorityError(&api.APIError{Status: http.StatusConflict, Code: "managed_ssh_client_key_capacity"})
	if code := managedSSHHealthCode(refused); code != "ssh_client_key_capacity" {
		t.Fatalf("code=%q", code)
	}
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", Online: true, InstallationGeneration: 1, SSHLocalCode: "ssh_client_key_capacity"}
	item := mapMachines([]api.UserMachine{machine})[0].Health[0]
	if item.Code != "ssh_client_key_capacity" || !strings.Contains(item.Title, "limit") || !strings.Contains(item.Recovery, "pb logout") {
		t.Fatalf("health = %+v", item)
	}
}
