// Command pb is the invisible terminal wrapper for the
// Paperboat platform. `pb <environment>` attaches an enrolled machine
// through Paperboat auth and bridges local file pastes into
// remote TUIs. Cross-service calls run behind interfaces so protocol behavior
// remains independently testable.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/fileindex"
	filetransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	service "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/machinename"
	"github.com/pinksaucepasta/paperboat/internal/paste"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/remotepath"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/session"
	"github.com/pinksaucepasta/paperboat/internal/statusbar"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
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
		if cliArgumentPresent(args, "--version", "-v") {
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
		return rejectDaemonAccount(approveRun(command, args))
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

func actionHome(command *cobra.Command) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return commandRejection{reason: commandRejectNoninteractiveHome}
	}
	previousContext := command.Context()
	homeContext, cancelHome := context.WithCancel(previousContext)
	command.SetContext(homeContext)
	defer func() { cancelHome(); command.SetContext(previousContext) }()
	endScreen := selector.BeginScreen(command.ErrOrStderr())
	defer endScreen()
	if prefetch, prefetchErr := startHomePrefetch(command); prefetchErr == nil {
		command.SetContext(context.WithValue(command.Context(), homePrefetchContextKey{}, prefetch))
	}
	primeHomeFileIndex(command.Context())
	revealEmail := false
	for {
		emailAction := "ctrl+e show email"
		if revealEmail {
			emailAction = "ctrl+e hide email"
		}
		selection, err := selector.ChooseWithAction(selector.Options{
			Header:  homeBrand(command, revealEmail),
			Title:   "What do you want to do?",
			Stdin:   os.Stdin,
			Context: command.Context(), Output: command.ErrOrStderr(),
			Footer:        "↑/↓ move  enter/click select  " + emailAction + "  esc exit",
			Actions:       map[string]string{"ctrl+e": "toggle-email"},
			HeaderActions: map[int]string{2: "toggle-email"},
			Items:         personalizedHomeItems(preferences.FromContext(command.Context())),
		})
		if err != nil {
			if errors.Is(err, selector.ErrCanceled) || errors.Is(err, selector.ErrInterrupted) {
				return nil
			}
			return err
		}
		if selection.Action == "toggle-email" {
			revealEmail = !revealEmail
			continue
		}
		err = runHomeAction(command, selection.Item.ID)
		if errors.Is(err, selector.ErrInterrupted) {
			return nil
		}
		if interactiveCanceled(err) || err == nil {
			continue
		}
		if displayErr := showHomeFailure(command, err); displayErr != nil {
			return displayErr
		}
	}
}

const homePrefetchFreshness = 5 * time.Second

type homePrefetchContextKey struct{}

type asyncHomeValue[T any] struct {
	done      chan struct{}
	value     T
	err       error
	fetchedAt time.Time
}

func startAsyncHomeValue[T any](ctx context.Context, load func(context.Context) (T, error)) *asyncHomeValue[T] {
	value := &asyncHomeValue[T]{done: make(chan struct{})}
	go func() {
		defer close(value.done)
		value.value, value.err = load(ctx)
		value.fetchedAt = time.Now()
	}()
	return value
}

func (v *asyncHomeValue[T]) ready() bool {
	select {
	case <-v.done:
		return true
	default:
		return false
	}
}

