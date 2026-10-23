// Command pb is the invisible terminal wrapper for the
// Paperboat platform. `pb <environment>` attaches an enrolled machine
// through Paperboat auth and bridges local file pastes into
// remote TUIs. Cross-service calls run behind interfaces so protocol behavior
// remains independently testable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	filetransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func main() {
	os.Exit(mainExit())
}

func mainExit() (exitCode int) {
	ctx := supportref.WithContext(context.Background(), supportref.New())
	// These invocation-owned signals are ordinary cancellation. Keep custom
	// cancellation causes elsewhere intact rather than classifying the private
	// cause attached by os/signal.NotifyContext as an operational failure.
	ctx, cancel := context.WithCancel(ctx)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	signalDone := make(chan struct{})
	go func() {
		defer close(signalDone)
		select {
		case <-interrupts:
			userInterrupted.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		signal.Stop(interrupts)
		cancel()
		<-signalDone
	}()
	reporter := errorreport.FromEnvironment()
	restoreReporter := errorreport.Install(reporter)
	defer restoreReporter()
	component := processComponent(os.Args[1:])
	ctx, closeDiagnostics := openProcessDiagnostics(ctx, os.Args[1:], reporter, os.Stderr)
	defer closeDiagnostics()
	defer func() {
		if failure := recover(); failure != nil {
			panicErr, ok := failure.(error)
			if !ok {
				panicErr = errors.New("process panic")
			}
			reporter.CaptureFailure(ctx, component, "process", "process", "process_panic", panicErr)
			if jsonArgumentRequested(os.Args[1:]) {
				_ = writeCLIJSONError(os.Stdout, &api.APIError{Code: "unexpected_failure", SupportReference: supportref.FromContext(ctx)})
			} else {
				fmt.Fprintf(os.Stderr, "pb: Paperboat stopped unexpectedly. Retry; if this continues, contact support with reference %s.\n", supportref.FromContext(ctx))
			}
			exitCode = 1
		}
	}()
	return runWithReporter(ctx, os.Args[1:], os.Stdout, os.Stderr, reporter)
}

func processComponent(args []string) string {
	root := newRootCommand()
	command, _, err := root.Find(args)
	if err != nil || command == nil {
		return "pb"
	}
	for command.Parent() != nil && command.Parent() != root {
		command = command.Parent()
	}
	if command.Name() == "daemon" || strings.HasPrefix(command.Name(), "__runtime-") {
		return "paperboatd"
	}
	return "pb"
}

var errUsage = errors.New("command usage error")

// currentLocalDaemonPaths is kept behind a small seam so command tests can
// use a private local API endpoint. Production callers use the platform
// owner's stable paths supplied by localdaemon.CurrentUserPaths.
var currentLocalDaemonPaths = localdaemon.CurrentUserPaths

// installLocalDaemonService owns the one production boundary that registers
// the local daemon. Command tests replace it with an in-process sentinel: a
// Go test executable must never be registered as a detached daemon service.
var installLocalDaemonService localDaemonServiceInstaller = localdaemon.InstallCurrentUserService

// removeLocalDaemonService removes the current owner daemon during a fresh reset.
var removeLocalDaemonService localDaemonServiceRemover = localdaemon.RemoveCurrentUserService

var verifySetupInstallSource = hostruntimecmd.VerifyInstallSource
var setupBackendClient = backendClient

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWithReporter(ctx, args, stdout, stderr, nil)
}

