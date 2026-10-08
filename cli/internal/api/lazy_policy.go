package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type LazyPolicyTarget struct {
	Scheme  string `json:"scheme"`
	Address string `json:"address"`
}

type LazyPolicy struct {
	ID                     string           `json:"id"`
	Hostname               string           `json:"hostname"`
	AccountID              string           `json:"account_id"`
	MachineID              string           `json:"machine_id"`
	InstallationGeneration int64            `json:"installation_generation"`
	Generation             int64            `json:"generation"`
	Target                 LazyPolicyTarget `json:"target"`
	AccessMode             string           `json:"access_mode"`
	OwnershipMode          string           `json:"ownership_mode"`
	ExpiresAt              time.Time        `json:"expires_at"`
	DeletedAt              *time.Time       `json:"deleted_at,omitempty"`
}

type LazyPolicyUpsertRequest struct {
	ID                 string           `json:"id,omitempty"`
	MachineID          string           `json:"machine_id"`
	ExpectedGeneration int64            `json:"expected_generation"`
	Target             LazyPolicyTarget `json:"target"`
	AccessMode         string           `json:"access_mode"`
	OwnershipMode      string           `json:"ownership_mode"`
	ExpiresAt          time.Time        `json:"expires_at"`
}

func (c *Client) UpsertLazyPolicy(ctx context.Context, input LazyPolicyUpsertRequest) (LazyPolicy, error) {
	if err := validateLazyPolicyInput(input); err != nil {
		return LazyPolicy{}, err
	}
	var out LazyPolicy
	err := c.doTunnelRequest(ctx, http.MethodPut, "/v1/lazy-policies", input, &out, nil, nil)
	if err == nil {
		err = validateLazyPolicy(out)
	}
	return out, err
}

func (c *Client) GetLazyPolicy(ctx context.Context, id string) (LazyPolicy, error) {
	if !validLazyPolicyID(id) {
		return LazyPolicy{}, ErrUnsafeTunnelResponse
	}
	var out LazyPolicy
	err := c.doTunnelRequest(ctx, http.MethodGet, "/v1/lazy-policies/"+url.PathEscape(id), nil, &out, nil, nil)
	if err == nil && (validateLazyPolicy(out) != nil || out.ID != id) {
		err = ErrUnsafeTunnelResponse
	}
	return out, err
}

func (c *Client) DeleteLazyPolicy(ctx context.Context, id string, expectedGeneration int64) error {
	if !validLazyPolicyID(id) || expectedGeneration < 1 {
		return ErrUnsafeTunnelResponse
	}
	return c.doTunnelRequest(ctx, http.MethodDelete, "/v1/lazy-policies/"+url.PathEscape(id), struct {
		ExpectedGeneration int64 `json:"expected_generation"`
	}{expectedGeneration}, nil, nil, nil)
}

func validateLazyPolicyInput(in LazyPolicyUpsertRequest) error {
	if (in.ID != "" && !validLazyPolicyID(in.ID)) || !validLazyPolicyID(in.MachineID) || in.ExpectedGeneration < 0 || !in.ExpiresAt.After(time.Now().UTC()) || in.OwnershipMode != "persistent_port" || in.AccessMode != "private" && in.AccessMode != "team" || !validLazyPolicyTarget(in.Target) {
		return ErrUnsafeTunnelResponse
	}
	if in.ExpectedGeneration > 0 && in.ID == "" || in.ID != "" && in.ExpectedGeneration < 1 {
		return ErrUnsafeTunnelResponse
	}
	return nil
}

func validateLazyPolicy(p LazyPolicy) error {
	if !validLazyPolicyID(p.ID) || !validLazyPolicyID(p.AccountID) || !validLazyPolicyID(p.MachineID) || p.Hostname == "" || strings.ContainsAny(p.Hostname, "\x00\r\n ") || p.InstallationGeneration < 1 || p.Generation < 1 || !validLazyPolicyTarget(p.Target) || p.AccessMode != "private" && p.AccessMode != "team" || p.OwnershipMode != "persistent_port" || p.ExpiresAt.IsZero() || p.DeletedAt != nil {
		return ErrUnsafeTunnelResponse
	}
	return nil
}

func validLazyPolicyTarget(target LazyPolicyTarget) bool {
	if target.Scheme != "http" && target.Scheme != "https" && target.Scheme != "h2c" {
		return false
	}
	host, port, err := net.SplitHostPort(target.Address)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != port {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validLazyPolicyID(value string) bool {
	if len(value) < 1 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
