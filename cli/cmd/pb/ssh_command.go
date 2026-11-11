package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/processlifetime"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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
	environment = pbSSHEnvironment(environment)
	if notice := agentForwardingNotice(machine.Platform, args[1:]); notice != "" {
		fmt.Fprintln(command.ErrOrStderr(), notice)
	}
	err = executeManagedSSH(command, ctx, machine, destination, args[1:], dash == 1, environment)
	clientFor := func() (*api.Client, error) { return sshClientFor(ctx, client) }
	if changed := changedSSHHostFailure(command.Context(), machine, err, clientFor); changed != nil {
		return changed
	}
	explainSSHFailure(command, machine, err, clientFor)
	return err
}

// changedSSHHostFailure is the refusal for a host key outside the active set when pb ran SSH itself (Windows
// `pb ssh <machine> -- <command>`): that failure is ErrKnownHosts, not OpenSSH's exit 255, so it gets the same
// trust-host answer pb ssh doctor gives. Any other failure returns nil.
func changedSSHHostFailure(ctx context.Context, machine api.UserMachine, err error, clientFor func() (*api.Client, error)) error {
	if !errors.Is(err, managedssh.ErrKnownHosts) {
		return nil
	}
	client, clientErr := clientFor()
	if clientErr != nil {
		return nil
	}
	return pendingHostKeyFailure(ctx, client, machine, err)
}

// explainSSHFailure follows a failed OpenSSH connection (exit 255) with its reason. Only then is the server
// needed; the warm path built no client.
func explainSSHFailure(command *cobra.Command, machine api.UserMachine, err error, clientFor func() (*api.Client, error)) {
	var exit managedssh.NativeExitError
	if !errors.As(err, &exit) || exit.Code != 255 {
		return
	}
	client, clientErr := clientFor()
	switch {
	case errors.Is(clientErr, config.ErrNoCredentials):
		// Signed out (pb logout, or the session revoked and removed): the host closed the session for it.
		fmt.Fprintf(command.ErrOrStderr(), sshSignInEndedMessage, machine.Alias)
	case clientErr == nil && !explainRevokedSSHAccess(command, client, machine, err):
		explainChangedSSHHostIdentity(command, client, machine, err)
	}
}

const sshSignInEndedMessage = "pb: access to %s was revoked: this sign-in was ended. Run `pb login` to sign in again.\n"

