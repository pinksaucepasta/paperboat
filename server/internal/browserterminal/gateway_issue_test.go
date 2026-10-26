package browserterminal

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

type issueTestTickets struct{}

func (issueTestTickets) Issue(_ context.Context, ticket Ticket) (Ticket, error) {
	ticket.Value = "one-use-ticket"
	return ticket, nil
}
func (issueTestTickets) ConsumeEdge(context.Context, string, string) (Ticket, error) {
	return Ticket{}, ErrTicketInvalid
}
func (issueTestTickets) SessionActive(context.Context, string, string) (bool, error) {
	return true, nil
}

func TestGatewayIssuesSelectedEdgeHostname(t *testing.T) {
	decision := Decision{WorkspaceID: "personal", UserID: "usr_1", OwnerAccountID: "usr_1", TerminalSessionID: "term_1", MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", EdgeEndpointHost: "edge-one.example.test", Role: "owner", PolicyGeneration: 1}
	authorizer := &testBrowserAuthorizer{decision: decision}
	gateway := &Gateway{Tickets: issueTestTickets{}, Authorizer: authorizer, AllowedOrigins: []string{"https://dashboard.example.test"}}
	_, streamURL, issued, err := gateway.Issue(context.Background(), "usr_1", "browser_1", "term_1", "https://dashboard.example.test", testBrowserKeyDigest)
	if err != nil {
		t.Fatal(err)
	}
	if streamURL != "wss://edge-one.example.test/v1/browser-terminal/machine.runtime.example.test" || issued.EdgeNodeID != "edge_1" {
		t.Fatalf("stream URL %q or edge ID %q did not match selected edge", streamURL, issued.EdgeNodeID)
	}
	authorizer.decision.EdgeEndpointHost = "edge-two.example.test"
	_, streamURL, _, err = gateway.Issue(context.Background(), "usr_1", "browser_1", "term_1", "https://dashboard.example.test", testBrowserKeyDigest)
	if err != nil || streamURL != "wss://edge-two.example.test/v1/browser-terminal/machine.runtime.example.test" {
		t.Fatalf("fresh ticket did not follow selected edge: %q, %v", streamURL, err)
	}
}

func TestGatewayInvalidAuthoritativeHostPreservesPrivateParseCause(t *testing.T) {
	const privateHost = "private-machine-host"
	decision := Decision{WorkspaceID: "personal", UserID: "usr_1", OwnerAccountID: "usr_1", TerminalSessionID: "term_1", MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://" + privateHost + "%zz/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", EdgeEndpointHost: "edge-one.example.test", Role: "owner", PolicyGeneration: 1}
	gateway := &Gateway{Tickets: issueTestTickets{}, Authorizer: &testBrowserAuthorizer{decision: decision}, AllowedOrigins: []string{"https://dashboard.example.test"}}

	_, _, _, err := gateway.Issue(context.Background(), "usr_1", "browser_1", "term_1", "https://dashboard.example.test", testBrowserKeyDigest)
	var parseError *url.Error
	if !errors.Is(err, ErrHostUnavailable) || errors.Is(err, ErrForbidden) || !errors.As(err, &parseError) {
		t.Fatalf("invalid authoritative route error = %T %v; want unavailable with retained URL parse cause", err, err)
	}
	if strings.Contains(err.Error(), privateHost) {
		t.Fatalf("private route detail appeared in safe error: %v", err)
	}
}

type quotaTestAuthorizer struct {
	testBrowserAuthorizer
	quota error
}

func (a *quotaTestAuthorizer) CheckBandwidth(context.Context, Decision) error { return a.quota }

func TestGatewayRefusesTicketAtTheBandwidthCap(t *testing.T) {
	decision := Decision{WorkspaceID: "personal", UserID: "usr_1", OwnerAccountID: "usr_1", TerminalSessionID: "term_1", MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", EdgeEndpointHost: "edge-one.example.test", Role: "owner", PolicyGeneration: 1}
	authorizer := &quotaTestAuthorizer{testBrowserAuthorizer: testBrowserAuthorizer{decision: decision}, quota: &usermachines.BandwidthQuotaError{}}
	gateway := &Gateway{Tickets: issueTestTickets{}, Authorizer: authorizer, AllowedOrigins: []string{"https://dashboard.example.test"}}
	issue := func() error {
		_, _, _, err := gateway.Issue(context.Background(), "usr_1", "browser_1", "term_1", "https://dashboard.example.test", testBrowserKeyDigest)
		return err
	}
	var refusal *usermachines.BandwidthQuotaError
	if err := issue(); !errors.As(err, &refusal) {
		t.Fatalf("ticket at the cap: %v", err)
	}
	authorizer.quota = errors.New("store down")
	if err := issue(); !errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("quota outage: %v", err)
	}
	authorizer.quota = nil
	if err := issue(); err != nil {
		t.Fatalf("ticket under the cap: %v", err)
	}
}

type resolveErrAuthorizer struct {
	testBrowserAuthorizer
	err error
}

func (a *resolveErrAuthorizer) ResolveRole(ctx context.Context, user, terminal string) (Decision, error) {
	if a.err != nil {
		return Decision{}, a.err
	}
	return a.testBrowserAuthorizer.ResolveRole(ctx, user, terminal)
}

// Slice 7 (Lane K terminal plan): the refusal says what is wrong, so the console does not show "No access" for a
// machine whose edge connection is down or an owner's closed terminal.
func TestGatewayIssueNamesWhyATerminalCannotBeOpened(t *testing.T) {
	routed := Decision{WorkspaceID: "personal", UserID: "usr_1", OwnerAccountID: "usr_1", TerminalSessionID: "term_1", MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", EdgeEndpointHost: "edge-one.example.test", Role: "owner", PolicyGeneration: 1}
	unrouted := routed
	unrouted.HostWSSURL = ""
	for _, tc := range []struct {
		name     string
		decision Decision
		err      error
		want     error
		not      []error
	}{
		{"no route", unrouted, nil, ErrMachineEdgeUnavailable, []error{ErrHostUnavailable, ErrForbidden}},
		{"edge node not ready", routed, usermachines.ErrMachineCapabilityUnavailable, ErrMachineEdgeUnavailable, []error{ErrForbidden}},
		{"owner's terminal closed", routed, usermachines.ErrTerminalBrowserSessionClosed, ErrSessionClosed, []error{ErrForbidden}},
		{"not found", routed, usermachines.ErrTerminalSessionNotFound, ErrForbidden, []error{ErrMachineEdgeUnavailable, ErrSessionClosed}},
		{"store down", routed, errors.New("store down"), ErrHostUnavailable, []error{ErrMachineEdgeUnavailable, ErrSessionClosed, ErrForbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := &resolveErrAuthorizer{testBrowserAuthorizer: testBrowserAuthorizer{decision: tc.decision}, err: tc.err}
			gateway := &Gateway{Tickets: issueTestTickets{}, Authorizer: authorizer, AllowedOrigins: []string{"https://dashboard.example.test"}}
			_, _, _, err := gateway.Issue(context.Background(), "usr_1", "browser_1", "term_1", "https://dashboard.example.test", testBrowserKeyDigest)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, other := range tc.not {
				if errors.Is(err, other) {
					t.Fatalf("err = %v also matches %v", err, other)
				}
			}
		})
	}
}