func runWithReporter(ctx context.Context, args []string, stdout, stderr io.Writer, reporter *errorreport.Reporter) (exitCode int) {
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	root := newRootCommand()
	operation := "command"
	if resolved, _, err := root.Find(args); err == nil && resolved != nil {
		for resolved.Parent() != nil && resolved.Parent() != root {
			resolved = resolved.Parent()
		}
		operation = errorreport.Operation(resolved.Name())
	}
	ctx, finishObservation := reporter.Start(ctx, processComponent(args), operation)
	outcome := "failed"
	defer func() {
		if failure := recover(); failure != nil {
			finishObservation("failed")
			panic(failure)
		}
		if exitCode != 0 && outcome == "success" {
			outcome = "failed"
		}
		if exitCode == 0 && outcome == "failed" {
			outcome = "success"
		}
		finishObservation(outcome)
	}()
	root.SetOut(stdout)
	root.SetErr(stderr)
	if jsonArgumentRequested(args) {
		if cliArgumentPresent(args, "--version", "-v") && !subcommandOwnsVersionFlag(root, args) {
			outcome = "success"
			if err := writeCLIJSON(stdout, map[string]any{"command": "pb", "version": buildinfo.Version}); err != nil {
				return 1
			}
			return 0
		}
		if cliArgumentPresent(args, "--help", "-h") {
			outcome = "success"
			if err := writeCLIJSON(stdout, cliCommandHelp(root, args)); err != nil {
				return 1
			}
			return 0
		}
	}
	resolvedArgs, preferenceContext, preferenceErr := preparePreferences(root, args, ctx)
	var executed *cobra.Command
	err := preferenceErr
	if err != nil {
		err = preferenceLoadFailure{cause: err}
	}
	if err == nil {
		if _, _, findErr := root.Find(resolvedArgs); findErr != nil {
			err = invocationError(findErr)
		} else {
			root.SetArgs(resolvedArgs)
			executed, err = root.ExecuteContextC(preferenceContext)
		}
	}
	if err == nil {
		outcome = "success"
		return 0
	}
	err = planLimitRefusal(preferenceContext, err)
	failure := classifyCommandFailure(err)
	switch failure.kind {
	case commandCanceled, commandInteractiveCanceled:
		outcome = "canceled"
	case commandUsage, commandRejected:
		outcome = "rejected"
	}
	diagnosticReference := ""
	if failure.kind != commandCanceled && failure.kind != commandInteractiveCanceled {
		faultCtx := ctx
		if supportref.Valid(failure.supportReference) {
			faultCtx = supportref.WithContext(ctx, failure.supportReference)
		}
		if failure.kind == commandUnexpected {
			diagnosticReference = reporter.CaptureFailure(faultCtx, processComponent(args), operation, "command", "unexpected_cli_failure", err).SupportReference
			if directory, dirErr := processDiagnosticsDirectory(executed); dirErr == nil {
				_ = recordUnexpectedFailureCause(directory, operation, diagnosticReference, err)
			}
		} else if failure.kind == commandOperational || failure.kind == commandDeadline || failure.kind == commandRejected {
			diagnosticReference = reporter.ObserveFailure(faultCtx, processComponent(args), operation, "command", "command_failed", err).SupportReference
		}
	}

	jsonOutput := jsonArgumentRequested(args)
	if executed != nil && jsonOutputRequested(executed) {
		jsonOutput = true
	}
	if failure.presented {
		return presentedCommandExitCode(failure)
	}
	if failure.kind == commandInteractiveCanceled {
		if jsonOutput {
			_ = writeCLIJSONError(stdout, err)
			return 130
		}
		return 0
	}
	if failure.kind == commandCanceled {
		for command := executed; command != nil; command = command.Parent() {
			if command.Name() == "daemon" && command.Parent() == root {
				return 0
			}
		}
		if jsonOutput {
			_ = writeCLIJSONError(stdout, err)
		} else {
			fmt.Fprintln(stderr, "pb: Operation canceled.")
		}
		return 130
	}
	if failure.kind == commandUsage {
		if jsonOutput {
			_ = writeCLIJSONError(stdout, err)
		} else {
			fmt.Fprintln(stderr, "pb:", commandFailureMessage(err, failure))
			usage := root
			if executed != nil {
				usage = executed
			}
			usage.SetOut(stderr)
			_ = usage.Usage()
		}
		return 2
	}
	if failure.kind == commandNativeExit {
		if exit, ok := failure.owner.(interface{ ExitCode() int }); ok {
			if _, ok := failure.owner.(jsonFailureExitCodeError); ok && jsonOutput {
				_ = writeCLIJSONError(stdout, err)
			}
			if code := exit.ExitCode(); code >= 0 && code <= 255 {
				return code
			}
		}
		return 1
	}

	if jsonOutput {
		_ = writeCLIJSONErrorWithReference(stdout, err, diagnosticReference)
		return 1
	}
	if message := commandFailureMessage(err, failure); message != "" {
		if diagnosticReference != "" && !strings.Contains(message, diagnosticReference) {
			message = sentence(message) + " Support reference: " + diagnosticReference + "."
		}
		fmt.Fprintln(stderr, "pb:", message)
	}
	return 1
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "pb [environment] [new]",
		Short: "Open Paperboat or connect to an environment terminal",
		Long: `Paperboat provides remote terminals, managed SSH and file transfers, previews,
tunnels, environment configuration, team management, and local runtime controls.

Run pb without arguments in a terminal to open the interactive home screen.
Run pb new to open a fresh terminal on this enrolled device in the current directory.
Use explicit subcommands in scripts; pb COMMAND --help describes each operation.
Use pb COMMAND --help --json for machine-readable command discovery.

The --json flag selects machine-readable output and suppresses interactive menus.
Raw terminal and native transfer commands reject unsupported JSON output before
execution. Streaming commands may emit multiple JSON records; consult their help.

Local preferences can define shortcuts, command defaults and TUI appearance.
Run pb config customize to edit them, or use --no-customization to bypass them.
Explicit flags override saved defaults. Built-in command names remain reserved.
Numeric invocations such as pb 3000 select the configured preview or tunnel action.

Run pb doctor for diagnostics. Check command output for partial changes and
recovery instructions before retrying an operation that may have created resources.`,
		Example: "  pb\n  pb new\n  pb auth login\n  pb environments\n  pb connect Studio\n  pb ssh Studio\n  pb preview 3000\n  pb tunnel create demo --port 3000\n  pb config customize\n  pb --no-customization environments --json",
		Args: commandArgs(func(command *cobra.Command, args []string) error {
			if command.ArgsLenAtDash() == 1 && len(args) >= 2 {
				return nil
			}
			return terminalArgs(0)(command, args)
		}),
		RunE: func(command *cobra.Command, args []string) error {
			if command.ArgsLenAtDash() == 1 {
				return actionExecCobra(command, args, true)
			}
			if len(args) == 0 {
				serverFlag := command.Flags().Lookup("server")
				if serverFlag != nil && serverFlag.Changed {
					cfg, err := config.Load(configPathFlag(command))
					if err != nil {
						return err
					}
					server, _ := command.Flags().GetString("server")
					normalized, err := config.NormalizeServerURL(server)
					if err != nil {
						return invocationError(err)
					}
					cfg.ServerURL = normalized
					if err := cfg.Save(); err != nil {
						return err
					}
					input, interactive := command.InOrStdin().(*os.File)
					if !interactive || !term.IsTerminal(int(input.Fd())) {
						if jsonOutputRequested(command) {
							return writeCLIJSON(command.OutOrStdout(), map[string]any{"server": normalized, "path": cfg.Path(), "updated": true})
						}
						return nil
					}
				}
				if jsonOutputRequested(command) {
					return writeCLIJSON(command.OutOrStdout(), map[string]any{"command": "pb", "version": buildinfo.Version, "commands": cliCommandCatalog(command.Root())})
				}
				return actionHome(command)
			}
			if jsonOutputRequested(command) {
				return unsupportedJSONOutputError{command: "pb terminal shorthand; use `pb exec --json` for machine output"}
			}
			if err := validateConnectInvocation(command); err != nil {
				return err
			}
			return actionConnect(actionContext(command, args))
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.Version = buildinfo.Version
	root.SetVersionTemplate(versionDisplay(buildinfo.Version))
	root.InitDefaultVersionFlag()
	root.Flags().Lookup("version").Shorthand = "v"
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return invocationError(err) })
	root.PersistentFlags().String("config", "", "path to the CLI config file")
	root.PersistentFlags().String("server", "", "paperboat-server base URL override")
	root.PersistentFlags().String("workspace", "", "resource workspace selector: personal or a team slug")
	root.PersistentFlags().Bool("json", false, "print machine-readable JSON")
	root.PersistentFlags().Bool("no-customization", false, "ignore local shortcuts, command defaults, and TUI preferences")
	root.SetHelpCommand(newCLIHelpCommand(root))
	root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if err := command.ValidateRequiredFlags(); err != nil {
			return invocationError(err)
		}
		if err := command.ValidateFlagGroups(); err != nil {
			// The registered audience flags are the only exclusive group. Build
			// public prose from their names, without exposing Cobra argument text.
			private := command.Flags().Lookup("private")
			team := command.Flags().Lookup("team")
			if private != nil && team != nil && private.Changed && team.Changed {
				return usageError{err: err, publicMessage: "Flags --team and --private are mutually exclusive. Choose one audience."}
			}
			return invocationError(err)
		}
		if err := captureWorkspaceInvocation(command, args); err != nil {
			return err
		}
		if err := prepareJSONCommand(command); err != nil {
			return err
		}
		showLocalUpdateNotice(command)
		return checkCLIUpdatePolicy(command)
	}
	addConnectFlags(root)

	connect := &cobra.Command{Use: "connect <environment> [new]", Short: "Create and attach to an environment terminal session", Args: commandArgs(terminalArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		if err := validateConnectInvocation(command); err != nil {
			return err
		}
		return actionConnect(actionContext(command, args))
	}}
	addConnectFlags(connect)
	root.AddCommand(connect)
	newTerminal := &cobra.Command{
		Use: "new", Short: "Open a fresh terminal on this device in the current directory",
		Long: "Create and attach a fresh durable terminal on this enrolled device in the current directory. The device runtime must be running and the selected workspace must authorize this device. Use --name to name the new session; use pb session attach to reconnect later.",
		Args: commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateConnectInvocation(command); err != nil {
				return err
			}
			if command.Flags().Changed("session") {
				return invocationError(errors.New("pb new always creates a fresh session; use `pb session attach` to reconnect"))
			}
			machineID, cwd, err := localTerminalTarget()
			if err != nil {
				return err
			}
			return actionConnectTargetInDirectory(actionContext(command, nil), machineID, cwd)
		},
	}
	addConnectFlags(newTerminal)
	root.AddCommand(newTerminal)
	execCommand := &cobra.Command{Use: "exec <machine> [flags] -- <argv...>", Short: "Execute an exact command on a machine", Args: commandArgs(cobra.ArbitraryArgs), RunE: func(command *cobra.Command, args []string) error {
		return actionExecCobra(command, args, false)
	}}
	execCommand.Flags().String("cwd", "", "absolute remote working directory")
	execCommand.Flags().Duration("timeout", 0, "remote execution timeout")
	execCommand.Flags().Bool("pty", false, "allocate a remote PTY")
	execCommand.Flags().StringArray("env", nil, "remote environment name=value")
	execCommand.Flags().Bool("json", false, "emit paperboat.exec-event/v1 JSON Lines")
	root.AddCommand(execCommand)
	sshCommand := &cobra.Command{Use: "ssh [user@]<machine> [-- <OpenSSH arguments...>]", Short: "Connect to a machine with OpenSSH", Args: commandArgs(cobra.ArbitraryArgs), RunE: actionSSH}
	sshCommand.Flags().String("user", "", "remote operating-system user")
	sshTrustHost := &cobra.Command{Use: "trust-host <machine>", Short: "Approve a changed SSH host identity", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionSSHTrustHost}
	sshTrustHost.Flags().String("fingerprint", "", "exact pending SHA256 fingerprint")
	sshTrustHost.Flags().Bool("json", false, "print JSON")
	sshCommand.AddCommand(sshTrustHost)
	sshDoctor := &cobra.Command{Use: "doctor <machine>", Short: "Check SSH integration for a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionSSHDoctor}
	sshDoctor.Flags().Bool("json", false, "print JSON")
	sshCommand.AddCommand(sshDoctor)
	root.AddCommand(sshCommand)
	root.AddCommand(newManagedSSHToolCommand("scp"))
	root.AddCommand(newManagedSSHToolCommand("sftp"))
	root.AddCommand(newManagedSSHToolCommand("rsync"))
	sshProxyCommand := &cobra.Command{Use: "__ssh-proxy", Hidden: true, Args: commandArgs(cobra.NoArgs), RunE: actionSSHProxy}
	sshProxyCommand.Flags().String("host", "", "")
	sshProxyCommand.Flags().String("port", "", "")
	sshProxyCommand.Flags().String("user", "", "")
	root.AddCommand(sshProxyCommand)
	sshKnownHostsCommand := &cobra.Command{Use: "__ssh-known-hosts", Hidden: true, Args: commandArgs(cobra.NoArgs), RunE: actionSSHKnownHosts}
	sshKnownHostsCommand.Flags().String("host", "", "")
	sshKnownHostsCommand.Flags().String("port", "", "")
	root.AddCommand(sshKnownHostsCommand)

	root.AddCommand(environmentsCobraCommand())
	root.AddCommand(workspaceSwitchCommand())
	root.AddCommand(environmentVariablesCobraCommand())
	root.AddCommand(teamCobraCommand())
	root.AddCommand(accountCobraCommand())
	root.AddCommand(versionCommand())
	root.AddCommand(desktopCommand())

	root.AddCommand(doctorCommandV1())
	ping := &cobra.Command{Use: "ping <machine>", Short: "Measure authenticated connectivity to a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionPing}
	ping.Flags().Int("count", 4, "number of authenticated native connections")
	ping.Flags().Duration("timeout", 10*time.Second, "timeout for each connection")
	ping.Flags().Bool("json", false, "print JSON")
	root.AddCommand(ping)
	relay := &cobra.Command{Use: "relay", Short: "Inspect hosted and self-hosted relays"}
	relayList := &cobra.Command{Use: "list", Short: "List hosted and self-hosted relays", Long: `List relay candidates from the signed native network authority and the
signed-in account's selected self-hosted relay pool. Mixed mode shows both
sources. Self-hosted-only mode shows only selected self-hosted relays,
including unavailable ones. Sign-in and the local daemon are required.

Reported_ready means a recent signed control-plane observation, not a live
data connection. Use --json for paperboat.relay-list/v1 output with the pool
mode, relays, and observation timestamps.`, Example: "  pb relay list\n  pb relay list --json", Args: commandArgs(cobra.NoArgs), RunE: actionRelayList}
	relayList.Flags().Bool("json", false, "print JSON")
	relay.AddCommand(relayList)
	root.AddCommand(relay)
	root.AddCommand(edgeCommand())
	root.AddCommand(policyCobraCommand())
	root.AddCommand(apiTokenCobraCommand())

	root.AddCommand(authLoginCommand())
	root.AddCommand(authCobraCommand())
	logout := &cobra.Command{Use: "logout", Short: "Revoke and remove the active client session", Args: commandArgs(cobra.NoArgs), RunE: actionRun(authLogout)}
	logout.Flags().Bool("json", false, "print JSON")
	root.AddCommand(logout)
	root.AddCommand(configCobraCommand())
	root.AddCommand(previewCobraCommandV1())
	root.AddCommand(tunnelCobraCommandV1())
	access := &cobra.Command{Use: "access", Short: "Open authenticated private access", Args: commandArgs(cobra.NoArgs)}
	access.AddCommand(accessTunnelCobraCommandV1())
	access.AddCommand(accessMachineCobraCommandV1())
	root.AddCommand(access)
	root.AddCommand(inboxCobraCommand())
	root.AddCommand(sessionCobraCommand())
	root.AddCommand(userMachineCobraCommand())
	root.AddCommand(pairCommand())
	root.AddCommand(setupCommand())
	root.AddCommand(updateCommand())
	root.AddCommand(uninstallCommand())
	root.AddCommand(freshEnrollmentResetCommand())
	root.AddCommand(manualInstallCommand())
	root.AddCommand(sendCommand())
	root.AddCommand(statusCommand())
	root.AddCommand(waitCommand())
	root.AddCommand(bugreportCommand())
	root.AddCommand(daemoncmd.NewCommand())
	root.AddCommand(daemoncmd.ServiceCommand())
	resolve := daemoncmd.ResolveCommand()
	resolve.Args = commandArgs(resolve.Args)
	resolveRun := resolve.RunE
	resolve.RunE = func(command *cobra.Command, args []string) error {
		return rejectDaemonAccount(resolveRun(command, args))
	}
	root.AddCommand(resolve)
	approve := daemoncmd.ApproveCommand()
	approve.Args = commandArgs(approve.Args)
	approveRun := approve.RunE
	approve.RunE = func(command *cobra.Command, args []string) error {
		if revoke, _ := command.Flags().GetBool("revoke"); revoke {
			return localArgumentError("pb approve only approves a peer. To revoke a machine, run `pb machine revoke <machine>`; it asks for a confirmation code and ends that machine's enrollment.")
		}
		return rejectApprovalRefusal(rejectDaemonAccount(approveRun(command, args)))
	}
	root.AddCommand(approve)
	root.AddCommand(privilegedServiceOperationCommand())
	root.AddCommand(platformInstallCommand())
	root.AddCommand(platformUninstallHelperCommand())
	configureShellCompletion(root)
	root.InitDefaultCompletionCmd()
	enrichCommandHelp(root)
	installGroupCommandHandlers(root)
	return root
}

const deliveredTransferKeyCleanupWarning = "Warning: File delivery completed, but local encryption-key cleanup could not be completed.\n"

func writeDeliveredTransferKeyCleanupWarning(writer io.Writer) {
	if writer != nil {
		_, _ = io.WriteString(writer, deliveredTransferKeyCleanupWarning)
	}
}

func addSendManagementCommands(root *cobra.Command) {
	destination := &cobra.Command{Use: "destination", Short: "Show the default transfer destination", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		sessionID, _ := command.Flags().GetString("session")
		var value api.TransferDestinationDefault
		if sessionID == "" {
			value, err = client.TransferDestinationDefault(ctx.Context)
		} else {
			value, err = client.TerminalSessionTransferDestination(ctx.Context, sessionID)
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": value})
		}
		if !value.Configured || value.Machine == nil {
			fmt.Fprintln(command.OutOrStdout(), "No default transfer destination.")
			return nil
		}
		fmt.Fprintf(command.OutOrStdout(), "%s (%s)\n", value.Machine.Alias, value.Machine.ID)
		return nil
	}}
	destination.Flags().Bool("json", false, "print JSON")
	set := &cobra.Command{Use: "set <machine>", Short: "Set the default transfer destination", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machine, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		sessionID, _ := command.Flags().GetString("session")
		var value api.TransferDestinationDefault
		if sessionID == "" {
			value, err = client.SetTransferDestinationDefault(ctx.Context, machine.ID)
		} else {
			value, err = client.SetTerminalSessionTransferDestination(ctx.Context, sessionID, machine.ID)
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": value})
		}
		fmt.Fprintf(command.OutOrStdout(), "Default transfer destination: %s (%s)\n", machine.Alias, machine.ID)
		return nil
	}}
	set.Flags().Bool("json", false, "print JSON")
	clear := &cobra.Command{Use: "clear", Short: "Clear the default transfer destination", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		sessionID, _ := command.Flags().GetString("session")
		if sessionID == "" {
			err = client.ClearTransferDestinationDefault(ctx.Context)
		} else {
			err = client.ClearTerminalSessionTransferDestination(ctx.Context, sessionID)
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]bool{"configured": false}})
		}
		fmt.Fprintln(command.OutOrStdout(), "Default transfer destination cleared.")
		return nil
	}}
	clear.Flags().Bool("json", false, "print JSON")
	destination.AddCommand(set, clear)
	destination.PersistentFlags().String("session", "", "terminal session ID for a session-specific destination")
	status := &cobra.Command{Use: "status <transfer-id>", Short: "Inspect a file transfer", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, _, lease, err := transferClientForCommand(command, args)
		if err != nil {
			return err
		}
		defer lease.Close()
		manifest, err := client.Status(command.Context(), args[0])
		if err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": manifest})
		}
		fmt.Fprintf(command.OutOrStdout(), "%s  %s  %s -> %s\n", manifest.TransferID, manifest.State, manifest.SourceMachineID, manifest.DestinationMachineID)
		return nil
	}}
	cancelTransfer := &cobra.Command{Use: "cancel <transfer-id>", Short: "Cancel a file transfer batch", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, destination, lease, err := transferClientForCommand(command, args)
		if err != nil {
			return err
		}
		defer lease.Close()
		if err := client.Cancel(command.Context(), args[0]); err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]any{"transfer_id": args[0], "state": "canceled", "destination_machine_id": destination.ID}})
		}
		fmt.Fprintf(command.OutOrStdout(), "%s: canceled\n", args[0])
		return nil
	}}
	list := &cobra.Command{Use: "list", Short: "List file transfers", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, _, lease, err := transferClientForCommand(command, args)
		if err != nil {
			return err
		}
		defer lease.Close()
		sessionID, _ := command.Flags().GetString("session")
		limit, _ := command.Flags().GetInt("limit")
		offset, _ := command.Flags().GetInt("offset")
		query, _ := command.Flags().GetString("q")
		state, _ := command.Flags().GetString("state")
		page, err := client.ListPage(command.Context(), sessionID, limit, offset, query, state)
		if err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": page})
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "TRANSFER\tSTATE\tFILE\tDESTINATION")
		for _, item := range page.Items {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", item.TransferID, item.State, item.Basename, item.DestinationMachineID)
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		if page.Pagination.NextOffset != nil {
			destination, _ := command.Flags().GetString("on")
			operands := []string{"--on", destination, "--offset", strconv.Itoa(*page.Pagination.NextOffset), "--limit", strconv.Itoa(limit)}
			if sessionID != "" {
				operands = append(operands, "--session", sessionID)
			}
			if query != "" {
				operands = append(operands, "--q", query)
			}
			if state != "" {
				operands = append(operands, "--state", state)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Next page: pb send list %s\n", formatPreferenceArgs(operands))
			return err
		}
		return nil
	}}
	list.Flags().Int("limit", 50, "maximum transfers to return")
	list.Flags().Int("offset", 0, "continue at a transfer history offset")
	list.Flags().String("q", "", "filter by file basename or transfer ID")
	list.Flags().String("state", "", "filter by transfer state")
	for _, command := range []*cobra.Command{list, status, cancelTransfer} {
		command.Flags().String("on", "", "destination machine name or ID")
		command.Flags().String("session", "", "terminal session ID for destination context")
		command.Flags().Bool("json", false, "print JSON")
	}
	root.AddCommand(destination, list, status, cancelTransfer)
}

