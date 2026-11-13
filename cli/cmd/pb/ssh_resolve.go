package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func resolveSSHCommandTarget(ctx *command.Context, requested string) (*api.Client, api.UserMachine, api.ManagedSSHTarget, error) {
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if d.cfg.ServerURL == "" {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("Paperboat server is required for SSH")
	}
	client, err := sshAPIClient(ctx, d)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	machine, err := resolveSSHMachine(ctx.Context, client, requested)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if machine.InstallationGeneration < 1 {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("machine has no active runtime generation")
	}
	target, err := client.ManagedSSHTarget(ctx.Context, machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, sshTargetLookupError(requested, err)
	}
	return client, machine, target, nil
}

// sshTargetLookupError says why a machine can have no SSH target for this account when the server answers 404.
// The server gives the same 404 for a missing target and a refused permission so nothing leaks, hence the
// causes are listed; the generic "resource not found" gave the user nothing to act on (X-28, an OS user
// that pairs without a target: a name with a quote or wildcard managed SSH cannot carry).
func sshTargetLookupError(alias string, err error) error {
	var refused *api.APIError
	if !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		return err
	}
	return &sshTargetNotReadyError{machine: alias, title: "Paperboat has no SSH target for it that you may use",
		recovery: "Managed SSH cannot carry an OS user name with a quote, comma, $, % or wildcard, so a machine paired by a user like that has none: pair it from an OS user with a plain name. Otherwise check that you have SSH permission on it and run `pb ssh doctor` on that machine"}
}

// resolveSSHCommandTargetWarm resolves the machine and its SSH target from the daemon's snapshot, which the
// machine-events topics keep current, so a connection asks Paperboat nothing (F5-07). Without a ready daemon
// entry it asks the server.
func resolveSSHCommandTargetWarm(ctx *command.Context, requested string) (*api.Client, api.UserMachine, api.ManagedSSHTarget, error) {
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if d.cfg.ServerURL == "" {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("Paperboat server is required for SSH")
	}
	machine, ok := resolveWarmUserMachine(ctx.Context, requested)
	if !ok || machine.SSHUser == "" || machine.SSHPort == 0 {
		return resolveSSHCommandTarget(ctx, requested)
	}
	// No client: the helpers run per connection and need no credential unless they must ask the server
	// (sshClientFor).
	return nil, machine, api.ManagedSSHTarget{MachineID: machine.ID, MachineGeneration: uint64(machine.InstallationGeneration), OSUser: machine.SSHUser, Port: machine.SSHPort}, nil
}

// sshClientFor returns client, or builds one when the target came from the daemon and the server is needed.
func sshClientFor(ctx *command.Context, client *api.Client) (*api.Client, error) {
	if client != nil {
		return client, nil
	}
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, err
	}
	return sshAPIClient(ctx, d)
}

func sshAPIClient(ctx *command.Context, d *deps) (*api.Client, error) {
	client, err := newWorkspaceAPIClient(ctx, d.cfg.ServerURL, d.auth)
	if err != nil {
		return nil, err
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return nil, err
	}
	client.SetSourceMachineID(sourceMachineID)
	return client, nil
}

func resolveSSHMachine(ctx context.Context, client *api.Client, requested string) (api.UserMachine, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return api.UserMachine{}, err
	}
	for _, machine := range machines {
		if machine.ID == requested {
			return machine, nil
		}
	}
	var matches []api.UserMachine
	for _, machine := range machines {
		if strings.EqualFold(machine.Alias, requested) {
			matches = append(matches, machine)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, machine := range matches {
			ids = append(ids, machine.ID)
		}
		return api.UserMachine{}, fmt.Errorf("%w: %q matches machine IDs %s; use an exact ID", resolver.ErrMachineAmbiguous, requested, strings.Join(ids, ", "))
	}
	return resolveUserMachine(ctx, client, requested)
}

func newSSHOperationID() string {
	return "operation_" + uuid.NewString()
}

// sshConnectInfo: loginUser is the OS user the session logs in as ("" when unknown), carried to the host.
func sshConnectInfo(machine api.UserMachine, descriptor api.SSHDescriptor, selectedWorkspace, loginUser string) resolver.ConnectInfo {
	workspace := selectedWorkspace
	return resolver.ConnectInfo{
		Workspace:  workspace,
		TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineState: machine.State,
		MachineGeneration: uint64(machine.InstallationGeneration), TunnelTarget: descriptor.Endpoints.WSS,
		Terminal: &resolver.TerminalTarget{LoginUser: loginUser, Protocol: "paperboat.ssh.v1", EnvironmentID: descriptor.Environment.ID, QUICEndpoint: descriptor.Endpoints.QUIC, WSSEndpoint: descriptor.Endpoints.WSS, Auth: resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.Format(time.RFC3339Nano), Scopes: descriptor.Auth.Scopes, ResourceID: descriptor.Auth.AccessSessionID}, CWD: descriptor.Environment.Root},
	}
}

func pendingSSHDescriptor(machine api.UserMachine, operationID string) api.SSHDescriptor {
	return api.SSHDescriptor{OperationID: operationID, ExpiresAt: time.Now().Add(2 * time.Minute), Auth: api.AuthMaterial{ExpiresAt: time.Now().Add(2 * time.Minute)}, Environment: &api.Environment{ID: machine.EnvironmentID, Kind: "machine", ResourceID: machine.ID, Root: machine.WorkspaceRoot}}
}

// refreshInventoryForSSHHostKeys is replaced in tests.
var refreshInventoryForSSHHostKeys = refreshLocalInventory

// sshHostKeysRefreshTimeout bounds the one refresh a connection waits for.
const sshHostKeysRefreshTimeout = 5 * time.Second

// freshenSSHHostKeys covers a machine that published its SSH host keys after this computer's last
// inventory refresh (sshd installed after pairing, a first set): the daemon writes the known-hosts file OpenSSH
// reads from that inventory, so until the next refresh OpenSSH finds no key and ends with a bare "Host key
// verification failed." A first set is not a changed key (the server activates it, as at pairing), so the
// connection asks the daemon for one refresh now instead of waiting for its event. The returned bool is true when
// the machine still has no host keys, which the caller names. Only the warm path (no client) needs it.
func freshenSSHHostKeys(ctx context.Context, machine api.UserMachine, resolve func() (api.UserMachine, error)) (api.UserMachine, bool) {
	if len(machine.SSHHostKeys) > 0 {
		return machine, false
	}
	refreshCtx, cancel := context.WithTimeout(ctx, sshHostKeysRefreshTimeout)
	defer cancel()
	if err := refreshInventoryForSSHHostKeys(refreshCtx); err == nil {
		if refreshed, err := resolve(); err == nil {
			machine = refreshed
		}
	}
	return machine, len(machine.SSHHostKeys) == 0
}
