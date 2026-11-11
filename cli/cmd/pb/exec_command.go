package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/session"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
	"golang.org/x/term"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func actionRemoteExec(c *command.Context, requested string, request tunnel.ExecRequest, jsonOutput bool) (returnErr error) {
	if c.Reader == nil {
		return errors.New("remote execution requires an explicit input reader")
	}
	if len(request.Argv) == 0 || request.Timeout < 0 || request.Timeout > 24*time.Hour {
		return invocationError(errors.New("exec timeout must be between zero and 24h and argv must not be empty"))
	}
	request.OperationID = newExecOperationID()
	fail := func(code int, errorCode string, changed, uncertain bool, err error) error {
		if !jsonOutput {
			return err
		}
		if encodeErr := writeExecJSONFailure(c.Writer, request.OperationID, errorCode, safeExecError(err), changed, uncertain); encodeErr != nil {
			return encodeErr
		}
		return execCommandFailure{cause: err, code: code, presented: true}
	}
	d, err := buildDeps(c)
	if err != nil {
		return fail(255, "local_configuration", false, false, err)
	}
	if d.peerApplications == nil || d.cfg.ServerURL == "" {
		err = errors.New("Paperboat server and peer transport are required for remote execution")
		return fail(255, "local_configuration", false, false, err)
	}
	_, err = d.auth.Credential()
	if api.Unreachable(err) {
		// The access token ran out during an outage and its refresh cannot reach Paperboat: the daemon opens this exec
		// offline, for the host to admit from its own signed scopes (F21-01, no time limit).
		err = nil
	}
	if onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return leaf == config.ErrNoCredentials }) {
		err = fmt.Errorf("authentication required: %w", err)
		return fail(255, "authentication_required", false, false, err)
	}
	if err != nil {
		return fail(255, "authentication_unavailable", false, false, err)
	}
	client, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, d.auth)
	if err != nil {
		return fail(255, "workspace_unavailable", false, false, err)
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return fail(255, "source_identity_unavailable", false, false, err)
	}
	if err := requireLocalDaemonService(c.Context, d.cfg); err != nil {
		return fail(255, "local_transport_unavailable", false, false, fmt.Errorf("prepare local peer transport: %w", err))
	}
	client.SetSourceMachineID(sourceMachineID)
	machine, warm := resolveWarmUserMachine(c.Context, requested)
	if !warm {
		machine, err = resolveUserMachine(c.Context, client, requested)
	}
	if err != nil {
		err = friendlyCommandError(err)
		switch {
		case errors.Is(err, resolver.ErrMachineNotFound):
			return fail(255, "machine_not_found", false, false, err)
		case errors.Is(err, resolver.ErrMachineAmbiguous):
			return fail(255, "machine_ambiguous", false, false, err)
		}
		return fail(255, "machine_resolution_failed", false, false, err)
	}
	if !machine.Online {
		return fail(255, "machine_offline", false, false, &api.APIError{Code: "machine_offline"})
	}
	if machine.InstallationGeneration < 1 {
		err = errors.New("machine has no active runtime generation")
		return fail(255, "runtime_unavailable", false, false, err)
	}
	// The daemon owns operation-descriptor issuance. This placeholder carries
	// only inventory authority and is replaced with fresh endpoints/credentials
	// inside the authenticated local API broker.
	descriptor := api.ExecDescriptor{OperationID: request.OperationID, ExpiresAt: time.Now().Add(2 * time.Minute), Auth: api.AuthMaterial{ExpiresAt: time.Now().Add(2 * time.Minute)}, Environment: &api.Environment{ID: machine.EnvironmentID, Kind: "machine", ResourceID: machine.ID, Root: machine.WorkspaceRoot}}
	if request.CWD == "" {
		request.CWD = machine.WorkspaceRoot
	}
	if !remoteAbsolutePath(machine.Platform, machine.WorkspaceRoot, request.CWD) {
		err = cwdRefusal(machine.Platform)
		return fail(2, "invalid_request", false, false, err)
	}
	if request.PTY {
		request.Columns, request.Rows = localTerminalSize()
		if request.Columns == 0 || request.Rows == 0 {
			request.Columns, request.Rows = 80, 24
		}
	}
	dial := func(attempt context.Context, current api.ExecDescriptor) (tunnel.ExecConn, error) {
		return dialExecAttached(c.Context, attempt, func(ctx context.Context) (tunnel.ExecConn, error) {
			return d.peerApplications.DialExec(ctx, execConnectInfo(machine, current, client.Workspace()), request)
		})
	}
	connection, initialUncertain, err := startExec(c.Context, request.Timeout, func(ctx context.Context) (tunnel.ExecConn, error) {
		return dial(ctx, descriptor)
	})
	if err != nil {
		var uncertain *tunnel.ExecStartUncertainError
		if initialUncertain || errors.As(err, &uncertain) || errors.Is(err, localapi.ErrExecStartUncertain) {
			return fail(255, "exec_start_uncertain", true, true, err)
		}
		if remote, ok := err.(*localapi.RemoteError); ok && remote.Code == "environment_unavailable" {
			return fail(125, "environment_unavailable", false, false, err)
		}
		if remote, ok := err.(*localapi.RemoteError); ok && localapi.Refusal(remote.Code) {
			return fail(255, remote.Code, false, false, err)
		}
		// The connection never opened, so nothing was started on the machine.
		return fail(255, "transport_unavailable", false, false, &execNotStartedError{err})
	}
	connectionRef := newExecConnectionRef(connection)
	workerCtx, stopWorkers := context.WithCancel(context.WithoutCancel(c.Context))
	inputCtx, stopInput := context.WithCancel(workerCtx)
	execDone := make(chan struct{})
	var workers sync.WaitGroup
	var restoreTerminal func() error
	defer func() {
		close(execDone)
		stopWorkers()
		if active := connectionRef.Current(); active != nil {
			if abortErr := abortExecAttachment(active); abortErr != nil {
				observeExecFailure(workerCtx, abortErr)
			}
		}
		workers.Wait()
		if restoreTerminal != nil {
			returnErr = appendExecFailure(returnErr, restoreTerminal())
		}
	}()
	processSignals := make(chan os.Signal, 1)
	var cancelRequested atomic.Bool
	cancelResult := make(chan execCancelOutcome, 1)
	cancelCtx, abandonCancel := context.WithCancel(workerCtx)
	defer abandonCancel()
	cancelRemote := func() execCancelOutcome {
		// A separate idempotent attachment avoids queuing cancellation behind output.
		ctx, cancel := context.WithTimeout(cancelCtx, 25*time.Second)
		defer cancel()
		var failures []error
		for attempt := 0; attempt < 2; attempt++ {
			retryDescriptor, descriptorErr := client.MachineExecDescriptor(ctx, machine.ID, request.OperationID)
			if descriptorErr != nil {
				return execCancelOutcome{err: errors.Join(append(failures, descriptorErr)...)}
			}
			replacement, dialErr := d.peerApplications.DialExec(ctx, execConnectInfo(machine, retryDescriptor, client.Workspace()), request)
			if dialErr != nil {
				failures = append(failures, dialErr)
				observeExecFailure(ctx, dialErr)
				continue
			}
			outcome := cancelExecAttachment(ctx, replacement)
			if outcome.terminal != nil {
				return outcome
			}
			failures = append(failures, outcome.err)
			observeExecFailure(ctx, outcome.err)
		}
		return execCancelOutcome{err: errors.Join(failures...)}
	}
	startCancel := func() {
		if !cancelRequested.CompareAndSwap(false, true) {
			return
		}
		stopInput()
		workers.Add(1)
		go func() { defer workers.Done(); cancelResult <- cancelRemote() }()
	}
	// SIGINT and SIGTERM cancel the remote process; hangup remains an explicit process signal.
	signal.Notify(processSignals, syscall.SIGHUP, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(processSignals)
	workers.Add(1)
	go func() {
		defer workers.Done()
		watchExecSignals(processSignals, c.Context.Done(), execDone, startCancel, abandonCancel, func(received os.Signal) {
			if active := connectionRef.Current(); active != nil {
				if signalErr := active.Signal(received.String()); signalErr != nil {
					observeExecFailure(workerCtx, signalErr)
				}
			}
		})
	}()
	if request.PTY && term.IsTerminal(int(os.Stdin.Fd())) {
		state, rawErr := term.MakeRaw(int(os.Stdin.Fd()))
		if rawErr != nil {
			return rawErr
		}
		restoreTerminal = func() error { return term.Restore(int(os.Stdin.Fd()), state) }
		resizeSignals := make(chan os.Signal, 1)
		defer session.NotifyWinch(resizeSignals)() // SIGWINCH on Unix; Windows has none, so it polls the console size
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-resizeSignals:
					cols, rows := localTerminalSize()
					if cols > 0 && rows > 0 {
						if active := connectionRef.Current(); active != nil {
							if resizeErr := active.Resize(rows, cols); resizeErr != nil {
								observeExecFailure(workerCtx, resizeErr)
							}
						}
					}
				case <-execDone:
					return
				}
			}
		}()
	}
	inputResult := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); inputResult <- forwardExecInput(inputCtx, c.Reader, connectionRef) }()
	encoder := json.NewEncoder(c.Writer)
	sawTerminalEvent := false
	emittedStarted := false
	lastEventSequence := uint64(0)
	// The command keeps running in its session owner through pb update; the
	// stream may be gone while the runtime restarts, so reattach for up to
	// execReattachWindow instead of 300 ms (pbvm4 feat runs: update restarts take seconds).
	recovery := reattach.New(reattach.Policy{Attempts: execReattachAttempts, Backoff: execRecoveryBackoff, Retryable: retryableExecReattachment})
	var code int
	var inputCloseFailure error
	contextDone := c.Context.Done()
	cancelFinished := false
	applyCancellation := func(outcome execCancelOutcome) error {
		cancelFinished = true
		// The execution owner's result supersedes loss of the retired output
		// attachment. Preserve unrelated input or I/O failures.
		if outcome.terminal != nil && (err == nil || retryableExecTransportFailure(err) || execCancellationOnly(err)) {
			err = outcome.err
		} else {
			err = appendExecFailure(err, outcome.err)
		}
		code = outcome.code
		if outcome.terminal != nil && !sawTerminalEvent {
			if writeErr := writeExecEvent(c, encoder, *outcome.terminal, request.PTY, jsonOutput); writeErr != nil {
				return execCommandFailure{cause: appendExecFailure(err, writeErr), code: 255, presented: jsonOutput}
			}
			sawTerminalEvent = true
		}
		if c.Context.Err() == context.DeadlineExceeded && (outcome.terminal == nil || outcome.terminal.State == "canceled") {
			err = appendExecFailure(err, context.DeadlineExceeded)
		}
		if abortErr := abortExecAttachment(connection); abortErr != nil {
			observeExecFailure(workerCtx, abortErr)
		}
		return nil
	}
	for {
		events := connection.Events()
		streamEnded := false
		for !streamEnded {
			select {
			case event, ok := <-events:
				if !ok {
					streamEnded = true
					continue
				}
				if event.EventSequence > lastEventSequence {
					lastEventSequence = event.EventSequence
				}
				if event.State == "started" {
					if emittedStarted {
						continue
					}
					emittedStarted = true
				}
				terminalEvent := event.Stream == "" && event.State != "" && event.State != "started"
				if terminalEvent {
					sawTerminalEvent = true
				}
				if writeErr := writeExecEvent(c, encoder, event, request.PTY, jsonOutput); writeErr != nil {
					return execCommandFailure{cause: writeErr, code: 255, presented: jsonOutput}
				}
				if terminalEvent {
					streamEnded = true
				}
			case inputErr := <-inputResult:
				inputResult = nil
				if inputErr != nil {
					if cancelRequested.Load() && classifyCommandFailure(inputErr).kind == commandCanceled {
						continue
					}
					if execInputClosedOnly(inputErr) {
						inputCloseFailure = inputErr
					} else {
						err = appendExecFailure(err, inputErr)
						streamEnded = true
					}
				}
			case outcome := <-cancelResult:
				if applyErr := applyCancellation(outcome); applyErr != nil {
					return applyErr
				}
				streamEnded = true
			case <-contextDone:
				contextDone = nil
				startCancel()
			}
		}
		if err == nil && !cancelFinished {
			code, err = connection.Wait()
		}
		if c.Context.Err() != nil {
			startCancel()
		}
		if cancelRequested.Load() {
			// Reattachment may retire the original stream before the cancel
			// RPC completes. Its bounded owner must finish before CLI teardown.
			if !cancelFinished {
				if applyErr := applyCancellation(<-cancelResult); applyErr != nil {
					return applyErr
				}
			}
			break
		}
		if !retryableExecTransportFailure(err) || sawTerminalEvent || recovery.Remaining() == 0 || c.Context.Err() != nil {
			break
		}
		observeExecFailure(c.Context, appendExecFailure(err, inputCloseFailure))
		connectionRef.Clear(connection)
		if abortErr := abortExecAttachment(connection); abortErr != nil {
			observeExecFailure(c.Context, abortErr)
		}
		request.FromSequence = lastEventSequence + 1
		if request.FromSequence == 0 {
			request.FromSequence = 1
		}
		previousFailure := err
		var replacement tunnel.ExecConn
		replacement, err = recoverExecConnection(c.Context, recovery, func(ctx context.Context) (tunnel.ExecConn, error) {
			return reattachExecConnection(ctx, connectionRef, &request,
				func(ctx context.Context) (api.ExecDescriptor, error) {
					return client.MachineExecDescriptor(ctx, machine.ID, request.OperationID)
				}, dial)
		})
		if err != nil || replacement == nil {
			connectionRef.AbortReattach()
			err = appendExecFailure(previousFailure, err)
			break
		}
		inputCloseFailure = nil
		connection = replacement
	}
	if err != nil {
		err = appendExecFailure(err, inputCloseFailure)
	}
	return finishExecResult(c.Writer, c.ErrWriter, request.OperationID, jsonOutput, sawTerminalEvent, code, err)
}

