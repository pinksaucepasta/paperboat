package browserterminal

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/inspectaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// RoleInspector marks a console inspector channel (F17): the browser reaches
// the preview machine's inspector over the same E2E browser transport as a
// terminal, admitted by one inspector credential instead of a terminal session.
const RoleInspector = "inspector"

// InspectorAuthorizer resolves a console inspector channel from one inspector
// credential held by the caller.
type InspectorAuthorizer interface {
	ResolveInspector(ctx context.Context, userID, credentialID string) (Decision, error)
}

// IssueInspector issues a single-use ticket bound to the user, their browser
// session and key, one live inspector credential and its preview's machine.
func (g *Gateway) IssueInspector(ctx context.Context, userID, browserSessionID, credentialID, origin, browserPublicKeySHA256 string) (Ticket, string, Decision, error) {
	return g.issue(ctx, Ticket{UserID: userID, BrowserSessionID: browserSessionID, InspectorCredentialID: credentialID, Role: RoleInspector, Origin: origin, BrowserPublicKeySHA256: browserPublicKeySHA256})
}

// ResolveInspector joins the inspector credential authority with the
// machine's browser route.
func (a MachineAuthorizer) ResolveInspector(ctx context.Context, userID, credentialID string) (Decision, error) {
	if a.Inspector == nil || a.Service == nil {
		return Decision{}, ErrHostUnavailable
	}
	credential, err := a.Inspector.ResolveBrowserCredential(ctx, userID, credentialID)
	if err != nil {
		if inspectaccess.IsExactExpectedRejection(err) {
			return Decision{}, ErrForbidden
		}
		return Decision{}, err
	}
	if credential.ResourceGeneration < 1 || credential.ResourceGeneration > math.MaxInt64 {
		return Decision{}, ErrForbidden
	}
	route, err := a.Service.ResolveMachineBrowserRoute(ctx, credential.MachineID)
	if err != nil {
		if errors.Is(err, usermachines.ErrMachineCapabilityUnavailable) {
			return Decision{}, ErrHostUnavailable
		}
		return Decision{}, err
	}
	return Decision{UserID: userID, OwnerAccountID: credential.OwnerAccountID, WorkspaceID: credential.WorkspaceID, InspectorCredentialID: credentialID, MachineID: route.MachineID, EnvironmentID: route.EnvironmentID, HostWSSURL: route.HostWSSURL, EdgeNodeID: route.EdgeNodeID, EdgeProcessEpoch: route.EdgeProcessEpoch, EdgeEndpointHost: route.EdgeEndpointHost, Role: RoleInspector, PolicyGeneration: int64(credential.ResourceGeneration)}, nil
}

// admitInspectorEdge mints the inspector channel's host credential. There is
// no terminal process to snapshot; the edge hub key is the attachment ID.
func (g *Gateway) admitInspectorEdge(ticket Ticket, decision Decision) (edgeAdmission, error) {
	now := time.Now().UTC()
	attachmentID, err := secureID("attachment")
	if err != nil {
		return edgeAdmission{}, err
	}
	jti, err := secureID("token")
	if err != nil {
		return edgeAdmission{}, err
	}
	issuer, err := admission.New(g.Signer, g.AdmissionLifetime)
	if err != nil {
		return edgeAdmission{}, err
	}
	credential, err := issuer.Issue(mint.CredentialInput{Issuer: g.Issuer, Audience: "paperboat-machine", Subject: ticket.UserID, JTI: jti, IssuedAt: now, CredentialClass: "browser_inspector_operation", Scopes: []string{"inspector:use"}, EnvironmentID: decision.EnvironmentID, AccountID: ticket.UserID, MachineID: decision.MachineID, UserID: ticket.UserID, BrowserAttachmentID: attachmentID, BrowserPublicKeySHA256: ticket.BrowserPublicKeySHA256})
	if err != nil {
		return edgeAdmission{}, err
	}
	return edgeAdmission{Ticket: ticket, Decision: decision, Credential: credential.Token, AttachmentID: attachmentID}, nil
}