func (v *asyncHomeValue[T]) await(ctx context.Context) (T, error) {
	select {
	case <-v.done:
		return v.value, v.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (v *asyncHomeValue[T]) fresh() bool {
	return v.ready() && v.err == nil && time.Since(v.fetchedAt) <= homePrefetchFreshness
}

type homePrefetch struct {
	favorites *asyncHomeValue[favoriteSet]
	machines  *asyncHomeValue[[]api.UserMachine]
	sessions  *asyncHomeValue[[]machineSession]

	machinesClaimed atomic.Bool
	sessionsClaimed atomic.Bool
}

func startHomePrefetch(command *cobra.Command) (*homePrefetch, error) {
	client, err := backendForCommand(command)
	if err != nil {
		return nil, err
	}
	ctx := command.Context()
	prefetch := &homePrefetch{
		favorites: startAsyncHomeValue(ctx, func(ctx context.Context) (favoriteSet, error) { return loadFavorites(ctx, client) }),
		machines:  startAsyncHomeValue(ctx, client.ListUserMachines),
	}
	prefetch.sessions = startAsyncHomeValue(ctx, func(ctx context.Context) ([]machineSession, error) {
		machines, loadErr := prefetch.machines.await(ctx)
		if loadErr != nil {
			return nil, loadErr
		}
		return listMachineSessionsForMachines(ctx, client, machines)
	})
	return prefetch, nil
}

func homePrefetchFor(command *cobra.Command) *homePrefetch {
	value, _ := command.Context().Value(homePrefetchContextKey{}).(*homePrefetch)
	return value
}

func primeHomeFileIndex(ctx context.Context) {
	go func() {
		root, rootErr := os.UserHomeDir()
		cachePath, cacheErr := fileindex.CachePath()
		if err := errors.Join(rootErr, cacheErr); err != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-cli", "send", "command", "command_failed", err)
			return
		}
		fileindex.RefreshInBackground(ctx, root, cachePath)
	}()
}

func runHomeAction(command *cobra.Command, action string) error {
	if strings.HasPrefix(action, "shortcut:") {
		return runPreferenceFavorite(command, strings.TrimPrefix(action, "shortcut:"))
	}
	switch action {
	case "customize":
		return editPreferences(command)
	case "previews":
		return actionHomePreviews(command)
	case "commands":
		return actionHomeCommands(command, nil)
	case "team":
		return actionHomeTeams(command)
	case "send":
		return actionHomeSentFiles(command)
	case "sessions":
		return actionHomeSessions(command)
	case "environment-variables":
		return runEnvironmentVariablesTUI(command)
	case "machines":
		return actionHomeMachines(command)
	case "inbox":
		return actionHomeInbox(command)
	case "config":
		return actionHomeConfig(command)
	case "doctor":
		return actionHomeDoctor(command)
	case "account":
		return actionHomeAccount(command)
	case "switch-workspace":
		selected, err := selectWorkspace(command, nil)
		if err != nil {
			return err
		}
		if !selected {
			return selector.ErrCanceled
		}
		return selector.ErrInterrupted
	default:
		return errors.New("unknown Paperboat action")
	}
}

func actionHomeInbox(command *cobra.Command) error {
	ctx := actionContext(command, nil)
	client, err := backendClient(ctx)
	if err != nil {
		return err
	}
	me, err := client.Me(ctx.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	for {
		requests, listErr := client.PendingTeamInboxRequests(ctx.Context)
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		pending := slices.DeleteFunc(requests, func(request api.TeamInboxRequest) bool {
			return request.RecipientAccount != me.ID || request.Status != "pending"
		})
		if len(pending) == 0 {
			return showInformation(command, "Team Inbox", "No file requests are waiting for your approval.", nil)
		}
		items := make([]selector.Item, len(pending))
		for i, request := range pending {
			items[i] = selector.Item{ID: request.RequestID, Title: fmt.Sprintf("%d file(s) from %s", len(request.Files), request.SenderAccount), Description: "Expires " + relativeTimestamp(request.ExpiresAt)}
		}
		selection, selectErr := selector.ChooseWithAction(selector.Options{Title: "Team Inbox approvals", Subtitle: "Approval applies only to the listed names, sizes, hashes, recipient and transfer", Items: items, Footer: "enter review  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if selectErr != nil {
			return selectErr
		}
		request := pending[slices.IndexFunc(pending, func(request api.TeamInboxRequest) bool { return request.RequestID == selection.Item.ID })]
		lines := make([]string, len(request.Files))
		for i, file := range request.Files {
			lines[i] = fmt.Sprintf("%s — %d bytes — SHA-256 %s", file.Basename, file.Size, file.SHA256)
		}
		action, actionErr := chooseHomeAction(command, "File request from "+request.SenderAccount, []selector.Item{{ID: "approve", Title: "Approve exact files", Description: strings.Join(lines, "; ")}, {ID: "decline", Title: "Decline", Description: "No file bytes will be accepted"}})
		if actionErr != nil {
			if errors.Is(actionErr, selector.ErrCanceled) {
				continue
			}
			return actionErr
		}
		if _, err = client.DecideTeamInboxRequest(ctx.Context, request.RequestID, action.ID, request.DecisionGeneration); err != nil {
			return friendlyCommandError(err)
		}
	}
}

func versionDisplay(version string) string {
	return brandDisplay(version, "") + "\n"
}

func homeBrand(command *cobra.Command, revealEmail bool) string {
	account := "Not signed in"
	configPath, _ := command.Flags().GetString("config")
	cfg, err := config.Load(configPath)
	if err == nil {
		if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
			cfg.ServerURL, err = config.NormalizeServerURL(server)
		}
	}
	if err == nil && cfg.ServerURL != "" {
		if store, storeErr := config.ProfileStoreFor(cfg); storeErr == nil {
			if profile, profileErr := store.Load(cfg.ServerURL); profileErr == nil {
				account = firstNonEmpty(profile.Account.Email, profile.Account.DisplayName, profile.Account.ID, account)
			}
		}
	}
	if !revealEmail {
		account = maskEmail(account)
	}
	if workspace, workspaceErr := effectiveWorkspace(command); workspaceErr == nil && workspace != "" {
		account += " · " + workspaceDisplayName(workspace)
	}
	return brandDisplay(buildinfo.Version, account)
}

func workspaceDisplayName(selector string) string {
	if selector == "personal" {
		return "Personal workspace"
	}
	return selector + " workspace"
}

func maskEmail(value string) string {
	local, domain, ok := strings.Cut(value, "@")
	if !ok {
		return value
	}
	maskPart := func(part string) string {
		return strings.Repeat("█", len([]rune(part)))
	}
	domainParts := strings.Split(domain, ".")
	for index, part := range domainParts {
		domainParts[index] = maskPart(part)
	}
	return maskPart(local) + "@" + strings.Join(domainParts, ".")
}

func brandDisplay(version, account string) string {
	art := []string{"      ▄█▄", "  ▄▄▝▀▀▀▀▀▘▄▄", "   ▀███████▀"}
	details := []string{"Paperboat", "Version " + version, account}
	artWidth := 0
	for _, line := range art {
		artWidth = max(artWidth, ansi.StringWidth(line))
	}
	lines := make([]string, len(art))
	for index, line := range art {
		lines[index] = line
		if details[index] != "" {
			lines[index] += strings.Repeat(" ", artWidth-ansi.StringWidth(line)+3) + details[index]
		}
	}
	return strings.Join(lines, "\n")
}

func actionEnvironmentsList(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	machines, err := client.ListUserMachinesFiltered(command.Context(), inventoryFilters(command))
	if err != nil {
		return friendlyCommandError(err)
	}
	currentMachineID, _ := configuredMachineID()
	sortMachinesForDisplay(machines, nil, currentMachineID)
	items := make([]selector.Item, 0, len(machines))
	for _, machine := range machines {
		items = append(items, selector.Item{ID: machine.ID, Title: machineDisplayTitle(machine, currentMachineID), Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.ID + " " + machine.WorkspaceRoot + " " + machineStatusSearch(machine)})
	}
	_, err = selector.Choose(selector.Options{Title: "Machines", Subtitle: "Computers available to this account", Items: items, Empty: "No machines yet", Footer: "↑/↓ inspect  type to filter  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return err
}

func actionHomeOwnedSessions(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	usePrefetch := true
	for {
		var favorites favoriteSet
		var sessions []machineSession
		var listErr error
		loaded := false
		if usePrefetch {
			usePrefetch = false
			if prefetch := homePrefetchFor(command); prefetch != nil && prefetch.sessionsClaimed.CompareAndSwap(false, true) {
				work := func(ctx context.Context) error {
					var loadErr error
					favorites, loadErr = prefetch.favorites.await(ctx)
					if loadErr == nil {
						sessions, loadErr = prefetch.sessions.await(ctx)
					}
					return loadErr
				}
				listErr = runPrefetchedHomeLoad(command, "Terminal sessions", "Loading sessions", prefetch.favorites.ready() && prefetch.sessions.ready(), work)
				loaded = listErr == nil && prefetch.favorites.fresh() && prefetch.sessions.fresh()
			}
		}
		if !loaded {
			listErr = homeLoading(command, "Terminal sessions", "Loading sessions", func(ctx context.Context) error {
				var err error
				favorites, err = loadFavorites(ctx, client)
				if err != nil {
					return err
				}
				sessions, err = listMachineSessions(ctx, client)
				return err
			})
		}
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		selected, selectErr := selectMachineSession(command, sessions, favorites)
		if errors.Is(selectErr, errFavoriteToggle) {
			if favoriteErr := setFavorite(command.Context(), client, "session", machineSessionFavoriteID(selected), !favorites.IsFavorite("session", machineSessionFavoriteID(selected))); favoriteErr != nil {
				return favoriteErr
			}
			continue
		}
		if selectErr != nil {
			return selectErr
		}
		if err := actionHomeSessionActions(command, selected.target, selected.session); err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

type machineSession struct {
	target  environmentTarget
	session api.TerminalSession
}

func listMachineSessions(ctx context.Context, client *api.Client) ([]machineSession, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return nil, err
	}
	return listMachineSessionsForMachines(ctx, client, machines)
}

func listMachineSessionsForMachines(ctx context.Context, client *api.Client, machines []api.UserMachine) ([]machineSession, error) {
	machines = slices.Clone(machines)
	machines = slices.DeleteFunc(machines, func(machine api.UserMachine) bool {
		return !machine.Capabilities.TerminalHost.Configured
	})
	results := make(chan []machineSession, len(machines))
	errorsOut := make(chan error, len(machines))
	workers := make(chan struct{}, 4)
	var group sync.WaitGroup
	for _, machine := range machines {
		machine := machine
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				errorsOut <- ctx.Err()
				return
			}
			items, loadErr := client.ListUserMachineTerminalSessions(ctx, machine.ID)
			if loadErr != nil {
				errorsOut <- loadErr
				return
			}
			target := environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}
			mapped := make([]machineSession, 0, len(items))
			for _, session := range items {
				mapped = append(mapped, machineSession{target: target, session: session})
			}
			results <- mapped
		}()
	}
	group.Wait()
	close(results)
	close(errorsOut)
	var loadErrors []error
	for loadErr := range errorsOut {
		loadErrors = append(loadErrors, loadErr)
	}
	if loadErr := errors.Join(loadErrors...); loadErr != nil {
		return nil, loadErr
	}
	var sessions []machineSession
	for result := range results {
		sessions = append(sessions, result...)
	}
	slices.SortStableFunc(sessions, func(a, b machineSession) int {
		return b.activity().Compare(a.activity())
	})
	return sessions, nil
}

func (s machineSession) activity() time.Time {
	if s.session.LastActiveAt != nil {
		return *s.session.LastActiveAt
	}
	if !s.session.UpdatedAt.IsZero() {
		return s.session.UpdatedAt
	}
	return s.session.CreatedAt
}

func selectMachineSession(command *cobra.Command, sessions []machineSession, favorites favoriteSet) (machineSession, error) {
	slices.SortStableFunc(sessions, func(a, b machineSession) int {
		return compareFavorites(favorites.IsFavorite("session", machineSessionFavoriteID(a)), favorites.IsFavorite("session", machineSessionFavoriteID(b)))
	})
	items := make([]selector.Item, 0, len(sessions))
	byID := make(map[string]machineSession, len(sessions))
	for _, entry := range sessions {
		item := machineSessionIdentificationItem(command.Context(), entry, favorites)
		items = append(items, item)
		byID[item.ID] = entry
	}
	client, clientErr := backendForCommand(command)
	if clientErr != nil {
		return machineSession{}, clientErr
	}
	refresh := func(ctx context.Context) ([]selector.Item, error) {
		fresh, err := refreshSessionIdentification(ctx, client, sessions)
		if err != nil {
			return nil, err
		}
		updated := make([]selector.Item, 0, len(fresh))
		for _, entry := range fresh {
			key := entry.target.id + ":" + entry.session.ID
			if _, ok := byID[key]; !ok {
				continue
			}
			updated = append(updated, machineSessionIdentificationItem(command.Context(), entry, favorites))
		}
		return updated, nil
	}
	selected, err := selector.ChooseWithAction(selector.Options{Context: command.Context(), Refresh: refresh, Title: "Terminal sessions", Subtitle: "All machine sessions, newest first", Items: items, Empty: "no terminal sessions are available", Footer: "↑/↓ move  enter open  ctrl+f favorite  esc back", Actions: map[string]string{"ctrl+f": "favorite"}, Stdin: os.Stdin, Output: os.Stderr})
	entry := byID[selected.Item.ID]
	if err == nil && selected.Action == "favorite" {
		err = errFavoriteToggle
	}
	return entry, err
}

func machineSessionFavoriteID(entry machineSession) string {
	return entry.target.id + ":" + entry.session.ID
}

func actionHomeMachines(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	usePrefetch := true
	for {
		var favorites favoriteSet
		var machines []api.UserMachine
		var err error
		loaded := false
		if usePrefetch {
			usePrefetch = false
			if prefetch := homePrefetchFor(command); prefetch != nil && prefetch.machinesClaimed.CompareAndSwap(false, true) {
				work := func(ctx context.Context) error {
					var loadErr error
					favorites, loadErr = prefetch.favorites.await(ctx)
					if loadErr == nil {
						machines, loadErr = prefetch.machines.await(ctx)
					}
					return loadErr
				}
				err = runPrefetchedHomeLoad(command, "Machines", "Loading machines", prefetch.favorites.ready() && prefetch.machines.ready(), work)
				loaded = err == nil && prefetch.favorites.fresh() && prefetch.machines.fresh()
			}
		}
		if !loaded {
			err = homeLoading(command, "Machines", "Loading machines", func(ctx context.Context) error {
				var loadErr error
				favorites, loadErr = loadFavorites(ctx, client)
				if loadErr != nil {
					return loadErr
				}
				machines, loadErr = client.ListUserMachines(ctx)
				return loadErr
			})
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		currentMachineID, _ := configuredMachineID()
		sortMachinesForDisplay(machines, favorites, currentMachineID)
		items := make([]selector.Item, 0, len(machines)+1)
		byID := make(map[string]api.UserMachine, len(machines))
		for _, machine := range machines {
			if machine.ID == currentMachineID {
				continue
			}
			favorite := favorites.IsFavorite("machine", machine.ID)
			items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.WorkspaceRoot + " " + machineStatusSearch(machine) + " favorite starred", Favorite: favorite})
			byID[machine.ID] = machine
		}
		for _, machine := range machines {
			if machine.ID != currentMachineID {
				continue
			}
			favorite := favorites.IsFavorite("machine", machine.ID)
			items = append(items, selector.Item{ID: machine.ID, Title: machineDisplayTitle(machine, currentMachineID), Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.WorkspaceRoot + " " + machineStatusSearch(machine) + " this machine favorite starred", Favorite: favorite})
			byID[machine.ID] = machine
		}
		items = append(items, selector.Item{ID: "add", Title: "+ Add machine", Description: "Set up another computer", Search: "new pair enroll", Action: true})
		selection, selectErr := selector.ChooseWithAction(selector.Options{Title: "Machines", Subtitle: "Paired computers", Items: items, Footer: "↑/↓ move  enter open  ctrl+f favorite  esc back", Actions: map[string]string{"ctrl+f": "favorite"}, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if selectErr != nil {
			return selectErr
		}
		choice := selection.Item
		if choice.ID == "add" && selection.Action == "favorite" {
			continue
		}
		if choice.ID == "add" {
			if addErr := executeInteractiveCommand(command, []string{"machine", "add"}); addErr != nil {
				if interactiveCanceled(addErr) {
					continue
				}
				return addErr
			}
			continue
		}
		machine := byID[choice.ID]
		if selection.Action == "favorite" {
			if favoriteErr := setFavorite(command.Context(), client, "machine", machine.ID, !favorites.IsFavorite("machine", machine.ID)); favoriteErr != nil {
				return favoriteErr
			}
			continue
		}
		for {
			action, actionErr := chooseMachineHomeAction(command, machine)
			if errors.Is(actionErr, selector.ErrCanceled) {
				break
			}
			if actionErr != nil {
				return actionErr
			}
			var runErr error
			switch action.ID {
			case "terminal":
				runErr = executeInteractiveCommand(command, []string{"connect", machine.ID, "new"})
			case "sessions":
				runErr = actionHomeMachineSessions(command, client, machine)
				if interactiveCanceled(runErr) {
					runErr = nil
				}
			case "environment-variables":
				runErr = runEnvironmentVariableScopeTUI(command, client, environmentVariableTarget{machineID: machine.ID, machineName: machine.Alias})
			case "send":
				runErr = actionHomeSendToMachine(command, machine)
			case "rename":
				name, readErr := prompt.Text(prompt.TextOptions{Title: "Rename machine", Description: machine.Alias, Initial: machine.Alias, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: machinename.Validate})
				if readErr != nil {
					runErr = readErr
					break
				}
				runErr = runHomeResult(command, []string{"machine", "rename", machine.ID, name})
				if runErr == nil {
					machine.Alias = name
				}
			case "allow-sleep":
				runErr = runHomeResult(command, []string{"machine", "availability", machine.ID, "--mode", "allow-sleep"})
			case "keep-awake":
				runErr = runHomeResult(command, []string{"machine", "availability", machine.ID, "--mode", "keep-awake"})
			default:
				runErr = errors.New("unknown machine action")
			}
			if interactiveCanceled(runErr) {
				continue
			}
			if runErr != nil {
				if errors.Is(runErr, selector.ErrInterrupted) {
					return runErr
				}
				if err := showHomeFailure(command, runErr); err != nil {
					return err
				}
			}
		}
	}
}

func machineHomeActions(machine api.UserMachine) []selector.Item {
	actions := make([]selector.Item, 0, 8)
	actions = append(actions, selector.Item{ID: "rename", Title: "Rename", Description: "Change this machine's alias"})
	if machineSupportsEnvironmentInjection(machine) {
		actions = append(actions, selector.Item{ID: "environment-variables", Title: "Environment variables", Description: "Manage variables applied to new processes on this machine"})
	}
	if machine.Capabilities.TerminalHost.Configured {
		actions = append(actions,
			selector.Item{ID: "terminal", Title: "Create terminal session", Description: "Start and attach to a new durable session"},
		)
	}
	if machine.Capabilities.FileReceive.Configured {
		if sourceMachineID, err := configuredMachineID(); err != nil || sourceMachineID != machine.ID {
			actions = append(actions, selector.Item{ID: "send", Title: "Send files", Description: "Search, select, drop, or paste files for this machine"})
		}
	}
	if machine.Capabilities.TerminalHost.Configured {
		actions = append(actions, selector.Item{ID: "sessions", Title: "Sessions", Description: "List durable terminal sessions on this machine"})
	}
	actions = append(actions,
		selector.Item{ID: "allow-sleep", Title: "Allow sleep", Description: "Let normal operating-system sleep policy apply"},
		selector.Item{ID: "keep-awake", Title: "Keep awake", Description: "Request availability even when idle"},
	)
	return actions
}

func chooseMachineHomeAction(command *cobra.Command, machine api.UserMachine) (selector.Item, error) {
	return selector.Choose(selector.Options{Title: machine.Alias, Subtitle: machineStatusSummary(machine), Items: machineHomeActions(machine), Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
}

func machineStatusSummary(machine api.UserMachine) string {
	return machineAvailabilityLabel(machine) + "  ·  " + machineCapabilityLabel(machine)
}

func machineStatusSearch(machine api.UserMachine) string {
	return strings.Join([]string{machineAvailabilityLabel(machine), machineCapabilityLabel(machine)}, " ")
}

func machineAvailabilityLabel(machine api.UserMachine) string {
	if machine.Online {
		return "Online"
	}
	switch strings.ToLower(strings.TrimSpace(machine.State)) {
	case "revoked", "deleted":
		return "Unavailable"
	default:
		return "Offline"
	}
}

func machineCapabilityLabel(machine api.UserMachine) string {
	count := 0
	for _, enabled := range []bool{machine.MachineCapabilities.Desired.Terminal, machine.MachineCapabilities.Desired.ManagedSSH, machine.MachineCapabilities.Desired.FileReceive, machine.MachineCapabilities.Desired.PreviewTunnel} {
		if enabled {
			count++
		}
	}
	return fmt.Sprintf("%d incoming services", count)
}

func machineDisplayTitle(machine api.UserMachine, currentMachineID string) string {
	if machine.ID == currentMachineID {
		return machine.Alias + " (this machine)"
	}
	return machine.Alias
}

func sortMachinesForDisplay(machines []api.UserMachine, favorites favoriteSet, currentMachineID string) {
	slices.SortStableFunc(machines, func(a, b api.UserMachine) int {
		aCurrent, bCurrent := a.ID == currentMachineID, b.ID == currentMachineID
		if aCurrent != bCurrent {
			if aCurrent {
				return 1
			}
			return -1
		}
		return compareFavorites(favorites.IsFavorite("machine", a.ID), favorites.IsFavorite("machine", b.ID))
	})
}

func actionHomeSendToMachine(command *cobra.Command, machine api.UserMachine) error {
	var candidates []string
	searchRoot, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cachePath, err := fileindex.CachePath()
	if err != nil {
		return err
	}
	loadIndex := func(ctx context.Context) error {
		var refreshErr error
		candidates, refreshErr = fileindex.Current(ctx, searchRoot, cachePath)
		return refreshErr
	}
	// A previously written index is sufficient to populate the picker
	// immediately. Refresh it in the background so opening Send Files never
	// blocks on a full profile walk (notably Windows' large AppData tree).
	if cached, cacheOK := fileindex.Load(searchRoot, cachePath); cacheOK {
		candidates = cached
		fileindex.RefreshInBackground(command.Context(), searchRoot, cachePath)
	} else if fileindex.RefreshReady(cachePath) {
		err = loadIndex(command.Context())
	} else {
		err = homeLoading(command, "Send files", "Refreshing files", loadIndex)
	}
	if err != nil {
		return err
	}
	defer fileindex.RefreshInBackground(command.Context(), searchRoot, cachePath)
	items := make([]selector.Item, 0, len(candidates)+1)
	for _, path := range candidates {
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		display := path
		if relative, relativeErr := filepath.Rel(searchRoot, path); relativeErr == nil {
			display = filepath.Join("~", relative)
		}
		items = append(items, selector.Item{ID: path, Title: display, Description: "File"})
	}
	selection, err := selector.ChooseWithAction(selector.Options{Title: "Send files to " + machine.Alias, Subtitle: "Type to fuzzy-search files in your home folder", Items: items, Empty: "Start typing to search", RequireFilter: true, Footer: "type to search  enter/click send  ctrl+p paste or drop paths  esc back", Actions: map[string]string{"ctrl+p": "paths"}, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	if err != nil {
		return err
	}
	if selection.Action != "paths" {
		return executeInteractiveCommand(command, []string{"send", selection.Item.ID, "--to", machine.ID})
	}
	value, err := prompt.Text(prompt.TextOptions{Title: "Send files to " + machine.Alias, Description: "Drag files here or paste one or more file or folder paths", Placeholder: "/path/to/file", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
		_, parseErr := droppedFilePaths(value)
		return parseErr
	}})
	if errors.Is(err, prompt.ErrCanceled) {
		return selector.ErrCanceled
	}
	if err != nil {
		return err
	}
	paths, err := droppedFilePaths(value)
	if err != nil {
		return err
	}
	arguments := append([]string{"send"}, paths...)
	arguments = append(arguments, "--to", machine.ID)
	return executeInteractiveCommand(command, arguments)
}

func droppedFilePaths(value string) ([]string, error) {
	paths, err := splitDroppedPaths(strings.TrimSpace(value))
	if err != nil || len(paths) == 0 {
		return nil, errors.New("drop or paste at least one valid file or folder path")
	}
	for index, path := range paths {
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) {
			absolute, absoluteErr := filepath.Abs(path)
			if absoluteErr != nil {
				return nil, absoluteErr
			}
			path = absolute
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, commandPreparationFailure{step: prepareSelectedFile, cause: statErr}
		}
		paths[index] = path
	}
	return paths, nil
}

func homeLoading(command *cobra.Command, title, detail string, work func(context.Context) error) error {
	return selector.Loading(command.Context(), title, detail, os.Stdin, command.ErrOrStderr(), work)
}

func runPrefetchedHomeLoad(command *cobra.Command, title, detail string, ready bool, work func(context.Context) error) error {
	if ready {
		return work(command.Context())
	}
	return homeLoading(command, title, detail, work)
}

func actionHomeMachineSessions(command *cobra.Command, client *api.Client, machine api.UserMachine) error {
	target := environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}
	for {
		records, err := listTerminalSessionsForTarget(command.Context(), client, target)
		if err != nil {
			return friendlyCommandError(err)
		}
		favorites, err := loadFavorites(command.Context(), client)
		if err != nil {
			return err
		}
		sessions := make([]machineSession, 0, len(records))
		for _, session := range records {
			sessions = append(sessions, machineSession{target: target, session: session})
		}
		selected, err := selectMachineSession(command, sessions, favorites)
		if errors.Is(err, errFavoriteToggle) {
			id := machineSessionFavoriteID(selected)
			if err := setFavorite(command.Context(), client, "session", id, !favorites.IsFavorite("session", id)); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := actionHomeSessionActions(command, target, selected.session); err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

func actionHomeAccount(command *cobra.Command) error {
	for {
		ctx := actionContext(command, nil)
		d, err := buildDeps(ctx)
		if err != nil {
			return err
		}
		items := []selector.Item{{ID: "status", Title: "Account status", Description: "Not signed in"}, {ID: "login", Title: "Sign in", Description: "Approve this CLI in your browser"}}
		if _, credentialErr := d.auth.Credential(); credentialErr == nil {
			items = []selector.Item{
				{ID: "status", Title: "Account status", Description: "Signed in"},
				{ID: "change-account", Title: "Switch account", Description: "Sign in with another account through browser approval"},
				{ID: "logout", Title: "Sign out", Description: "Revoke this CLI session"},
			}
		}
		choice, err := chooseHomeAction(command, "Account", items)
		if err != nil {
			return err
		}
		switch choice.ID {
		case "status":
			client, clientErr := backendForCommand(command)
			if clientErr != nil {
				if infoErr := showInformation(command, "Account", "Not signed in", nil); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
					return infoErr
				}
				continue
			}
			var me api.Me
			meErr := homeLoading(command, "Account", "Loading account", func(ctx context.Context) error {
				var loadErr error
				me, loadErr = client.Me(ctx)
				return loadErr
			})
			if meErr != nil {
				return friendlyCommandError(meErr)
			}
			if infoErr := showInformation(command, "Account", firstNonEmpty(me.Email, me.DisplayName, me.ID), []selector.Item{{ID: "server", Title: "Paperboat server", Description: d.cfg.ServerURL}}); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
				return infoErr
			}
		case "login":
			if err := executeInteractiveCommand(command, []string{"auth", "login"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
		case "change-account":
			if err := executeInteractiveCommand(command, []string{"auth", "login", "--change-account"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
			return selector.ErrInterrupted
		case "logout":
			if err := executeInteractiveCommand(command, []string{"auth", "logout"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
		}
	}
}

func actionHomeConfig(command *cobra.Command) error {
	for {
		cfg, loadErr := config.Load(configPathFlag(command))
		if loadErr != nil {
			return loadErr
		}
		choice, err := chooseHomeAction(command, "Configuration", []selector.Item{
			{ID: "customize", Title: "Customize your CLI", Description: "Shortcuts, defaults, themes, keys, and home layout"},
			{ID: "server", Title: "Paperboat server", Description: orNone(cfg.ServerURL), Search: "edit url endpoint"},
			{ID: "auth", Title: "File credential fallback", Description: onOff(cfg.Auth.AllowFileFallback), Search: "toggle auth credentials"},
			{ID: "status-bar", Title: "Status bar", Description: cfg.StatusBar.Mode + "  ·  " + cfg.StatusBar.Theme + "  ·  fullscreen " + cfg.StatusBar.Fullscreen},
			{ID: "sync", Title: "Config sync", Description: "Enable, configure, inspect or disable sync on this machine"},
			{ID: "path", Title: "Configuration file", Description: cfg.Path()},
		})
		if err != nil {
			return err
		}
		switch choice.ID {
		case "customize":
			if err := editPreferences(command); err != nil && !interactiveCanceled(err) {
				return err
			}
		case "server":
			value, inputErr := prompt.Text(prompt.TextOptions{Title: "Paperboat server", Description: "Control-plane URL used by this CLI", Placeholder: "https://api.example.test", Initial: cfg.ServerURL, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
				if strings.TrimSpace(value) == "" {
					return nil
				}
				_, normalizeErr := config.NormalizeServerURL(value)
				return normalizeErr
			}})
			if errors.Is(inputErr, prompt.ErrCanceled) {
				continue
			}
			if inputErr != nil {
				return inputErr
			}
			if strings.TrimSpace(value) == "" {
				cfg.ServerURL = ""
			} else {
				cfg.ServerURL, loadErr = config.NormalizeServerURL(value)
				if loadErr != nil {
					return loadErr
				}
			}
			if saveErr := cfg.Save(); saveErr != nil {
				return saveErr
			}
		case "auth":
			cfg.Auth.AllowFileFallback = !cfg.Auth.AllowFileFallback
			if saveErr := cfg.Save(); saveErr != nil {
				return saveErr
			}
		case "status-bar":
			if statusErr := actionHomeStatusBar(command); !errors.Is(statusErr, selector.ErrCanceled) {
				return statusErr
			}
		case "sync":
			if syncErr := actionHomeConfigSync(command); syncErr != nil && !interactiveCanceled(syncErr) {
				if err := showHomeFailure(command, syncErr); err != nil && !interactiveCanceled(err) {
					return err
				}
			}
		case "path":
			if infoErr := showInformation(command, "Configuration file", cfg.Path(), nil); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
				return infoErr
			}
		}
	}
}

func actionHomeStatusBar(command *cobra.Command) error {
	for {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		choice, err := chooseHomeAction(command, "Status bar", []selector.Item{
			{ID: "mode", Title: "Mode", Description: cfg.StatusBar.Mode},
			{ID: "fullscreen", Title: "Full-screen applications", Description: cfg.StatusBar.Fullscreen},
			{ID: "theme", Title: "Theme", Description: cfg.StatusBar.Theme},
			{ID: "privacy", Title: "Privacy", Description: onOff(cfg.StatusBar.Privacy)},
			{ID: "title", Title: "Terminal title", Description: onOff(cfg.StatusBar.TerminalTitle)},
			{ID: "reset", Title: "Restore defaults", Description: "Reset all status-bar preferences"},
		})
		if err != nil {
			return err
		}
		switch choice.ID {
		case "mode":
			cfg.StatusBar.Mode, err = chooseValue(command, "Status bar mode", cfg.StatusBar.Mode, []string{"auto", "on", "off"})
		case "fullscreen":
			cfg.StatusBar.Fullscreen, err = chooseValue(command, "Full-screen applications", cfg.StatusBar.Fullscreen, []string{"hide", "show"})
		case "theme":
			cfg.StatusBar.Theme, err = chooseValue(command, "Status bar theme", cfg.StatusBar.Theme, []string{"terminal", "dark", "light", "mono"})
		case "privacy":
			cfg.StatusBar.Privacy = !cfg.StatusBar.Privacy
		case "title":
			cfg.StatusBar.TerminalTitle = !cfg.StatusBar.TerminalTitle
		case "reset":
			cfg.StatusBar = config.DefaultStatusBarConfig()
		}
		if errors.Is(err, selector.ErrCanceled) {
			continue
		}
		if err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
	}
}

func chooseValue(command *cobra.Command, title, current string, values []string) (string, error) {
	items := make([]selector.Item, 0, len(values))
	for _, value := range values {
		description := ""
		if value == current {
			description = "Current"
		}
		items = append(items, selector.Item{ID: value, Title: value, Description: description})
	}
	choice, err := selector.Choose(selector.Options{Title: title, Subtitle: "Choose a value", Items: items, Initial: current, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return choice.ID, err
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func actionHomeDoctor(command *cobra.Command) error {
	for {
		items, err := homeDoctorItems(command)
		if err != nil {
			return err
		}
		choice, err := selector.Choose(selector.Options{Title: "Diagnostics", Subtitle: "Local setup and Paperboat connectivity", Items: items, Footer: "enter view details · esc back", Context: command.Context(), Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if choice.ID != "config" {
			if err := showHomeText(command, choice.Title, homeDoctorDetail(choice)); err != nil {
				return err
			}
			continue
		}
		if err = actionHomeConfigSync(command); err != nil && !interactiveCanceled(err) {
			if err = showHomeFailure(command, err); err != nil && !interactiveCanceled(err) {
				return err
			}
		}
	}
}

func homeDoctorItems(command *cobra.Command) ([]selector.Item, error) {
	report := collectLocalDoctor(command.Context())
	items := []selector.Item{
		{ID: "setup", Title: "Local setup", Description: report.SetupState},
		{ID: "identity", Title: "Machine identity", Description: report.IdentityState},
		{ID: "credential", Title: "Machine credential", Description: report.CredentialState},
		{ID: "inbox", Title: "Paperboat Inbox", Description: report.InboxState + "  ·  " + report.InboxPath},
		{ID: "config", Title: "Config sync", Description: diagnosticConfigSync(report.ConfigService, nil, errors.New("assignment not checked")), Action: true},
		{ID: "runtime", Title: "Host runtime", Description: report.HostRuntime},
		{ID: "workloads", Title: "Local workloads", Description: diagnosticWorkloads(report)},
	}
	ctx := actionContext(command, nil)
	d, err := buildDeps(ctx)
	if err == nil {
		credential, credentialErr := d.auth.Credential()
		if credentialErr != nil {
			items = append(items, selector.Item{ID: "auth", Title: "Account", Description: diagnosticCredentialFailure(credentialErr)})
		} else if strings.TrimSpace(d.cfg.ServerURL) == "" {
			items = append(items, selector.Item{ID: "backend", Title: "Control plane", Description: "server not configured"})
		} else {
			var me api.Me
			meErr := homeLoading(command, "Diagnostics", "Checking control plane", func(ctx context.Context) error {
				var loadErr error
				me, loadErr = api.New(d.cfg.ServerURL, credential, nil).Me(ctx)
				return loadErr
			})
			if meErr != nil {
				items = append(items, selector.Item{ID: "backend", Title: "Control plane", Description: "unavailable  ·  " + userFacingError(meErr)})
			} else {
				items = append(items, selector.Item{ID: "auth", Title: "Account", Description: firstNonEmpty(me.Email, me.DisplayName, me.ID)}, selector.Item{ID: "backend", Title: "Control plane", Description: "authenticated"})
				if report.MachineID != "" {
					checkCtx, cancel := context.WithTimeout(command.Context(), 5*time.Second)
					assignment, assignmentErr := api.New(d.cfg.ServerURL, credential, nil).ConfigAssignment(checkCtx, report.MachineID)
					cancel()
					for index := range items {
						if items[index].ID == "config" {
							items[index].Description = diagnosticConfigSync(report.ConfigService, &assignment, assignmentErr)
						}
					}
					if assignmentErr == nil && configAssignmentEnabled(assignment) && report.ConfigService == "not_installed" {
						report.RecoveryActions = append(report.RecoveryActions, "configuration sync is assigned but its local worker is missing; reapply the configuration assignment to reinstall it")
					}
				}
			}
		}
	}
	if err != nil {
		items = append(items, selector.Item{ID: "auth", Title: "Account", Description: "saved sign-in could not be inspected"})
	}
	for index, recovery := range report.RecoveryActions {
		items = append(items, selector.Item{ID: fmt.Sprintf("recovery-%d", index), Title: "Needs attention", Description: recovery})
	}
	return items, nil
}

func showInformation(command *cobra.Command, title, subtitle string, items []selector.Item) error {
	for {
		choice, err := selector.Choose(selector.Options{Title: title, Subtitle: subtitle, Items: items, Empty: "Press Esc to go back", Footer: "enter view details  type to filter  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if err := showHomeText(command, choice.Title, choice.Description); err != nil && !interactiveCanceled(err) {
			return err
		}
	}
}

func chooseHomeAction(command *cobra.Command, title string, items []selector.Item) (selector.Item, error) {
	choice, err := selector.Choose(selector.Options{Title: title, Subtitle: "Choose an action", Items: items, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return choice, err
}

func compareFavorites(a, b bool) int {
	if a == b {
		return 0
	}
	if a {
		return -1
	}
	return 1
}

type favoriteSet map[string]struct{}

func (f favoriteSet) IsFavorite(kind, id string) bool {
	_, ok := f[kind+":"+id]
	return ok
}

func (f favoriteSet) Set(kind, id string, favorite bool) {
	key := kind + ":" + id
	if favorite {
		f[key] = struct{}{}
		return
	}
	delete(f, key)
}

func loadFavorites(ctx context.Context, client *api.Client) (favoriteSet, error) {
	items, err := client.ListFavorites(ctx)
	if err != nil {
		return nil, friendlyCommandError(err)
	}
	favorites := make(favoriteSet, len(items))
	for _, item := range items {
		favorites[item.Kind+":"+item.ResourceID] = struct{}{}
	}
	return favorites, nil
}

var errFavoriteToggle = errors.New("favorite toggle requested")

func setFavorite(ctx context.Context, client *api.Client, kind, id string, favorite bool) error {
	_, err := client.SetFavorite(ctx, kind, id, favorite)
	if err == nil {
		return nil
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "favorite_limit_reached" {
		return errors.New("you can favorite up to five items; unfavorite another machine, session, or preview first")
	}
	return friendlyCommandError(err)
}

func backendForCommand(command *cobra.Command) (*api.Client, error) {
	ctx := actionContext(command, nil)
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, err
	}
	return newWorkspaceAPIClient(ctx, d.cfg.ServerURL, d.auth)
}

func executeInteractiveCommand(parent *cobra.Command, args []string) error {
	command := newInteractiveRootCommand()
	command.SetIn(parent.InOrStdin())
	command.SetOut(parent.OutOrStdout())
	command.SetErr(parent.ErrOrStderr())
	resolved, ctx, err := preparePreferences(command, interactiveArgs(parent, args), parent.Context())
	if err != nil {
		return err
	}
	command.SetArgs(resolved)
	restore := selector.SuspendScreen(parent.ErrOrStderr())
	defer restore()
	return command.ExecuteContext(ctx)
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

func transferClientForCommand(cobraCommand *cobra.Command, args []string) (*filetransfer.NativeClient, api.UserMachine, io.Closer, error) {
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

func addConnectFlags(command *cobra.Command) {
	command.Flags().String("name", "", "name for the fresh terminal session")
	command.Flags().String("session", "", "attach an existing terminal session by name or ID")
	command.Flags().Bool("debug", false, "show the pb versions used by this terminal session")
	addStatusBarFlags(command)
}

func addStatusBarFlags(command *cobra.Command) {
	command.Flags().String("status-bar", "", "status bar for this attach: auto, on, or off")
	command.Flags().String("status-bar-fullscreen", "", "status bar in full-screen applications: hide or show")
	command.Flags().String("status-bar-theme", "", "status bar theme: terminal, dark, light, or mono")
}

func validateConnectInvocation(command *cobra.Command) error {
	name, _ := command.Flags().GetString("name")
	ref, _ := command.Flags().GetString("session")
	if strings.TrimSpace(name) != "" && strings.TrimSpace(ref) != "" {
		return invocationError(errors.New("--name and --session cannot be used together"))
	}
	for name, allowed := range map[string][]string{
		"status-bar":            {"auto", "on", "off"},
		"status-bar-fullscreen": {"hide", "show"},
		"status-bar-theme":      {"terminal", "dark", "light", "mono"},
	} {
		value, _ := command.Flags().GetString(name)
		if value != "" && !containsString(allowed, strings.ToLower(strings.TrimSpace(value))) {
			return invocationError(fmt.Errorf("--%s must be one of %s", name, strings.Join(allowed, ", ")))
		}
	}
	server, _ := command.Flags().GetString("server")
	if strings.TrimSpace(server) != "" {
		if _, err := config.NormalizeServerURL(server); err != nil {
			return invocationError(err)
		}
	}
	return nil
}

func windowsEnrollmentCommand(installerURL string) string {
	return "powershell -c \"iex (irm '" + installerURL + "')\""
}

func resolveUserMachineTarget(ctx context.Context, client *api.Client, requested string) (string, string, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return "", "", friendlyCommandError(err)
	}
	for _, machine := range machines {
		if machine.ID == requested {
			return machine.ID, machine.Alias, nil
		}
	}
	matches := make([]api.UserMachine, 0, 1)
	for _, machine := range machines {
		if strings.EqualFold(machine.Alias, requested) {
			matches = append(matches, machine)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, matches[0].Alias, nil
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("machine name %q is ambiguous; use a stable machine ID", requested)
	}
	return "", "", fmt.Errorf("machine %q was not found", requested)
}

func actionRun(action command.Action) func(*cobra.Command, []string) error {
	return func(command *cobra.Command, args []string) error {
		c := actionContext(command, args)
		c.Context = context.WithValue(c.Context, confirmationCommandKey{}, command)
		return action(c)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func requireAuthConfig(c *command.Context) (*config.Config, config.ProfileStore, error) {
	d, err := buildDeps(c)
	if err != nil {
		return nil, config.ProfileStore{}, err
	}
	if strings.TrimSpace(d.cfg.ServerURL) == "" {
		return nil, config.ProfileStore{}, commandRejection{reason: commandRejectMissingServer}
	}
	s, err := config.ProfileStoreFor(d.cfg)
	if err != nil {
		return nil, config.ProfileStore{}, err
	}
	return d.cfg, s, nil
}

const dashboardEnrollmentGuidance = "Run `pb login` and approve this CLI in your browser. The printed link works on another device when this machine has no browser."

func e2eeClient(c *command.Context) (*api.Client, config.ProfileStore, config.Profile, error) {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	if server := strings.TrimSpace(c.String("server")); server != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(server)
		if err != nil {
			return nil, config.ProfileStore{}, config.Profile{}, err
		}
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return nil, config.ProfileStore{}, config.Profile{}, commandRejection{reason: commandRejectMissingServer}
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	source := (&sessionauth.Source{Store: store, Issuer: cfg.ServerURL}).WithContext(c.Context)
	client, err := newWorkspaceAPIClient(c, cfg.ServerURL, source)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	return client, store, profile, nil
}

func authStatus(c *command.Context) error {
	cfg, store, err := requireAuthConfig(c)
	if err != nil {
		return err
	}
	if err := store.Recover(cfg.ServerURL); err != nil {
		return fmt.Errorf("recover interrupted Paperboat sign-in: %w", err)
	}
	p, err := store.Load(cfg.ServerURL)
	if onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return leaf == config.ErrNoCredentials }) {
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(map[string]any{"signed_in": false})
		}
		fmt.Fprintln(c.Writer, "Not signed in")
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := store.CredentialFor(cfg.ServerURL); err != nil {
		return &sessionauth.CredentialFailure{Cause: err}
	}
	if c.Bool("json") {
		document := map[string]any{"signed_in": true, "issuer": p.Issuer, "cli_client_session_id": p.CLIClientSessionID, "access_expires_at": p.AccessExpiresAt, "account": p.Account}
		if root, rootErr := store.LoadPeerAccountRootPublic(p.Issuer, p.Account.ID); rootErr == nil {
			fingerprint, fingerprintErr := endpointidentity.RootFingerprint(root)
			if fingerprintErr != nil {
				return fingerprintErr
			}
			document["trusted_root_public_key"] = base64.RawURLEncoding.EncodeToString(root)
			document["trusted_root_fingerprint"] = fingerprint
		} else if !onlyEnvironmentFailureLeaves(rootErr, func(leaf error) bool { return leaf == config.ErrSecretNotFound }) {
			return fmt.Errorf("load locally trusted identity: %w", rootErr)
		}
		return json.NewEncoder(c.Writer).Encode(document)
	}
	fmt.Fprintf(c.Writer, "Signed in as %s\nServer: %s\nSession: %s\nAccess expires: %s\n", firstNonEmpty(p.Account.Email, p.Account.DisplayName, p.Account.ID), p.Issuer, p.CLIClientSessionID, p.AccessExpiresAt.Format(time.RFC3339))
	return nil
}

func authLogout(c *command.Context) error {
	cfg, store, err := requireAuthConfig(c)
	if err != nil {
		return err
	}
	if err := store.Recover(cfg.ServerURL); err != nil {
		return fmt.Errorf("recover interrupted Paperboat sign-in: %w", err)
	}
	return store.WithBrowserLogin(cfg.ServerURL, func(state *config.BrowserLoginState, save func() error) error {
		cancellationErr := cancelBrowserLogin(c.Context, cfg.ServerURL, state, save)
		return authLogoutSessions(c, cfg, store, cancellationErr)
	})
}

func authLogoutSessions(c *command.Context, cfg *config.Config, store config.ProfileStore, cancellationErr error) error {
	credentials, err := store.TakeLogoutCredentials(cfg.ServerURL)
	partial, locallySignedOut := err.(*config.LogoutRevocationIncompleteError)
	if err != nil && (!locallySignedOut || partial == nil) {
		return fmt.Errorf("remove local Paperboat sessions: %w", err)
	}
	unconfirmed := locallySignedOut
	observe := func(cause error) {
		if cause != nil && !errorreport.HTTPAttemptObserved(cause) {
			if _, localRead := cause.(*config.LogoutRevocationIncompleteError); localRead {
				errorreport.Current().ObserveFailure(c.Context, "pb", "auth", "command", "command_failed", cause)
				return
			}
			errorreport.Current().ObserveFailure(c.Context, "pb", "auth", "control_request", "control_request_failed", cause)
		}
	}
	observe(err)
	revokeCtx, cancel := context.WithTimeout(c.Context, 2*time.Second)
	defer cancel()
	// Sequential requests share one deadline. No worker can outlive local
	// signout, and the number of historical sessions never grows concurrency.
	for _, credential := range credentials {
		if revokeCtx.Err() != nil {
			unconfirmed = true
			observe(revokeCtx.Err())
			break
		}
		if err := api.RevokeToken(revokeCtx, cfg.ServerURL, credential.RefreshToken, nil); err != nil {
			unconfirmed = true
			observe(err)
		}
	}
	if cancellationErr != nil {
		observe(cancellationErr)
	}
	status := "complete"
	if unconfirmed {
		status = "unconfirmed"
	}
	if c.Bool("json") {
		result := map[string]any{"signed_out": true, "server": cfg.ServerURL, "server_revocation": status}
		if cancellationErr != nil {
			result["browser_login_cancellation"] = "unconfirmed"
		}
		return writeCLIJSON(c.Writer, result)
	}
	if _, err := fmt.Fprintln(c.Writer, "Signed out"); err != nil {
		return err
	}
	if unconfirmed {
		if _, err := fmt.Fprintln(c.ErrWriter, "Some server sessions could not be confirmed as revoked. Local credentials were removed; manage active sessions in the Paperboat dashboard."); err != nil {
			return err
		}
	}
	if cancellationErr != nil {
		if _, err := fmt.Fprintln(c.ErrWriter, "Cancellation of an earlier browser login could not be confirmed. Check pending sign-in requests in the Paperboat dashboard."); err != nil {
			return err
		}
	}
	return nil
}

func drainPendingRevocations(ctx context.Context, issuer string, store config.ProfileStore) error {
	records, err := store.PendingRevocations(issuer)
	if err != nil {
		return err
	}
	var errs []error
	for _, record := range records {
		if record.Cancelled {
			if err := store.CompleteRevocation(record); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if record.ServerRevoked {
			if err := store.CompleteRevocation(record); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		cred, err := store.PendingRevocationCredential(record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := api.RevokeToken(ctx, issuer, cred.RefreshToken, nil); err != nil {
			errs = append(errs, fmt.Errorf("revoke client session %s: %w", record.CLIClientSessionID, err))
			continue
		}
		record, err = store.MarkRevocationSucceeded(record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := store.CompleteRevocation(record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var openBrowser = platformOpenBrowser

func backendClient(c *command.Context) (*api.Client, error) {
	d, err := buildDeps(c)
	if err != nil {
		return nil, err
	}
	if d.cfg.ServerURL == "" {
		return nil, commandRejection{reason: commandRejectMissingServer}
	}
	client, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, d.auth)
	if err != nil {
		return nil, err
	}
	return client, nil
}

type environmentTarget struct {
	kind string
	id   string
	name string
}

const environmentUserMachine = "machine"

func selectEnvironment(ctx context.Context, client *api.Client, title string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("an environment is required in non-interactive use")
	}
	var machines []api.UserMachine
	load := func(loadCtx context.Context) error {
		var err error
		machines, err = client.ListUserMachines(loadCtx)
		return friendlyCommandError(err)
	}
	var err error
	if selector.ScreenActive() {
		err = selector.Loading(ctx, title, "Loading environments", os.Stdin, os.Stderr, load)
	} else {
		err = load(ctx)
	}
	if err != nil {
		return "", err
	}
	machines = terminalHostMachines(machines)
	items := make([]selector.Item, 0, len(machines))
	for _, machine := range machines {
		if !machine.Capabilities.TerminalHost.Configured || !machine.Capabilities.TerminalHost.Observed {
			continue
		}
		items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(ctx, "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: "machine " + machineStatusSearch(machine)})
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Title: title, Subtitle: "Terminal-capable machines", Items: items, Empty: "no terminal-capable machines are available; run `pb setup` or `pb machine add`", Stdin: os.Stdin, Output: os.Stderr})
	return selected.ID, err
}

func selectSession(ctx context.Context, client *api.Client, target environmentTarget, sessions []api.TerminalSession, title string) (api.TerminalSession, error) {
	items := make([]selector.Item, 0, len(sessions))
	byID := make(map[string]api.TerminalSession, len(sessions))
	for _, session := range sessions {
		attached := "no attachments"
		if session.AttachedCount != nil {
			attached = fmt.Sprintf("%d attached", *session.AttachedCount)
		}
		activity := "last active " + relativeTime(session.LastActiveAt)
		created := "created " + relativeTimestamp(session.CreatedAt)
		description := sessionIdentificationDetails(session, preferenceDetails(ctx, "sessions", map[string]string{"machine": target.name, "state": strings.Join([]string{session.State, attached, activity, created}, " · "), "id": session.ID}))
		items = append(items, selector.Item{ID: session.ID, Title: session.Name, Description: description, Search: target.name + " " + session.State + " " + sessionIdentificationSearch(session)})
		byID[session.ID] = session
	}
	refresh := func(refreshCtx context.Context) ([]selector.Item, error) {
		fresh, err := listTerminalSessionsForTarget(refreshCtx, client, target)
		if err != nil {
			return nil, err
		}
		updated := make([]selector.Item, 0, len(fresh))
		for _, entry := range fresh {
			if _, ok := byID[entry.ID]; !ok {
				continue
			}
			updated = append(updated, selector.Item{ID: entry.ID, Title: entry.Name, Description: sessionIdentificationDetails(entry, target.name+" · "+entry.State), Search: sessionIdentificationSearch(entry)})
		}
		return updated, nil
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Refresh: refresh, Title: title, Subtitle: target.name + "  ·  " + target.kind, Items: items, Empty: "no terminal sessions are available", Stdin: os.Stdin, Output: os.Stderr})
	return byID[selected.ID], err
}

func relativeTimestamp(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	value := at
	return relativeTime(&value)
}

func defaultEnvironment(ctx context.Context, client *api.Client, rememberedID, workspace string) (string, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return "", friendlyCommandError(err)
	}
	machines = terminalHostMachines(machines)
	if rememberedID = strings.TrimSpace(rememberedID); rememberedID != "" {
		for _, machine := range machines {
			if machine.ID == rememberedID {
				return rememberedID, nil
			}
		}
		return "", fmt.Errorf("last environment %q is not available in the selected %s workspace; choose a machine in this workspace or run `pb switch <workspace>`", rememberedID, workspaceDisplayName(workspace))
	}
	if len(machines) == 1 {
		return machines[0].ID, nil
	}
	if len(machines) == 0 {
		return "", errors.New("no terminal-capable machines are available; run `pb setup` or `pb machine add`")
	}
	choices := make([]string, 0, len(machines))
	for _, machine := range machines {
		choices = append(choices, fmt.Sprintf("%s (%s)", machine.Alias, machine.ID))
	}
	return "", fmt.Errorf("multiple environments are available: %s; choose one with `pb <environment>`", strings.Join(choices, ", "))
}

func defaultWorkspaceEnvironment(ctx context.Context, commandContext *command.Context, client *api.Client, cfg *config.Config) (string, error) {
	workspace := client.Workspace()
	if workspace == "" {
		var err error
		workspace, err = workspaceForCommandContext(commandContext)
		if err != nil {
			return "", err
		}
	}
	accountID, err := workspaceAccountID(cfg)
	if err != nil {
		return "", err
	}
	rememberedID := ""
	if accountID != "" {
		rememberedID, err = workspaceLastEnvironment(cfg, cfg.ServerURL, accountID, workspace)
		if err != nil {
			return "", err
		}
	}
	return defaultEnvironment(ctx, client, rememberedID, workspace)
}

func resolveEnvironmentTarget(ctx context.Context, client *api.Client, requested string) (environmentTarget, error) {
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, nil
}

func resolveTerminalEnvironmentTarget(ctx context.Context, client *api.Client, requested string) (environmentTarget, error) {
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, err
	}
	if err := terminalHostError(machine); err != nil {
		return environmentTarget{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, nil
}

func terminalHostMachines(machines []api.UserMachine) []api.UserMachine {
	eligible := make([]api.UserMachine, 0, len(machines))
	for _, machine := range machines {
		if terminalHostError(machine) == nil {
			eligible = append(eligible, machine)
		}
	}
	return eligible
}

func terminalHostError(machine api.UserMachine) error {
	if !machine.Capabilities.TerminalHost.Configured {
		return &api.APIError{Code: "machine_capability_unavailable", Message: "This machine is not configured to host terminals."}
	}
	if !machine.Online || !machine.Capabilities.TerminalHost.Observed {
		return &api.APIError{Code: "machine_offline", Message: "This terminal host is offline."}
	}
	return nil
}

func listTerminalSessionsForTarget(ctx context.Context, client *api.Client, target environmentTarget) ([]api.TerminalSession, error) {
	return client.ListUserMachineTerminalSessions(ctx, target.id)
}

func createTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, name, idempotencyKey string) (api.TerminalSession, error) {
	return client.CreateUserMachineTerminalSession(ctx, target.id, name, idempotencyKey)
}

func renameTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID, name string) (api.TerminalSession, error) {
	return client.RenameUserMachineTerminalSession(ctx, target.id, sessionID, name)
}

func closeTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID string) (api.TerminalSessionCloseResult, error) {
	return client.CloseUserMachineTerminalSession(ctx, target.id, sessionID)
}

func deleteTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID string) error {
	return client.DeleteUserMachineTerminalSession(ctx, target.id, sessionID)
}

// deps bundles production dependencies for a command.
type deps struct {
	cfg              *config.Config
	auth             config.AuthSource
	resolver         resolver.MachineResolver
	tunnel           tunnel.Tunnel
	peerLocal        *localapi.Client
	peerApplications peerApplicationTunnel
	telemetry        telemetry.Sink
}

type peerApplicationTunnel interface {
	Dial(context.Context, resolver.ConnectInfo) (tunnel.Conn, error)
	DialExec(context.Context, resolver.ConnectInfo, tunnel.ExecRequest) (tunnel.ExecConn, error)
	DialSSH(context.Context, resolver.ConnectInfo, string) (tunnel.Conn, error)
}

func buildDeps(c *command.Context) (*deps, error) {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return nil, err
	}
	if s := c.String("server"); s != "" {
		normalized, err := config.NormalizeServerURL(s)
		if err != nil {
			return nil, err
		}
		cfg.ServerURL = normalized
	} else if cfg.ServerURL != "" {
		normalized, err := config.NormalizeServerURL(cfg.ServerURL)
		if err != nil {
			return nil, fmt.Errorf("invalid configured Paperboat server: %w", err)
		}
		cfg.ServerURL = normalized
	}
	var authSource config.AuthSource = config.NoCredentialsSource{}
	if cfg.ServerURL != "" {
		authSource, err = sessionauth.NewSource(cfg)
		if err != nil {
			return nil, err
		}
	}
	var selectedTunnel tunnel.Tunnel
	var peerApplications peerApplicationTunnel
	var peerLocal *localapi.Client
	if cfg.ServerURL != "" {
		paths, pathsErr := currentLocalDaemonPaths()
		if pathsErr != nil {
			return nil, pathsErr
		}
		localClient, clientErr := localapi.NewClient(paths.SocketPath, time.Duration(config.PeerConnectTimeoutMilliseconds)*time.Millisecond)
		if clientErr != nil {
			return nil, clientErr
		}
		localPeer := &tunnel.LocalPeerTunnel{Client: localClient}
		peerApplications = localPeer
		peerLocal = localClient
		selectedTunnel = localPeer
	}
	return &deps{
		cfg:              cfg,
		auth:             authSource,
		resolver:         nil,
		tunnel:           selectedTunnel,
		peerLocal:        peerLocal,
		peerApplications: peerApplications,
	}, nil
}

type pingSample struct {
	Sequence     int     `json:"sequence"`
	Path         string  `json:"path,omitempty"`
	ConnectionMS float64 `json:"connection_ms,omitempty"`
	Lost         bool    `json:"lost"`
	Transition   bool    `json:"path_transition"`
}

type pingReport struct {
	Schema              string       `json:"schema"`
	MachineID           string       `json:"machine_id"`
	MachineName         string       `json:"machine_name"`
	Sent                int          `json:"sent"`
	Received            int          `json:"received"`
	Lost                int          `json:"lost"`
	LossPercent         float64      `json:"loss_percent"`
	MinConnectionMS     float64      `json:"min_connection_ms,omitempty"`
	AverageConnectionMS float64      `json:"average_connection_ms,omitempty"`
	MaxConnectionMS     float64      `json:"max_connection_ms,omitempty"`
	Samples             []pingSample `json:"samples"`
}

// peerProber is the existing authenticated daemon probe boundary.
type peerProber interface {
	ProbePeer(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)
}

func probeDaemonPeer(ctx context.Context, client peerProber, target resolver.ConnectInfo, operation string) (tunnel.NativeProbe, error) {
	if client == nil || target.Terminal == nil {
		return tunnel.NativeProbe{}, errors.New("authenticated daemon probe is unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return tunnel.NativeProbe{}, errors.New("daemon probe requires a deadline")
	}
	request, err := localapi.NewPeerStreamRequest(target.MachineID, target.Terminal.EnvironmentID, target.MachineGeneration, "health_probe", operation, "local-health-probe", deadline, 1<<20, nil)
	if err != nil {
		return tunnel.NativeProbe{}, err
	}
	if target.Workspace != "" {
		request.Workspace = target.Workspace
	}
	probe, err := client.ProbePeer(ctx, request)
	if err != nil {
		return tunnel.NativeProbe{}, err
	}
	return tunnel.NativeProbe{Path: probe.Path, Connection: time.Duration(probe.ConnectionNanoseconds)}, nil
}

func actionPing(command *cobra.Command, args []string) error {
	count, _ := command.Flags().GetInt("count")
	timeout, _ := command.Flags().GetDuration("timeout")
	jsonOutput, _ := command.Flags().GetBool("json")
	if count < 1 || count > 100 {
		return invocationError(errors.New("--count must be between 1 and 100"))
	}
	if timeout <= 0 || timeout > time.Minute {
		return invocationError(errors.New("--timeout must be greater than zero and no more than 1m"))
	}
	ctx := actionContext(command, args)
	dependencies, err := buildDeps(ctx)
	if err != nil {
		return err
	}
	if dependencies.peerLocal == nil {
		return errors.New("pb ping requires an authenticated Paperboat server")
	}
	client, err := backendClient(ctx)
	if err != nil {
		return err
	}
	machine, err := resolveUserMachine(command.Context(), client, args[0])
	if err != nil {
		return err
	}
	if !machine.Online {
		return &api.APIError{Code: "machine_offline", Message: "This machine is offline."}
	}
	if err := requireLocalDaemonService(command.Context(), dependencies.cfg); err != nil {
		return fmt.Errorf("prepare local peer transport: %w", err)
	}
	target := resolver.ConnectInfo{TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
	report := pingReport{Schema: "paperboat.ping/v1", MachineID: machine.ID, MachineName: machine.Alias, Sent: count, Samples: make([]pingSample, 0, count)}
	previousSelection := ""
	var total time.Duration
	for sequence := 1; sequence <= count; sequence++ {
		sampleCtx, cancel := context.WithTimeout(command.Context(), timeout)
		result, pingErr := probeDaemonPeer(sampleCtx, dependencies.peerLocal, target, newIdempotencyKey())
		cancel()
		if pingErr != nil {
			if command.Context().Err() != nil {
				return command.Context().Err()
			}
			if !errors.Is(pingErr, context.DeadlineExceeded) && !errors.Is(pingErr, context.Canceled) {
				return fmt.Errorf("authenticated ping sample %d: %w", sequence, pingErr)
			}
			report.Lost++
			report.Samples = append(report.Samples, pingSample{Sequence: sequence, Lost: true})
			if !jsonOutput {
				fmt.Fprintf(command.OutOrStdout(), "sample %d: timeout\n", sequence)
			}
			continue
		}
		path := pingPath(result.Path)
		selection := path
		transition := previousSelection != "" && previousSelection != selection
		previousSelection = selection
		connectionMS := float64(result.Connection) / float64(time.Millisecond)
		report.Received++
		total += result.Connection
		if report.MinConnectionMS == 0 || connectionMS < report.MinConnectionMS {
			report.MinConnectionMS = connectionMS
		}
		if connectionMS > report.MaxConnectionMS {
			report.MaxConnectionMS = connectionMS
		}
		sample := pingSample{Sequence: sequence, Path: path, ConnectionMS: connectionMS, Transition: transition}
		report.Samples = append(report.Samples, sample)
		if !jsonOutput {
			transitionText := ""
			if transition {
				transitionText = " transition"
			}
			fmt.Fprintf(command.OutOrStdout(), "sample %d: path=%s connect=%.2fms%s\n", sequence, path, sample.ConnectionMS, transitionText)
		}
	}
	if report.Received > 0 {
		report.AverageConnectionMS = float64(total) / float64(time.Millisecond) / float64(report.Received)
	}
	report.LossPercent = float64(report.Lost) * 100 / float64(report.Sent)
	if jsonOutput {
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}
	fmt.Fprintf(command.OutOrStdout(), "summary: sent=%d received=%d loss=%.1f%%", report.Sent, report.Received, report.LossPercent)
	if report.Received > 0 {
		fmt.Fprintf(command.OutOrStdout(), " connect min/avg/max=%.2f/%.2f/%.2fms", report.MinConnectionMS, report.AverageConnectionMS, report.MaxConnectionMS)
	}
	fmt.Fprintln(command.OutOrStdout())
	if report.Received == 0 {
		return exitCodeError{code: 1}
	}
	return nil
}

func pingPath(path string) string {
	switch path {
	case "direct", "peer_relay", "regional_relay", "unknown":
		return path
	default:
		return "unknown"
	}
}

func nativeSnapshotPath(path string) string {
	switch path {
	case "direct":
		return "direct"
	case "peer_relay", "regional_relay":
		return "relay"
	default:
		return ""
	}
}

var errOwnerOnlyFileInvalid = errors.New("invalid owner-only file")

func readOwnerOnlyFile(path string, limit int64) (data []byte, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateOwnerOnlyRegularFile(path, info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			clear(data)
			data = nil
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errOwnerOnlyFileInvalid
	}
	if err := validateOwnerOnlyRegularFile(path, opened); err != nil {
		return nil, err
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		clear(data)
		return nil, err
	}
	if int64(len(data)) > limit {
		clear(data)
		return nil, errOwnerOnlyFileInvalid
	}
	return data, nil
}

func inboxRequestsCommand(c *command.Context) error {
	if c.Args().Len() != 0 {
		return localArgumentError("pb inbox requests does not accept arguments")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	requests, err := client.TeamInboxRequests(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]any{"requests": requests}})
	}
	if len(requests) == 0 {
		fmt.Fprintln(c.Writer, "No team file requests.")
		return nil
	}
	w := tabwriter.NewWriter(c.Writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REQUEST\tSENDER\tFILES\tSTATUS\tEXPIRES")
	for _, request := range requests {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", request.RequestID, request.SenderAccount, len(request.Files), request.Status, relativeTimestamp(request.ExpiresAt))
	}
	return w.Flush()
}

func inboxDecisionCommand(c *command.Context, action string) error {
	if c.Args().Len() != 1 {
		return localArgumentError("usage: pb inbox approve|decline <request-id> --generation <generation>")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	generation := uint64(c.Uint("generation"))
	if generation == 0 {
		request, getErr := client.TeamInboxRequest(c.Context, c.Args().First())
		if getErr != nil {
			return friendlyCommandError(getErr)
		}
		generation = request.DecisionGeneration
	}
	request, err := client.DecideTeamInboxRequest(c.Context, c.Args().First(), action, generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": request})
	}
	fmt.Fprintf(c.Writer, "%s: %s\n", request.RequestID, request.Status)
	return nil
}

func inboxPolicyCommand(c *command.Context) error {
	if c.Args().Len() > 1 {
		return localArgumentError("usage: pb inbox policy [manual|automatic] [--receipt-email]")
	}
	if c.Args().Len() == 1 && c.Args().First() != "manual" && c.Args().First() != "automatic" {
		return localArgumentError("acceptance must be manual or automatic")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	policy, err := client.TeamInboxPolicy(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Args().Len() == 1 {
		acceptance := c.Args().First()
		policy.Acceptance, policy.ReceiptEmail = acceptance, c.Bool("receipt-email")
		policy, err = client.SetTeamInboxPolicy(c.Context, policy)
		if err != nil {
			return friendlyCommandError(err)
		}
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": policy})
	}
	fmt.Fprintf(c.Writer, "Acceptance: %s\nReceipt email: %t\n", policy.Acceptance, policy.ReceiptEmail)
	return nil
}

func runtimeIdentityStore() (*identity.Store, error) {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return nil, err
		}
	}
	return identity.Open(identity.Config{StateRoot: stateRoot})
}

func configuredInboxPath() (string, error) {
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if err != nil {
		return "", commandPreparationFailure{step: prepareInboxRegistration, cause: err}
	}
	if err := inbox.ValidatePath(registration.InboxPath); err != nil {
		return "", err
	}
	return registration.InboxPath, nil
}

func configuredMachineID() (string, error) {
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID == "" {
		return "", commandPreparationFailure{step: prepareMachineRegistration, cause: err}
	}
	return registration.MachineID, nil
}

func inboxPathCommand(c *command.Context) error {
	path, err := configuredInboxPath()
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]string{"path": path}})
	}
	fmt.Fprintln(c.Writer, path)
	return nil
}

func inboxSetCommand(c *command.Context) error {
	if c.Args().Len() != 1 {
		return localArgumentError("usage: pb inbox set <directory>")
	}
	return setInboxPath(c, c.Args().First())
}

func inboxResetCommand(c *command.Context) error {
	if c.Args().Len() != 0 {
		return localArgumentError("pb inbox reset does not accept arguments")
	}
	path, err := inbox.DefaultPath()
	if err != nil {
		return err
	}
	return setInboxPath(c, path)
}

func setInboxPath(c *command.Context, path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if err := inbox.EnsurePath(path); err != nil {
		return err
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil {
		return commandPreparationFailure{step: prepareInboxRegistration, cause: err}
	}
	registration.InboxPath = path
	registration.UpdatedAt = time.Now().UTC()
	if err := store.SaveRegistration(registration); err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]string{"path": path}})
	}
	fmt.Fprintln(c.Writer, path)
	return nil
}

func selectTerminalSession(ctx context.Context, client *api.Client, projectRef, name, ref string) (api.TerminalSession, environmentTarget, api.UserMachine, error) {
	target, machine, err := resolveEnvironmentTargetWithMachine(ctx, client, projectRef)
	if err != nil {
		return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, err
	}
	if strings.TrimSpace(ref) == "" {
		if err := validateSessionNameOptional(name); err != nil {
			return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, err
		}
		session, err := createTerminalSessionForTarget(ctx, client, target, name, newIdempotencyKey())
		if err != nil {
			return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, friendlyCommandError(err)
		}
		if session.EvictedSession != nil {
			fmt.Fprintf(os.Stderr, "Session limit reached; removed least-recent session %q (%s).\n", session.EvictedSession.Name, session.EvictedSession.State)
		}
		return session, target, machine, nil
	}
	session, err := resolveTerminalSession(ctx, client, target, ref)
	if err != nil {
		return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, friendlyCommandError(err)
	}
	return session, target, machine, nil
}

// resolveEnvironmentTargetWithMachine resolves like resolveEnvironmentTarget but
// also returns the machine catalog entry for machine targets so callers can
// reuse it without a second listing round trip. The daemon's warm inventory
// snapshot is consulted first: a machine it already tracks needs no catalog
// listing at all, and the server revalidates the machine on every session
// create and connection descriptor anyway.
func resolveEnvironmentTargetWithMachine(ctx context.Context, client *api.Client, requested string) (environmentTarget, api.UserMachine, error) {
	if machine, ok := resolveWarmUserMachine(ctx, requested); ok && machine.InstallationGeneration > 0 {
		return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, machine, nil
	}
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, api.UserMachine{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, machine, nil
}

func resolveUserMachine(ctx context.Context, client *api.Client, requested string) (api.UserMachine, error) {
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
	return api.UserMachine{}, fmt.Errorf("%w: %q", resolver.ErrMachineNotFound, requested)
}

// resolveWarmUserMachine uses the daemon's authenticated inventory snapshot
// for selection. Operation descriptors remain fetched from the control plane;
// this only removes a redundant catalog round trip on an already-warm client.
var loadWarmMachineSnapshot = func(ctx context.Context) (localapi.Snapshot, error) {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 300*time.Millisecond)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	return client.Snapshot(ctx)
}

func resolveWarmUserMachine(ctx context.Context, requested string) (api.UserMachine, bool) {
	if _, scoped := workspaceFromContext(ctx); scoped {
		return api.UserMachine{}, false
	}
	snapshot, err := loadWarmMachineSnapshot(ctx)
	if err != nil {
		return api.UserMachine{}, false
	}
	status, err := localwait.ResolveMachine(snapshot.Machines, requested)
	if err != nil || !status.Eligible || status.Generation == 0 {
		return api.UserMachine{}, false
	}
	return api.UserMachine{ID: status.ID, EnvironmentID: status.EnvironmentID, WorkspaceRoot: status.WorkspaceRoot, Alias: status.Alias, Platform: status.Platform, State: "ready", Online: status.RuntimeState == "ready", InstallationGeneration: int64(status.Generation)}, true
}

func resolveTerminalSession(ctx context.Context, client *api.Client, target environmentTarget, ref string) (api.TerminalSession, error) {
	sessions, err := listTerminalSessionsForTarget(ctx, client, target)
	if err != nil {
		return api.TerminalSession{}, friendlyCommandError(err)
	}
	for _, s := range sessions {
		if s.ID == ref || strings.EqualFold(s.Name, ref) {
			return s, nil
		}
	}
	var suggestions []string
	for _, s := range sessions {
		if strings.HasPrefix(strings.ToLower(s.Name), strings.ToLower(ref)) || editDistance(strings.ToLower(s.Name), strings.ToLower(ref)) <= 2 {
			suggestions = append(suggestions, s.Name)
			if len(suggestions) == 3 {
				break
			}
		}
	}
	message := fmt.Sprintf("terminal session %q was not found", ref)
	if len(suggestions) > 0 {
		message += "; did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	message += "; create one with `pb <environment> new`"
	return api.TerminalSession{}, errors.New(message)
}

var sessionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var automaticSessionNamePattern = regexp.MustCompile(`^shell-[0-9]+$`)

func validateSessionNameOptional(name string) error {
	if name == "" {
		return nil
	}
	return validateSessionName(name)
}
func validateSessionName(name string) error {
	if name == "default" || automaticSessionNamePattern.MatchString(name) || !sessionNamePattern.MatchString(name) {
		return localArgumentError("session names must be lowercase 1-64 character values matching [a-z0-9][a-z0-9._-]{0,63}; default and shell-N are reserved")
	}
	return nil
}

func newIdempotencyKey() string {
	return "operation_" + uuid.NewString()
}
func relativeTime(at *time.Time) string {
	if at == nil {
		return "-"
	}
	d := time.Since(*at)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ra := range a {
		current := make([]int, len(b)+1)
		current[0] = i + 1
		for j, rb := range b {
			cost := 0
			if ra != rb {
				cost = 1
			}
			current[j+1] = minInt(current[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = current
	}
	return prev[len(b)]
}
func minInt(values ...int) int {
	value := values[0]
	for _, candidate := range values[1:] {
		if candidate < value {
			value = candidate
		}
	}
	return value
}

// Presentation belongs to the invocation owner; forwarding keeps typed
// status, attempt ownership and support references intact.
func friendlyCommandError(err error) error { return err }

func actionConnect(c *command.Context) error {
	return actionConnectTarget(c, "")
}

func actionExecCobra(cobraCommand *cobra.Command, args []string, shorthand bool) error {
	dash := cobraCommand.ArgsLenAtDash()
	if dash < 0 || dash >= len(args) || dash != 1 || len(args[dash:]) == 0 {
		return invocationError(errors.New("remote execution requires <machine> -- <argv...>"))
	}
	request := tunnel.ExecRequest{Argv: append([]string(nil), args[dash:]...)}
	jsonOutput := false
	if !shorthand {
		request.CWD, _ = cobraCommand.Flags().GetString("cwd")
		request.Timeout, _ = cobraCommand.Flags().GetDuration("timeout")
		request.PTY, _ = cobraCommand.Flags().GetBool("pty")
		jsonOutput, _ = cobraCommand.Flags().GetBool("json")
		values, _ := cobraCommand.Flags().GetStringArray("env")
		var err error
		request.Environment, err = parseExecEnvironment(values)
		if err != nil {
			return invocationError(err)
		}
	}
	return actionRemoteExec(actionContext(cobraCommand, args), args[0], request, jsonOutput)
}

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

func parseExecEnvironment(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	namePattern := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for _, value := range values {
		name, item, ok := strings.Cut(value, "=")
		if !ok || !namePattern.MatchString(name) || strings.ContainsRune(item, '\x00') {
			return nil, localArgumentError("Each --env must use a valid variable name followed by =value; values cannot contain NUL bytes.")
		}
		if _, exists := result[name]; exists {
			return nil, localArgumentError("An --env variable was specified more than once; provide each variable once.")
		}
		result[name] = item
	}
	return result, nil
}

// A cancellation attachment owns only its transport and output drain. The
// deadline interrupts actual network writes/ack reads and joins both workers.
type execCancelOutcome struct {
	code     int
	err      error
	terminal *tunnel.ExecEvent
}

func cancelExecAttachment(ctx context.Context, connection tunnel.ExecConn) execCancelOutcome {
	drainDone := make(chan struct{})
	var terminal *tunnel.ExecEvent
	go func() {
		defer close(drainDone)
		for event := range connection.Events() {
			if event.Stream == "" && event.State != "" && event.State != "started" {
				copy := event
				terminal = &copy
			}
		}
	}()
	hookDone := make(chan struct{})
	stopHook := context.AfterFunc(ctx, func() { defer close(hookDone); _ = abortExecAttachment(connection) })
	cancelErr := connection.Cancel()
	code := 0
	if cancelErr == nil {
		code, cancelErr = connection.Wait()
	}
	if !stopHook() {
		<-hookDone
	}
	abortErr := abortExecAttachment(connection)
	<-drainDone
	if terminal != nil {
		if abortErr != nil {
			observeExecFailure(ctx, abortErr)
		}
		return execCancelOutcome{code: code, err: cancelErr, terminal: terminal}
	}
	return execCancelOutcome{code: code, err: appendExecFailure(appendExecFailure(cancelErr, abortErr), ctx.Err())}
}

func appendExecFailure(previous, next error) error {
	if previous == nil {
		return next
	}
	if next == nil {
		return previous
	}
	return errors.Join(previous, next)
}

func abortExecAttachment(connection tunnel.ExecConn) error {
	if aborter, ok := connection.(interface{ Abort() error }); ok {
		return aborter.Abort()
	}
	return connection.Detach()
}

func retryableExecTransportFailure(err error) bool {
	// A dropped attachment carries both the owned transport marker and EOF.
	// EOF alone is unexpected to the general CLI classifier, but it must not
	// prevent resuming this already-started operation. The bounded peer policy
	// checks every joined cause, so independent I/O or authorization failures
	// still prevent recovery.
	// Validate the bounded cause tree before errors.Is, which can otherwise
	// recurse forever through a malformed cyclic error.
	return retryableInitialConnectFailure(err) && errors.Is(err, tunnel.ErrTransportLost)
}

func observeExecFailure(ctx context.Context, err error) {
	if err == nil || errorreport.HTTPAttemptObserved(err) {
		return
	}
	failure := classifyCommandFailure(err)
	if failure.kind == commandCanceled || failure.kind == commandInteractiveCanceled {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "pb", "exec", "command", "command_failed", err)
}

func remoteAbsolutePath(platform, workspaceRoot, path string) bool {
	return remotepath.AbsoluteForTarget(platform, workspaceRoot, path)
}

func finishExecResult(stdout, stderr io.Writer, operationID string, jsonOutput, sawTerminalEvent bool, code int, err error) error {
	if err == nil && !sawTerminalEvent {
		err = tunnel.ErrTransportLost
	}
	if err == nil {
		if code != 0 {
			return exitCodeError{code: code}
		}
		return nil
	}
	reserved := 255
	if execCancellationOnly(err) {
		if classifyCommandFailure(err).kind == commandDeadline {
			reserved = 124
		} else {
			reserved = 130
		}
	}
	if remote, ok := soleExecRemoteError(err); ok {
		switch remote.Code {
		case "exec_timeout":
			reserved = 124
		case "exec_canceled", "canceled":
			reserved = 130
		default:
			if publicExecErrorCode(remote.Code) == remote.Code {
				reserved = 125
			}
		}
	}
	if jsonOutput {
		if !sawTerminalEvent {
			if encodeErr := writeExecJSONFailure(stdout, operationID, "transport_lost", safeExecError(err), true, true); encodeErr != nil {
				return errors.Join(err, encodeErr)
			}
		}
		return execCommandFailure{cause: err, code: reserved, presented: true}
	}
	if _, writeErr := fmt.Fprintln(stderr, "pb:", safeExecError(err)); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return execCommandFailure{cause: err, code: reserved, presented: true}
}

// Only a fully traversed cancellation/deadline tree can claim a certain
// cancellation result. Bounds, cycles and unrelated failures fail closed.
func execInputClosedOnly(err error) bool {
	pending := []error{err}
	seen := make(map[error]bool)
	for count := 0; len(pending) > 0 && count < 32; count++ {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if current == io.EOF || current == io.ErrClosedPipe || current == net.ErrClosed || current == syscall.EPIPE {
			continue
		}
		if !value.Type().Comparable() || seen[current] {
			return false
		}
		seen[current] = true
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > 32-len(pending) {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			pending = append(pending, wrapped.Unwrap())
			continue
		}
		return false
	}
	return len(pending) == 0
}

func execCancellationOnly(err error) bool {
	pending := []error{err}
	seen := make(map[error]bool)
	for count := 0; len(pending) > 0 && count < 32; count++ {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if current == context.Canceled || current == context.DeadlineExceeded {
			continue
		}
		if remote, ok := current.(*tunnel.RemoteExecError); ok {
			if remote.Code == "exec_canceled" || remote.Code == "canceled" || remote.Code == "exec_timeout" {
				continue
			}
			return false
		}
		if !value.Type().Comparable() || seen[current] {
			return false
		}
		seen[current] = true
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > 32-len(pending) {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			pending = append(pending, wrapped.Unwrap())
			continue
		}
		return false
	}
	return len(pending) == 0
}

func soleExecRemoteError(err error) (*tunnel.RemoteExecError, bool) {
	for i := 0; err != nil && i < 32; i++ {
		if remote, ok := err.(*tunnel.RemoteExecError); ok {
			return remote, remote != nil
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return nil, false
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = wrapper.Unwrap()
	}
	return nil, false
}

func writeExecJSONFailure(writer io.Writer, operationID, errorCode, detail string, changed, uncertain bool) error {
	return json.NewEncoder(writer).Encode(struct {
		Version     string `json:"version"`
		Event       string `json:"event"`
		OperationID string `json:"operation_id"`
		ErrorCode   string `json:"error_code"`
		Detail      string `json:"detail,omitempty"`
		Changed     bool   `json:"changed"`
		Uncertain   bool   `json:"uncertain,omitempty"`
	}{Version: "paperboat.exec-event/v1", Event: "failed", OperationID: operationID, ErrorCode: errorCode, Detail: detail, Changed: changed, Uncertain: uncertain})
}

func publicExecErrorCode(code string) string {
	switch code {
	case "environment_unavailable", "", "exec_timeout", "exec_canceled", "exec_result_unavailable", "exec_failed", "exec_start_failed", "exec_start_uncertain", "exec_cancel_failed", "exec_wait_failed", "exec_already_running", "failed", "canceled":
		return code
	default:
		return "exec_failed"
	}
}

func execConnectInfo(machine api.UserMachine, descriptor api.ExecDescriptor, selectedWorkspace string) resolver.ConnectInfo {
	workspace := selectedWorkspace
	return resolver.ConnectInfo{
		Workspace:  workspace,
		TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineState: machine.State,
		MachineGeneration: uint64(machine.InstallationGeneration), TunnelTarget: descriptor.Endpoints.WSS,
		Terminal: &resolver.TerminalTarget{Protocol: "paperboat.exec.v1", EnvironmentID: descriptor.Environment.ID, QUICEndpoint: descriptor.Endpoints.QUIC, WSSEndpoint: descriptor.Endpoints.WSS, Auth: resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.Format(time.RFC3339Nano), Scopes: descriptor.Auth.Scopes, ResourceID: descriptor.Auth.AccessSessionID}, CWD: descriptor.Environment.Root},
	}
}

func writeExecEvent(c *command.Context, encoder *json.Encoder, event tunnel.ExecEvent, pty, jsonOutput bool) error {
	if jsonOutput {
		wire := struct {
			Version     string             `json:"version"`
			Event       string             `json:"event"`
			OperationID string             `json:"operation_id"`
			Stream      string             `json:"stream,omitempty"`
			Sequence    *uint64            `json:"sequence,omitempty"`
			Data        []byte             `json:"data,omitempty"`
			State       string             `json:"state,omitempty"`
			Result      *tunnel.ExecResult `json:"result,omitempty"`
			ErrorCode   string             `json:"error_code,omitempty"`
			Changed     *bool              `json:"changed,omitempty"`
			Uncertain   bool               `json:"uncertain,omitempty"`
			Warnings    []launchWarning    `json:"warnings,omitempty"`
		}{Version: "paperboat.exec-event/v1", Event: execEventName(event), OperationID: event.OperationID, Stream: event.Stream, Sequence: execEventSequence(event), Data: event.Data, State: event.State, Result: event.Result, ErrorCode: publicExecErrorCode(event.ErrorCode), Changed: execEventChanged(event), Warnings: launchWarnings(event.Warnings)}
		return encoder.Encode(wire)
	}
	if line := launchWarningLine(event.Warnings); line != "" {
		if _, err := fmt.Fprintln(c.ErrWriter, line); err != nil {
			return err
		}
	}
	if len(event.Data) == 0 {
		return nil
	}
	writer := c.Writer
	if event.Stream == "stderr" && !pty {
		writer = c.ErrWriter
	}
	_, err := writer.Write(event.Data)
	return err
}

type execConnectionRef struct {
	mu      sync.Mutex
	conn    tunnel.ExecConn
	changed chan struct{}
	eof     bool
}

func newExecConnectionRef(connection tunnel.ExecConn) *execConnectionRef {
	return &execConnectionRef{conn: connection, changed: make(chan struct{})}
}

func (r *execConnectionRef) Current() tunnel.ExecConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn
}

func (r *execConnectionRef) Set(connection tunnel.ExecConn) error {
	r.mu.Lock()
	r.conn = connection
	eof := r.eof
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	if eof {
		return connection.CloseWrite()
	}
	return nil
}

func (r *execConnectionRef) Clear(connection tunnel.ExecConn) {
	r.mu.Lock()
	if r.conn == connection {
		r.conn = nil
		close(r.changed)
		r.changed = make(chan struct{})
	}
	r.mu.Unlock()
}

func (r *execConnectionRef) Wait(ctx context.Context) (tunnel.ExecConn, error) {
	for {
		r.mu.Lock()
		connection, changed := r.conn, r.changed
		r.mu.Unlock()
		if connection != nil {
			return connection, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *execConnectionRef) CloseInput() error {
	r.mu.Lock()
	r.eof = true
	connection := r.conn
	r.mu.Unlock()
	if connection != nil {
		return connection.CloseWrite()
	}
	return nil
}

func forwardExecInput(ctx context.Context, reader io.Reader, connections *execConnectionRef) error {
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := readExecInput(ctx, reader, buffer)
		if n < 0 || n > len(buffer) {
			return io.ErrShortBuffer
		}
		if n > 0 {
			connection, err := connections.Wait(ctx)
			if err != nil {
				return err
			}
			remaining := buffer[:n]
			for len(remaining) > 0 {
				written, writeErr := connection.Write(remaining)
				if writeErr != nil || written <= 0 || written > len(remaining) {
					if writeErr == nil {
						writeErr = io.ErrShortWrite
					}
					connections.Clear(connection)
					abortErr := abortExecAttachment(connection)
					cause := appendExecFailure(writeErr, abortErr)
					if execInputClosedOnly(cause) || classifyCommandFailure(cause).kind == commandUnexpected {
						return cause
					}
					observeExecFailure(ctx, cause)
					break
				}
				remaining = remaining[written:]
			}
		}
		if n == 0 && readErr == nil {
			return io.ErrNoProgress
		}
		if readErr != nil {
			if readErr == io.EOF {
				return connections.CloseInput()
			}
			return readErr
		}
	}
}

func execEventName(event tunnel.ExecEvent) string {
	if event.Stream != "" {
		return event.Stream
	}
	if event.State == "started" {
		return "started"
	}
	if event.State == "exited" && event.Result != nil && event.Result.Signal != "" {
		return "signaled"
	}
	if event.State == "exited" || event.State == "signaled" {
		return event.State
	}
	return "failed"
}

func execEventChanged(event tunnel.ExecEvent) *bool {
	if event.Stream != "" || event.State == "started" {
		return nil
	}
	changed := event.State == "exited" || event.State == "signaled" || event.State == "canceled" || event.Result != nil
	return &changed
}

func execEventSequence(event tunnel.ExecEvent) *uint64 {
	if event.Stream == "" {
		return nil
	}
	sequence := event.Sequence
	return &sequence
}

func newExecOperationID() string {
	return "operation_" + uuid.NewString()
}

func localTerminalTarget() (string, string, error) {
	machineID, err := configuredMachineID()
	if err != nil {
		return "", "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("read current directory: %w", err)
	}
	return machineID, cwd, nil
}

func actionConnectTarget(c *command.Context, requested string) error {
	return actionConnectTargetInDirectory(c, requested, "")
}

func actionConnectTargetInDirectory(c *command.Context, requested, cwd string) error {
	machineRef := strings.TrimSpace(requested)
	if machineRef == "" {
		machineRef = c.Args().First()
	}
	if machineRef == "" && strings.TrimSpace(c.String("server")) != "" {
		cfg, err := config.Load(c.String("config"))
		if err != nil {
			return err
		}
		cfg.ServerURL, err = config.NormalizeServerURL(c.String("server"))
		if err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintln(c.Writer, cfg.Path())
		return nil
	}
	if c.Args().Len() > 2 {
		return localArgumentError("expected an environment and optional `new`")
	}
	if c.Args().Len() == 2 && c.Args().Get(1) != "new" {
		return localArgumentError("second argument must be `new`")
	}
	if c.Args().Len() == 2 && strings.TrimSpace(c.String("session")) != "" {
		return localArgumentError("`new` and --session cannot be used together")
	}
	if strings.TrimSpace(c.String("name")) != "" && strings.TrimSpace(c.String("session")) != "" {
		return localArgumentError("--name and --session cannot be used together")
	}

	ctx, cancelConnection := context.WithCancel(c.Context)
	defer cancelConnection()
	ownedCommand := *c
	ownedCommand.Context = ctx
	c = &ownedCommand
	d, err := buildDeps(c)
	if err != nil {
		return err
	}
	if strings.TrimSpace(d.cfg.ServerURL) == "" {
		return commandRejection{reason: commandRejectMissingServer}
	}
	cred, err := d.auth.Credential()
	if err != nil {
		return err
	}
	backend, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, d.auth)
	if err != nil {
		return err
	}
	if machineRef == "" {
		machineRef, err = defaultWorkspaceEnvironment(c.Context, c, backend, d.cfg)
		if err != nil {
			return err
		}
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return err
	}
	backend.SetSourceMachineID(sourceMachineID)
	statusConfig := d.cfg.StatusBar
	if value := strings.TrimSpace(c.String("status-bar")); value != "" {
		statusConfig.Mode = strings.ToLower(value)
	}
	if value := strings.TrimSpace(c.String("status-bar-fullscreen")); value != "" {
		statusConfig.Fullscreen = strings.ToLower(value)
	}
	if value := strings.TrimSpace(c.String("status-bar-theme")); value != "" {
		statusConfig.Theme = strings.ToLower(value)
	}
	bar := statusbar.New(statusbar.Options{
		Mode:          statusConfig.Mode,
		Fullscreen:    statusConfig.Fullscreen,
		Theme:         statusConfig.Theme,
		Privacy:       statusConfig.Privacy,
		TerminalTitle: statusConfig.TerminalTitle,
		Colors: statusbar.Colors{
			Foreground: statusConfig.Colors.Foreground,
			Background: statusConfig.Colors.Background,
			Accent:     statusConfig.Colors.Accent,
			Warning:    statusConfig.Colors.Warning,
			Error:      statusConfig.Colors.Error,
		},
		NoticeDuration: time.Duration(d.cfg.StatusBar.NoticeSeconds) * time.Second,
		Layout: statusbar.Layout{
			Left:   d.cfg.StatusBar.Left,
			Center: d.cfg.StatusBar.Center,
			Right:  d.cfg.StatusBar.Right,
		},
	})
	if c.Bool("debug") {
		bar.SetDebugVersions(buildinfo.Version, "")
	}
	defer func() { _ = bar.Close() }()
	useStatusBar := bar.Enabled()
	observeTransferFailure := func(cause error) {
		reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", cause)
		if reference == "" {
			return
		}
		if useStatusBar {
			bar.FailureFor("file_transfer", "File transfer unavailable · "+reference)
		} else {
			fmt.Fprintf(os.Stderr, "File transfer is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: %s.\n", reference)
		}
	}
	var launchWarned sync.Once
	recordLaunchWarning := func(codes []string) {
		line := launchWarningLine(codes)
		if line == "" {
			return
		}
		launchWarned.Do(func() {
			if useStatusBar {
				bar.Notice("ENV not applied: delivery pending (pb env vault unlock)")
			} else {
				fmt.Fprintln(os.Stderr, line)
			}
		})
	}
	var closeTelemetry func()
	d.telemetry, closeTelemetry = connectTelemetry(d.cfg, os.Stderr)
	defer closeTelemetry()

	sessionName := c.String("name")
	sessionRef := c.String("session")
	target, resolvedMachine, err := resolveEnvironmentTargetWithMachine(c.Context, backend, machineRef)
	if err != nil {
		return err
	}
	var createSession *resolver.TerminalSessionCreate
	terminalSessionID := ""
	terminalSessionName := ""
	if strings.TrimSpace(sessionRef) != "" {
		session, err := resolveTerminalSession(c.Context, backend, target, sessionRef)
		if err != nil {
			return friendlyCommandError(err)
		}
		terminalSessionID = session.ID
		terminalSessionName = session.Name
	} else if err := validateSessionNameOptional(sessionName); err != nil {
		return err
	} else {
		// Create the durable session and fetch its connection descriptor in
		// one round trip; the idempotency key binds descriptor retries.
		createSession = &resolver.TerminalSessionCreate{Name: sessionName, IdempotencyKey: newIdempotencyKey(), CWD: cwd}
		terminalSessionName = sessionName
	}
	bar.SetIdentity(machineRef, terminalSessionName)
	newResolver := func() (*resolver.APIResolver, error) {
		client, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, d.auth)
		if err != nil {
			return nil, err
		}
		client.SetSourceMachineID(sourceMachineID)
		apiResolver := resolver.NewAPIResolver(client, d.cfg)
		apiResolver.Telemetry = d.telemetry
		return apiResolver, nil
	}
	d.resolver, err = newResolver()
	if err != nil {
		return err
	}
	if apiResolver, ok := d.resolver.(*resolver.APIResolver); ok {
		apiResolver.Progress = func(status, reason string, retryAfter time.Duration) {
			if useStatusBar {
				bar.SetConnection("connecting")
				bar.Loading("Preparing connection")
				return
			}
			fmt.Fprintf(os.Stderr, "Connecting: %s (%s), retrying in %s...\n", status, reason, retryAfter.Round(time.Second))
		}
	}
	remoteSize := func() (uint16, uint16) {
		if useStatusBar {
			if cols, rows := bar.RemoteSize(); cols > 0 && rows > 0 {
				return cols, rows
			}
		}
		return localTerminalSize()
	}

	var info resolver.ConnectInfo
	var conn tunnel.Conn
	var lastTerminalSequence atomic.Int64
	recordTerminalSequence := func(sequence int) {
		for {
			current := lastTerminalSequence.Load()
			if int64(sequence) <= current || lastTerminalSequence.CompareAndSwap(current, int64(sequence)) {
				return
			}
		}
	}
	recordReplayGap := func(requested, earliest, _ uint64) {
		missing := uint64(0)
		if earliest > requested {
			missing = earliest - requested
		}
		event := telemetry.Event{Name: "terminal.replay_gap", At: time.Now(), Outcome: "recovered", MachineID: info.MachineID, Count: int64(min(missing, math.MaxInt64))}
		if info.Terminal != nil {
			event.EnvironmentID = info.Terminal.EnvironmentID
		}
		if event.Validate() == nil {
			d.telemetry.Record(event)
		}
	}
	var transferClient *filetransfer.NativeClient
	var transferLease *localapi.FileTransferLease
	for attempt := 0; attempt <= d.cfg.Connect.DialRetries; attempt++ {
		resolveRequest := resolver.ConnectRequest{Machine: machineRef, Credential: cred, TerminalSessionID: terminalSessionID, CreateTerminalSession: createSession}
		if target.kind == environmentUserMachine && resolvedMachine.ID != "" && resolvedMachine.InstallationGeneration > 0 {
			resolveRequest.ResolvedMachine = &resolver.ResolvedMachine{ID: resolvedMachine.ID, Name: resolvedMachine.Alias, State: resolvedMachine.State, Generation: uint64(resolvedMachine.InstallationGeneration)}
		}
		info, err = d.resolver.Resolve(ctx, resolveRequest)
		if err == nil {
			if createSession != nil && info.TerminalSession != nil {
				created := info.TerminalSession
				if terminalSessionID == "" {
					terminalSessionID = created.ID
				}
				if created.Name != "" && created.Name != terminalSessionName {
					terminalSessionName = created.Name
					bar.SetIdentity(machineRef, terminalSessionName)
				}
				if created.EvictedSession != nil {
					fmt.Fprintf(os.Stderr, "Session limit reached; removed least-recent session %q (%s).\n", created.EvictedSession.Name, created.EvictedSession.State)
				}
			}
			if info.Terminal != nil {
				info.Terminal.Debug = c.Bool("debug")
				info.Terminal.RestartIfNotRunning = true
				info.Terminal.ReplayHistory = true
				info.Terminal.SequenceSink = recordTerminalSequence
				info.Terminal.ReplayGapSink = recordReplayGap
				info.Terminal.LaunchWarningSink = recordLaunchWarning
				info.Terminal.Env = forwardedTerminalEnv(config.TerminalEnv)
				info.Terminal.Cols, info.Terminal.Rows = remoteSize()
			}
			if transferLease != nil {
				observeTransferFailure(transferLease.Close())
				transferLease = nil
			}
			transferClient = nil
			if info.FileTransfer != nil && d.peerLocal != nil {
				var transferErr error
				transferClient, transferLease, transferErr = nativeFileTransferClient(ctx, d.peerLocal, info, newIdempotencyKey())
				observeTransferFailure(transferErr)
			}
			// The file-transfer policy check runs concurrently with the
			// transport dial. Paste availability is decided before any input
			// is accepted, but the health check round trip never delays the
			// shell becoming interactive.
			verifyDone := make(chan error, 1)
			var clientToVerify *filetransfer.Client
			if transferClient != nil {
				clientToVerify = transferClient.Client
			}
			if clientToVerify != nil {
				policyToVerify := descriptorFileTransferPolicy(info.FileTransfer)
				go func() { verifyDone <- clientToVerify.VerifyPolicy(ctx, policyToVerify) }()
			} else {
				verifyDone <- nil
			}
			conn, err = d.tunnel.Dial(ctx, info)
			if policyErr := <-verifyDone; policyErr != nil {
				observeTransferFailure(policyErr)
				transferClient = nil
				if transferLease != nil {
					observeTransferFailure(transferLease.Close())
					transferLease = nil
				}
				if useStatusBar {
					bar.FailureFor("file_transfer", "File transfer unavailable")
				} else {
					fmt.Fprintln(os.Stderr, "File transfer is unavailable for this connection. Terminal access will continue.")
				}
			}
		}
		if err == nil {
			break
		}
		if errors.Is(err, api.ErrUnauthenticated) {
			return err
		}
		if attempt == d.cfg.Connect.DialRetries || !retryableInitialConnectError(err) {
			break
		}
		if useStatusBar {
			bar.SetConnection("reconnecting")
			bar.Loading("Retrying connection")
		} else {
			fmt.Fprintf(os.Stderr, "Connection attempt %d failed; refreshing the descriptor in %ds...\n", attempt+1, d.cfg.Connect.DialRetrySeconds)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(time.Duration(d.cfg.Connect.DialRetrySeconds) * time.Second):
		}
	}
	if err != nil {
		if useStatusBar {
			bar.SetConnection("failed")
			bar.FailureFor("connection", "Connection failed")
		}

		return fmt.Errorf("connect to %q: %w", machineRef, err)
	}
	if c.Bool("debug") {
		bar.SetDebugVersions(buildinfo.Version, tunnel.TerminalRuntimeVersion(conn))
	}
	defer func() {
		if transferLease != nil {
			observeTransferFailure(transferLease.Close())
		}
	}()
	if err := rememberWorkspaceEnvironment(c, d.cfg, info.MachineID); err != nil {
		closeErr := conn.Close()
		return fmt.Errorf("remember connected environment: %w", errors.Join(err, closeErr))
	}
	if useStatusBar {
		bar.SetConnection("connected")
		bar.Notice("Connected")
		bar.ClearRemoteViewport()
	} else if term.IsTerminal(int(os.Stdout.Fd())) {
		_, _ = fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	}
	cols, rows := remoteSize()
	if err := initializeTerminalDisplay(conn, info.Terminal, rows, cols); err != nil {
		return fmt.Errorf("refresh terminal display: %w", errors.Join(err, conn.Close()))
	}
	if useStatusBar && d.peerLocal != nil && info.TargetKind == "machine" {
		pathDone := make(chan struct{})
		go func() { defer close(pathDone); watchMachineTransportPath(ctx, d.peerLocal, info.MachineID, bar) }()
		defer func() { cancelConnection(); <-pathDone }()
	}
	var inboxWorker terminalInboxWorker
	stopInbox := inboxWorker.stop
	startInbox := func(client *filetransfer.Client, sessionID string) {
		stopInbox()
		if client == nil || sessionID == "" {
			return
		}
		notify := func(message string) {
			if useStatusBar {
				bar.Notice(message)
				return
			}
			fmt.Fprintln(os.Stderr, message)
		}
		inboxPath, pathErr := configuredInboxPath()
		if pathErr != nil {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", pathErr); reference != "" {
				notify("File delivery is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
			return
		}
		machineID, machineErr := configuredMachineID()
		if machineErr != nil {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", machineErr); reference != "" {
				notify("File delivery is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
			return
		}
		inboxConfig := inbox.Config{Client: client, MachineID: machineID, SessionID: sessionID, Path: inboxPath, Notify: notify}
		receiver, inboxErr := inbox.New(inboxConfig)
		if inboxErr != nil {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", inboxErr); reference != "" {
				notify("File delivery is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
			return
		}
		inboxWorker.start(ctx, receiver.Run, func(err error) {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", err); reference != "" {
				notify("File delivery stopped. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
		})
	}
	defer stopInbox()
	var pastePolicy *paste.Policy
	conn = tunnel.NewObservedReconnectingConn(ctx, conn, d.cfg.Connect.DialRetries, time.Duration(d.cfg.Connect.DialRetrySeconds)*time.Second, func(reconnectCtx context.Context) (tunnel.Conn, error) {
		freshCred, credErr := d.auth.Credential()
		if credErr != nil {
			return nil, credErr
		}
		freshResolver, resolverErr := newResolver()
		if resolverErr != nil {
			return nil, resolverErr
		}
		reconnectRequest := resolver.ConnectRequest{Machine: info.MachineID, Credential: freshCred, TerminalSessionID: terminalSessionID}
		if target.kind == environmentUserMachine && info.MachineGeneration > 0 {
			reconnectRequest.ResolvedMachine = &resolver.ResolvedMachine{ID: info.MachineID, Name: info.Machine, State: info.MachineState, Generation: info.MachineGeneration}
		}
		freshInfo, resolveErr := freshResolver.Resolve(reconnectCtx, reconnectRequest)
		if resolveErr != nil {
			var apiErr *api.APIError
			if errors.As(resolveErr, &apiErr) && apiErr.Code == "machine_revoked" {
				resolveErr = tunnel.StopReconnect(resolveErr)
			}
			return nil, resolveErr
		}
		if freshInfo.Terminal != nil {
			freshInfo.Terminal.Debug = c.Bool("debug")
			freshInfo.Terminal.RestartIfNotRunning = false
			freshInfo.Terminal.ReplayHistory = false
			freshInfo.Terminal.AfterSequence = int(lastTerminalSequence.Load())
			freshInfo.Terminal.SequenceSink = recordTerminalSequence
			freshInfo.Terminal.ReplayGapSink = recordReplayGap
			freshInfo.Terminal.Env = forwardedTerminalEnv(config.TerminalEnv)
			freshInfo.Terminal.Cols, freshInfo.Terminal.Rows = remoteSize()
		}
		freshConn, dialErr := d.tunnel.Dial(reconnectCtx, freshInfo)
		if dialErr != nil {
			if !tunnel.FallbackEligible(dialErr) {
				return nil, tunnel.StopReconnect(dialErr)
			}
			return nil, dialErr
		}
		if c.Bool("debug") {
			bar.SetDebugVersions(buildinfo.Version, tunnel.TerminalRuntimeVersion(freshConn))
		}
		if pastePolicy != nil {
			var freshTransfer *filetransfer.NativeClient
			var freshLease *localapi.FileTransferLease
			if freshInfo.FileTransfer != nil && d.peerLocal != nil {
				var transferErr error
				freshTransfer, freshLease, transferErr = nativeFileTransferClient(reconnectCtx, d.peerLocal, freshInfo, newIdempotencyKey())
				observeTransferFailure(transferErr)
				if freshTransfer != nil {
					if policyErr := freshTransfer.VerifyPolicy(reconnectCtx, descriptorFileTransferPolicy(freshInfo.FileTransfer)); policyErr != nil {
						observeTransferFailure(policyErr)
						observeTransferFailure(freshLease.Close())
						freshTransfer, freshLease = nil, nil
					}
				}
			}
			stopInbox()
			oldLease := transferLease
			transferClient, transferLease = freshTransfer, freshLease
			var freshUploader paste.BatchUploader = freshTransfer
			pastePolicy.Update(freshUploader, freshInfo.Terminal.SessionID, fileTransferLimits(freshInfo.FileTransfer))
			if oldLease != nil {
				observeTransferFailure(oldLease.Close())
			}
			if freshTransfer != nil {
				startInbox(freshTransfer.Client, freshInfo.Terminal.SessionID)
			} else {
				startInbox(nil, freshInfo.Terminal.SessionID)
			}
		}
		return freshConn, nil
	}, d.telemetry, nil, tunnel.TelemetryContext{MachineID: info.MachineID, EnvironmentID: info.Terminal.EnvironmentID}, tunnel.WithReconnectingOutput(
		d.cfg.Connect.TerminalOutputQueueChunks,
		time.Duration(d.cfg.Connect.TerminalOutputBatchMilliseconds)*time.Millisecond,
	), tunnel.WithReconnectObserver(func(event tunnel.ReconnectEvent) {
		if event == tunnel.ReconnectRecovered {
			bar.ResetRemoteState()
			cols, rows := remoteSize()
			_ = forceTerminalRedraw(conn, rows, cols)
		}
		if !useStatusBar {
			return
		}
		switch event {
		case tunnel.ReconnectStarted:
			bar.SetConnection("reconnecting")
			bar.Loading("Reconnecting")
		case tunnel.ReconnectRecovered:
			bar.RecoverFailureFor("connection")
			bar.SetConnection("connected")
			bar.Notice("Reconnected")
		case tunnel.ReconnectFailed:
			bar.SetConnection("failed")
			bar.FailureFor("connection", "Connection lost")
		}
	}))

	// Wrap remote input with the file-paste interceptor.
	var pasteUploader paste.BatchUploader = transferClient
	pastePolicy = paste.NewPolicy(pasteUploader, info.Terminal.SessionID, fileTransferLimits(info.FileTransfer))
	interceptor := paste.NewWithPolicy(conn, pastePolicy,
		paste.WithDirectInput(),
		paste.WithNotifier(statusNotifier(useStatusBar)),
		paste.WithFailureReporter(func(cause error) string {
			reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", cause)
			if useStatusBar && reference != "" {
				bar.FailureFor("upload", "File paste failed · "+reference)
			}
			return reference
		}),
		paste.WithLifecycle(func(event paste.LifecycleEvent) {
			if !useStatusBar {
				return
			}
			switch event {
			case paste.FileDetected:
				bar.LoadingPersistent("Preparing file")
			case paste.FileUploading:
				bar.LoadingPersistent("Uploading file")
			case paste.FileComplete:
				bar.RecoverFailureFor("upload")
				bar.Notice("File uploaded")
			case paste.FileFailed:
				bar.FailureFor("upload", "File upload failed; pasted original")
			}
		}),
		paste.WithWatchDirs(expandDirs(d.cfg.FilePaste.WatchDirs)),
		paste.WithTempFilePatterns(d.cfg.FilePaste.TempFilePatterns),
		paste.WithMaxQueuedBytes(d.cfg.FilePaste.MaxQueuedInputBytes),
		paste.WithPartialFlushDelay(time.Duration(d.cfg.Connect.InputPartialFlushMilliseconds)*time.Millisecond),
	)
	if transferClient != nil {
		startInbox(transferClient.Client, info.Terminal.SessionID)
	}

	runOptions := []session.RunOption{
		session.WithOutputBufferBytes(d.cfg.Connect.TerminalOutputBufferBytes),
		session.WithBracketedPaste(),
	}
	if useStatusBar {
		bar.SetViewportChanged(func(cols, rows uint16) {
			if cols > 0 && rows > 0 {
				_ = conn.Resize(rows, cols)
			}
		})
		runOptions = append(runOptions, session.WithOutput(bar), session.WithRemoteSize(remoteSize))
	}
	code, err := session.Run(ctx, conn, interceptor, runOptions...)
	if err == nil {
		if useStatusBar {
			bar.ClearForExit()
		} else if term.IsTerminal(int(os.Stdout.Fd())) {
			_, _ = fmt.Fprint(os.Stdout, "\x1b[r\x1b[2J\x1b[H")
		}
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCodeError{code: code}
	}
	return nil
}

// watchMachineTransportPath keeps the session marker in sync with the observed
// machine path. A mixed machine snapshot cannot identify this session's path.
type machineTransportWatcher interface {
	Snapshot(context.Context) (localapi.Snapshot, error)
	Watch(context.Context, uint64) (<-chan localapi.Snapshot, <-chan error)
}

func watchMachineTransportPath(ctx context.Context, client machineTransportWatcher, machineID string, bar *statusbar.Bar) {
	if client == nil || bar == nil || machineID == "" {
		return
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		reportTerminalAuxFailure(ctx, "local_gateway", "native_private_failed", err)
		return
	}
	applyMachineTransportPath(snapshot, machineID, bar)
	updates, watchErr := client.Watch(ctx, snapshot.Generation)
	defer func() {
		// Both channels close after the API's stream reader and body cleanup.
		// Consume its final error even when cancellation or update EOF wins
		// the select; neither event proves there was no independent I/O fault.
		for err := range watchErr {
			reportTerminalAuxFailure(ctx, "local_gateway", "native_private_failed", err)
		}
		for range updates {
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			applyMachineTransportPath(snapshot, machineID, bar)
		case err := <-watchErr:
			if reportTerminalAuxFailure(ctx, "local_gateway", "native_private_failed", err) != "" && ctx.Err() == nil {
				bar.SetTransport("")
				bar.FailureFor("transport_status", "Connection path status unavailable")
			}
			return
		}
	}
}

func applyMachineTransportPath(snapshot localapi.Snapshot, machineID string, bar *statusbar.Bar) {
	for _, machine := range snapshot.Machines {
		if machine.ID == machineID && (machine.SelectedPath == "direct" || machine.SelectedPath == "relay" || machine.SelectedPath == "wss") {
			bar.SetTransport(machine.SelectedPath)
			return
		}
	}
}

func initializeTerminalDisplay(conn tunnel.Conn, target *resolver.TerminalTarget, rows, cols uint16) error {
	// Shared attachments replay pre-join output and must not mutate the
	// owner's geometry merely to display it. Personal attachments join at the
	// live boundary, so an already-idle shell needs a fresh application redraw.
	if target == nil || target.Shared() {
		return nil
	}
	return forceTerminalRedraw(conn, rows, cols)
}

func forceTerminalRedraw(conn tunnel.Conn, rows, cols uint16) error {
	probeRows, probeCols := rows, cols
	if probeRows > 1 {
		probeRows--
	} else if probeCols > 1 {
		probeCols--
	}
	var probeErr error
	if probeRows != rows || probeCols != cols {
		probeErr = conn.Resize(probeRows, probeCols)
	}
	// Restore the requested geometry even if delivery of the probe failed:
	// its remote effect can be ambiguous after a connection failure.
	return errors.Join(probeErr, conn.Resize(rows, cols))
}

func chooseIndex(ctx context.Context, title, subtitle string, count int, item func(int) selector.Item) (int, error) {
	items := make([]selector.Item, count)
	indexes := make(map[string]int, count)
	for index := range count {
		items[index] = item(index)
		if items[index].ID == "" {
			items[index].ID = strconv.Itoa(index)
		}
		indexes[items[index].ID] = index
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Title: title, Subtitle: subtitle, Items: items, Stdin: os.Stdin, Output: os.Stderr})
	return indexes[selected.ID], err
}

func statusNotifier(enabled bool) io.Writer {
	if enabled {
		return io.Discard
	}
	return os.Stderr
}

func connectTelemetry(cfg *config.Config, warnings io.Writer) (telemetry.Sink, func()) {
	if path := cfg.TelemetryPath(); path != "" {
		fileSink, err := telemetry.NewJSONFileSinkWithLimit(path, cfg.Diagnostics.MaxEventLogBytes)
		if err == nil {
			return fileSink, func() {
				if closeErr := fileSink.Close(); closeErr != nil {
					fault := errorreport.Current().ObserveFailure(context.Background(), "pb", "diagnostic", "diagnostic_storage", "diagnostic_storage_unavailable", closeErr)
					fmt.Fprintf(warnings, "warning: local event log could not be saved; check its storage permissions. Support reference: %s.\n", fault.SupportReference)
				}
			}
		}
		fault := errorreport.Current().ObserveFailure(context.Background(), "pb", "diagnostic", "diagnostic_storage", "diagnostic_storage_unavailable", err)
		fmt.Fprintf(warnings, "warning: local event log unavailable; check its storage permissions. Support reference: %s.\n", fault.SupportReference)
	}
	return telemetry.NopSink{}, func() {}
}

func retryableInitialConnectError(err error) bool {
	return retryableInitialConnectFailure(err)
}

func friendlyAPIError(err error) string {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch apiErr.Code {
	case "team_subscription_required":
		return "An active team subscription is required. " + teamSubscriptionRecovery + " No resource was created."
	case "credits_exhausted":
		return "credits are exhausted; top up credits in Paperboat, then retry"
	case "entitlement_lost", "payment_required":
		return "your Paperboat plan is inactive; restore billing access, then retry"
	case "tunnel_unavailable":
		return "the secure tunnel is not available yet; retry in a moment"
	case "machine_not_ready":
		return "the machine is not ready yet; retry in a moment"
	case "machine_offline":
		return "the machine is offline; start or repair its Paperboat connector, then retry"
	case "machine_revoked":
		return "this machine has been disconnected or revoked; repair or reconnect it in the Paperboat dashboard"
	}
	return ""
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

func parseAuthExpiry(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

// terminalEnvKeyPattern mirrors the terminal RPC environment schema; an invalid
// key or oversized value would reject the whole attach, so filter locally.
var terminalEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const maxTerminalEnvValueChars = 8_192

// forwardedTerminalEnv snapshots the configured local environment variables so
// the remote PTY spawns with the client terminal's capabilities.
func forwardedTerminalEnv(keys []string) map[string]string {
	env := make(map[string]string, len(keys))
	for _, key := range keys {
		if !terminalEnvKeyPattern.MatchString(key) {
			continue
		}
		value, ok := os.LookupEnv(key)
		if !ok || value == "" || len(value) > maxTerminalEnvValueChars {
			continue
		}
		env[key] = value
	}
	return env
}

// localTerminalSize returns the current terminal geometry, clamped to the
// terminal RPC schema bounds, or zeros when stdout is not a terminal.
func localTerminalSize() (cols, rows uint16) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0
	}
	if w > 1000 {
		w = 1000
	}
	if h > 500 {
		h = 500
	}
	return uint16(w), uint16(h)
}

func expandDirs(dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if home != "" && len(d) >= 1 && d[0] == '~' {
			d = home + d[1:]
		}
		out = append(out, d)
	}
	return out
}

// configuredSSHTargetPort is the SSH target port `pb config set ssh-target-port` stored in this
// machine's registration. It reports false on a machine that was never enrolled; it never creates
// any state.
func configuredSSHTargetPort() (uint16, bool) {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	if stateRoot == "" {
		var err error
		if stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv); err != nil {
			return 0, false
		}
	}
	if info, err := os.Stat(stateRoot); err != nil || !info.IsDir() {
		return 0, false
	}
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return 0, false
	}
	registration, err := store.Registration()
	if err != nil || registration.SSHPort == 0 {
		return 0, false
	}
	return registration.SSHPort, true
}

func configSetSSHTargetPort(c *command.Context, port uint16) error {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return err
		}
	}
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil || registration.InstallationGeneration < 1 {
		return commandPreparationFailure{step: prepareMachineRegistration, cause: err}
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	generation := uint64(registration.InstallationGeneration)
	current, err := client.ManagedSSHTarget(c.Context, registration.MachineID, generation)
	if err != nil {
		return err
	}
	if current.Port != port {
		if _, err := client.UpdateManagedSSHTargetPort(c.Context, registration.MachineID, generation, current.ReconciliationVersion, port, newIdempotencyKey()); err != nil {
			return err
		}
	}
	registration.SSHPort = port
	registration.UpdatedAt = time.Now().UTC()
	if err := store.SaveRegistration(registration); err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"key": "ssh-target-port", "port": port, "updated": true})
	}
	fmt.Fprintf(c.Writer, "SSH target port set to %d\n", port)
	return nil
}

func statusBarConfigCommand() *cobra.Command {
	status := &cobra.Command{
		Use:   "status-bar",
		Short: "Configure the interactive terminal status bar",
		Args:  commandArgs(cobra.NoArgs),
		RunE:  func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	show := &cobra.Command{Use: "show", Short: "Show the effective status-bar configuration", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(cfg.StatusBar)
		}
		return printStatusBarConfig(command.OutOrStdout(), cfg.StatusBar)
	}}
	show.Flags().Bool("json", false, "print JSON")
	set := &cobra.Command{Use: "set <key> <value>", Short: "Set a status-bar preference", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(command *cobra.Command, args []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		if err := setStatusBarValue(&cfg.StatusBar, args[0], args[1]); err != nil {
			return invocationError(err)
		}
		if err := cfg.Validate(); err != nil {
			return invocationError(err)
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"path": cfg.Path(), "status_bar": cfg.StatusBar})
		}
		fmt.Fprintln(command.OutOrStdout(), cfg.Path())
		return nil
	}}
	set.Flags().Bool("json", false, "print JSON")
	reset := &cobra.Command{Use: "reset", Short: "Restore status-bar defaults", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		cfg.StatusBar = config.DefaultStatusBarConfig()
		if err := cfg.Save(); err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"path": cfg.Path(), "status_bar": cfg.StatusBar})
		}
		fmt.Fprintln(command.OutOrStdout(), cfg.Path())
		return nil
	}}
	reset.Flags().Bool("json", false, "print JSON")
	preview := &cobra.Command{Use: "preview", Short: "Preview the configured status bar", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		width, _ := command.Flags().GetInt("width")
		if width == 0 {
			width = 80
			if term.IsTerminal(int(os.Stdout.Fd())) {
				if detected, _, sizeErr := term.GetSize(int(os.Stdout.Fd())); sizeErr == nil {
					width = detected
				}
			}
		}
		bar := newConfiguredStatusBar(cfg.StatusBar, statusbar.ModeOff)
		defer bar.Close()
		bar.SetIdentity("paperboat", "default")
		bar.SetUsage("100", "12 GB")
		bar.SetConfigSync("healthy")
		bar.SetConnection("connected")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		bar.SetConnection("reconnecting")
		bar.Loading("Reconnecting")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		bar.SetConnection("failed")
		bar.FailureFor("preview", "Connection lost")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		return nil
	}}
	preview.Flags().Int("width", 0, "preview width (20-500 columns)")
	preview.PreRunE = func(command *cobra.Command, _ []string) error {
		width, _ := command.Flags().GetInt("width")
		if width != 0 && (width < 20 || width > 500) {
			return invocationError(errors.New("--width must be between 20 and 500"))
		}
		return nil
	}
	status.AddCommand(show, set, reset, preview)
	return status
}

func configPathFlag(command *cobra.Command) string {
	value, _ := command.Flags().GetString("config")
	return value
}

func printStatusBarConfig(writer io.Writer, value config.StatusBarConfig) error {
	_, err := fmt.Fprintf(writer, "mode: %s\nfullscreen: %s\ntheme: %s\nprivacy: %t\nterminal_title: %t\nnotice_seconds: %d\nleft: %s\ncenter: %s\nright: %s\nforeground: %s\nbackground: %s\naccent: %s\nwarning: %s\nerror: %s\n",
		value.Mode, value.Fullscreen, value.Theme, value.Privacy, value.TerminalTitle, value.NoticeSeconds,
		formatWidgetList(value.Left), formatWidgetList(value.Center), formatWidgetList(value.Right),
		inheritedColor(value.Colors.Foreground), inheritedColor(value.Colors.Background), inheritedColor(value.Colors.Accent), inheritedColor(value.Colors.Warning), inheritedColor(value.Colors.Error))
	return err
}

func inheritedColor(value string) string {
	if value == "" {
		return "inherit"
	}
	return value
}

func formatWidgetList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}

func setStatusBarValue(value *config.StatusBarConfig, key, raw string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	raw = strings.TrimSpace(raw)
	switch key {
	case "mode":
		value.Mode = strings.ToLower(raw)
	case "fullscreen":
		value.Fullscreen = strings.ToLower(raw)
	case "theme":
		value.Theme = strings.ToLower(raw)
	case "privacy", "terminal-title":
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%s must be true or false", key)
		}
		if key == "privacy" {
			value.Privacy = parsed
		} else {
			value.TerminalTitle = parsed
		}
	case "notice-seconds":
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 60 {
			return localArgumentError("notice-seconds must be between 1 and 60")
		}
		value.NoticeSeconds = parsed
	case "left", "center", "right":
		widgets := parseWidgetList(raw)
		switch key {
		case "left":
			value.Left = widgets
		case "center":
			value.Center = widgets
		case "right":
			value.Right = widgets
		}
	case "foreground", "background", "accent", "warning", "error":
		switch key {
		case "foreground":
			value.Colors.Foreground = raw
		case "background":
			value.Colors.Background = raw
		case "accent":
			value.Colors.Accent = raw
		case "warning":
			value.Colors.Warning = raw
		case "error":
			value.Colors.Error = raw
		}
	default:
		return fmt.Errorf("unknown status-bar key %q", key)
	}
	return nil
}

func parseWidgetList(raw string) []string {
	if raw == "" || strings.EqualFold(raw, "none") {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, strings.ToLower(strings.TrimSpace(part)))
	}
	return result
}

