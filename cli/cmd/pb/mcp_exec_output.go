package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/mcpserver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// Keep consuming events after output truncation so the final operation result
// and uncertain-start metadata are never lost to a noisy command.
type mcpExecOutput struct {
	result    mcpserver.ExecResult
	pending   []byte
	remaining int
	started   bool
}

func newMCPExecOutput() *mcpExecOutput {
	return &mcpExecOutput{result: mcpserver.ExecResult{Outcome: "not_started"}, remaining: mcpserver.MaxOutputBytes}
}

func (w *mcpExecOutput) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		part, rest, complete := bytes.Cut(data, []byte{'\n'})
		if len(w.pending)+len(part) > base64.StdEncoding.EncodedLen(protocol.MaxBinaryFrame)+protocol.MaxStructuredFrame {
			return 0, errors.New("execution event exceeds its protocol bound")
		}
		w.pending = append(w.pending, part...)
		if !complete {
			return n, nil
		}
		if err := w.event(w.pending); err != nil {
			return 0, err
		}
		w.pending = w.pending[:0]
		data = rest
	}
	return n, nil
}

func (w *mcpExecOutput) event(data []byte) error {
	var event struct {
		OperationID string             `json:"operation_id"`
		Event       string             `json:"event"`
		Stream      string             `json:"stream"`
		Data        []byte             `json:"data"`
		Result      *tunnel.ExecResult `json:"result"`
		ErrorCode   string             `json:"error_code"`
		Detail      string             `json:"detail"`
		Changed     *bool              `json:"changed"`
		Uncertain   bool               `json:"uncertain"`
		Warnings    []launchWarning    `json:"warnings"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return errors.New("execution returned an invalid event")
	}
	if event.OperationID != "" {
		w.result.OperationID = event.OperationID
	}
	for _, warning := range event.Warnings {
		if len(w.result.Warnings) < 8 && !slices.Contains(w.result.Warnings, warning.Code) {
			w.result.Warnings = append(w.result.Warnings, warning.Code)
		}
	}
	if event.Event == "started" {
		w.started = true
		w.result.Outcome = "unknown"
	}
	if event.Result != nil {
		w.result.ExitCode = &event.Result.Code
		w.result.Outcome = "exited"
	}
	if event.ErrorCode != "" {
		w.result.ErrorCode, w.result.Detail = event.ErrorCode, event.Detail
		w.result.Outcome = "failed"
		if event.Changed != nil && !*event.Changed && !w.started {
			w.result.Outcome = "not_started"
		}
		if event.Uncertain {
			w.result.Outcome = "unknown"
		}
	}
	if len(event.Data) > w.remaining {
		w.result.Truncated = true
		event.Data = event.Data[:w.remaining]
	}
	w.remaining -= len(event.Data)
	if event.Stream == "stderr" {
		w.result.Stderr += string(event.Data)
	} else {
		w.result.Stdout += string(event.Data)
	}
	return nil
}

func (w *mcpExecOutput) finish(err error) mcpserver.ExecResult {
	if len(w.pending) > 0 {
		err = errors.Join(err, io.ErrUnexpectedEOF)
	}
	_, exitOnly := err.(exitCodeError)
	if err != nil && !exitOnly && w.result.ErrorCode == "" {
		w.result.ErrorCode, w.result.Detail = "exec_failed", safeExecError(err)
		if w.result.ExitCode == nil {
			w.result.Outcome = "unknown"
		}
		var notStarted *execNotStartedError
		if errors.As(err, &notStarted) {
			w.result.Outcome = "not_started"
		}
	}
	return w.result
}
