package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

type TeamMember struct {
	AccountID            string `json:"account_id"`
	MembershipGeneration uint64 `json:"membership_generation"`
	Role                 string `json:"role"`
	Active               bool   `json:"active"`
}
type TeamGrant struct {
	AccountID    string `json:"account_id"`
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
	Permission   string `json:"permission"`
	Generation   uint64 `json:"generation"`
	Active       bool   `json:"active"`
}
type Team struct {
	Name                  string                     `json:"name"`
	Suspended             bool                       `json:"suspended"`
	TerminalSessionGrants []TeamTerminalSessionGrant `json:"terminal_session_grants"`
	ENVStatus             string                     `json:"env_status"`
	TeamID                string                     `json:"team_id"`
	OwnerAccount          string                     `json:"owner_account"`
	Generation            uint64                     `json:"generation"`
	Deleted               bool                       `json:"deleted"`
	Members               []TeamMember               `json:"members"`
	Grants                []TeamGrant                `json:"grants"`
	Machines              []TeamMachineBinding       `json:"machines"`
	MachineGrants         []TeamMachineGrant         `json:"machine_grants"`
	// ApproveNewMachines: machines that members add wait for an owner or admin.
	ApproveNewMachines bool                   `json:"approve_new_machines"`
	MachineRequests    []TeamMachineAdmission `json:"machine_requests"`
	// AllowPrivateAccess: members may reach the ports of machines shared into the team.
	AllowPrivateAccess bool `json:"allow_private_access"`
}

// TeamMachineAdmission is a machine waiting for (or refused by) a team's owners and admins.
type TeamMachineAdmission struct {
	MachineID   string    `json:"machine_id"`
	Alias       string    `json:"alias"`
	RequestedBy string    `json:"requested_by"`
	State       string    `json:"state"`
	RequestedAt time.Time `json:"requested_at"`
	DecidedBy   string    `json:"decided_by,omitempty"`
}
type TeamTerminalSessionGrant struct {
	TerminalSessionID string `json:"terminal_session_id"`
	Audience          string `json:"audience"`
	AccountID         string `json:"account_id,omitempty"`
	Role              string `json:"role"`
	Generation        uint64 `json:"generation"`
	Active            bool   `json:"active"`
}

// TeamCreateRequest reserves a Team and asks for Checkout of its purchased seats;
// the Team exists only after payment succeeds. The Idempotency-Key equals OperationID.
type TeamCreateRequest struct {
	OperationID string `json:"operation_id"`
	TeamID      string `json:"team_id"`
	Seats       int    `json:"seats"`
	Interval    string `json:"interval"`
	SuccessURL  string `json:"success_url"`
	CancelURL   string `json:"cancel_url"`
}

// TeamCheckout is the Stripe Checkout address that completes Team creation.
type TeamCheckout struct {
	URL string `json:"url"`
}
type TeamMutationRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	Action             string `json:"action"`
	AccountID          string `json:"account_id"`
	Role               string `json:"role"`
	Confirmation       string `json:"confirmation"`
	// ApproveNewMachines is the value for the machine_approval action.
	ApproveNewMachines *bool `json:"approve_new_machines,omitempty"`
}
type TeamInviteRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	AccountID          string `json:"account_id,omitempty"`
	Email              string `json:"email,omitempty"`
}
type TeamAcceptRequest struct {
	OperationID string `json:"operation_id"`
}
type TeamInvitation struct {
	InvitationID string    `json:"invitation_id"`
	TeamID       string    `json:"team_id"`
	AccountID    string    `json:"account_id"`
	Email        string    `json:"email,omitempty"`
	Role         string    `json:"role"`
	ExpiresAt    time.Time `json:"expires_at"`
	Generation   uint64    `json:"generation"`
	State        string    `json:"state,omitempty"`
}
type TeamGrantRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	AccountID          string `json:"account_id"`
	ResourceKind       string `json:"resource_kind"`
	ResourceID         string `json:"resource_id"`
	Permission         string `json:"permission"`
	Active             bool   `json:"active"`
}
type TeamAttachRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	ResourceKind       string `json:"resource_kind"`
	ResourceID         string `json:"resource_id"`
	Active             bool   `json:"active"`
}

