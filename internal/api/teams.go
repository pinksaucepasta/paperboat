package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
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
	TeamID       string       `json:"team_id"`
	OwnerAccount string       `json:"owner_account"`
	Generation   uint64       `json:"generation"`
	Deleted      bool         `json:"deleted"`
	Members      []TeamMember `json:"members"`
	Grants       []TeamGrant  `json:"grants"`
}
type TeamCreateRequest struct {
	OperationID string `json:"operation_id"`
	TeamID      string `json:"team_id"`
}
type TeamMutationRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	Action             string `json:"action"`
	AccountID          string `json:"account_id"`
	Role               string `json:"role"`
	Confirmation       string `json:"confirmation"`
}
type TeamInviteRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	AccountID          string `json:"account_id"`
}
type TeamAcceptRequest struct {
	OperationID string `json:"operation_id"`
}
type TeamInvitation struct {
	InvitationID string    `json:"invitation_id"`
	TeamID       string    `json:"team_id"`
	AccountID    string    `json:"account_id"`
	Role         string    `json:"role"`
	ExpiresAt    time.Time `json:"expires_at"`
	Generation   uint64    `json:"generation"`
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
func (c *Client) CreateTeam(ctx context.Context, in TeamCreateRequest) (Team, error) {
	var out Team
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
func TeamGenerationString(generation uint64) string { return strconv.FormatUint(generation, 10) }