const execReattachAttempts = 40

// execReattachAttemptTimeout bounds one reattach attempt; the daemon's own peer open takes up to 10 s.
var execReattachAttemptTimeout = 15 * time.Second

// execReattachWindow bounds one recovery: it outlasts a machine restarting for an update (a Windows worker was
// unavailable 41 and 48 s through pb install on winpool-8; 40 s dropped the exec 2 s before it was back). Attempts alone
// do not bound time: 40 attempts that each hang to 15 s take 10 min.
var execReattachWindow = 90 * time.Second

// recoverExecConnection runs one recovery under execReattachWindow; the attempt budget stays recovery's own.
func recoverExecConnection(
	ctx context.Context, recovery *reattach.Recovery, attempt func(context.Context) (tunnel.ExecConn, error),
) (tunnel.ExecConn, error) {
	bounded, cancel := context.WithTimeout(ctx, execReattachWindow)
	defer cancel()
	var replacement tunnel.ExecConn
	err := recovery.Run(bounded, func(ctx context.Context) error {
		var attemptErr error
		replacement, attemptErr = attempt(ctx)
		return attemptErr
	})
	if err != nil && ctx.Err() == nil && bounded.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("the machine did not answer within %v: %w", execReattachWindow, reattach.ErrExhausted)
	}
	return replacement, err
}