func transferClientForCommand(cobraCommand *cobra.Command, args []string) (transferHistory, api.UserMachine, io.Closer, error) {
	destinationRef, _ := cobraCommand.Flags().GetString("on")
	if strings.TrimSpace(destinationRef) == "" {
		return nil, api.UserMachine{}, nil, invocationError(errors.New("--on is required"))
	}
	ctx := actionContext(cobraCommand, args)
	backend, err := backendClient(ctx)
	if err != nil {
		return nil, api.UserMachine{}, nil, err
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return nil, api.UserMachine{}, nil, err
	}
	destination, err := resolveUserMachine(ctx.Context, backend, destinationRef)
	if err != nil {
		return nil, api.UserMachine{}, nil, friendlyCommandError(err)
	}
	if destination.ID == sourceMachineID {
		// Files sent to this computer are copied into its own Inbox: the Inbox is the history.
		root, err := configuredInboxPath()
		if err != nil {
			return nil, api.UserMachine{}, nil, err
		}
		return thisComputerTransfers{root: root, machineID: sourceMachineID, userID: destination.OwnerAccount}, destination, noopCloser{}, nil
	}
	sessionID, _ := cobraCommand.Flags().GetString("session")
	descriptor, err := backend.MachineFileTransferDescriptor(ctx.Context, destination.ID, sourceMachineID, sessionID)
	if err != nil {
		return nil, api.UserMachine{}, nil, friendlyCommandError(err)
	}
	target := &resolver.FileTransferTarget{Endpoint: descriptor.Endpoint, SourceMachineID: descriptor.SourceMachineID, DestinationMachineID: descriptor.DestinationMachineID, InitiatingUserID: descriptor.InitiatingUserID, Auth: resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.UTC().Format(time.RFC3339Nano), ResourceID: descriptor.Auth.AccessSessionID}, Policy: descriptor.Policy}
	client, lease, err := newTransferCommandClient(ctx.Context, target, destination, newIdempotencyKey())
	if err != nil {
		return nil, api.UserMachine{}, nil, err
	}
	return client, destination, lease, nil
}

