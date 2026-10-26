package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/processlifetime"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func actionSSH(command *cobra.Command, args []string) error {
	dash := command.ArgsLenAtDash()
	if len(args) == 0 || dash > 1 || dash < 0 && len(args) != 1 {
		return invocationError(errors.New("SSH requires [user@]<machine> [-- <OpenSSH arguments...>]"))
	}
	targetName, targetUser, err := managedssh.ParseMachineTarget(args[0])
	if err != nil {
		return invocationError(err)
	}
	flagUser, _ := command.Flags().GetString("user")
	requestedUser, err := resolveSSHRequestedUser(targetUser, flagUser)
	if err != nil {
		return invocationError(err)
	}
	ctx := actionContext(command, args)
	client, machine, target, err := resolveSSHCommandTargetWarm(ctx, targetName)
	if err != nil {
		return friendlyCommandError(err)
	}
	if err := sshTargetHealth(command.Context(), machine.ID, machine.Alias); err != nil {
		return err
	}
	domain, err := configuredManagedSSHDomain(ctx)
	if err != nil {
		return err
	}
	destination, err := managedssh.ResolveDestination(managedssh.DestinationInput{AliasDomain: domain, Alias: machine.Alias, RegisteredPort: target.Port, RequestedUser: requestedUser, RegisteredUser: target.OSUser, HasRegisteredUser: true, Platform: machine.Platform})
	if err != nil {
		return err
	}
	environment, err := managedSSHEnvironment(ctx, os.Environ())
	if err != nil {
		return err
	}
	err = executeManagedSSH(command, ctx, machine, destination, args[1:], dash == 1, environment)
	var exit managedssh.NativeExitError
	if errors.As(err, &exit) && exit.Code == 255 {
		// Only a failed connection needs the server; the warm path built no client (sshClientFor).
		if client, clientErr := sshClientFor(ctx, client); clientErr == nil && !explainRevokedSSHAccess(command, client, machine, err) {
			explainChangedSSHHostIdentity(command, client, machine, err)
		}
	}
	return err
}

// explainRevokedSSHAccess follows an OpenSSH connection failure (exit 255) with the reason when it is a revocation: the
// host closes open sessions within seconds of the user's access ending, and OpenSSH itself only says the connection closed.
// The server answers a cheap read about this machine with 401 (this sign-in was revoked) or 403 (access to the machine ended).
func explainRevokedSSHAccess(command *cobra.Command, client *api.Client, machine api.UserMachine, err error) bool {
	var exit managedssh.NativeExitError
	if !errors.As(err, &exit) || exit.Code != 255 || client == nil {
		return false
	}
	_, probeErr := client.ManagedSSHPendingHostKeys(command.Context(), machine.ID, uint64(machine.InstallationGeneration))
	var apiErr *api.APIError
	switch {
	case errors.Is(probeErr, api.ErrUnauthenticated):
		fmt.Fprintf(command.ErrOrStderr(), "pb: access to %s was revoked: this sign-in was ended. Run `pb login` to sign in again.\n", machine.Alias)
	case errors.As(probeErr, &apiErr) && apiErr.Status == http.StatusForbidden:
		fmt.Fprintf(command.ErrOrStderr(), "pb: access to %s was revoked: your account no longer has permission to use it. Ask its owner or team admin.\n", machine.Alias)
	default:
		return false
	}
	return true
}

// explainChangedSSHHostIdentity follows an OpenSSH connection failure (exit 255) with what to do when the
// machine has published new host keys that are not trusted yet: OpenSSH itself only says the key changed
// and suggests editing known_hosts, which managed SSH does not use.
func explainChangedSSHHostIdentity(command *cobra.Command, client *api.Client, machine api.UserMachine, err error) {
	var exit managedssh.NativeExitError
	if !errors.As(err, &exit) || exit.Code != 255 || client == nil {
		return
	}
	generation := uint64(machine.InstallationGeneration)
	pending, pendingErr := client.ManagedSSHPendingHostKeys(command.Context(), machine.ID, generation)
	if pendingErr != nil || pending.Fingerprint == "" {
		return
	}
	fmt.Fprintf(command.ErrOrStderr(), "pb: %s published a new SSH host identity that is not trusted yet (pending %s). If you expected the change, trust it with `pb ssh trust-host %s --fingerprint %s`.\n", machine.Alias, pending.Fingerprint, machine.Alias, pending.Fingerprint)
}

func resolveSSHRequestedUser(targetUser, flagUser string) (string, error) {
	if flagUser != "" {
		if err := managedssh.ValidateUsername(flagUser); err != nil {
			return "", err
		}
	}
	if flagUser != "" && targetUser != "" && flagUser != targetUser {
		return "", managedssh.ErrSSHUsernameConflict
	}
	if flagUser != "" {
		return flagUser, nil
	}
	return targetUser, nil
}