func newConfiguredStatusBar(value config.StatusBarConfig, mode string) *statusbar.Bar {
	return statusbar.New(statusbar.Options{
		Mode: mode, Fullscreen: value.Fullscreen, Theme: value.Theme, Privacy: value.Privacy, TerminalTitle: false,
		Colors:         statusbar.Colors{Foreground: value.Colors.Foreground, Background: value.Colors.Background, Accent: value.Colors.Accent, Warning: value.Colors.Warning, Error: value.Colors.Error},
		NoticeDuration: time.Duration(value.NoticeSeconds) * time.Second,
		Layout:         statusbar.Layout{Left: value.Left, Center: value.Center, Right: value.Right},
	})
}

func configUnassign(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	machineID := target.id
	assignment, err := client.ConfigAssignment(c.Context, machineID)
	if err != nil {
		return friendlyCommandError(err)
	}
	if err := confirmContextMutation(c, fmt.Sprintf("config-unassign:%s:%d", target.id, assignment.Version), fmt.Sprintf("Remove the config repository assignment from %s (%s)? The repository and its content will remain.", target.name, target.id)); err != nil {
		return err
	}
	if err := manageConfigService(c.Context, machineID, false); err != nil {
		return fmt.Errorf("stop config sync service: %w", err)
	}
	if err := client.UnassignConfig(c.Context, machineID, assignment.Version); err != nil {
		repairErr := manageConfigService(c.Context, machineID, true)
		return errors.Join(friendlyCommandError(err), repairErr)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "state": "unassigned", "outcome": "confirmed"})
	}
	fmt.Fprintf(c.Writer, "Removed config assignment from %s.\n", target.name)
	return nil
}

