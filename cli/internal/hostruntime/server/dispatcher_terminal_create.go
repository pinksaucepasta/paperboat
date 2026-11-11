package server

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func (d *Dispatcher) createTerminal(ctx context.Context, authorization Authorization, request terminalRequest) operation.Outcome {
	ctx = diagnostics.WithReconnect(ctx, rand.Text(), "target", "create-or-get")
	if authorization.BrowserTerminal {
		return failure("not_found_or_forbidden")
	}
	if request.SessionID == "" {
		request.SessionID = authorization.SessionID
	}
	cwdEnd := diagnostics.ReconnectStep(ctx, "create_cwd")
	cwd, ok := d.cwd(request.CWD)
	cwdEnd(nil)
	if !ok {
		return failure("invalid_terminal_workspace")
	}
	if request.Columns == 0 || request.Rows == 0 {
		return failure("invalid_terminal_dimensions")
	}
	launchEnd := diagnostics.ReconnectStep(ctx, "create_launch")
	value, err := d.config.SessionLauncher.Launch(ctx, process.LaunchRequest{ID: request.SessionID, Name: request.Name, CWD: cwd, Dimensions: pty.Dimensions{Columns: request.Columns, Rows: request.Rows}, Environment: request.Environment})
	launchEnd(err)
	if err != nil && !allErrorLeavesMatch(err, func(leaf error) bool { return leaf == session.ErrSessionExists }) {
		outcome := domainResult(nil, err)
		if outcome.ErrorCode == "unavailable" {
			fault := errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "sessions", "command", "terminal_session_failed", err)
			return failureDetails("terminal_create_failed", struct {
				ErrorType        string `json:"error_type"`
				Errno            int    `json:"errno"`
				SupportReference string `json:"support_reference"`
			}{fault.ErrorType, fault.Errno, fault.SupportReference})
		}
		return outcome
	}
	if request.ExistingSnapshot {
		// create-or-get lets one round trip both create a fresh session
		// and resolve an already-running one. A name collision without a
		// matching session ID keeps the original failure so clients
		// cannot claim another session's name.
		if errors.Is(err, session.ErrSessionExists) {
			snapshotEnd := diagnostics.ReconnectStep(ctx, "create_existing_snapshot")
			snapshot, snapshotErr := d.config.Sessions.Snapshot(request.SessionID)
			snapshotEnd(snapshotErr)
			if snapshotErr == nil {
				return result(struct {
					session.Snapshot
					Existing bool `json:"existing"`
				}{Snapshot: snapshot, Existing: true})
			}
		}
		if err == nil {
			return result(struct {
				session.Snapshot
				Existing bool `json:"existing"`
			}{Snapshot: value, Existing: false})
		}
	}
	return domainResult(value, err)
}

type terminalRequest struct {
	Action       string `json:"action"`
	OperationID  string `json:"operation_id,omitempty"`
	Name         string `json:"name,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	AttachmentID string `json:"attachment_id,omitempty"`
	// AttachmentLease names which attach of AttachmentID a detach ends; see attachmentControl.
	AttachmentLease uint64            `json:"attachment_lease,omitempty"`
	FromSequence    uint64            `json:"from_sequence,omitempty"`
	AtLiveBoundary  bool              `json:"at_live_boundary,omitempty"`
	Generation      uint64            `json:"generation,omitempty"`
	Columns         uint16            `json:"columns,omitempty"`
	Rows            uint16            `json:"rows,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	Signal          string            `json:"signal,omitempty"`
	// ExistingSnapshot asks create to return the current snapshot instead of
	// failing when the session already exists. Responses to such creates carry
	// an "existing" flag so the client can pick the right attach boundary.
	ExistingSnapshot bool `json:"existing_snapshot,omitempty"`
	// ResumeSecret proves a resume attach; see TerminalResume.
	ResumeSecret []byte `json:"resume_secret,omitempty"`
}

type TerminalJoin struct {
	AccessSessionID   string `json:"access_session_id"`
	TerminalSessionID string `json:"terminal_session_id"`
	AttachmentID      string `json:"attachment_id"`
}
type TerminalJoinRecorder func(context.Context, TerminalJoin) error