func reattachExecDial(
	attempt context.Context, descriptor func(context.Context) (api.ExecDescriptor, error),
	dial func(context.Context, api.ExecDescriptor) (tunnel.ExecConn, error),
) (tunnel.ExecConn, error) {
	fresh, err := descriptor(attempt)
	if err != nil {
		return nil, err
	}
	return dial(attempt, fresh)
}

// errExecInputFlush marks a replacement connection that dropped again while the stdin journal was written to it.
var errExecInputFlush = errors.New("stdin could not be resent on the replacement connection")

// reattachExecConnection dials the replacement and hands it the stdin journal. The replacement starts at the oldest
// stdin offset still journaled; a second blip during the resend is a lost transport like the first, so the caller's
// bounded recovery tries again from the journal.
func reattachExecConnection(
	ctx context.Context, ref *execConnectionRef, request *tunnel.ExecRequest,
	descriptor func(context.Context) (api.ExecDescriptor, error),
	dial func(context.Context, api.ExecDescriptor) (tunnel.ExecConn, error),
) (tunnel.ExecConn, error) {
	request.InputOffset = ref.BeginReattach()
	// A daemon that accepts the socket and never answers must not hang one attempt until Ctrl-C.
	attempt, cancel := context.WithTimeout(ctx, execReattachAttemptTimeout)
	defer cancel()
	replacement, err := reattachExecDial(attempt, descriptor, dial)
	if err != nil {
		// Only the attempt's own expiry becomes retryable: a permanent answer racing the timer stays permanent.
		if ctx.Err() == nil && attempt.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("the reattach attempt got no answer in %v: %w", execReattachAttemptTimeout, tunnel.ErrPeerStreamOpen)
		}
		return nil, err
	}
	// The resend is part of the attempt: a replacement that connects and then stalls its writes is cut at the same
	// deadline (the write has no context of its own, so closing the connection is what unblocks it).
	stopAbort := context.AfterFunc(attempt, func() { _ = abortExecAttachment(replacement) })
	setErr := ref.Set(replacement)
	if !stopAbort() {
		setErr = errors.Join(setErr, errors.New("the replacement did not take the stdin resend before the reattach deadline"))
	}
	if setErr != nil {
		ref.Clear(replacement)
		_ = abortExecAttachment(replacement)
		return nil, errors.Join(errExecInputFlush, setErr)
	}
	return replacement, nil
}