func openSSHArguments(destination managedssh.Destination, passthrough []string, includePassthrough bool) []string {
	arguments := openSSHSecurityArguments(destination)
	if destination.Port != 22 {
		arguments = append(arguments, "-p", strconv.Itoa(int(destination.Port)))
	}
	arguments = append(arguments, destination.User+"@"+destination.Host)
	if includePassthrough {
		arguments = append(arguments, passthrough...)
	}
	return arguments
}

// openSSHSecurityArguments pins the managed key (public key only, nothing to type) for the user who
// set pb up. Any other user gets none of it: OpenSSH authenticates them as usual with their own
// agent, keys or a password prompt, over the same Paperboat transport.
func openSSHSecurityArguments(destination managedssh.Destination) []string {
	if !destination.Managed {
		return nil
	}
	return []string{
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "PreferredAuthentications=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
	}
}

func actionSSHProxy(command *cobra.Command, _ []string) error {
	if err := processlifetime.ArmParentDeath(command.Context()); err != nil {
		return err
	}
	host, _ := command.Flags().GetString("host")
	portText, _ := command.Flags().GetString("port")
	proxyUser, _ := command.Flags().GetString("user")
	if err := managedssh.ValidateUsername(proxyUser); err != nil {
		return err
	}
	ctx := actionContext(command, nil)
	domain, err := configuredManagedSSHDomain(ctx)
	if err != nil {
		return err
	}
	alias, err := managedssh.ParseAliasHost(host, domain)
	if err != nil {
		return err
	}
	_, machine, target, err := resolveSSHCommandTargetWarm(ctx, alias)
	if err != nil {
		return friendlyCommandError(err)
	}
	if _, err := managedssh.ResolveDestination(managedssh.DestinationInput{AliasDomain: domain, Alias: machine.Alias, RegisteredPort: target.Port, RequestedUser: proxyUser, RegisteredUser: target.OSUser, HasRegisteredUser: true, Platform: machine.Platform}); err != nil {
		return err
	}
	if _, err := managedssh.ValidateDestinationPort(portText, target.Port); err != nil {
		return err
	}
	d, err := buildDeps(ctx)
	if err != nil {
		return err
	}
	if d.peerApplications == nil {
		return errors.New("Paperboat peer transport is unavailable")
	}
	operationID := newSSHOperationID()
	descriptor := pendingSSHDescriptor(machine, operationID)
	workspace, err := workspaceForCommandContext(ctx)
	if err != nil {
		return err
	}
	connection, err := d.peerApplications.DialSSH(command.Context(), sshConnectInfo(machine, descriptor, workspace), operationID)
	if err != nil {
		if notReady := sshTargetHealth(command.Context(), machine.ID, machine.Alias); notReady != nil {
			return notReady
		}
		return err
	}
	halfCloser, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		_ = connection.Close()
		return tunnel.ErrInputEOFUnsupported
	}
	defer connection.Close()
	go func() {
		<-command.Context().Done()
		_ = connection.Close()
	}()
	err = copySSHProxy(connection, halfCloser, command.InOrStdin(), command.OutOrStdout())
	return err
}

func copySSHProxy(connection io.ReadWriteCloser, halfCloser interface{ CloseWrite() error }, input io.Reader, output io.Writer) error {
	type inputResult struct {
		copyErr       error
		closeWriteErr error
	}
	inputDone := make(chan inputResult, 1)
	go func() {
		_, copyErr := io.Copy(connection, input)
		inputDone <- inputResult{copyErr: copyErr, closeWriteErr: halfCloser.CloseWrite()}
	}()
	remote := bufio.NewReader(connection)
	if refused := managedssh.ReadRefusal(remote); refused != nil {
		return refused
	}
	copied, outputErr := io.Copy(output, remote)
	// A remote end with no byte at all is sshd ending the connection before its banner.
	var closed error
	if copied == 0 {
		closed = managedssh.ErrSSHServerClosed
	}
	if outputErr != nil && !errors.Is(outputErr, io.EOF) {
		return outputErr
	}
	// Remote EOF terminates ProxyCommand even when OpenSSH deliberately keeps
	// its input pipe open until the proxy exits. Input EOF still half-closes the
	// stream above and permits all remaining remote output to drain first.
	select {
	case result := <-inputDone:
		// The authenticated remote EOF is authoritative. A concurrent local
		// half-close can observe any platform-specific socket shutdown error after
		// that EOF; this is normal full-duplex shutdown, not a failed SSH
		// transport. Input-copy failures remain authoritative because bytes may
		// have been lost before the remote EOF.
		if result.copyErr != nil {
			return result.copyErr
		}
		return closed
	case <-time.After(25 * time.Millisecond):
		// Keep remote EOF bounded when OpenSSH intentionally leaves stdin open,
		// while giving an already-failed input copy a deterministic chance to
		// publish its authoritative transport error.
		return closed
	}
}

