//go:build darwin || linux

package runtime

import (
	"context"
	"net/http"
	"os/user"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/availability"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func initializeProductionManagedSSH(ctx context.Context, host *managedssh.Host, controlURL string, transport http.RoundTripper, registration runtimeidentity.Registration, identity managedSSHIdentitySource, generation uint64) (Service, error) {
	return initializeProductionManagedSSHUnix(ctx, host, controlURL, transport, registration, identity, generation)
}

func validatedMachineShell(path string) (string, error) { return validatedMachineShellUnix(path) }
func validateMachineWorkspace(path string) error        { return validateMachineWorkspaceUnix(path) }

func newProductionAvailabilityHostClient(timeout time.Duration) (*availability.HostClient, error) {
	return availability.NewHostClient("/var/run/paperboat/host-service.sock", timeout)
}

func cleanupProductionManagedSSH(ctx context.Context, registration runtimeidentity.Registration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	account, err := user.Lookup(registration.SSHUser)
	if err != nil {
		return ErrManagedSSHUnavailable
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return ErrManagedSSHUnavailable
	}
	_, err = reconcilePlatformAuthorizedKeys(account.HomeDir, uint32(uid), nil)
	return err
}
