package browserterminal

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// RoleRecording marks a console session-recording replay channel: the browser reaches the recording's machine over
// the same E2E browser transport as a terminal, admitted by one recording instead of a terminal session. The replay
// audit event is written before the ticket is issued (recordings.Service.Replay); permission is checked again at
// admission.
const RoleRecording = "recording"

// RecordingReplays answers whether account may replay recording now, and where it is.
type RecordingReplays interface {
	ResolveReplay(ctx context.Context, account, recordingID string) (team, machine, owner string, err error)
}

// RecordingAuthorizer resolves a recording replay channel.
type RecordingAuthorizer interface {
	ResolveRecording(ctx context.Context, userID, recordingID string) (Decision, error)
}

// IssueRecording issues a single-use ticket bound to the user, their browser session and key, and one recording.
func (g *Gateway) IssueRecording(ctx context.Context, userID, browserSessionID, recordingID, origin, browserPublicKeySHA256 string) (Ticket, string, Decision, error) {
	return g.issue(ctx, Ticket{UserID: userID, BrowserSessionID: browserSessionID, RecordingID: recordingID, Role: RoleRecording, Origin: origin, BrowserPublicKeySHA256: browserPublicKeySHA256})
}

// ResolveRecording joins the recording's replay permission with its machine's browser route. A recording channel
// has no generation to fence; its permission is checked again at every admission. OwnerAccountID is the machine's
// account, whose transport root the browser pins, not the viewer's.
func (a MachineAuthorizer) ResolveRecording(ctx context.Context, userID, recordingID string) (Decision, error) {
	if a.Recordings == nil || a.Service == nil {
		return Decision{}, ErrHostUnavailable
	}
	team, machine, owner, err := a.Recordings.ResolveReplay(ctx, userID, recordingID)
	if err != nil {
		return Decision{}, err
	}
	route, err := a.Service.ResolveMachineBrowserRoute(ctx, machine)
	if err != nil {
		if errors.Is(err, usermachines.ErrMachineCapabilityUnavailable) {
			return Decision{}, ErrHostUnavailable
		}
		return Decision{}, err
	}
	return Decision{UserID: userID, OwnerAccountID: owner, WorkspaceID: team, RecordingID: recordingID, MachineID: route.MachineID, EnvironmentID: route.EnvironmentID,
		HostWSSURL: route.HostWSSURL, EdgeNodeID: route.EdgeNodeID, EdgeProcessEpoch: route.EdgeProcessEpoch, EdgeEndpointHost: route.EdgeEndpointHost, Role: RoleRecording, PolicyGeneration: 1}, nil
}

// admitRecordingEdge mints the replay channel's host credential: the recording_replay class with the browser
// attachment and key bound. The edge hub key is the attachment ID.
func (g *Gateway) admitRecordingEdge(ticket Ticket, decision Decision, attachmentID string) (edgeAdmission, error) {
	now := time.Now().UTC()
	jti, err := secureID("token")
	if err != nil {
		return edgeAdmission{}, err
	}
	issuer, err := admission.New(g.Signer, recording.TicketLifetime)
	if err != nil {
		return edgeAdmission{}, err
	}
	credential, err := issuer.Issue(mint.CredentialInput{Issuer: g.Issuer, Audience: "paperboat-machine", Subject: ticket.UserID, JTI: jti, IssuedAt: now, ExpiresAt: now.Add(recording.TicketLifetime),
		CredentialClass: recording.CredentialClass, Scopes: []string{recording.Scope}, EnvironmentID: decision.EnvironmentID, AccountID: ticket.UserID, UserID: ticket.UserID,
		WorkspaceID: decision.WorkspaceID, MachineID: decision.MachineID, ResourceKind: recording.ResourceKind, ResourceID: ticket.RecordingID,
		BrowserAttachmentID: attachmentID, BrowserPublicKeySHA256: ticket.BrowserPublicKeySHA256})
	if err != nil {
		return edgeAdmission{}, err
	}
	return edgeAdmission{Ticket: ticket, Decision: decision, Credential: credential.Token, AttachmentID: attachmentID}, nil
}
