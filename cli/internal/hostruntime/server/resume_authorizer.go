package server

import (
	"context"
	"sync/atomic"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// ResumePeerConn is a native terminal stream admitted, from the host's current authority, to move
// one existing attachment without a fresh credential.
type ResumePeerConn interface {
	ResumePeer() (accountID, endpointID, sessionID, resource string, standing bool)
}

// TerminalResume marks a connection that must prove an attachment's resume secret (held only by the
// PTY owner) before any terminal request runs on it.
type TerminalResume struct {
	Resource string
	Standing bool
	resumed  atomic.Bool
}

// resumeAuthorizer names the peer the native session proved as the terminal owner of one session.
// Until a resume succeeds, the dispatcher accepts only that resume attach.
type resumeAuthorizer struct {
	accountID, endpointID, sessionID string
	resume                           *TerminalResume
}

func newResumeAuthorizer(conn ResumePeerConn) (*resumeAuthorizer, error) {
	accountID, endpointID, sessionID, resource, standing := conn.ResumePeer()
	if accountID == "" || endpointID == "" || sessionID == "" || resource == "" {
		return nil, ErrCredentialPolicy
	}
	return &resumeAuthorizer{accountID: accountID, endpointID: endpointID, sessionID: sessionID, resume: &TerminalResume{Resource: resource, Standing: standing}}, nil
}

func (a *resumeAuthorizer) Authorize(_ context.Context, frame protocol.Frame) (Authorization, error) {
	if frame.Capability != "terminal.v1" && frame.Type != "ack" && frame.Type != "detach" {
		return Authorization{}, ErrCredentialPolicy
	}
	return Authorization{
		JournalBinding: "resume\x00" + a.accountID + "\x00" + a.endpointID + "\x00" + a.sessionID,
		UserID:         a.accountID,
		AccountID:      a.accountID,
		ActorAccountID: a.accountID,
		ClientID:       a.endpointID,
		SessionID:      a.sessionID,
		TerminalRole:   TerminalRoleOwner,
		ResourceID:     a.resume.Resource,
		Resume:         a.resume,
	}, nil
}
