package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

func (c *Client) ListTunnelConnectorsV1(ctx context.Context, tunnel, cursor string, limit int) (TunnelConnectorPage, error) {
	p, e := tunnelPath("tunnels", tunnel, "connectors")
	if e != nil {
		return TunnelConnectorPage{}, e
	}
	q, e := pageQuery(cursor, limit)
	if e != nil {
		return TunnelConnectorPage{}, e
	}
	var out TunnelConnectorPage
	e = c.doTunnelRequest(ctx, http.MethodGet, p+q, nil, &out, nil, nil)
	if e == nil {
		if len(out.Items) > limit || !validNextCursor(out.NextCursor) {
			return TunnelConnectorPage{}, ErrUnsafeTunnelResponse
		}
		for _, v := range out.Items {
			if e = validateConnector(v); e == nil && v.TunnelID != tunnel {
				e = ErrUnsafeTunnelResponse
			}
			if e != nil {
				break
			}
		}
	}
	return out, e
}

func (c *Client) GetTunnelConnectorV1(ctx context.Context, tunnel, id string) (TunnelConnector, error) {
	p, e := tunnelPath("tunnels", tunnel, "connectors", id)
	if e != nil {
		return TunnelConnector{}, e
	}
	var out TunnelConnector
	var h http.Header
	e = c.doTunnelRequest(ctx, http.MethodGet, p, nil, &out, nil, &h)
	if e == nil {
		e = validateConnector(out)
	}
	if e == nil && out.TunnelID != tunnel {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil {
		e = responseETag(h, out.ETag)
	}
	return out, e
}

func (c *Client) MutateTunnelConnectorV1(ctx context.Context, tunnel, id, action, etag, key string) (TunnelConnectorMutation, error) {
	if action != "drain" && action != "revoke" {
		return TunnelConnectorMutation{}, errors.New("invalid connector action")
	}
	p, e := tunnelPath("tunnels", tunnel, "connectors", id)
	if action == "drain" {
		p += "/drain"
	}
	if e != nil {
		return TunnelConnectorMutation{}, e
	}
	method := http.MethodPost
	if action == "revoke" {
		method = http.MethodDelete
	}
	h, e := requiredTunnelMutationHeaders(etag, key)
	if e != nil {
		return TunnelConnectorMutation{}, e
	}
	var out TunnelConnectorMutation
	e = c.doTunnelRequest(ctx, method, p, struct{}{}, &out, h, nil)
	if e == nil && out.Connector.Schema != "" {
		e = validateConnector(out.Connector)
	}
	if e == nil && out.Connector.Schema != "" && out.Connector.TunnelID != tunnel {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil && out.Operation.Schema != "" {
		e = validateOperation(&out.Operation, "connector", out.Operation.ResourceID)
	}
	if e == nil && out.Connector.Schema == "" && out.Operation.Schema == "" {
		e = ErrUnsafeTunnelResponse
	}
	return out, e
}

// IssueTunnelConnectorEnrollmentV1 returns the one-time enrollment secret. The
// response is explicitly no-store and the token is never accepted by any
// read method in this client.
func (c *Client) IssueTunnelConnectorEnrollmentV1(ctx context.Context, tunnel, key string, in TunnelConnectorEnrollmentInput) (TunnelConnectorEnrollment, error) {
	if err := validateTunnelEnrollmentInput(in); err != nil {
		return TunnelConnectorEnrollment{}, err
	}
	p, err := tunnelPath("tunnels", tunnel, "connectors", "enrollments")
	if err != nil {
		return TunnelConnectorEnrollment{}, err
	}
	h, err := mutationHeaders("", key)
	if err != nil {
		return TunnelConnectorEnrollment{}, err
	}
	var out TunnelConnectorEnrollment
	var responseHeaders http.Header
	if err := c.doTunnelRequest(ctx, http.MethodPost, p, in, &out, h, &responseHeaders); err != nil {
		return TunnelConnectorEnrollment{}, err
	}
	if !strings.Contains(strings.ToLower(responseHeaders.Get("Cache-Control")), "no-store") {
		return TunnelConnectorEnrollment{}, ErrUnsafeTunnelResponse
	}
	if err := validateTunnelEnrollment(&out, tunnel); err != nil {
		return TunnelConnectorEnrollment{}, err
	}
	return out, nil
}

// CreateTunnelConnectorEnrollmentV1 is an explicit alias for callers that
// use create terminology for issuing a one-time connector enrollment.
func (c *Client) CreateTunnelConnectorEnrollmentV1(ctx context.Context, tunnel, key string, in TunnelConnectorEnrollmentInput) (TunnelConnectorEnrollment, error) {
	return c.IssueTunnelConnectorEnrollmentV1(ctx, tunnel, key, in)
}

func (c *Client) ExchangeTunnelConnectorEnrollmentV1(ctx context.Context, tunnel, key string, in TunnelConnectorEnrollmentExchangeInput) (TunnelConnectorActivation, error) {
	if err := validateTunnelEnrollmentExchangeInput(in); err != nil {
		return TunnelConnectorActivation{}, err
	}
	p, err := tunnelPath("tunnels", tunnel, "connectors", "enrollments", "exchange")
	if err != nil {
		return TunnelConnectorActivation{}, err
	}
	h, err := mutationHeaders("", key)
	if err != nil {
		return TunnelConnectorActivation{}, err
	}
	var out TunnelConnectorActivation
	var responseHeaders http.Header
	if err := c.doTunnelRequest(ctx, http.MethodPost, p, in, &out, h, &responseHeaders); err != nil {
		return TunnelConnectorActivation{}, err
	}
	if !strings.Contains(strings.ToLower(responseHeaders.Get("Cache-Control")), "no-store") {
		return TunnelConnectorActivation{}, ErrUnsafeTunnelResponse
	}
	if err := validateTunnelActivation(&out, tunnel); err != nil {
		return TunnelConnectorActivation{}, err
	}
	return out, nil
}

func (c *Client) EnrollTunnelConnectorV1(ctx context.Context, tunnel, key string, in TunnelConnectorEnrollmentExchangeInput) (TunnelConnectorActivation, error) {
	return c.ExchangeTunnelConnectorEnrollmentV1(ctx, tunnel, key, in)
}

func (c *Client) RotateTunnelCredentialsV1(ctx context.Context, tunnel, etag, key string) (TunnelOperation, error) {
	p, e := tunnelPath("tunnels", tunnel, "credentials")
	if e != nil {
		return TunnelOperation{}, e
	}
	h, e := requiredTunnelMutationHeaders(etag, key)
	if e != nil {
		return TunnelOperation{}, e
	}
	var out TunnelOperation
	e = c.doTunnelRequest(ctx, http.MethodPost, p+"/rotate", struct{}{}, &out, h, nil)
	if e == nil {
		e = validateOperation(&out, "tunnel", tunnel)
	}
	return out, e
}