var newTransferCommandClient = func(ctx context.Context, target *resolver.FileTransferTarget, destination api.UserMachine, operationID string) (*filetransfer.NativeClient, io.Closer, error) {
	if target == nil || target.Auth.Method != "bearer" || target.Auth.ResourceID == "" || destination.EnvironmentID == "" || destination.InstallationGeneration <= 0 || destination.ID != target.DestinationMachineID || operationID == "" {
		return nil, nil, errors.New("server returned an invalid file transfer descriptor")
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, nil, err
	}
	local, err := localapi.NewClient(paths.SocketPath, time.Duration(config.PeerConnectTimeoutMilliseconds)*time.Millisecond)
	if err != nil {
		return nil, nil, err
	}
	deadline := parseAuthExpiry(target.Auth.ExpiresAt)
	lease, err := local.PrepareFileTransfer(ctx, localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: destination.ID, EnvironmentID: destination.EnvironmentID, MachineGeneration: uint64(destination.InstallationGeneration), OperationID: operationID, Credential: target.Auth.Token, AccessSessionID: target.Auth.ResourceID, Deadline: deadline, MaximumBytes: uint64(target.Policy.MaxFileBytes)})
	if err != nil {
		return nil, nil, err
	}
	client, err := filetransfer.NewNativeClient(target.Endpoint, filetransfer.Auth{Token: target.Auth.Token, ExpiresAt: deadline}, filetransfer.Binding{SourceMachineID: target.SourceMachineID, DestinationMachineID: target.DestinationMachineID, InitiatingUserID: target.InitiatingUserID}, lease.OpenTransferStream)
	if err != nil {
		_ = lease.Close()
		return nil, nil, err
	}
	if target.Policy.DeliveryTimeoutSeconds > 0 {
		client.DeliveryTimeout = time.Duration(target.Policy.DeliveryTimeoutSeconds) * time.Second
	}
	return client, lease, nil
}