// execRecoveryBackoff tries at once, then polls every 150 ms for the first 20 attempts, the length of a network handover
// (the pause of a call that reattaches is the sleep after the machine is back, so it stays short), then backs off by
// 100 ms per attempt up to 2 s; 40 attempts sleep about 24 s in all (execReattachWindow bounds the whole recovery). A successor that is already up costs no sleep, like
// EternalTerminal, which dials on loss and sleeps only after a failed attempt (ClientConnection.cpp:210).
func execRecoveryBackoff(attempt int) time.Duration {
	if attempt == 1 {
		return 0
	}
	if attempt <= 20 {
		return 150 * time.Millisecond
	}
	return min(time.Duration(attempt-20)*100*time.Millisecond, 2*time.Second)
}
func execStartUncertain(err error) bool {
	var uncertain *tunnel.ExecStartUncertainError
	return errors.As(err, &uncertain) || errors.Is(err, localapi.ErrExecStartUncertain)
}

// retryUncertainExecStart retries an exec whose start is uncertain, with the same operation ID (the host runs it at
// most once), for as long as a running exec may reattach: a machine restarting for an update answers again within that
// window, where three 10 s peer opens gave up first (winpool pb install: worker unavailable 41 s, pb exec failed at 31 s).
// startExec opens the exec and retries an uncertain start (retryUncertainExecStart), all within the user's --timeout
// when one is set: the timeout bounds the whole exec, its start included (W1: --timeout 2s waited out a peer update
// for 10 s). A start that has not opened when it ends is reported as uncertain: the open may have reached the host.
// uncertain reports whether the start's outcome is unknown.
func startExec(ctx context.Context, timeout time.Duration, dial func(context.Context) (tunnel.ExecConn, error)) (connection tunnel.ExecConn, uncertain bool, err error) {
	start := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		start, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	connection, err = dial(start)
	if err == nil {
		return connection, false, nil
	}
	if ctx.Err() == nil && start.Err() != nil {
		return nil, true, fmt.Errorf("the command did not start within --timeout %v: %w", timeout, localapi.ErrExecStartUncertain)
	}
	if !execStartUncertain(err) {
		return nil, false, err
	}
	observeExecFailure(ctx, err)
	connection, err = retryUncertainExecStart(start, dial)
	return connection, true, err
}