// explainRevokedSSHAccess follows an OpenSSH connection failure (exit 255) with the reason when it is a revocation: the
// host closes open sessions within seconds of the user's access ending, and OpenSSH itself only says the connection closed.
// The server answers a read of this machine with 401 (this sign-in was revoked), 404 user_machine_not_found (the machine is
// no longer visible to this account: its grant, the team share or the membership ended) or a machine whose permissions no
// longer include managed_ssh. The managed-SSH host-key reads cannot tell these apart: they answer 404 for a missing
// pending set and for no access alike.
func explainRevokedSSHAccess(command *cobra.Command, client *api.Client, machine api.UserMachine, err error) bool {
	var exit managedssh.NativeExitError
	if !errors.As(err, &exit) || exit.Code != 255 || client == nil {
		return false
	}
	current, probeErr := client.GetUserMachine(command.Context(), machine.ID)
	var apiErr *api.APIError
	switch {
	case errors.Is(probeErr, api.ErrUnauthenticated):
		fmt.Fprintf(command.ErrOrStderr(), sshSignInEndedMessage, machine.Alias)
	case errors.As(probeErr, &apiErr) && (apiErr.Status == http.StatusNotFound && apiErr.Code == "user_machine_not_found"):
		fmt.Fprintf(command.ErrOrStderr(), "pb: access to %s was revoked: it is no longer shared with you. Ask its owner or team admin.\n", machine.Alias)
	case errors.As(probeErr, &apiErr) && apiErr.Status == http.StatusForbidden,
		probeErr == nil && current.Ownership != "personal" && !slices.Contains(current.Permissions, "managed_ssh"):
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
	fmt.Fprintf(command.ErrOrStderr(), "pb: %s\n", pendingSSHHostIdentityMessage(machine.Alias, pending.Fingerprint))
}

// pendingSSHHostIdentityMessage names a changed host identity that waits for the owner's trust, with the exact command.
func pendingSSHHostIdentityMessage(alias, fingerprint string) string {
	return fmt.Sprintf("%s published a new SSH host identity that is not trusted yet (pending %s). If you expected the change, trust it with `pb ssh trust-host %s --fingerprint %s`.", alias, fingerprint, alias, fingerprint)
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

// windowsLoopbackOpenSSHArguments are ssh.exe's arguments for the Windows loopback connection (pb ssh without a
// command). It overrides Hostname with 127.0.0.1, so the managed fragment's `Match host` group does not apply and
// the managed identity is passed here instead (identity: managedssh.ManagedIdentityArguments).
func windowsLoopbackOpenSSHArguments(destination managedssh.Destination, port uint16, executable string, identity, passthrough []string, includePassthrough bool) []string {
	knownHostsCommand := quoteWindowsOpenSSHCommand(executable) + " __ssh-known-hosts --host " + destination.Host + " --port " + strconv.Itoa(int(destination.Port))
	arguments := openSSHSecurityArguments(destination)
	if destination.Managed {
		arguments = append(arguments, identity...)
	}
	arguments = append(arguments,
		"-o", "ProxyCommand=none",
		"-o", "Hostname=127.0.0.1",
		"-o", "HostKeyAlias="+destination.Host,
		"-o", "KnownHostsCommand="+knownHostsCommand,
		"-p", strconv.Itoa(int(port)),
		destination.User+"@"+destination.Host,
	)
	if includePassthrough {
		arguments = append(arguments, passthrough...)
	}
	return arguments
}

func quoteWindowsOpenSSHCommand(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
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
	var machine api.UserMachine
	var target api.ManagedSSHTarget
	var resolvedClient *api.Client
	if _, err := retryWhileMachineOffline(ctx.Context, func() (struct{}, error) {
		var resolveErr error
		resolvedClient, machine, target, resolveErr = resolveSSHCommandTargetWarm(ctx, alias)
		return struct{}{}, resolveErr
	}); err != nil {
		return friendlyCommandError(err)
	}
	if resolvedClient == nil {
		var noHostKeys bool
		machine, noHostKeys = freshenSSHHostKeys(command.Context(), machine, func() (api.UserMachine, error) {
			_, refreshed, _, err := resolveSSHCommandTargetWarm(ctx, alias)
			return refreshed, err
		})
		if noHostKeys {
			fmt.Fprintf(command.ErrOrStderr(), "pb: %s has not published its SSH host keys yet. Check that sshd is installed and running there; a new sshd is picked up within about 30 seconds. `pb ssh doctor %s` shows the state.\n", machine.Alias, machine.Alias)
		}
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
	// Bounds only the stream setup: a dial to an offline target otherwise waits until OpenSSH gives up.
	dialCtx, cancelDial := context.WithTimeout(command.Context(), sshDialTimeout)
	// The target's carrier restarts for a pb update: a dial that finds its transport closed is repeated for a few seconds.
	connection, err := retrySSHDial(dialCtx, func() (tunnel.Conn, error) {
		return d.peerApplications.DialSSH(dialCtx, sshConnectInfo(machine, descriptor, workspace, proxyUser), operationID)
	})
	cancelDial()
	if err != nil {
		if notReady := sshTargetHealth(command.Context(), machine.ID, machine.Alias); notReady != nil {
			return notReady
		}
		return explainSSHDial(machine.Alias, err)
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
	input := &eofTracker{Reader: command.InOrStdin()}
	err = copySSHProxy(connection, halfCloser, input, command.OutOrStdout())
	explainEndedSSHProxy(command, machine, input.sawEOF(), func() (*api.Client, error) { return sshClientFor(ctx, nil) })
	return finishSSHProxy(command.Context(), err, input.sawEOF())
}

// finishSSHProxy is the proxy's result. A session that ended normally (OpenSSH closed its input, or exited and took the
// proxy's context with it) closes the stream under the copy loop; that read or write on a closed stream is the end of
// the session, not a failure, and must not print Paperboat's generic network text after a successful `ssh host cmd`.
// A closed-stream error with neither of those is a real mid-session failure and is kept.
func finishSSHProxy(ctx context.Context, err error, inputClosed bool) error {
	if errors.Is(err, net.ErrClosed) && (inputClosed || ctx.Err() != nil) {
		return nil
	}
	return err
}

// eofTracker remembers that OpenSSH closed the proxy's input.
type eofTracker struct {
	io.Reader
	eof atomic.Bool
}

func (t *eofTracker) Read(p []byte) (int, error) {
	n, err := t.Reader.Read(p)
	if err == io.EOF {
		t.eof.Store(true)
	}
	return n, err
}

func (t *eofTracker) sawEOF() bool { return t.eof.Load() }

// sshMessageOwnerEnv is set by `pb ssh` for the OpenSSH it runs (and so for its ProxyCommand): pb ssh explains the
// failure itself once OpenSSH exits, so the proxy stays silent and the user reads the message once. A plain `ssh`
// has no pb around it and the proxy prints it.
const sshMessageOwnerEnv = "PAPERBOAT_SSH_MESSAGE_OWNER"

// pbSSHEnvironment marks the environment of the OpenSSH that `pb ssh` runs.
func pbSSHEnvironment(environment []string) []string {
	return append(environment, sshMessageOwnerEnv+"=1")
}

// explainEndedSSHProxy says why the stream ended when the host closed it while OpenSSH still held it open: OpenSSH shows
// this ProxyCommand's stderr, and alone it only prints a broken pipe (F5-10). An ordinary logout closes OpenSSH's side
// first, so it asks nothing and prints nothing; a stream closed for another reason prints nothing either.
func explainEndedSSHProxy(command *cobra.Command, machine api.UserMachine, inputClosed bool, clientFor func() (*api.Client, error)) {
	if inputClosed || os.Getenv(sshMessageOwnerEnv) != "" {
		return
	}
	explainSSHFailure(command, machine, managedssh.NativeExitError{Code: 255}, clientFor)
}

// sshDialError names why pb's ProxyCommand could not open the SSH stream, in pb's words (F5-09).
type sshDialError struct {
	message string
	cause   error
}

func (e *sshDialError) Error() string { return e.message }
func (e *sshDialError) Unwrap() error { return e.cause }

func explainSSHDial(alias string, err error) error {
	var remote *localapi.RemoteError
	switch {
	case errors.Is(err, localapi.ErrTransportUnavailable):
		return &sshDialError{"this computer's Paperboat service is not running; start it (`pb status` shows it), then retry", err}
	case errors.As(err, &remote) && remote.Code == "peer_transport_unavailable", sshDialTimedOut(err):
		return &sshDialError{"Paperboat could not reach " + alias + ": it may be offline, or its Paperboat service is not running; check it in `pb status`", err}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return &sshDialError{"this computer's Paperboat service dropped the connection while opening the SSH stream to " + alias + " (step: open stream); retry", err}
	}
	// Any other network error would reach the generic "Paperboat is unreachable" text, which hides that the dial to the
	// local service (not Paperboat) failed and where.
	var network net.Error
	if errors.As(err, &network) {
		return &sshDialError{"opening the SSH stream to " + alias + " through this computer's Paperboat service failed (step: open stream): " + api.BoundedServerMessage(err.Error()), err}
	}
	return err
}

// sshHandoverGrace bounds the repeated dial while the target's network is handed over (about a second on pbvm4).
const sshHandoverGrace = 10 * time.Second

// sshHandoverBlip is a dial failure that a moment later no longer applies: the peer transport was lost or is not
// there yet. A refusal, a timeout or a stopped local service is not.
func sshHandoverBlip(err error) bool {
	var remote *localapi.RemoteError
	return errors.Is(err, tunnel.ErrTransportLost) || errors.As(err, &remote) && remote.Code == "peer_transport_unavailable"
}

// retrySSHDial repeats a dial that failed before any byte reached the target (nothing to replay) while the failure is a
// blip: a handover of the target's network for sshHandoverGrace, and for the whole dial bound the target not yet
// accepting this machine or the local service dropping the setup connection, as happens on the first relayed dial
// right after access appeared (the host and the relay learn the new peer seconds later).
func retrySSHDial(ctx context.Context, dial func() (tunnel.Conn, error)) (tunnel.Conn, error) {
	started := time.Now()
	return retryWhile(ctx, sshDialTimeout, func(err error) bool {
		if sshHandoverBlip(err) {
			return time.Since(started) < sshHandoverGrace
		}
		return sshFirstDialBlip(err)
	}, dial)
}

func sshFirstDialBlip(err error) bool {
	var remote *localapi.RemoteError
	if errors.As(err, &remote) {
		return remote.StatusCode == http.StatusServiceUnavailable && remote.Code == "peer_stream_unavailable"
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// sshDialTimeout bounds opening the SSH stream to the target (a warm connection takes well under a second).
const sshDialTimeout = 30 * time.Second

// sshDialTimedOut is a dial to the target that got no answer in time: the target, not Paperboat, is unreachable.
func sshDialTimedOut(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()
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

// refreshAfterSSHTrustHost is replaced in tests.
var refreshAfterSSHTrustHost = refreshLocalInventory

func actionSSHTrustHost(command *cobra.Command, args []string) error {
	client, machine, _, err := resolveSSHCommandTarget(actionContext(command, args), args[0])
	if err != nil {
		return friendlyCommandError(err)
	}
	return trustSSHHost(command, client, machine)
}

func trustSSHHost(command *cobra.Command, client *api.Client, machine api.UserMachine) error {
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
	// pb ssh reads host keys from the daemon's machine list (F5-07): refresh it so the next pb ssh offers the
	// trusted set at once instead of the old one (C066).
	if err := refreshAfterSSHTrustHost(command.Context()); err != nil {
		fmt.Fprintf(command.ErrOrStderr(), "pb: trusted, but this computer's machine list did not refresh yet; `pb ssh %s` can need a few seconds.\n", machine.Alias)
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

// sshWindowsAgentForwardingMessage is printed when `pb ssh -A` targets a Windows machine. Microsoft's sshd cannot forward an
// agent (Win32-OpenSSH #1461, #1865: session.c hardcodes /tmp/ssh-XXX and the AF_UNIX server bind is refused), so the session
// goes on without one. tools/device-smoke/ssh_mac_rows.py asserts this exact line.
const sshWindowsAgentForwardingMessage = "Agent forwarding isn't available on Windows machines (Microsoft OpenSSH limitation); port forwarding works."

// agentForwardingNotice is the line to print before the session starts: only a Windows machine asked to forward an agent.
func agentForwardingNotice(platform string, args []string) string {
	if strings.EqualFold(platform, "windows") && requestsAgentForwarding(args) {
		return sshWindowsAgentForwardingMessage
	}
	return ""
}

// requestsAgentForwarding reports whether OpenSSH arguments ask for agent forwarding: -A (alone or inside a flag cluster)
// or -o ForwardAgent=yes, before the remote command starts. A later -a turns it off again.
func requestsAgentForwarding(args []string) bool {
	const withValue = "BbcDEeFIiJLlmOopQRSWw" // OpenSSH flags whose value is the rest of the word or the next one
	forwarding := false
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" || !strings.HasPrefix(argument, "-") || argument == "-" {
			break
		}
		for position := 1; position < len(argument); position++ {
			flag := argument[position]
			if flag == 'A' {
				forwarding = true
			} else if flag == 'a' {
				forwarding = false
			} else if strings.IndexByte(withValue, flag) >= 0 {
				value := argument[position+1:]
				if value == "" && index+1 < len(args) {
					index++
					value = args[index]
				}
				if flag == 'o' {
					if name, setting, ok := strings.Cut(strings.NewReplacer("=", " ").Replace(value), " "); ok && strings.EqualFold(name, "ForwardAgent") {
						forwarding = strings.EqualFold(strings.TrimSpace(setting), "yes")
					}
				}
				break
			}
		}
	}
	return forwarding
}