func (c *Client) ListTeams(ctx context.Context) ([]Team, error) {
	var out []Team
	err := c.do(ctx, http.MethodGet, "/v1/teams", nil, &out)
	return out, err
}
func (c *Client) GetTeam(ctx context.Context, id string) (Team, error) {
	var out Team
	err := c.do(ctx, http.MethodGet, "/v1/teams/"+url.PathEscape(id), nil, &out)
	return out, err
}
func (c *Client) CreateTeam(ctx context.Context, in TeamCreateRequest) (TeamCheckout, error) {
	var out TeamCheckout
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) MutateTeam(ctx context.Context, id string, in TeamMutationRequest) (Team, error) {
	var out Team
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(id)+"/actions", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) InviteTeamMember(ctx context.Context, id string, in TeamInviteRequest) (TeamInvitation, error) {
	var out TeamInvitation
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(id)+"/invitations", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) AcceptTeamInvitation(ctx context.Context, id string, in TeamAcceptRequest) (Team, error) {
	var out Team
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/team-invitations/"+url.PathEscape(id)+"/accept", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) DeclineTeamInvitation(ctx context.Context, id string, in TeamAcceptRequest) (TeamInvitation, error) {
	var out TeamInvitation
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/team-invitations/"+url.PathEscape(id)+"/decline", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) CancelTeamInvitation(ctx context.Context, team, invitation string, in TeamMutationRequest) (Team, error) {
	var out Team
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(team)+"/invitations/"+url.PathEscape(invitation)+"/cancel", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) GrantTeamResource(ctx context.Context, id string, in TeamGrantRequest) (Team, error) {
	var out Team
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(id)+"/grants", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) AttachTeamResource(ctx context.Context, id string, in TeamAttachRequest) (Team, error) {
	var out Team
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(id)+"/resources", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func teamHeaders(operation string) http.Header {
	return http.Header{"Idempotency-Key": []string{operation}}
}

type TeamMachineBinding struct {
	MachineID              string   `json:"machine_id"`
	OwnerAccount           string   `json:"owner_account"`
	OwnerTeamID            string   `json:"owner_team_id,omitempty"`
	Alias                  string   `json:"alias"`
	State                  string   `json:"state"`
	Online                 bool     `json:"online"`
	ConfiguredCapabilities []string `json:"configured_capabilities"`
	Generation             uint64   `json:"generation"`
	Active                 bool     `json:"active"`
}
type TeamMachineGrant struct {
	MachineID    string   `json:"machine_id"`
	Audience     string   `json:"audience"`
	AccountID    string   `json:"account_id,omitempty"`
	Capabilities []string `json:"capabilities"`
	Generation   uint64   `json:"generation"`
	Active       bool     `json:"active"`
}
type TeamMachineRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	MachineID          string `json:"machine_id"`
	Action             string `json:"action"`
	Confirmation       string `json:"confirmation,omitempty"`
}
type TeamMachineGrantRequest struct {
	OperationID        string   `json:"operation_id"`
	ExpectedGeneration uint64   `json:"expected_generation"`
	MachineID          string   `json:"machine_id"`
	Audience           string   `json:"audience"`
	AccountID          string   `json:"account_id,omitempty"`
	Capabilities       []string `json:"capabilities"`
	Active             bool     `json:"active"`
}

func (c *Client) MutateTeamMachine(ctx context.Context, team string, in TeamMachineRequest) (Team, error) {
	var out Team
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(team)+"/machines", in, &out, teamHeaders(in.OperationID))
	return out, err
}
func (c *Client) GrantTeamMachine(ctx context.Context, team string, in TeamMachineGrantRequest) (Team, error) {
	var out Team
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(team)+"/machine-grants", in, &out, teamHeaders(in.OperationID))
	return out, err
}

// TeamInvitations discovers invitations bound to this account, or administered
// by the account in the explicitly selected team. Resources remain authorized
// by the server before returning their invitation IDs.
func (c *Client) TeamInvitations(ctx context.Context, team string) ([]TeamInvitation, error) {
	items, _, err := c.TeamInvitationsPage(ctx, team, nil, 0)
	return items, err
}

// TeamInvitationsPage reads at most maxItems pending invitations (0: all) and the server's total (PQ-04).
func (c *Client) TeamInvitationsPage(ctx context.Context, team string, extra url.Values, maxItems int) ([]TeamInvitation, int, error) {
	path := "/v1/team-invitations"
	filters := url.Values{"state": {"pending"}}
	if team != "" {
		path = "/v1/teams/" + url.PathEscape(team) + "/invitations"
	} else {
		filters.Set("owner", "mine")
	}
	if q := extra.Get("q"); q != "" {
		filters.Set("q", q)
	}
	return collectOffsetInventoryPage[TeamInvitation](ctx, c, path, "items", filters, maxItems)
}
func (c *Client) GetTeamInvitation(ctx context.Context, id string) (TeamInvitation, error) {
	var out TeamInvitation
	err := c.do(ctx, http.MethodGet, "/v1/team-invitations/"+url.PathEscape(id), nil, &out)
	return out, err
}
