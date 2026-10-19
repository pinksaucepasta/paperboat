package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (c *Client) ListTunnelsV1(ctx context.Context, cursor string, limit int) (TunnelPage, error) {
	return c.ListTunnelsV1Filtered(ctx, cursor, limit, nil)
}

func (c *Client) ListTunnelsV1Filtered(ctx context.Context, cursor string, limit int, filters url.Values) (TunnelPage, error) {
	q, e := pageQuery(cursor, limit)
	if e != nil {
		return TunnelPage{}, e
	}
	if len(filters) > 0 {
		values, err := url.ParseQuery(strings.TrimPrefix(q, "?"))
		if err != nil {
			return TunnelPage{}, err
		}
		for _, key := range []string{"q", "state", "owner"} {
			if value := filters.Get(key); value != "" {
				values.Set(key, value)
			}
		}
		q = "?" + values.Encode()
	}
	var out TunnelPage
	e = c.doTunnelRequest(ctx, http.MethodGet, "/v1/tunnels"+q, nil, &out, nil, nil)
	if e == nil {
		if len(out.Items) > limit || !validNextCursor(out.NextCursor) {
			return TunnelPage{}, ErrUnsafeTunnelResponse
		}
		for _, v := range out.Items {
			if e = validateTunnel(v); e != nil {
				break
			}
		}
	}
	return out, e
}
func (c *Client) GetTunnelV1(ctx context.Context, id string) (Tunnel, error) {
	p, e := tunnelPath("tunnels", id)
	if e != nil {
		return Tunnel{}, e
	}
	var out Tunnel
	var h http.Header
	// The ETag is the strong OCC token for the exact tunnel projection. A
	// compression proxy may weaken it, so request the identity representation
	// before comparing the header to the body ETag.
	e = c.doTunnelRequest(ctx, http.MethodGet, p, nil, &out, http.Header{"Accept-Encoding": []string{"identity"}}, &h)
	if e == nil {
		e = validateTunnel(out)
	}
	if e == nil && out.ID != id {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil {
		e = responseETag(h, out.ETag)
	}
	return out, e
}
func (c *Client) CreateTunnelV1(ctx context.Context, in TunnelCreateInput, key string) (TunnelMutation, error) {
	if err := c.bindCreateWorkspace(&in.Workspace); err != nil {
		return TunnelMutation{}, err
	}
	if err := validateTunnelCreateInput(in); err != nil {
		return TunnelMutation{}, err
	}
	h, e := mutationHeaders("", key)
	if e != nil {
		return TunnelMutation{}, e
	}
	var out TunnelMutation
	e = c.doTunnelRequest(ctx, http.MethodPost, "/v1/tunnels", in, &out, h, nil)
	if e != nil {
		return out, e
	}
	if out.Operation.Schema != "" {
		// An operation response carries the durable resource ID but not the
		// resource projection. Validate that identity before using it for the
		// follow-up read; this keeps an untrusted response from selecting an
		// arbitrary path.
		if e = validateOperation(&out.Operation, "tunnel", out.Tunnel.ID); e != nil {
			return out, e
		}
	}
	operationOnly := out.Tunnel.Schema == ""
	if operationOnly {
		if out.Operation.Schema == "" {
			return out, ErrUnsafeTunnelResponse
		}
		if c.machineAuth != nil && c.credentialSource == nil && strings.TrimSpace(c.accessToken) == "" {
			return out, ErrMachineAuthReadRequiresClientSession
		}
		resolved, fetchErr := c.GetTunnelV1(ctx, out.Operation.ResourceID)
		if fetchErr != nil {
			return out, fmt.Errorf("fetch tunnel after create operation: %w", fetchErr)
		}
		if resolved.ID != out.Operation.ResourceID {
			return out, ErrUnsafeTunnelResponse
		}
		out.Tunnel = resolved
	}
	if e = validateTunnel(out.Tunnel); e != nil {
		return out, e
	}
	if operationOnly {
		if out.Tunnel.Name != in.Name {
			return out, ErrUnsafeTunnelResponse
		}
		wantAccessMode := in.AccessMode
		if wantAccessMode == "" {
			wantAccessMode = "public"
		}
		if out.Tunnel.AccessMode != wantAccessMode {
			return out, ErrUnsafeTunnelResponse
		}
	}
	if out.Operation.Schema != "" {
		if e = validateOperation(&out.Operation, "tunnel", out.Tunnel.ID); e != nil {
			return out, e
		}
	}
	return out, e
}
func (c *Client) PatchTunnelV1(ctx context.Context, id, etag, key string, in TunnelPatchInput) (TunnelMutation, error) {
	if in.Name == nil && in.AccessMode == nil && in.ExpiresAt == nil {
		return TunnelMutation{}, errors.New("tunnel patch must contain a field")
	}
	if in.Name != nil {
		if err := validateTunnelName(*in.Name); err != nil {
			return TunnelMutation{}, err
		}
	}
	if in.AccessMode != nil && *in.AccessMode != "public" && *in.AccessMode != "private" && *in.AccessMode != "team" {
		return TunnelMutation{}, errors.New("access mode must be public, private, or team")
	}
	return c.tunnelMutation(ctx, http.MethodPatch, id, "", etag, key, in)
}
func (c *Client) ChangeTunnelStateV1(ctx context.Context, id, action, etag, key string) (TunnelMutation, error) {
	if action != "pause" && action != "resume" && action != "delete" {
		return TunnelMutation{}, errors.New("invalid tunnel action")
	}
	method := http.MethodPost
	if action == "delete" {
		method = http.MethodDelete
	}
	return c.tunnelMutation(ctx, method, id, action, etag, key, struct{}{})
}
func (c *Client) tunnelMutation(ctx context.Context, method, id, action, etag, key string, body any) (TunnelMutation, error) {
	p, e := tunnelPath("tunnels", id)
	if action != "" && action != "delete" {
		p += "/" + action
	}
	if e != nil {
		return TunnelMutation{}, e
	}
	h, e := requiredTunnelMutationHeaders(etag, key)
	if e != nil {
		return TunnelMutation{}, e
	}
	var out TunnelMutation
	e = c.doTunnelRequest(ctx, method, p, body, &out, h, nil)
	if e == nil && out.Tunnel.Schema != "" {
		e = validateTunnel(out.Tunnel)
	}
	if e == nil && out.Operation.Schema != "" {
		e = validateOperation(&out.Operation, "tunnel", id)
	}
	if e == nil && out.Tunnel.Schema == "" && out.Operation.Schema != "" {
		out.Tunnel, e = c.GetTunnelV1(ctx, id)
	}
	if e == nil && out.Tunnel.Schema == "" && out.Operation.Schema == "" {
		e = ErrUnsafeTunnelResponse
	}
	return out, e
}

func (c *Client) ListTunnelRoutesV1(ctx context.Context, tunnel, cursor string, limit int) (TunnelRoutePage, error) {
	p, e := tunnelPath("tunnels", tunnel, "routes")
	if e != nil {
		return TunnelRoutePage{}, e
	}
	q, e := pageQuery(cursor, limit)
	if e != nil {
		return TunnelRoutePage{}, e
	}
	var out TunnelRoutePage
	e = c.doTunnelRequest(ctx, http.MethodGet, p+q, nil, &out, nil, nil)
	if e == nil {
		if len(out.Items) > limit || !validNextCursor(out.NextCursor) {
			return TunnelRoutePage{}, ErrUnsafeTunnelResponse
		}
		for _, v := range out.Items {
			if e = validateRoute(v); e == nil && v.TunnelID != tunnel {
				e = ErrUnsafeTunnelResponse
			}
			if e != nil {
				break
			}
		}
	}
	return out, e
}
func (c *Client) GetTunnelRouteV1(ctx context.Context, tunnel, id string) (TunnelRoute, error) {
	p, e := tunnelPath("tunnels", tunnel, "routes", id)
	if e != nil {
		return TunnelRoute{}, e
	}
	var out TunnelRoute
	var h http.Header
	e = c.doTunnelRequest(ctx, http.MethodGet, p, nil, &out, nil, &h)
	if e == nil {
		e = validateRoute(out)
	}
	if e == nil && out.TunnelID != tunnel {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil {
		e = responseETag(h, out.ETag)
	}
	return out, e
}
func (c *Client) CreateTunnelRouteV1(ctx context.Context, tunnel, key string, in TunnelRouteInput) (TunnelRouteMutation, error) {
	if err := validateTunnelRouteInput(in, true); err != nil {
		return TunnelRouteMutation{}, err
	}
	p, e := tunnelPath("tunnels", tunnel, "routes")
	if e != nil {
		return TunnelRouteMutation{}, e
	}
	return c.routeMutation(ctx, http.MethodPost, p, "", key, in)
}
func (c *Client) PatchTunnelRouteV1(ctx context.Context, tunnel, id, etag, key string, in TunnelRoutePatch) (TunnelRouteMutation, error) {
	if in.Name == nil && in.Protocol == nil && in.HostMatch == nil && !in.PathPrefixSet && in.PathPrefix == nil && in.Origin == nil && in.Priority == nil && in.ConnectTimeoutMS == nil && in.IdleTimeoutMS == nil && in.MaxConcurrentStreams == nil && in.DesiredState == nil {
		return TunnelRouteMutation{}, errors.New("route patch must contain a field")
	}
	if in.Name != nil && !tunnelRouteNamePatternV1.MatchString(*in.Name) {
		return TunnelRouteMutation{}, errors.New("route name is invalid")
	}
	if in.Protocol != nil && *in.Protocol != "http" && *in.Protocol != "tcp_private" {
		return TunnelRouteMutation{}, errors.New("route protocol must be http or tcp_private")
	}
	if in.HostMatch != nil {
		if err := validateHostMatch(*in.HostMatch); err != nil {
			return TunnelRouteMutation{}, err
		}
	}
	if in.Origin != nil {
		if err := validateRouteOrigin(*in.Origin); err != nil {
			return TunnelRouteMutation{}, err
		}
	}
	if in.PathPrefix != nil && (*in.PathPrefix == "" || len(*in.PathPrefix) > 2048 || !strings.HasPrefix(*in.PathPrefix, "/") || strings.ContainsAny(*in.PathPrefix, "\x00\r\n")) {
		return TunnelRouteMutation{}, errors.New("path prefix is invalid")
	}
	if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 1000000) {
		return TunnelRouteMutation{}, errors.New("priority is out of range")
	}
	if in.ConnectTimeoutMS != nil && (*in.ConnectTimeoutMS < 100 || *in.ConnectTimeoutMS > 120000) {
		return TunnelRouteMutation{}, errors.New("connect timeout is out of range")
	}
	if in.IdleTimeoutMS != nil && (*in.IdleTimeoutMS < 1000 || *in.IdleTimeoutMS > 3600000) {
		return TunnelRouteMutation{}, errors.New("idle timeout is out of range")
	}
	if in.MaxConcurrentStreams != nil && (*in.MaxConcurrentStreams < 1 || *in.MaxConcurrentStreams > 10000) {
		return TunnelRouteMutation{}, errors.New("stream limit is out of range")
	}
	if in.DesiredState != nil && *in.DesiredState != "active" && *in.DesiredState != "disabled" {
		return TunnelRouteMutation{}, errors.New("route state must be active or disabled")
	}
	p, e := tunnelPath("tunnels", tunnel, "routes", id)
	if e != nil {
		return TunnelRouteMutation{}, e
	}
	return c.routeMutation(ctx, http.MethodPatch, p, etag, key, in)
}
func (c *Client) DeleteTunnelRouteV1(ctx context.Context, tunnel, id, etag, key string) (TunnelRouteMutation, error) {
	p, e := tunnelPath("tunnels", tunnel, "routes", id)
	if e != nil {
		return TunnelRouteMutation{}, e
	}
	return c.routeMutation(ctx, http.MethodDelete, p, etag, key, struct{}{})
}
func (c *Client) routeMutation(ctx context.Context, method, path, etag, key string, body any) (TunnelRouteMutation, error) {
	var h http.Header
	var e error
	if method == http.MethodPost && strings.HasSuffix(path, "/routes") {
		h, e = mutationHeaders("", key)
	} else {
		h, e = requiredTunnelMutationHeaders(etag, key)
	}
	if e != nil {
		return TunnelRouteMutation{}, e
	}
	var out TunnelRouteMutation
	e = c.doTunnelRequest(ctx, method, path, body, &out, h, nil)
	if e == nil && out.Route.Schema != "" {
		e = validateRoute(out.Route)
	}
	if e == nil && out.Route.Schema != "" && out.Route.TunnelID != strings.Split(strings.TrimPrefix(path, "/v1/tunnels/"), "/")[0] {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil && out.Operation.Schema != "" {
		e = validateOperation(&out.Operation, "route", out.Operation.ResourceID)
	}
	if e == nil && out.Route.Schema == "" && out.Operation.Schema == "" {
		e = ErrUnsafeTunnelResponse
	}
	return out, e
}

func (c *Client) ListTunnelDomainsV1(ctx context.Context, tunnel, cursor string, limit int) (TunnelDomainPage, error) {
	p, e := tunnelPath("tunnels", tunnel, "domains")
	if e != nil {
		return TunnelDomainPage{}, e
	}
	q, e := pageQuery(cursor, limit)
	if e != nil {
		return TunnelDomainPage{}, e
	}
	var out TunnelDomainPage
	e = c.doTunnelRequest(ctx, http.MethodGet, p+q, nil, &out, nil, nil)
	if e == nil {
		if len(out.Items) > limit || !validNextCursor(out.NextCursor) {
			return TunnelDomainPage{}, ErrUnsafeTunnelResponse
		}
		for i := range out.Items {
			if e = validateDomain(&out.Items[i]); e == nil && out.Items[i].TunnelID != tunnel {
				e = ErrUnsafeTunnelResponse
			}
			if e != nil {
				break
			}
		}
	}
	return out, e
}
func (c *Client) GetTunnelDomainV1(ctx context.Context, tunnel, id string) (TunnelDomain, error) {
	p, e := tunnelPath("tunnels", tunnel, "domains", id)
	if e != nil {
		return TunnelDomain{}, e
	}
	var out TunnelDomain
	var h http.Header
	e = c.doTunnelRequest(ctx, http.MethodGet, p, nil, &out, http.Header{"Accept-Encoding": []string{"identity"}}, &h)
	if e == nil {
		e = validateDomain(&out)
	}
	if e == nil && out.TunnelID != tunnel {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil {
		e = responseETag(h, out.ETag)
	}
	return out, e
}
func (c *Client) CreateTunnelDomainV1(ctx context.Context, tunnel, key string, in TunnelDomainInput) (TunnelDomainMutation, error) {
	if err := validateTunnelDomainInput(in); err != nil {
		return TunnelDomainMutation{}, err
	}
	p, e := tunnelPath("tunnels", tunnel, "domains")
	if e != nil {
		return TunnelDomainMutation{}, e
	}
	return c.domainMutation(ctx, http.MethodPost, p, "", key, in)
}
func (c *Client) MutateTunnelDomainV1(ctx context.Context, tunnel, id, action, etag, key string) (TunnelDomainMutation, error) {
	if action != "delete" && action != "verify" {
		return TunnelDomainMutation{}, errors.New("invalid domain action")
	}
	p, e := tunnelPath("tunnels", tunnel, "domains", id)
	if action == "verify" {
		p += "/verify"
	}
	if e != nil {
		return TunnelDomainMutation{}, e
	}
	method := http.MethodPost
	if action == "delete" {
		method = http.MethodDelete
	}
	return c.domainMutation(ctx, method, p, etag, key, struct{}{})
}
func (c *Client) domainMutation(ctx context.Context, method, path, etag, key string, body any) (TunnelDomainMutation, error) {
	var h http.Header
	var e error
	if method == http.MethodPost && strings.HasSuffix(path, "/domains") {
		h, e = mutationHeaders("", key)
	} else {
		h, e = requiredTunnelMutationHeaders(etag, key)
	}
	if e != nil {
		return TunnelDomainMutation{}, e
	}
	var out TunnelDomainMutation
	e = c.doTunnelRequest(ctx, method, path, body, &out, h, nil)
	if e == nil && out.Domain.Schema != "" {
		e = validateDomain(&out.Domain)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/tunnels/"), "/")
	if e == nil && out.Domain.Schema != "" && (len(parts) < 1 || out.Domain.TunnelID != parts[0]) {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil && out.Operation.Schema != "" {
		e = validateOperation(&out.Operation, "domain_binding", out.Operation.ResourceID)
	}
	if e == nil && out.Domain.Schema == "" && out.Operation.Schema == "" {
		e = ErrUnsafeTunnelResponse
	}
	if e == nil && out.Domain.Schema == "" && out.Operation.Schema != "" && method != http.MethodDelete {
		out.Domain, e = c.GetTunnelDomainV1(ctx, parts[0], out.Operation.ResourceID)
	}
	return out, e
}
func (c *Client) TunnelDomainInstructionsV1(ctx context.Context, tunnel, id string) (TunnelDNSInstructions, error) {
	p, e := tunnelPath("tunnels", tunnel, "domains", id)
	if e != nil {
		return TunnelDNSInstructions{}, e
	}
	var out TunnelDNSInstructions
	e = c.doStrict(ctx, http.MethodGet, p+"/instructions", nil, &out)
	if e == nil {
		if out.Schema != TunnelV1Schema || out.Kind != "dns_instructions" || out.TunnelID != tunnel || out.DomainID != id || validateDomainHostname(out.Hostname) != nil || out.Provider == "" || len(out.Provider) > 64 || out.CertificateStrategy == "" || len(out.CertificateStrategy) > 64 || out.VerificationState == "" || len(out.VerificationState) > 64 || len(out.Records) > 32 || len(out.Note) > 2000 || out.PublicTCPPort < 0 || out.PublicTCPPort > 65535 || (out.PublicTCPPort > 0 && out.PublicTCPPort < 1024) || containsTunnelControl(out.Provider+out.CertificateStrategy+out.VerificationState+out.Note) {
			e = ErrUnsafeTunnelResponse
		}
		for _, record := range out.Records {
			if record.Name == "" || len(record.Name) > 253 || record.Type == "" || len(record.Type) > 16 || record.Value == "" || len(record.Value) > 2048 || record.TTL < 0 || record.TTL > 604800 || containsTunnelControl(record.Name+record.Type+record.Value) {
				e = ErrUnsafeTunnelResponse
				break
			}
		}
		out.Note = redactTunnelText(out.Note)
	}
	return out, e
}

// GetTunnelOperationV1 returns resumable progress for tunnel-family work. It
// accepts only resource kinds exposed by this client surface.
func (c *Client) GetTunnelOperationV1(ctx context.Context, id string) (TunnelOperation, error) {
	p, err := tunnelPath("operations", id)
	if err != nil {
		return TunnelOperation{}, err
	}
	var out TunnelOperation
	err = c.doTunnelRequest(ctx, http.MethodGet, p, nil, &out, nil, nil)
	if err != nil {
		return TunnelOperation{}, err
	}
	switch out.ResourceKind {
	case "tunnel", "route", "domain_binding", "connector":
	default:
		return TunnelOperation{}, ErrUnsafeTunnelResponse
	}
	if err = validateOperation(&out, out.ResourceKind, out.ResourceID); err != nil {
		return TunnelOperation{}, err
	}
	return out, nil
}

func (c *Client) TunnelStatusV1(ctx context.Context, tunnel string) (TunnelHealth, error) {
	p, err := tunnelPath("tunnels", tunnel)
	if err != nil {
		return TunnelHealth{}, err
	}
	var out TunnelHealth
	err = c.doTunnelRequest(ctx, http.MethodGet, p+"/status", nil, &out, nil, nil)
	if err == nil {
		err = validateHealth(&out, tunnel)
	}
	return out, err
}

func (c *Client) ListTunnelLogsV1(ctx context.Context, tunnel, cursor string, limit int) (TunnelLogPage, error) {
	p, err := tunnelPath("tunnels", tunnel, "logs")
	if err != nil {
		return TunnelLogPage{}, err
	}
	q, err := pageQuery(cursor, limit)
	if err != nil {
		return TunnelLogPage{}, err
	}
	var out TunnelLogPage
	err = c.doTunnelRequest(ctx, http.MethodGet, p+q, nil, &out, nil, nil)
	if err == nil {
		if len(out.Items) > limit || len(out.NextCursor) > 4096 {
			return TunnelLogPage{}, ErrUnsafeTunnelResponse
		}
		for i := range out.Items {
			if err = validateLogEntry(&out.Items[i], tunnel); err != nil {
				return TunnelLogPage{}, err
			}
		}
	}
	return out, err
}

func (c *Client) ListTunnelEventsV1(ctx context.Context, tunnel, cursor string, limit int) (TunnelEventPage, error) {
	p, err := tunnelPath("tunnels", tunnel, "events")
	if err != nil {
		return TunnelEventPage{}, err
	}
	q, err := pageQuery(cursor, limit)
	if err != nil {
		return TunnelEventPage{}, err
	}
	var out TunnelEventPage
	if err := c.doTunnelRequest(ctx, http.MethodGet, p+q, nil, &out, nil, nil); err != nil {
		return TunnelEventPage{}, err
	}
	if len(out.Items) > limit || !validNextCursor(out.NextCursor) {
		return TunnelEventPage{}, ErrUnsafeTunnelResponse
	}
	for i := range out.Items {
		if err := validateTunnelEvent(&out.Items[i], tunnel); err != nil {
			return TunnelEventPage{}, err
		}
	}
	return out, nil
}

// DiscoverTunnelPrivateAccessRoutesV1 obtains the complete host-scoped
// prerequisite snapshot. The idempotency key is included in both the body and
// signed header because the endpoint signs the exact request document.
func (c *Client) DiscoverTunnelPrivateAccessRoutesV1(ctx context.Context, key string) (TunnelPrivateAccessSnapshot, error) {
	h, err := mutationHeaders("", key)
	if err != nil {
		return TunnelPrivateAccessSnapshot{}, err
	}
	var out TunnelPrivateAccessSnapshot
	if err := c.doTunnelRequest(ctx, http.MethodPost, "/v1/private-access/routes", struct {
		IdempotencyKey string `json:"idempotency_key"`
	}{IdempotencyKey: key}, &out, h, nil); err != nil {
		return TunnelPrivateAccessSnapshot{}, err
	}
	if err := validateTunnelPrivateAccessSnapshot(&out); err != nil {
		return TunnelPrivateAccessSnapshot{}, err
	}
	return out, nil
}

func (c *Client) ListTunnelPrivateAccessRoutesV1(ctx context.Context, key string) (TunnelPrivateAccessSnapshot, error) {
	return c.DiscoverTunnelPrivateAccessRoutesV1(ctx, key)
}
