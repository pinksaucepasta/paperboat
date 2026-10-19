package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/remotepath"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
)

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
