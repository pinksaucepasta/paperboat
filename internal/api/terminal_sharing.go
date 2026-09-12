package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

func (c *Client) SharedTerminalConnectionDescriptor(ctx context.Context, sessionID string) (ConnectionDescriptor, error) {
	if sessionID == "" || c.sourceMachineID == "" {
		return ConnectionDescriptor{}, errors.New("session and enrolled source machine are required")
	}
	var out ConnectionDescriptor
	err := c.do(ctx, http.MethodPost, "/v1/terminal-sessions/"+url.PathEscape(sessionID)+"/connection-descriptor", map[string]string{"source_machine_id": c.sourceMachineID}, &out)
	if err == nil {
		err = out.NormalizeConnectionDescriptor()
	}
	return out, err
}

type SharedTerminalParticipant struct {
	AttachmentID string `json:"attachment_id"`
	AccountID    string `json:"account_id"`
	ClientID     string `json:"client_id"`
	Role         string `json:"role"`
}
type TerminalSharingGrant struct {
	Audience   string `json:"audience"`
	AccountID  string `json:"account_id,omitempty"`
	Role       string `json:"role"`
	Active     bool   `json:"active"`
	Generation uint64 `json:"generation"`
}
type SharedTerminalSession struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Target struct {
		Kind              string `json:"kind"`
		ID                string `json:"id"`
		Name              string `json:"name"`
		MachineID         string `json:"machine_id"`
		MachineGeneration uint64 `json:"machine_generation"`
		Status            string `json:"status"`
		Reason            string `json:"reason"`
	} `json:"target"`
	OwnerAccount string `json:"owner_account"`
	CanManage    bool   `json:"can_manage"`
	Role         string `json:"role"`
	Sharing      struct {
		Generation          uint64                 `json:"generation"`
		TeamID              string                 `json:"team_id"`
		Audience            string                 `json:"audience"`
		Grants              []TerminalSharingGrant `json:"grants"`
		RecentOutputWarning string                 `json:"recent_output_warning"`
	} `json:"sharing"`
	Participants          []SharedTerminalParticipant `json:"participants"`
	ParticipantsAvailable bool                        `json:"participants_available"`
}
type TerminalSharingMutation struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	TeamID             string `json:"team_id,omitempty"`
	Audience           string `json:"audience,omitempty"`
	AccountID          string `json:"account_id,omitempty"`
	Role               string `json:"role,omitempty"`
	Active             bool   `json:"active,omitempty"`
}

func (c *Client) SharedTerminalSessions(ctx context.Context) ([]SharedTerminalSession, error) {
	var out struct {
		Sessions []SharedTerminalSession `json:"sessions"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/terminal-sessions", nil, &out)
	return out.Sessions, err
}
func (c *Client) TerminalSharing(ctx context.Context, id string) (SharedTerminalSession, error) {
	var out SharedTerminalSession
	err := c.do(ctx, http.MethodGet, "/v1/terminal-sessions/"+url.PathEscape(id)+"/sharing", nil, &out)
	return out, err
}
func (c *Client) GrantTerminalSharing(ctx context.Context, id string, in TerminalSharingMutation) (SharedTerminalSession, error) {
	var out SharedTerminalSession
	err := c.do(ctx, http.MethodPost, "/v1/terminal-sessions/"+url.PathEscape(id)+"/sharing", in, &out)
	return out, err
}
func (c *Client) EndTerminalSharing(ctx context.Context, id string, in TerminalSharingMutation) (SharedTerminalSession, error) {
	var out SharedTerminalSession
	err := c.do(ctx, http.MethodDelete, "/v1/terminal-sessions/"+url.PathEscape(id)+"/sharing", in, &out)
	return out, err
}
func (c *Client) RemoveTerminalParticipant(ctx context.Context, id, account string, in TerminalSharingMutation) (SharedTerminalSession, error) {
	var out SharedTerminalSession
	err := c.do(ctx, http.MethodPost, "/v1/terminal-sessions/"+url.PathEscape(id)+"/participants/"+url.PathEscape(account)+"/remove", in, &out)
	return out, err
}
