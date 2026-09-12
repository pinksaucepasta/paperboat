package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

type TeamInboxFile struct {
	Basename string `json:"basename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type TeamInboxRequest struct {
	RequestID            string          `json:"request_id"`
	TeamID               string          `json:"team_id"`
	SenderAccount        string          `json:"sender_account"`
	RecipientAccount     string          `json:"recipient_account"`
	SourceMachineID      string          `json:"source_machine_id"`
	DestinationMachineID string          `json:"destination_machine_id"`
	BatchID              string          `json:"batch_id"`
	ManifestDigest       string          `json:"manifest_digest"`
	Files                []TeamInboxFile `json:"files"`
	Status               string          `json:"status"`
	DecisionGeneration   uint64          `json:"decision_generation"`
	ExpiresAt            time.Time       `json:"expires_at"`
}

type TeamInboxPolicy struct {
	Acceptance   string `json:"acceptance"`
	ReceiptEmail bool   `json:"receipt_email"`
	Generation   uint64 `json:"generation"`
}

func (c *Client) TeamInboxPolicy(ctx context.Context) (TeamInboxPolicy, error) {
	var out TeamInboxPolicy
	err := c.do(ctx, http.MethodGet, "/v1/team-inbox/policy", nil, &out)
	return out, err
}

func (c *Client) SetTeamInboxPolicy(ctx context.Context, policy TeamInboxPolicy) (TeamInboxPolicy, error) {
	var out TeamInboxPolicy
	err := c.do(ctx, http.MethodPut, "/v1/team-inbox/policy", map[string]any{"acceptance": policy.Acceptance, "receipt_email": policy.ReceiptEmail, "expected_generation": policy.Generation}, &out)
	return out, err
}

func (c *Client) CreateTeamInboxRequest(ctx context.Context, request TeamInboxRequest, operationID string) (TeamInboxRequest, error) {
	var out TeamInboxRequest
	err := c.do(ctx, http.MethodPost, "/v1/team-inbox/requests", map[string]any{"request_id": request.RequestID, "operation_id": operationID, "source_machine_id": request.SourceMachineID, "destination_machine_id": request.DestinationMachineID, "batch_id": request.BatchID, "files": request.Files, "expires_at": request.ExpiresAt}, &out)
	return out, err
}

func (c *Client) TeamInboxRequests(ctx context.Context) ([]TeamInboxRequest, error) {
	var out struct {
		Requests []TeamInboxRequest `json:"requests"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/team-inbox/requests", nil, &out)
	return out.Requests, err
}

func (c *Client) TeamInboxRequest(ctx context.Context, requestID string) (TeamInboxRequest, error) {
	var out TeamInboxRequest
	err := c.do(ctx, http.MethodGet, "/v1/team-inbox/requests/"+url.PathEscape(requestID), nil, &out)
	return out, err
}

func (c *Client) DecideTeamInboxRequest(ctx context.Context, requestID, action string, generation uint64) (TeamInboxRequest, error) {
	var out TeamInboxRequest
	err := c.do(ctx, http.MethodPost, "/v1/team-inbox/requests/"+url.PathEscape(requestID)+"/"+action, map[string]uint64{"expected_generation": generation}, &out)
	return out, err
}

func (c *Client) CompleteTeamInboxRequest(ctx context.Context, requestID, digest string) error {
	return c.do(ctx, http.MethodPost, "/v1/team-inbox/requests/"+url.PathEscape(requestID)+"/complete", map[string]string{"manifest_digest": digest}, &TeamInboxRequest{})
}
