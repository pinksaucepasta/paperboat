package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

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
		// A 409 from the server was hidden as "target not ready" on every fleet machine (FIX-SSHREG).
		{name: "server refusal", err: fmt.Errorf("configure: %w", &api.APIError{Status: http.StatusConflict, Code: "operation_conflict"}), want: "ssh_server_refused"},
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

func TestManagedSSHServerRefusalNamesServerCode(t *testing.T) {
	err := fmt.Errorf("configure: %w", &api.APIError{Status: http.StatusConflict, Code: "operation_conflict"})
	if got := managedSSHServerRefusal(err); got != "operation_conflict" {
		t.Fatalf("refusal = %q", got)
	}
	if got := managedSSHServerRefusal(&api.APIError{Status: http.StatusBadGateway, Code: "unavailable"}); got != "" {
		t.Fatalf("5xx refusal = %q", got)
	}
}

func TestManagedSSHHealthCodeBoundsCyclicCause(t *testing.T) {
	if got := ManagedSSHHealthCode(&managedSSHCyclicError{}); got != "ssh_target_not_ready" {
		t.Fatalf("cyclic health code = %q", got)
	}
}

// A .ssh folder Windows OpenSSH would not trust is its own health item pointing at the doctor, not "SSH target is
// not ready" on every peer (a Windows test VM 2026-11-11: the Windows client refused SSH to a healthy Linux machine).
func TestManagedSSHUnsafeDirectoryIsNamed(t *testing.T) {
	startup := fmt.Errorf("install config: %w", &managedssh.UnsafeSSHPathError{Path: `C:\Users\me\.ssh`, Problem: "Everyone can change it"})
	if code := managedSSHHealthCode(startup); code != "ssh_directory_unsafe" {
		t.Fatalf("code=%q", code)
	}
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", Online: true, InstallationGeneration: 1, SSHLocalCode: "ssh_directory_unsafe"}
	item := mapMachines([]api.UserMachine{machine})[0].Health[0]
	if item.Code != "ssh_directory_unsafe" || !strings.Contains(item.Title, ".ssh folder") || !strings.Contains(item.Recovery, "pb ssh doctor") {
		t.Fatalf("health = %+v", item)
	}
}