func configApproveRevision(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, target.id)
	if err != nil || len(items) != 1 {
		return errors.Join(errors.New("configuration revision is unavailable"), err)
	}
	item := items[0]
	if item.RemoteRevision == "" || item.AssignmentVersion < 1 {
		return errors.New("no reviewed configuration revision is pending")
	}
	if len(item.Review) > 0 && !c.Bool("json") {
		fmt.Fprintf(c.Writer, "Revision %s changes:\n", item.RemoteRevision)
		for _, change := range item.Review {
			fmt.Fprintf(c.Writer, "  %s  %s\n", change.Reason, change.Path)
		}
	}
	approved, err := client.ApproveConfigPullRevision(c.Context, target.id, item.RemoteRevision, item.AssignmentVersion)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "revision": item.RemoteRevision, "assignment": approved})
	}
	fmt.Fprintf(c.Writer, "Approved configuration revision %s for %s (assignment version %d).\n", item.RemoteRevision, target.name, approved.Version)
	return nil
}

func configTeamDefaultSet(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	repositories, err := client.ListConfigRepositories(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	repository, err := resolveConfigRepository(repositories, c.Args().Get(1))
	if err != nil {
		return err
	}
	version := int64(0)
	if current, getErr := client.ConfigTeamDefault(c.Context, c.Args().First()); getErr == nil {
		version = current.Version
	} else if !api.IsNotFound(getErr) {
		return friendlyCommandError(getErr)
	}
	item, err := client.SetConfigTeamDefault(c.Context, c.Args().First(), repository.ID, version)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, item)
	}
	fmt.Fprintf(c.Writer, "Team %s default pull repository is %s (version %d).\n", item.TeamID, item.DisplayName, item.Version)
	return nil
}

