package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
)

// WorkspaceDomain is a domain added to the active workspace (GET/POST /v1/domains). Tunnel and preview
// hostnames one level under a verified domain use it without a DNS step of their own.
type WorkspaceDomain struct {
	ID          string                  `json:"id"`
	Hostname    string                  `json:"hostname"`
	Workspace   string                  `json:"workspace"`
	State       string                  `json:"state"` // pending_dns, verified, failed
	Records     []WorkspaceDomainRecord `json:"records"`
	Observed    []string                `json:"observed_records"`
	CheckedAt   string                  `json:"last_checked_at,omitempty"`
	NextCheckAt string                  `json:"next_check_at"`
	VerifiedAt  string                  `json:"verified_at,omitempty"`
	DNSFailure  *WorkspaceDomainFailure `json:"dns_failure,omitempty"`
	Certificate struct {
		State     string                  `json:"state"` // pending, ready, failed
		ExpiresAt string                  `json:"expires_at,omitempty"`
		Failure   *WorkspaceDomainFailure `json:"failure,omitempty"`
	} `json:"certificate"`
	UsedBy    []WorkspaceDomainUse `json:"used_by"`
	CreatedAt string               `json:"created_at"`
}

type WorkspaceDomainRecord struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Purpose  string `json:"purpose"` // verification, routing, apex_routing
	Required bool   `json:"required"`
}

type WorkspaceDomainFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type WorkspaceDomainUse struct {
	Kind             string `json:"kind"` // tunnel or preview
	ID               string `json:"id"`
	Name             string `json:"name"`
	Hostname         string `json:"hostname"`
	CertificateState string `json:"certificate_state"`
}

var workspaceDomainIDPattern = regexp.MustCompile(`^wsd_[a-z0-9]{8,64}$`)

func (d WorkspaceDomain) valid() bool {
	return workspaceDomainIDPattern.MatchString(d.ID) && d.Hostname != ""
}

var errUnsafeWorkspaceDomain = errors.New("the server returned an invalid domain")

func (c *Client) ListWorkspaceDomains(ctx context.Context) ([]WorkspaceDomain, error) {
	var out struct {
		Items []WorkspaceDomain `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/domains", nil, &out); err != nil {
		return nil, err
	}
	for _, d := range out.Items {
		if !d.valid() {
			return nil, errUnsafeWorkspaceDomain
		}
	}
	return out.Items, nil
}

func (c *Client) AddWorkspaceDomain(ctx context.Context, hostname string) (WorkspaceDomain, error) {
	var out WorkspaceDomain
	if err := c.do(ctx, http.MethodPost, "/v1/domains", map[string]string{"hostname": hostname}, &out); err != nil {
		return WorkspaceDomain{}, err
	}
	if !out.valid() {
		return WorkspaceDomain{}, errUnsafeWorkspaceDomain
	}
	return out, nil
}

func (c *Client) GetWorkspaceDomain(ctx context.Context, id string) (WorkspaceDomain, error) {
	return c.workspaceDomainCall(ctx, http.MethodGet, id, "")
}

// CheckWorkspaceDomain asks the server to check the domain's DNS now.
func (c *Client) CheckWorkspaceDomain(ctx context.Context, id string) (WorkspaceDomain, error) {
	return c.workspaceDomainCall(ctx, http.MethodPost, id, "/check")
}

func (c *Client) RemoveWorkspaceDomain(ctx context.Context, id string) error {
	if !workspaceDomainIDPattern.MatchString(id) {
		return errUnsafeWorkspaceDomain
	}
	return c.do(ctx, http.MethodDelete, "/v1/domains/"+url.PathEscape(id), nil, nil)
}

func (c *Client) workspaceDomainCall(ctx context.Context, method, id, suffix string) (WorkspaceDomain, error) {
	if !workspaceDomainIDPattern.MatchString(id) {
		return WorkspaceDomain{}, errUnsafeWorkspaceDomain
	}
	var out WorkspaceDomain
	if err := c.do(ctx, method, "/v1/domains/"+url.PathEscape(id)+suffix, nil, &out); err != nil {
		return WorkspaceDomain{}, err
	}
	if !out.valid() || out.ID != id {
		return WorkspaceDomain{}, errUnsafeWorkspaceDomain
	}
	return out, nil
}
