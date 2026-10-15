package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-relay/reattach"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"golang.org/x/term"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func actionRemoteExec(c *command.Context, requested string, request tunnel.ExecRequest, jsonOutput bool) (returnErr error) {
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
		return fail(255, "machine_resolution_failed", false, false, err)
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
		err = invocationError(errors.New("--cwd must be an absolute path"))
		return fail(2, "invalid_request", false, false, err)
	}
	if request.PTY {
		request.Columns, request.Rows = localTerminalSize()
		if request.Columns == 0 || request.Rows == 0 {
			request.Columns, request.Rows = 80, 24
		}
	}
	dial := func(current api.ExecDescriptor) (tunnel.ExecConn, error) {
		// Keep caller cancellation attached while dialing, then detach the
		// established carrier so actionRemoteExec can complete the remote cancel
		// RPC before transport teardown.
		dialCtx, cancelDial := context.WithCancel(context.WithoutCancel(c.Context))
		defer cancelDial()
		stopCallerCancel := context.AfterFunc(c.Context, cancelDial)
		connection, dialErr := d.peerApplications.DialExec(dialCtx, execConnectInfo(machine, current, client.Workspace()), request)
		if dialErr != nil {
			stopCallerCancel()
			cancelDial()
			return nil, dialErr
		}
		if !stopCallerCancel() || c.Context.Err() != nil {
			cancelDial()
			_ = connection.Close()
			return nil, c.Context.Err()
		}
		return connection, nil
	}
	connection, err := dial(descriptor)
	initialUncertain := false
	if err != nil {
		var uncertain *tunnel.ExecStartUncertainError
		if errors.As(err, &uncertain) || errors.Is(err, localapi.ErrExecStartUncertain) {
			initialUncertain = true
			observeExecFailure(c.Context, err)
			initial := reattach.New(reattach.Policy{Attempts: 2, Backoff: execRecoveryBackoff, Retryable: func(err error) bool {
				var uncertain *tunnel.ExecStartUncertainError
				return errors.As(err, &uncertain) || errors.Is(err, localapi.ErrExecStartUncertain)
			}})
			err = initial.Run(c.Context, func(context.Context) error {
				var attemptErr error
				connection, attemptErr = dial(descriptor)
				return attemptErr
			})
		}
	}
	if err != nil {
		var uncertain *tunnel.ExecStartUncertainError
		if initialUncertain || errors.As(err, &uncertain) || errors.Is(err, localapi.ErrExecStartUncertain) {
			return fail(255, "exec_start_uncertain", true, true, err)
		}
		if remote, ok := err.(*localapi.RemoteError); ok && remote.Code == "environment_unavailable" {
			return fail(125, "environment_unavailable", false, false, err)
		}
		return fail(255, "transport_unavailable", false, false, err)
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
	cancelRemote := func() execCancelOutcome {
		// A separate idempotent attachment avoids queuing cancellation behind output.
		ctx, cancel := context.WithTimeout(workerCtx, 25*time.Second)
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
	// SIGINT cancels the remote process; hangup remains an explicit process signal.
	signal.Notify(processSignals, syscall.SIGHUP, os.Interrupt)
	defer signal.Stop(processSignals)
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case received := <-processSignals:
				if received == os.Interrupt {
					startCancel()
				} else if active := connectionRef.Current(); active != nil {
					if signalErr := active.Signal(received.String()); signalErr != nil {
						observeExecFailure(workerCtx, signalErr)
					}
				}
			case <-c.Context.Done():
				startCancel()
				return
			case <-execDone:
				return
			}
		}
	}()
	if request.PTY && term.IsTerminal(int(os.Stdin.Fd())) {
		state, rawErr := term.MakeRaw(int(os.Stdin.Fd()))
		if rawErr != nil {
			return rawErr
		}
		restoreTerminal = func() error { return term.Restore(int(os.Stdin.Fd()), state) }
		resizeSignals := make(chan os.Signal, 1)
		notifyResizeSignals(resizeSignals)
		defer signal.Stop(resizeSignals)
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
	go func() { defer workers.Done(); inputResult <- forwardExecInput(inputCtx, os.Stdin, connectionRef) }()
	encoder := json.NewEncoder(c.Writer)
	sawTerminalEvent := false
	emittedStarted := false
	lastEventSequence := uint64(0)
	recovery := reattach.New(reattach.Policy{Attempts: 2, Backoff: execRecoveryBackoff, Retryable: retryableExecReattachment})
	var code int
	var inputCloseFailure error
	contextDone := c.Context.Done()
	var cancelDeadline <-chan time.Time
	var cancelTimer *time.Timer
	defer func() {
		if cancelTimer != nil {
			cancelTimer.Stop()
		}
	}()
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
				err = appendExecFailure(err, outcome.err)
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
				streamEnded = true
			case <-contextDone:
				contextDone = nil
				startCancel()
				if cancelDeadline == nil {
					cancelTimer = time.NewTimer(30 * time.Second)
					cancelDeadline = cancelTimer.C
				}
			case <-cancelDeadline:
				err = errors.New("remote execution cancellation outcome is uncertain")
				_ = abortExecAttachment(connection)
				streamEnded = true
			}
		}
		if err == nil {
			code, err = connection.Wait()
		}
		if cancelRequested.Load() || c.Context.Err() != nil {
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
		err = recovery.Run(c.Context, func(ctx context.Context) error {
			fresh, err := client.MachineExecDescriptor(ctx, machine.ID, request.OperationID)
			if err != nil {
				return err
			}
			replacement, err = dial(fresh)
			return err
		})
		if err != nil || replacement == nil {
			err = appendExecFailure(previousFailure, err)
			break
		}
		inputCloseFailure = nil
		connection = replacement
		if setErr := connectionRef.Set(connection); setErr != nil {
			err = setErr
			break
		}
	}
	if err != nil {
		err = appendExecFailure(err, inputCloseFailure)
	}
	return finishExecResult(c.Writer, c.ErrWriter, request.OperationID, jsonOutput, sawTerminalEvent, code, err)
}

func execRecoveryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * 100 * time.Millisecond
}
func retryableExecReattachment(err error) bool {
	var uncertain *tunnel.ExecStartUncertainError
	return errors.As(err, &uncertain) || errors.Is(err, localapi.ErrExecStartUncertain) || retryableInitialConnectFailure(err)
}
