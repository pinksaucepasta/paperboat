package server

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// OfflinePeerConn is a native stream the host admitted from its own signed session scope while
// the client could not reach Paperboat (F21-11). The stream carries no server credential, so its
// requests are authorized from the admitted peer instead.
type OfflinePeerConn interface {
	OfflinePeer() (accountID, endpointID, consumer, operationID string, deadline time.Time)
}

// offlineAuthorizer admits only the one session the stream was opened for: a new terminal session
// owned by the peer, named by the operation, or the one exec operation. Nothing else (other
// sessions, file transfers, previews, ENV) is offline.
type offlineAuthorizer struct {
	accountID, endpointID, consumer, operationID string
	deadline                                     time.Time
}

func newOfflineAuthorizer(conn OfflinePeerConn) (*offlineAuthorizer, error) {
	accountID, endpointID, consumer, operationID, deadline := conn.OfflinePeer()
	if accountID == "" || endpointID == "" || operationID == "" || consumer != "terminal" && consumer != "exec" {
		return nil, ErrCredentialPolicy
	}
	return &offlineAuthorizer{accountID: accountID, endpointID: endpointID, consumer: consumer, operationID: operationID, deadline: deadline}, nil
}

func (a *offlineAuthorizer) Authorize(_ context.Context, frame protocol.Frame) (Authorization, error) {
	terminalFrame := frame.Capability == "terminal.v1" || frame.Capability == "health.v1" || frame.Type == "ack" || frame.Type == "detach"
	execFrame := frame.Capability == "exec.v1" && frame.OperationID == a.operationID
	if a.consumer == "terminal" && !terminalFrame || a.consumer == "exec" && !execFrame || time.Now().After(a.deadline) {
		return Authorization{}, ErrCredentialPolicy
	}
	authorization := Authorization{
		JournalBinding: "offline\x00" + a.accountID + "\x00" + a.endpointID + "\x00" + a.operationID,
		UserID:         a.accountID,
		AccountID:      a.accountID,
		ActorAccountID: a.accountID,
		ClientID:       a.endpointID,
		OperationID:    a.operationID,
		ExpiresAt:      a.deadline,
	}
	if a.consumer == "terminal" {
		authorization.SessionID = a.operationID
		authorization.TerminalRole = TerminalRoleOwner
	}
	return authorization, nil
}