func configTeamDefaultAdopt(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	item, err := client.ConfigTeamDefault(c.Context, c.Args().First())
	if err != nil {
		return friendlyCommandError(err)
	}
	adoption, err := client.AdoptConfigTeamDefault(c.Context, item.TeamID, item.Version)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, adoption)
	}
	fmt.Fprintf(c.Writer, "Adopted %s from team %s at version %d; personal machine assignments still take precedence.\n", adoption.DisplayName, adoption.TeamID, adoption.AdoptedVersion)
	return nil
}

func configTeamDefaultUnadopt(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	if err := client.UnadoptConfigTeamDefault(c.Context); err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"adopted": false})
	}
	fmt.Fprintln(c.Writer, "Stopped inheriting the team configuration default; personal assignments were unchanged.")
	return nil
}

func manageConfigService(ctx context.Context, machineID string, install bool) error {
	stateRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "machine-registration.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return err
	}
	registration, err := identityStore.Registration()
	if err != nil {
		return err
	}
	if registration.MachineID != machineID {
		return nil
	}
	if handled, windowsErr := manageWindowsConfigService(ctx, stateRoot, install); handled {
		return windowsErr
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if install {
		executable, err = endpointbinary.Daemon(executable)
	} else {
		executable, err = endpointbinary.DaemonPathForRemoval(executable)
	}
	if err != nil {
		return err
	}
	account, err := user.Current()
	if err != nil {
		return err
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return err
	}
	query := &exec.Cmd{}
	prepareConfigServiceQuery(query)
	runner := service.ExecRunner{Environment: query.Env}
	var controller service.Controller
	switch runtime.GOOS {
	case "darwin":
		uid, parseErr := strconv.Atoi(account.Uid)
		if parseErr != nil {
			return parseErr
		}
		controller = service.LaunchdController{Runner: runner, UID: uid, Label: service.ConfigLabel, UserDomain: true}
	case "linux":
		controller = service.SystemdController{Runner: runner, Unit: "paperboat-runtime-config.service", User: true}
	default:
		return service.ErrUnsupportedPlatform
	}
	definition := service.Config{Platform: runtime.GOOS, Kind: service.ConfigKind, ConfigRoot: home, Executable: executable, User: account.Username, Group: group.Name, Arguments: []string{"daemon", "__runtime-config", "--state-root", stateRoot}, Environment: map[string]string{"HOME": home}, Controller: controller}
	if !install {
		return service.Remove(ctx, definition)
	}
	installer, err := service.New(definition)
	if err != nil {
		return err
	}
	return installer.Install(ctx)
}