func actionSSHKnownHosts(command *cobra.Command, _ []string) error {
	host, _ := command.Flags().GetString("host")
	portText, _ := command.Flags().GetString("port")
	ctx := actionContext(command, nil)
	domain, err := configuredManagedSSHDomain(ctx)
	if err != nil {
		return err
	}
	alias, err := managedssh.ParseAliasHost(host, domain)
	if err != nil {
		return err
	}
	client, machine, target, err := resolveSSHCommandTargetWarm(ctx, alias)
	if err != nil {
		return friendlyCommandError(err)
	}
	if _, err := managedssh.ValidateDestinationPort(portText, target.Port); err != nil {
		return err
	}
	hostKeys := machine.SSHHostKeys // from the daemon's inventory (F5-07)
	if len(hostKeys) == 0 {
		if client, err = sshClientFor(ctx, client); err != nil {
			return err
		}
		set, err := client.ManagedSSHHostKeys(command.Context(), machine.ID, uint64(machine.InstallationGeneration))
		if err != nil {
			return friendlyCommandError(err)
		}
		hostKeys = set.Keys
	}
	keys, err := managedssh.ParseHostPublicKeys(hostKeys)
	if err != nil {
		return err
	}
	knownHosts, err := managedssh.FormatKnownHosts(host, target.Port, keys)
	if err != nil {
		return err
	}
	_, err = command.OutOrStdout().Write(knownHosts)
	return err
}

func actionSSHTrustHost(command *cobra.Command, args []string) error {
	client, machine, _, err := resolveSSHCommandTarget(actionContext(command, args), args[0])
	if err != nil {
		return friendlyCommandError(err)
	}
	generation := uint64(machine.InstallationGeneration)
	active, err := client.ManagedSSHHostKeys(command.Context(), machine.ID, generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	pending, err := client.ManagedSSHPendingHostKeys(command.Context(), machine.ID, generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	fingerprint, _ := command.Flags().GetString("fingerprint")
	fingerprint = strings.TrimSpace(fingerprint)
	jsonOutput, _ := command.Flags().GetBool("json")
	if fingerprint != "" && fingerprint != pending.Fingerprint {
		return localArgumentError("--fingerprint does not match the pending SSH host identity of " + machine.Alias + " (" + pending.Fingerprint + "); nothing was trusted.")
	}
	if fingerprint == "" {
		if jsonOutput {
			return invocationError(errors.New("ssh trust-host with --json requires the exact --fingerprint"))
		}
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("interactive confirmation is required; pass the exact --fingerprint")
		}
		fmt.Fprintf(command.ErrOrStderr(), "Current SSH host: %s\nPending SSH host: %s\nTrust the pending host identity? [y/N] ", active.Fingerprint, pending.Fingerprint)
		answer, readErr := bufio.NewReader(command.InOrStdin()).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return errors.New("SSH host trust was not changed")
		}
		fingerprint = pending.Fingerprint
	}
	_, err = client.PromoteManagedSSHHostKeys(command.Context(), machine.ID, pending.SetID, fingerprint, newIdempotencyKey(), generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	if jsonOutput {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"machine": map[string]string{"id": machine.ID, "alias": machine.Alias}, "fingerprint": fingerprint, "trusted": true})
	}
	fmt.Fprintf(command.OutOrStdout(), "Trusted SSH host identity for %s (%s)\n", machine.Alias, fingerprint)
	return nil
}

func managedSSHDomain(ctx context.Context, client *api.Client) (string, error) {
	lookup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	configuration, err := client.ClientConfiguration(lookup)
	if err != nil {
		return "", fmt.Errorf("discover managed SSH domain: %w", err)
	}
	if err := managedssh.ValidateAliasDomain(configuration.LocalAccessBaseDomain); err != nil {
		return "", fmt.Errorf("server returned an invalid managed SSH domain: %w", err)
	}
	return configuration.LocalAccessBaseDomain, nil
}

// configuredManagedSSHDomain is the alias domain from the daemon's snapshot (no request), else from the server.
func configuredManagedSSHDomain(ctx *command.Context) (string, error) {
	if snapshot, err := loadWarmMachineSnapshot(ctx.Context); err == nil && managedssh.ValidateAliasDomain(snapshot.SSHDomain) == nil {
		return snapshot.SSHDomain, nil
	}
	dependencies, err := buildDeps(ctx)
	if err != nil {
		return "", err
	}
	client, err := sshAPIClient(ctx, dependencies)
	if err != nil {
		return "", err
	}
	return managedSSHDomain(ctx.Context, client)
}
