package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
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
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	observeSSHTargetCacheFailure(ctx.Context, sshTargetCacheStore(d.cfg, machine, target, time.Now()))
	return client, machine, target, nil
}

// resolveSSHCommandTargetFast resolves the machine from the daemon's warm
// inventory snapshot and the SSH target from a short-lived local cache when
// fresh, avoiding catalog listing round trips on the interactive SSH path.
// Any unavailable shortcut falls back to the canonical live resolution so
// error behavior is unchanged.
func resolveSSHCommandTargetFast(ctx *command.Context, requested string) (*api.Client, api.UserMachine, api.ManagedSSHTarget, error) {
	var cacheFailures error
	defer func() { observeSSHTargetCacheFailure(ctx.Context, cacheFailures) }()

	d, err := buildDeps(ctx)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if d.cfg.ServerURL == "" {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("Paperboat server is required for SSH")
	}
	machine, ok := resolveWarmUserMachine(ctx.Context, requested)
	if !ok || machine.InstallationGeneration < 1 {
		return resolveSSHCommandTarget(ctx, requested)
	}
	client, err := sshAPIClient(ctx, d)
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	if target, fresh, cacheErr := sshTargetCacheLookup(d.cfg, machine, time.Now()); cacheErr != nil {
		cacheFailures = errors.Join(cacheFailures, cacheErr)
	} else if fresh {
		return client, machine, target, nil
	}
	target, err := client.ManagedSSHTarget(ctx.Context, machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	cacheFailures = errors.Join(cacheFailures, sshTargetCacheStore(d.cfg, machine, target, time.Now()))
	return client, machine, target, nil
}

// resolveSSHCommandTargetLive skips the machine catalog listing through the
// daemon's warm snapshot but always validates the SSH target against fresh
// server data. The managed SSH proxy uses this so destination port validation
// can never trust a stale local cache.
func resolveSSHCommandTargetLive(ctx *command.Context, requested string) (*api.Client, api.UserMachine, api.ManagedSSHTarget, error) {
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if d.cfg.ServerURL == "" {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("Paperboat server is required for SSH")
	}
	machine, ok := resolveWarmUserMachine(ctx.Context, requested)
	if !ok || machine.InstallationGeneration < 1 {
		return resolveSSHCommandTarget(ctx, requested)
	}
	client, err := sshAPIClient(ctx, d)
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	target, err := client.ManagedSSHTarget(ctx.Context, machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	observeSSHTargetCacheFailure(ctx.Context, sshTargetCacheStore(d.cfg, machine, target, time.Now()))
	return client, machine, target, nil
}

func observeSSHTargetCacheFailure(ctx context.Context, err error) {
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "ssh", "command", "managed_ssh_failed", err)
	}
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

func sshConnectInfo(machine api.UserMachine, descriptor api.SSHDescriptor, selectedWorkspace string) resolver.ConnectInfo {
	workspace := selectedWorkspace
	return resolver.ConnectInfo{
		Workspace:  workspace,
		TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineState: machine.State,
		MachineGeneration: uint64(machine.InstallationGeneration), TunnelTarget: descriptor.Endpoints.WSS,
		Terminal: &resolver.TerminalTarget{Protocol: "paperboat.ssh.v1", EnvironmentID: descriptor.Environment.ID, QUICEndpoint: descriptor.Endpoints.QUIC, WSSEndpoint: descriptor.Endpoints.WSS, Auth: resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.Format(time.RFC3339Nano), Scopes: descriptor.Auth.Scopes, ResourceID: descriptor.Auth.AccessSessionID}, CWD: descriptor.Environment.Root},
	}
}

func pendingSSHDescriptor(machine api.UserMachine, operationID string) api.SSHDescriptor {
	return api.SSHDescriptor{OperationID: operationID, ExpiresAt: time.Now().Add(2 * time.Minute), Auth: api.AuthMaterial{ExpiresAt: time.Now().Add(2 * time.Minute)}, Environment: &api.Environment{ID: machine.EnvironmentID, Kind: "machine", ResourceID: machine.ID, Root: machine.WorkspaceRoot}}
}