func localFileTransferSenderFromEnvironment() (*filetransfer.LocalSender, error) {
	endpoint := strings.TrimSpace(os.Getenv("PAPERBOAT_FILE_TRANSFER_STAGING_ENDPOINT"))
	tokenPath := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_AGENT_TOKEN_FILE"))
	if endpoint == "" && tokenPath == "" {
		return nil, nil
	}
	if endpoint == "" || !filepath.IsAbs(tokenPath) {
		return nil, errors.New("Paperboat host-local file transfer environment is invalid")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "/v1/local-file-transfers" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Paperboat host-local file transfer endpoint is invalid")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "" {
		return nil, errors.New("Paperboat host-local file transfer endpoint is not loopback")
	}
	token, err := readOwnerOnlyFile(tokenPath, 1024)
	if err != nil {
		return nil, fmt.Errorf("read Paperboat host-local file transfer token: %w", err)
	}
	value := strings.TrimSpace(string(token))
	clear(token)
	if len(value) < 32 || strings.ContainsAny(value, " \t\r\n") {
		return nil, errors.New("Paperboat host-local file transfer token is invalid")
	}
	return &filetransfer.LocalSender{Endpoint: endpoint, Token: value}, nil
}

func nativeFileTransferClient(ctx context.Context, local *localapi.Client, target resolver.ConnectInfo, operationID string) (*filetransfer.NativeClient, *localapi.FileTransferLease, error) {
	if ctx == nil || local == nil || target.TargetKind != "machine" || target.MachineID == "" || target.MachineGeneration == 0 || target.Terminal == nil || target.Terminal.EnvironmentID == "" || target.FileTransfer == nil || target.FileTransfer.Auth.ResourceID == "" || operationID == "" {
		return nil, nil, errors.New("native file transfer target is invalid")
	}
	if target.FileTransfer.SourceMachineID == target.FileTransfer.DestinationMachineID {
		// Connecting a computer to itself: the files are already there, so there is nothing to transfer.
		return nil, nil, nil
	}
	deadline := parseAuthExpiry(target.FileTransfer.Auth.ExpiresAt)
	lease, err := local.PrepareFileTransfer(ctx, localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: target.MachineID, EnvironmentID: target.Terminal.EnvironmentID, MachineGeneration: target.MachineGeneration, OperationID: operationID, Credential: target.FileTransfer.Auth.Token, AccessSessionID: target.FileTransfer.Auth.ResourceID, Deadline: deadline, MaximumBytes: uint64(target.FileTransfer.Policy.MaxFileBytes)})
	if err != nil {
		return nil, nil, err
	}
	client, err := filetransfer.NewNativeClient(target.FileTransfer.Endpoint, filetransfer.Auth{Token: target.FileTransfer.Auth.Token, ExpiresAt: deadline}, filetransfer.Binding{SourceMachineID: target.FileTransfer.SourceMachineID, DestinationMachineID: target.FileTransfer.DestinationMachineID, InitiatingUserID: target.FileTransfer.InitiatingUserID}, lease.OpenTransferStream)
	if err != nil {
		_ = lease.Close()
		return nil, nil, err
	}
	if target.FileTransfer.Policy.DeliveryTimeoutSeconds > 0 {
		client.DeliveryTimeout = time.Duration(target.FileTransfer.Policy.DeliveryTimeoutSeconds) * time.Second
	}
	return client, lease, nil
}

