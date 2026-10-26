package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	filetransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/paste"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/session"
	"github.com/pinksaucepasta/paperboat/internal/statusbar"
	"github.com/pinksaucepasta/paperboat/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

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

func actionConnect(c *command.Context) error {
	return actionConnectTarget(c, "")
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
	createdSessionID, createdMachineID := "", ""
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
					createdSessionID, createdMachineID = created.ID, info.MachineID
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
		if createdSessionID != "" {
			reportTerminalAuxFailure(ctx, "cleanup", "terminal_session_failed", closeAbandonedSession(backend, createdMachineID, createdSessionID))
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
	case "team_suspended":
		return apiErr.Error()
	case "bandwidth_quota_exhausted", "machine_limit_reached", "preview_limit_reached", "browser_terminal_limit_reached", "account_deletion_console_only", "destination_update_required", "transfer_too_large":
		// The authored message plus the vetted billing or console link from the response (apiErrorMessageFor).
		return apiErr.Message
	case "policy_version_conflict", "policy_changes_unconfirmed", "invalid_policy", "policy_edit_forbidden", "policy_explain_forbidden", "api_token_name_taken", "invalid_api_token_scope", "api_token_invalid", "outside_token_scope",
		"team_sso_required", "directory_managed", "invitation_domain_not_verified":
		return api.PublicMessage(apiErr.Code)
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

type terminalSessionCloser interface {
	CloseUserMachineTerminalSession(ctx context.Context, machineID, sessionID string) (api.TerminalSessionCloseResult, error)
}

// closeAbandonedSession closes the session this connect created when it never attached (for
// example the machine refused it at its terminal ceiling), so the server does not keep a live row
// the user never got. The server also reconciles such rows with the host; this is the prompt path.
func closeAbandonedSession(closer terminalSessionCloser, machineID, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := closer.CloseUserMachineTerminalSession(ctx, machineID, sessionID); err != nil {
		return fmt.Errorf("close the terminal session that did not attach: %w", err)
	}
	return nil
}
