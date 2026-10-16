package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
)

func actionSSHDoctor(command *cobra.Command, args []string) error {
	capabilities, err := managedssh.ProbeOpenSSH(command.Context(), "ssh", 5*time.Second)
	if err != nil || !capabilities.Ready() {
		if command.Context().Err() != nil {
			return command.Context().Err()
		}
		return commandPreparationFailure{step: prepareOpenSSH, cause: errors.Join(managedssh.ErrOpenSSHUnavailable, err)}
	}
	client, machine, target, err := resolveSSHCommandTarget(actionContext(command, args), args[0])
	if err != nil {
		return friendlyCommandError(err)
	}
	keys, err := client.ManagedSSHHostKeys(command.Context(), machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return friendlyCommandError(err)
	}
	cfg, store, err := requireAuthConfig(actionContext(command, args))
	if err != nil {
		return err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return err
	}
	identity, err := store.ManagedSSHIdentity(cfg.ServerURL, profile.CLIClientSessionID)
	if err != nil {
		return err
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	domain, err := managedSSHDomain(command.Context(), client)
	if err != nil {
		return err
	}
	agentSocket := managedssh.InstalledAgentSocket(paths.RuntimeRoot)
	if err := managedssh.ValidateInstalledOpenSSHConfig(home, uint32(os.Geteuid()), agentSocket, domain); err != nil {
		return fmt.Errorf("managed OpenSSH configuration is not ready; rerun `pb login`: %w", err)
	}
	if err := managedssh.ValidateManagedIdentityPublicKey(home, uint32(os.Geteuid()), identity.PublicKey); err != nil {
		return fmt.Errorf("managed SSH public identity is not ready; rerun `pb login`: %w", err)
	}
	if err := managedssh.ProbeAgentIdentity(command.Context(), agentSocket, identity.Fingerprint, 5*time.Second); err != nil {
		return fmt.Errorf("managed SSH agent is not ready; rerun `pb login`: %w", err)
	}
	d, err := buildDeps(actionContext(command, args))
	if err != nil {
		return err
	}
	if d.peerApplications == nil {
		return errors.New("Paperboat peer transport is unavailable")
	}
	operationID := newSSHOperationID()
	descriptor := pendingSSHDescriptor(machine, operationID)
	probeCtx, cancel := context.WithTimeout(command.Context(), 20*time.Second)
	defer cancel()
	connection, err := d.peerApplications.DialSSH(probeCtx, sshConnectInfo(machine, descriptor, client.Workspace()), operationID)
	if err != nil {
		return fmt.Errorf("managed SSH transport or loopback target is not ready: %w", err)
	}
	host, err := managedssh.AliasHost(machine.Alias, domain)
	if err != nil {
		_ = connection.Close()
		return err
	}
	authErr := managedssh.ProbeSSHAuthentication(probeCtx, connection, net.JoinHostPort(host, strconv.Itoa(int(target.Port))), target.OSUser, identity.Signer, keys.Keys)
	closeErr := connection.Close()
	if authErr != nil {
		return fmt.Errorf("managed SSH host verification or key reconciliation is not ready: %w", authErr)
	}
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return fmt.Errorf("close managed SSH readiness probe: %w", closeErr)
	}
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"openssh": map[string]any{"version": capabilities.Version, "ready": true}, "local_configuration": "ready", "managed_agent": "ready", "machine": map[string]any{"id": machine.ID, "alias": machine.Alias}, "transport": "ready", "target": map[string]any{"state": "ready", "port": target.Port}, "host_identity": keys.Fingerprint, "managed_keys": "reconciled"})
	}
	fmt.Fprintf(command.OutOrStdout(), "OpenSSH: %s\nLocal configuration: ready\nManaged agent: ready\nMachine: %s\nTransport: ready\nTarget: ready on port %d\nHost identity: %s\nManaged keys: reconciled\n", capabilities.Version, machine.Alias, target.Port, keys.Fingerprint)
	return nil
}
