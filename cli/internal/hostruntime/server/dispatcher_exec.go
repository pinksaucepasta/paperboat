package server

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
	"time"
)

type execRequest struct {
	Action       string            `json:"action"`
	OperationID  string            `json:"operation_id"`
	Argv         []string          `json:"argv,omitempty"`
	CWD          string            `json:"cwd,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	TimeoutMS    uint32            `json:"timeout_ms,omitempty"`
	PTY          bool              `json:"pty,omitempty"`
	Columns      uint16            `json:"columns,omitempty"`
	Rows         uint16            `json:"rows,omitempty"`
	FromSequence uint64            `json:"from_sequence,omitempty"`
	Data         []byte            `json:"data,omitempty"`
	Signal       string            `json:"signal,omitempty"`
}

// ExecEnd is one finished execution a peer started. A standing-rule exec makes no control-plane request, so the host
// queues its end (outcome, exit code, duration) for the audit log, as it does an SSH session's (Teleport's session.end).
type ExecEnd struct {
	AccountID, EndpointID, OperationID string
	Offline                            bool
	Snapshot                           execprocess.Snapshot
}

// ExecEndRecorder queues an ExecEnd; it must not block for long.
type ExecEndRecorder func(ExecEnd)

func (d *Dispatcher) exec(ctx context.Context, authorization Authorization, payload json.RawMessage) operation.Outcome {
	if d.config.Exec == nil {
		return failure("capability_required")
	}
	var request execRequest
	if decodeStrict(payload, &request) != nil || request.OperationID == "" {
		return failure("invalid_request")
	}
	switch request.Action {
	case "start":
		execution, replay, err := d.config.Exec.Start(ctx, execprocess.Request{OperationID: request.OperationID, Argv: request.Argv, CWD: request.CWD, Environment: request.Environment, Timeout: time.Duration(request.TimeoutMS) * time.Millisecond, PTY: request.PTY, Dimensions: pty.Dimensions{Columns: request.Columns, Rows: request.Rows}})
		if err != nil {
			return execResult(nil, err)
		}
		if !replay && d.config.RecordExecEnd != nil {
			// Only the attachment that started it records the end; reattaches and replays name the same execution.
			end := ExecEnd{AccountID: authorization.AccountID, EndpointID: authorization.ClientID, OperationID: request.OperationID, Offline: authorization.Offline}
			if end.AccountID == "" {
				end.AccountID = authorization.UserID
			}
			go func() {
				end.Snapshot, _ = execution.Wait(context.Background()) // an execution always ends (exit, timeout, linger, shutdown)
				d.config.RecordExecEnd(end)
			}()
		}
		return execAttachmentResult(ctx, execution, replay)
	case "attach":
		execution, err := d.config.Exec.Get(request.OperationID)
		if err != nil {
			return execResult(nil, err)
		}
		return execAttachmentResult(ctx, execution, true)
	case "status":
		execution, err := d.config.Exec.Get(request.OperationID)
		if err != nil {
			return execResult(nil, err)
		}
		return result(execution.Snapshot())
	case "input":
		execution, err := d.config.Exec.Get(request.OperationID)
		if err == nil {
			_, err = execution.Write(request.Data)
		}
		return execResult(struct{}{}, err)
	case "signal":
		execution, err := d.config.Exec.Get(request.OperationID)
		if err == nil {
			err = execution.Signal(pty.Signal(request.Signal))
		}
		return execResult(struct{}{}, err)
	case "resize":
		execution, err := d.config.Exec.Get(request.OperationID)
		if err == nil {
			err = execution.Resize(pty.Dimensions{Columns: request.Columns, Rows: request.Rows})
		}
		return execResult(struct{}{}, err)
	case "cancel":
		execution, err := d.config.Exec.Get(request.OperationID)
		if err == nil {
			cancelCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = execution.Cancel(cancelCtx)
			cancel()
		}
		return execResult(struct{}{}, err)
	default:
		return failure("invalid_request")
	}
}

func execAttachmentResult(ctx context.Context, execution execprocess.ExecutionService, replay bool) operation.Outcome {
	snapshot, err := execution.WaitStarted(ctx)
	if err != nil {
		return execResult(nil, err)
	}
	return result(struct {
		Snapshot execprocess.Snapshot `json:"snapshot"`
		Replay   bool                 `json:"replay"`
	}{snapshot, replay})
}