func retryUncertainExecStart(ctx context.Context, dial func(context.Context) (tunnel.ExecConn, error)) (tunnel.ExecConn, error) {
	recovery := reattach.New(reattach.Policy{Attempts: execReattachAttempts, Backoff: execRecoveryBackoff, Retryable: execStartUncertain})
	var uncertain error
	connection, err := recoverExecConnection(ctx, recovery, func(ctx context.Context) (tunnel.ExecConn, error) {
		connection, err := dial(ctx)
		if execStartUncertain(err) {
			uncertain = err
		}
		return connection, err
	})
	if uncertain != nil && (errors.Is(err, reattach.ErrExhausted) || errors.Is(err, context.DeadlineExceeded)) {
		// The window or the user's --timeout ended the last open; the outcome is still that the start is uncertain.
		return nil, uncertain
	}
	return connection, err
}

func retryableExecReattachment(err error) bool {
	return execStartUncertain(err) || retryableInitialConnectFailure(err) || machineOfflineError(err) || errors.Is(err, errExecInputFlush)
}

func safeExecError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, resolver.ErrMachineNotFound) {
		return "No machine has that name or ID. Run `pb machine list` to see your machines."
	}
	if errors.Is(err, resolver.ErrMachineAmbiguous) {
		return "More than one machine has that name. Run `pb machine list` and retry with the machine ID."
	}
	if remote, ok := err.(*api.APIError); ok && remote.Code == "machine_offline" {
		return sentence(friendlyAPIError(remote))
	}
	if remote, ok := err.(*localapi.RemoteError); ok && remote.Code == "environment_unavailable" {
		return "Managed environment variables could not be delivered on the target. Check ENV delivery and retry."
	}
	if remote, ok := err.(*localapi.RemoteError); ok && localapi.Refusal(remote.Code) {
		return remote.Message
	}
	if remote, ok := soleExecRemoteError(err); ok {
		switch remote.Code {
		case "environment_unavailable":
			return "Managed environment variables could not be delivered on the target. Check ENV delivery and retry."
		case "exec_timeout":
			return "Remote execution exceeded its time limit. Check the operation before retrying."
		case "exec_canceled", "canceled":
			return "Remote execution was canceled."
		case "exec_result_unavailable":
			return "The remote execution result is unavailable. Check the machine before retrying."
		}
	}
	var notStarted *execNotStartedError
	if errors.As(err, &notStarted) {
		return "Paperboat could not open a connection to the machine, so the command did not run. Retry; run `pb doctor` if this continues."
	}
	return "Remote execution did not finish. Check the machine connection and retry; the remote outcome may be unknown."
}

