package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
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
	connection, err := d.peerApplications.DialSSH(probeCtx, sshConnectInfo(machine, descriptor, client.Workspace(), target.OSUser), operationID)
	if err != nil {
		return fmt.Errorf("managed SSH transport or loopback target is not ready: %w", err)
	}
	host, err := managedssh.AliasHost(machine.Alias, domain)
	if err != nil {
		_ = connection.Close()
		return err
	}
	// The host writes a refusal line before sshd's banner when it knows sshd will refuse (macOS Remote Login), and
	// ends the stream with nothing when it does not serve it; read that first, and stop waiting for a silent
	// stream when the probe's time is up.
	stopWatch := context.AfterFunc(probeCtx, func() { _ = connection.Close() })
	reader := bufio.NewReader(connection)
	if refused := managedssh.ReadServerStart(reader); refused != nil {
		stopWatch()
		_ = connection.Close()
		return refused
	}
	authErr := managedssh.ProbeSSHAuthentication(probeCtx, bufferedStream{ReadWriteCloser: connection, reader: reader}, net.JoinHostPort(host, strconv.Itoa(int(target.Port))), target.OSUser, identity.Signer, keys.Keys)
	stopWatch()
	closeErr := connection.Close()
	if authErr != nil {
		if changed := pendingHostKeyFailure(command.Context(), client, machine, authErr); changed != nil {
			return changed
		}
		return fmt.Errorf("managed SSH host verification or key reconciliation is not ready: %w", authErr)
	}
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return fmt.Errorf("close managed SSH readiness probe: %w", closeErr)
	}
	gitAdvice := sshDoctorGitAdvice()
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		result := map[string]any{"openssh": map[string]any{"version": capabilities.Version, "ready": true}, "local_configuration": "ready", "managed_agent": "ready", "machine": map[string]any{"id": machine.ID, "alias": machine.Alias}, "transport": "ready", "target": map[string]any{"state": "ready", "port": target.Port}, "host_identity": keys.Fingerprint, "managed_keys": "reconciled"}
		if gitAdvice != "" {
			result["git_for_windows"] = map[string]any{"state": "uses_its_own_ssh", "fix": gitAdvice}
		}
		return writeCLIJSON(command.OutOrStdout(), result)
	}
	fmt.Fprintf(command.OutOrStdout(), "OpenSSH: %s\nLocal configuration: ready\nManaged agent: ready\nMachine: %s\nTransport: ready\nTarget: ready on port %d\nHost identity: %s\nManaged keys: reconciled\n", capabilities.Version, machine.Alias, target.Port, keys.Fingerprint)
	if gitAdvice != "" {
		fmt.Fprintf(command.OutOrStdout(), "Git for Windows: git uses its own ssh, which cannot use the Paperboat agent. Run once:\n  %s\n", gitAdvice)
	}
	return nil
}

// bufferedStream reads through the reader that already took the first bytes of the stream.
type bufferedStream struct {
	io.ReadWriteCloser
	reader *bufio.Reader
}

func (s bufferedStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

// pendingHostKeyFailure is the doctor's answer when the machine presented a host key outside the active set: the
// changed identity and the command to trust it when the server holds a newer set waiting for the owner's trust, and
// otherwise (the machine has not reported its new keys yet, or this is a different machine) the plain fact and what
// to do, in pb's words: the generic failure text names neither. Any other failure returns nil.
func pendingHostKeyFailure(ctx context.Context, client *api.Client, machine api.UserMachine, authErr error) error {
	if !errors.Is(authErr, managedssh.ErrKnownHosts) {
		return nil
	}
	pending, err := client.ManagedSSHPendingHostKeys(ctx, machine.ID, uint64(machine.InstallationGeneration))
	if err != nil || pending.Fingerprint == "" {
		return &sshDialError{message: machine.Alias + " presented an SSH host key that is not in its trusted set, and has not reported new keys yet. If you changed its host keys, retry `pb ssh doctor " + machine.Alias + "` in a minute; if you did not, do not connect: it may not be that machine.", cause: authErr}
	}
	return &sshDialError{message: pendingSSHHostIdentityMessage(machine.Alias, pending.Fingerprint), cause: authErr}
}
