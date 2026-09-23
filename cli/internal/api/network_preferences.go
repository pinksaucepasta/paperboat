package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

type NetworkPreferences struct {
	Revision           int64   `json:"revision"`
	DeviceSuffix       *string `json:"device_suffix"`
	DeviceLoopbackCIDR *string `json:"device_loopback_cidr"`
	SelectedTeamID     *string `json:"selected_team_id"`
	TeamID             string  `json:"team_id,omitempty"`
}
type NetworkPreferenceValues struct {
	DeviceSuffix       string `json:"device_suffix"`
	DeviceLoopbackCIDR string `json:"device_loopback_cidr"`
}
type EffectiveNetworkPreferences struct {
	Account                 NetworkPreferences      `json:"account"`
	SelectedTeam            *NetworkPreferences     `json:"selected_team"`
	SelectedTeamUnavailable bool                    `json:"selected_team_unavailable"`
	Effective               NetworkPreferenceValues `json:"effective"`
	Sources                 NetworkPreferenceValues `json:"sources"`
}
type SetNetworkPreferences struct {
	ExpectedRevision   int64   `json:"expected_revision"`
	DeviceSuffix       *string `json:"device_suffix"`
	DeviceLoopbackCIDR *string `json:"device_loopback_cidr"`
	SelectedTeamID     *string `json:"selected_team_id,omitempty"`
}

func networkPreferencesRoute(team string) string {
	if team != "" {
		return "/v1/teams/" + url.PathEscape(team) + "/network-preferences"
	}
	return "/v1/account/network-preferences"
}
func (c *Client) NetworkPreferences(ctx context.Context, team string) (out NetworkPreferences, err error) {
	err = c.do(ctx, http.MethodGet, networkPreferencesRoute(team), nil, &out)
	return
}
func (c *Client) SetNetworkPreferences(ctx context.Context, team string, value SetNetworkPreferences) (out NetworkPreferences, err error) {
	err = c.do(ctx, http.MethodPut, networkPreferencesRoute(team), value, &out)
	return
}
func (c *Client) EffectiveNetworkPreferences(ctx context.Context) (out EffectiveNetworkPreferences, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/account/network-preferences/effective", nil, &out)
	return
}

// Desktop management operations use fixed resource paths and the existing
// authenticated client, never a renderer-supplied URL or method.
func (c *Client) MachineUpdateStatus(ctx context.Context, id string) (out map[string]any, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/update-status", nil, &out)
	return
}
func (c *Client) MachineUpdateSummary(ctx context.Context) (out map[string]any, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/machines/update-summary", nil, &out)
	return
}
func (c *Client) SetMachineMetadata(ctx context.Context, id, alias, description string) (out UserMachine, err error) {
	err = c.do(ctx, http.MethodPatch, "/v1/machines/"+url.PathEscape(id), map[string]string{"alias": alias, "description": description}, &out)
	return
}
func (c *Client) RequestMachineMaintenance(ctx context.Context, id, operation, action, version, reason string) (out map[string]any, err error) {
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/maintenance-approvals", map[string]string{"action": action, "target_version": version, "reason": reason}, &out, http.Header{"Idempotency-Key": {operation}})
	return
}
func (c *Client) MachineMaintenanceApprovals(ctx context.Context, id string) (out map[string]any, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/maintenance-approvals", nil, &out)
	return
}
func (c *Client) DecideMachineMaintenance(ctx context.Context, id, approval, decision string) (out map[string]any, err error) {
	if decision != "approve" && decision != "reject" {
		return nil, errors.New("select approve or reject")
	}
	err = c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/maintenance-approvals/"+url.PathEscape(approval)+"/"+decision, nil, &out)
	return
}
func (c *Client) ManagementSessions(ctx context.Context, offset int) (out map[string]any, err error) {
	if offset < 0 {
		return nil, errors.New("invalid session page")
	}
	err = c.do(ctx, http.MethodGet, "/v1/auth/cli-client-sessions?state=active&limit=50&offset="+strconv.Itoa(offset), nil, &out)
	return
}
func (c *Client) RevokeManagementSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/auth/cli-client-sessions/"+url.PathEscape(id), nil, nil)
}