func fileTransferLimits(target *resolver.FileTransferTarget) filetransfer.Limits {
	if target == nil {
		return filetransfer.Limits{MaxFileBytes: 50 << 20, MaxBatchFiles: 10, MaxBatchBytes: 500 << 20}
	}
	return filetransfer.Limits{MaxFileBytes: target.Policy.MaxFileBytes, MaxBatchFiles: target.Policy.MaxBatchFiles, MaxBatchBytes: target.Policy.MaxBatchBytes}
}

func descriptorFileTransferPolicy(target *resolver.FileTransferTarget) filetransfer.Policy {
	if target == nil {
		return filetransfer.Policy{}
	}
	policy := target.Policy
	return filetransfer.Policy{Revision: policy.Revision, MaxFileBytes: policy.MaxFileBytes, MaxBatchFiles: policy.MaxBatchFiles, MaxBatchBytes: policy.MaxBatchBytes, MaxConcurrentTransfers: policy.MaxConcurrentTransfers, RetentionSeconds: policy.RetentionSeconds, DeliveryTimeoutSeconds: policy.DeliveryTimeoutSeconds, MaxPendingSpoolBytes: policy.MaxPendingSpoolBytes}
}

// subcommandOwnsVersionFlag reports whether the invoked subcommand defines its own --version
// (such as pb policy get --version N), which then is not the root's version request.
func subcommandOwnsVersionFlag(root *cobra.Command, args []string) bool {
	target, _, err := root.Find(args)
	return err == nil && target != root && target.LocalNonPersistentFlags().Lookup("version") != nil
}