func configStatus(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().First())
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "state": status.State, "environments": items})
	}
	if len(items) == 0 {
		fmt.Fprintln(c.Writer, "No configuration assignments are reporting status.")
		return nil
	}
	for _, item := range items {
		name := item.Alias
		if name == "" {
			name = item.EnvironmentID
		}
		fmt.Fprintf(c.Writer, "%s: %s, %s, manifest %s, %d managed, %d pending clean\n",
			name, item.State, strings.ReplaceAll(item.Mode, "_", "-"), item.ManifestHealth,
			item.ManagedPathCount, item.PendingCleanPathCount)
		switch item.ErrorCode {
		case "configuration_changed":
			fmt.Fprintln(c.Writer, "  Configuration changed; run pb config sync apply on this machine to approve it.")
		case "configuration_invalid":
			fmt.Fprintln(c.Writer, "  Fix this machine's TOML file, then run pb config sync validate and pb config sync apply.")
		}
	}
	return nil
}

func configConflictCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "conflict", Short: "Inspect and resolve configuration conflicts"}
	list := &cobra.Command{Use: "list [environment]", Short: "List current path conflicts", Args: commandArgs(cobra.MaximumNArgs(1)), RunE: actionRun(configConflictList)}
	list.Flags().Bool("json", false, "print JSON")
	show := &cobra.Command{Use: "show <environment> <path>", Short: "Show a current path conflict", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictShow)}
	show.Flags().Bool("json", false, "print JSON")
	resolve := &cobra.Command{Use: "resolve <environment> <path>", Short: "Choose the machine or repository version", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictResolve)}
	resolve.Flags().String("keep", "", "version to keep: machine or repository")
	resolve.Flags().Bool("json", false, "print JSON")
	compare := &cobra.Command{Use: "compare <environment> <path>", Short: "Read current machine and repository conflict contents through an encrypted connection", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictCompare)}
	compare.Flags().Bool("json", false, "print comparison content as base64 JSON")
	root.AddCommand(list, show, compare, resolve)
	return root
}

func configForceCobraCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "force <pull|push> <environment> [path]", Short: "Force a scoped configuration direction",
		Args: commandArgs(cobra.RangeArgs(2, 3)), RunE: actionRun(configForce),
	}
	command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func configConflictList(c *command.Context) error {
	status, items, err := loadSelectedConfigStatus(c, c.Args().First())
	if err != nil {
		return err
	}
	type listedConflict struct {
		EnvironmentID   string                    `json:"environment_id"`
		EnvironmentName string                    `json:"environment_name"`
		Conflict        api.ConfigSyncPathSummary `json:"conflict"`
	}
	conflicts := make([]listedConflict, 0)
	for _, item := range items {
		for _, conflict := range item.Conflicts {
			conflicts = append(conflicts, listedConflict{item.EnvironmentID, item.Alias, conflict})
		}
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "state": status.State, "conflicts": conflicts})
	}
	if len(conflicts) == 0 {
		fmt.Fprintln(c.Writer, "No configuration conflicts.")
		return nil
	}
	for _, item := range conflicts {
		name := item.EnvironmentName
		if name == "" {
			name = item.EnvironmentID
		}
		fmt.Fprintf(c.Writer, "%s\t%s\t%s\n", name, item.Conflict.Path, strings.ReplaceAll(item.Conflict.Reason, "_", " "))
	}
	return nil
}