// execNotStartedError is an exec whose connection to the machine never opened: the command did not run.
type execNotStartedError struct{ cause error }

func (e *execNotStartedError) Error() string {
	return "exec connection did not open: " + e.cause.Error()
}
func (e *execNotStartedError) Unwrap() error { return e.cause }

// dialExecAttached dials one exec attachment under the caller's and the attempt's cancellation.
func dialExecAttached(caller, attempt context.Context, dial func(context.Context) (tunnel.ExecConn, error)) (tunnel.ExecConn, error) {
	dialCtx, cancelDial := context.WithCancel(context.WithoutCancel(caller))
	defer cancelDial()
	stopCallerCancel := context.AfterFunc(caller, cancelDial)
	stopAttempt := context.AfterFunc(attempt, cancelDial)
	defer stopAttempt()
	defer stopCallerCancel()
	// An opened connection is kept even if the caller cancelled meanwhile: the host may have started the command, and
	// the caller's cancel path stops it remotely.
	return dial(dialCtx)
}

// watchExecSignals runs until execDone: an interrupt, SIGTERM or the caller's cancellation cancels the remote process,
// a second interrupt or SIGTERM abandons that cancel so the CLI exits now; other signals are forwarded. One Ctrl-C both
// cancels the caller's context and arrives on signals, so only signals are counted.
func watchExecSignals(signals <-chan os.Signal, callerDone, execDone <-chan struct{}, cancel, abandon func(), forward func(os.Signal)) {
	stops := 0
	for {
		select {
		case received := <-signals:
			if received != os.Interrupt && received != syscall.SIGTERM {
				forward(received)
				continue
			}
			if stops++; stops > 1 {
				abandon()
				return
			}
			cancel()
		case <-callerDone:
			callerDone = nil
			cancel()
		case <-execDone:
			return
		}
	}
}

// cwdRefusal names the working-directory form the target machine accepts: a Unix client cannot guess
// that a Windows target wants a drive letter, nor a Windows client that a Unix target wants a slash.
func cwdRefusal(platform string) error {
	form := "starting with / such as /home/user"
	if platform == "windows" {
		form = `with a drive letter such as C:\Users\user`
	}
	return localArgumentError("--cwd must be an absolute path on the target machine, in its own form, " + form)
}
