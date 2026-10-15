package browserterminal

import (
	"context"
	"github.com/google/uuid"

	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

var ErrForbidden = errors.New("browser terminal access is unavailable")
var ErrHostUnavailable = errors.New("browser terminal host is unavailable")
var ErrBrowserKeyInvalid = errors.New("browser terminal key digest is invalid")

type Decision struct {
	GuestSessionID       string
	UserID               string
	OwnerAccountID       string
	WorkspaceID          string
	TerminalSessionID    string
	MachineID            string
	EnvironmentID        string
	HostWSSURL           string
	EdgeNodeID           string
	EdgeProcessEpoch     string
	EdgeEndpointHost     string
	Role                 string
	PolicyGeneration     int64
	TeamGeneration       int64
	MembershipGeneration int64
	BindingGeneration    int64
}

// Authorizer owns current browser policy and the host's observed PTY process
// generation. Neither a browser ticket nor a team binding is runtime proof.
type Authorizer interface {
	ResolveRole(context.Context, string, string) (Decision, error)
	SnapshotProcess(context.Context, Decision) (int64, error)
}

type TicketAuthority interface {
	Issue(context.Context, Ticket) (Ticket, error)
	ConsumeEdge(context.Context, string, string) (Ticket, error)
	SessionActive(context.Context, string, string) (bool, error)
}

type Gateway struct {
	AdmissionLifetime    time.Duration
	RecordUsageAdmission func(context.Context, string, string, string, string, string, time.Time) error
	Tickets              TicketAuthority
	Authorizer           Authorizer
	Signer               *mint.Provider
	Issuer               string
	AllowedOrigins       []string
	mu                   sync.Mutex
	active               map[string]int
	activeTotal          int
}

func (g *Gateway) Issue(ctx context.Context, userID, browserSessionID, terminalSessionID, origin, browserPublicKeySHA256 string) (Ticket, string, Decision, error) {
	return g.issue(ctx, Ticket{UserID: userID, BrowserSessionID: browserSessionID, TerminalSessionID: terminalSessionID, Origin: origin, BrowserPublicKeySHA256: browserPublicKeySHA256})
}
func (g *Gateway) IssueGuest(ctx context.Context, guestID, terminalSessionID, origin, browserPublicKeySHA256 string) (Ticket, string, Decision, error) {
	return g.issue(ctx, Ticket{GuestSessionID: guestID, TerminalSessionID: terminalSessionID, Origin: origin, BrowserPublicKeySHA256: browserPublicKeySHA256})
}
func (g *Gateway) issue(ctx context.Context, in Ticket) (Ticket, string, Decision, error) {
	userID, terminalSessionID, origin, browserPublicKeySHA256 := in.subjectID(), in.TerminalSessionID, in.Origin, in.BrowserPublicKeySHA256
	if !validPublicKeySHA256(browserPublicKeySHA256) {
		return Ticket{}, "", Decision{}, ErrBrowserKeyInvalid
	}
	if g == nil || g.Authorizer == nil || !slices.Contains(g.AllowedOrigins, origin) || origin == "" {
		return Ticket{}, "", Decision{}, ErrForbidden
	}
	decision, err := g.resolveTicketRole(ctx, in)
	if err != nil {
		if expectedTerminalRejection(err) {
			return Ticket{}, "", Decision{}, ErrForbidden
		}
		return Ticket{}, "", Decision{}, unavailableCause(err)
	}
	if !validDecision(decision, userID, terminalSessionID) {
		return Ticket{}, "", Decision{}, ErrForbidden
	}
	if decision.HostWSSURL == "" {
		return Ticket{}, "", Decision{}, ErrHostUnavailable
	}
	runtimeURL, err := url.Parse(decision.HostWSSURL)
	if err != nil {
		return Ticket{}, "", Decision{}, unavailableCause(err)
	}
	if runtimeURL.Scheme != "wss" || runtimeURL.Hostname() == "" || runtimeURL.Port() != "" || runtimeURL.Path != "/v1/browser-terminal" || runtimeURL.RawQuery != "" || runtimeURL.Fragment != "" || decision.EdgeEndpointHost == "" || strings.ContainsAny(decision.EdgeEndpointHost, "/:@?#") {
		return Ticket{}, "", Decision{}, ErrHostUnavailable
	}
	streamURL := (&url.URL{Scheme: "wss", Host: decision.EdgeEndpointHost, Path: "/v1/browser-terminal/" + runtimeURL.Hostname()}).String()
	ticket, err := g.Tickets.Issue(ctx, Ticket{GuestSessionID: in.GuestSessionID, UserID: in.UserID, BrowserSessionID: in.BrowserSessionID, TerminalSessionID: terminalSessionID, Origin: origin, Role: decision.Role, PolicyGeneration: decision.PolicyGeneration, TeamGeneration: decision.TeamGeneration, MembershipGeneration: decision.MembershipGeneration, BindingGeneration: decision.BindingGeneration, BrowserPublicKeySHA256: browserPublicKeySHA256})
	return ticket, streamURL, decision, err
}

func validDecision(d Decision, userID, terminalSessionID string) bool {
	return d.UserID == userID && d.TerminalSessionID == terminalSessionID && d.OwnerAccountID != "" && d.WorkspaceID != "" && d.MachineID != "" && d.EnvironmentID != "" && d.PolicyGeneration > 0 && d.TeamGeneration >= 0 && d.MembershipGeneration >= 0 && d.BindingGeneration >= 0 && validRole(d.Role)
}

func sameDecision(a, b Decision) bool {
	return validDecision(b, a.UserID, a.TerminalSessionID) && a.GuestSessionID == b.GuestSessionID && a.Role == b.Role && a.PolicyGeneration == b.PolicyGeneration && a.TeamGeneration == b.TeamGeneration && a.MembershipGeneration == b.MembershipGeneration && a.BindingGeneration == b.BindingGeneration && a.MachineID == b.MachineID && a.EnvironmentID == b.EnvironmentID && a.HostWSSURL == b.HostWSSURL && a.EdgeNodeID == b.EdgeNodeID && a.EdgeProcessEpoch == b.EdgeProcessEpoch && a.EdgeEndpointHost == b.EdgeEndpointHost && a.OwnerAccountID == b.OwnerAccountID && a.WorkspaceID == b.WorkspaceID
}

func scopeForRole(role string) string {
	switch role {
	case "owner":
		return "terminal:operate"
	case "interactive":
		return "terminal:control"
	default:
		return "terminal:view"
	}
}

func secureID(prefix string) (string, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return prefix + "_" + value.String(), nil
}

func (g *Gateway) acquire(browserSessionID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if browserSessionID == "" || g.activeTotal >= 256 || g.active[browserSessionID] >= 8 {
		return false
	}
	if g.active == nil {
		g.active = make(map[string]int)
	}
	g.active[browserSessionID]++
	g.activeTotal++
	return true
}

func (g *Gateway) release(browserSessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[browserSessionID] > 0 {
		g.active[browserSessionID]--
		g.activeTotal--
		if g.active[browserSessionID] == 0 {
			delete(g.active, browserSessionID)
		}
	}
}

// GuestAuthorizer resolves account and code guests through distinct authoritative boundaries.
type GuestAuthorizer interface {
	ResolveGuestRole(context.Context, string, string) (Decision, error)
}
type GuestSessionAuthority interface {
	GuestSessionActive(context.Context, string) (bool, error)
}

func (g *Gateway) resolveTicketRole(ctx context.Context, ticket Ticket) (Decision, error) {
	if ticket.GuestSessionID != "" {
		a, ok := g.Authorizer.(GuestAuthorizer)
		if !ok {
			return Decision{}, ErrForbidden
		}
		d, err := a.ResolveGuestRole(ctx, ticket.GuestSessionID, ticket.TerminalSessionID)
		if err != nil {
			return d, err
		}
		if d.GuestSessionID != ticket.GuestSessionID || d.Role == "owner" {
			return Decision{}, ErrForbidden
		}
		return d, nil
	}
	d, err := g.Authorizer.ResolveRole(ctx, ticket.UserID, ticket.TerminalSessionID)
	if err != nil {
		return d, err
	}
	if d.GuestSessionID != "" {
		return Decision{}, ErrForbidden
	}
	return d, nil
}
func (g *Gateway) ticketSessionActive(ctx context.Context, ticket Ticket) (bool, error) {
	if ticket.GuestSessionID != "" {
		s, ok := g.Tickets.(GuestSessionAuthority)
		if !ok {
			return false, ErrForbidden
		}
		return s.GuestSessionActive(ctx, ticket.GuestSessionID)
	}
	return g.Tickets.SessionActive(ctx, ticket.UserID, ticket.BrowserSessionID)
}
