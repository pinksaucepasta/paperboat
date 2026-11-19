package main

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/localwait"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
	"github.com/spf13/cobra"
)

// resolveWaitMachine names a machine the daemon's snapshot does not list the way exec does (resolveUserMachine:
// --workspace honoured, team machines included). An unreachable server returns a nil client: the caller keeps
// waiting on the daemon, which may still learn the name (F21-01). An unknown name is an error at once.
func resolveWaitMachine(ctx context.Context, command *cobra.Command, target string) (api.UserMachine, *api.Client, error) {
	backend, err := backendClient(actionContext(command, []string{target}))
	if err != nil {
		return api.UserMachine{}, nil, nil
	}
	return resolveWaitMachineOn(ctx, backend, target)
}

func resolveWaitMachineOn(ctx context.Context, backend *api.Client, target string) (api.UserMachine, *api.Client, error) {
	machine, err := resolveUserMachine(ctx, backend, target)
	if errors.Is(err, resolver.ErrMachineNotFound) || errors.Is(err, resolver.ErrMachineAmbiguous) {
		return api.UserMachine{}, nil, friendlyCommandError(err)
	}
	if err != nil {
		return api.UserMachine{}, nil, nil
	}
	return machine, backend, nil
}

// waitForServerMachine waits for a machine only the server lists (a team machine) with the server's own view of it,
// then, for transport and ssh, the authenticated peer probe wait already uses for personal machines.
func waitForServerMachine(ctx context.Context, client *localapi.Client, backend *api.Client, snapshot localapi.Snapshot, machine api.UserMachine, condition string) (localwait.Result, error) {
	return waitForServerMachineWith(ctx, backend.Workspace(), snapshot, machine, condition, client, func() (api.UserMachine, error) {
		return resolveUserMachine(ctx, backend, machine.ID)
	})
}

func waitForServerMachineWith(ctx context.Context, workspace string, snapshot localapi.Snapshot, machine api.UserMachine, condition string, prober peerProber, refresh func() (api.UserMachine, error)) (localwait.Result, error) {
	path := "none"
	for {
		_, sshReady := machine.CurrentSSH()
		ready := machine.Online && machine.InstallationGeneration > 0 && (condition != "ssh" || sshReady)
		if ready && condition != "runtime" {
			info := resolver.ConnectInfo{Workspace: workspace, TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			probe, err := probeDaemonPeer(probeCtx, prober, info, "wait_transport")
			cancel()
			if errors.Is(err, localapi.ErrPermission) {
				return localwait.Result{}, err
			}
			if err == nil {
				path = nativeSnapshotPath(probe.Path)
				if path == "" {
					path = "none"
				}
			}
			ready = err == nil
		}
		if ready {
			return serverMachineResult(snapshot, machine, condition, "ready", "", "ready", path), nil
		}
		select {
		case <-ctx.Done():
			outcome, code := "canceled", "wait_canceled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				outcome, code = "timeout", "wait_timeout"
			}
			return serverMachineResult(snapshot, machine, condition, outcome, code, "offline", "none"), nil
		case <-time.After(timingsOf(ctx).waitServerPoll):
		}
		next, err := refresh()
		if errors.Is(err, resolver.ErrMachineNotFound) {
			return serverMachineResult(snapshot, machine, condition, "failed", "machine_removed", "offline", "none"), nil
		}
		// A failed lookup (server blip) keeps the last known state; the deadline ends the wait.
		if err == nil {
			machine = next
		}
	}
}

func serverMachineResult(snapshot localapi.Snapshot, machine api.UserMachine, condition, outcome, code, state, path string) localwait.Result {
	observed := snapshot.ObservedAt
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	readiness := "unavailable"
	if state == "ready" {
		readiness = "ready"
	}
	status := localapi.MachineStatus{
		ID: machine.ID, EnvironmentID: machine.EnvironmentID, WorkspaceRoot: machine.WorkspaceRoot, Alias: machine.Alias,
		Ownership: machine.Ownership, OwnerAccount: machine.OwnerAccount, Tags: machine.Tags, Platform: machine.Platform,
		Eligible: true, RuntimeState: state, Generation: uint64(machine.InstallationGeneration), SelectedPath: path,
		TransferReadiness: readiness, PreviewReadiness: readiness, SSHReadiness: readiness,
		NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown",
		RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown",
	}
	return localwait.Result{Schema: localwait.ResultSchemaV1, Condition: condition, Outcome: outcome, Code: code, SnapshotGeneration: snapshot.Generation, ObservedAt: observed, Machine: status}
}