func configConflictShow(c *command.Context) error {
	_, items, err := loadSelectedConfigStatus(c, c.Args().First())
	if err != nil {
		return err
	}
	conflict, err := findConfigConflict(items[0], c.Args().Get(1))
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": items[0], "conflict": conflict})
	}
	fmt.Fprintf(c.Writer, "%s on %s\nReason: %s\nChoices: keep this machine's version or use repository version.\n",
		conflict.Path, items[0].Alias, strings.ReplaceAll(conflict.Reason, "_", " "))
	return nil
}

func configConflictResolve(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().First())
	if err != nil {
		return err
	}
	conflict, err := findConfigConflict(items[0], c.Args().Get(1))
	if err != nil {
		return err
	}
	action := ""
	switch strings.ToLower(strings.TrimSpace(c.String("keep"))) {
	case "machine", "local":
		action = "keep_local"
	case "repository", "remote":
		action = "keep_remote"
	default:
		return localArgumentError("config conflict resolve --keep must be machine or repository")
	}
	operation, err := client.ResolveConfigConflict(c.Context, items[0].EnvironmentID, api.ConfigConflictRequest{
		Path: conflict.Path, ConflictRevision: conflict.Revision, ExpectedRemoteRevision: items[0].RemoteRevision,
		ExpectedAssignmentVersion: items[0].AssignmentVersion, Action: action,
	})
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "operation": operation, "outcome": "queued"})
	}
	fmt.Fprintf(c.Writer, "Queued %s for %s on %s.\n", strings.ReplaceAll(action, "_", " "), conflict.Path, items[0].Alias)
	return nil
}

func configForce(c *command.Context) error {
	direction := strings.ToLower(strings.TrimSpace(c.Args().First()))
	if direction != "pull" && direction != "push" {
		return localArgumentError("config force direction must be pull or push")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().Get(1))
	if err != nil {
		return err
	}
	item := items[0]
	request := api.ConfigForceRequest{
		Scope: "config", ExpectedRemoteRevision: item.RemoteRevision, ExpectedAssignmentVersion: item.AssignmentVersion,
		Action: "force_" + direction, Confirmation: "FORCE " + strings.ToUpper(direction),
	}
	if c.Args().Len() == 3 {
		conflict, findErr := findConfigConflict(item, c.Args().Get(2))
		if findErr != nil {
			return findErr
		}
		request.Scope, request.Path, request.ConflictRevision = "path", conflict.Path, conflict.Revision
	}
	if err := confirmContextMutation(c, "config-force:"+item.EnvironmentID+":"+direction+":"+request.Scope+":"+request.Path+":"+request.ConflictRevision, fmt.Sprintf("Force %s for %s scope on %s (%s)? This queues a recoverable config sync operation.", direction, request.Scope, item.Alias, item.EnvironmentID)); err != nil {
		return err
	}
	operation, err := client.ForceConfig(c.Context, item.EnvironmentID, request)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "operation": operation, "outcome": "queued"})
	}
	fmt.Fprintf(c.Writer, "Queued force %s for %s on %s.\n", direction, request.Scope, item.Alias)
	return nil
}

func loadSelectedConfigStatus(c *command.Context, requested string) (api.ConfigSyncStatus, []api.ConfigSyncEnvironmentState, error) {
	client, err := backendClient(c)
	if err != nil {
		return api.ConfigSyncStatus{}, nil, err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return api.ConfigSyncStatus{}, nil, friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, requested)
	return status, items, err
}

func selectConfigEnvironments(items []api.ConfigSyncEnvironmentState, requested string) ([]api.ConfigSyncEnvironmentState, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return items, nil
	}
	matches := make([]api.ConfigSyncEnvironmentState, 0, 1)
	for _, item := range items {
		if item.EnvironmentID == requested || item.MachineID == requested || strings.EqualFold(item.Alias, requested) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 1 {
		return matches, nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("configuration environment %q is ambiguous; use its stable ID", requested)
	}
	return nil, fmt.Errorf("configuration environment %q was not found", requested)
}

func findConfigConflict(item api.ConfigSyncEnvironmentState, requested string) (api.ConfigSyncPathSummary, error) {
	for _, conflict := range item.Conflicts {
		if conflict.Path == requested {
			return conflict, nil
		}
	}
	return api.ConfigSyncPathSummary{}, fmt.Errorf("configuration conflict %q was not found on %s", requested, item.Alias)
}

func resolveConfigRepository(items []api.ConfigRepository, requested string) (api.ConfigRepository, error) {
	for _, item := range items {
		if item.ID == requested {
			return item, nil
		}
	}
	matches := make([]api.ConfigRepository, 0, 1)
	for _, item := range items {
		if strings.EqualFold(item.DisplayName, requested) || strings.EqualFold(item.ExternalRef, requested) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return api.ConfigRepository{}, fmt.Errorf("config repository %q is ambiguous; use a stable repository ID", requested)
	}
	return api.ConfigRepository{}, fmt.Errorf("config repository %q was not found", requested)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func refreshSessionIdentification(ctx context.Context, client *api.Client, sessions []machineSession) ([]machineSession, error) {
	targets := make(map[string]environmentTarget)
	for _, entry := range sessions {
		targets[entry.target.id] = entry.target
	}
	var fresh []machineSession
	for _, target := range targets {
		listed, err := listTerminalSessionsForTarget(ctx, client, target)
		if err != nil {
			return nil, err
		}
		for _, entry := range listed {
			fresh = append(fresh, machineSession{target: target, session: entry})
		}
	}
	return fresh, nil
}

func machineSessionIdentificationItem(ctx context.Context, entry machineSession, favorites favoriteSet) selector.Item {
	attached := "no attachments"
	if entry.session.AttachedCount != nil {
		attached = fmt.Sprintf("%d attached", *entry.session.AttachedCount)
	}
	details := preferenceDetails(ctx, "sessions", map[string]string{"machine": entry.target.name, "state": entry.session.State + " · " + attached + " · last active " + relativeTime(entry.session.LastActiveAt), "id": entry.session.ID})
	return selector.Item{ID: entry.target.id + ":" + entry.session.ID, Title: entry.session.Name, Description: sessionIdentificationDetails(entry.session, details), Search: entry.target.name + " " + sessionIdentificationSearch(entry.session) + " favorite starred", Favorite: favorites.IsFavorite("session", machineSessionFavoriteID(entry))}
}
