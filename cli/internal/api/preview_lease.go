package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/net/idna"
)

func decodePreviewLeaseStrict(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing response data", ErrPreviewLeaseInvalid)
	}
	return nil
}

// NewPreviewLeaseIdempotencyKey creates an opaque key suitable for one v1
// lease mutation. It carries no credential and is safe to log as metadata.
func NewPreviewLeaseIdempotencyKey() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "operation_" + id.String(), nil
}

// CreatePreviewLease creates a lease and fetches the resource when the server
// initially responds with a resumable operation. The returned endpoint is
// still unpublished to users until the host carrier reports readiness.
func (c *Client) CreatePreviewLease(ctx context.Context, input PreviewLeaseCreateRequest, idempotencyKey string) (PreviewLease, error) {
	if err := validatePreviewLeaseIdempotencyKey(idempotencyKey); err != nil {
		return PreviewLease{}, err
	}
	if err := c.bindCreateWorkspace(&input.Workspace); err != nil {
		return PreviewLease{}, err
	}
	input.OwnerMachineID = strings.TrimSpace(input.OwnerMachineID)
	input.OwnerSessionID = strings.TrimSpace(input.OwnerSessionID)
	input.Target.Scheme = strings.ToLower(strings.TrimSpace(input.Target.Scheme))
	input.Target.Address = strings.TrimSpace(input.Target.Address)
	input.AccessMode = strings.ToLower(strings.TrimSpace(input.AccessMode))
	domains, err := NormalizePreviewDomains(input.Domains)
	if err != nil {
		return PreviewLease{}, err
	}
	input.Domains = domains
	if err := validatePreviewLeaseCreateInput(input); err != nil {
		return PreviewLease{}, err
	}
	var response previewLeaseCreateResponse
	var headers http.Header
	err = c.doRequestMeta(ctx, http.MethodPost, "/v1/previews", input, &response, http.Header{
		"Idempotency-Key": []string{idempotencyKey},
		// The control plane's ETag is a strong OCC token. Explicitly request
		// the identity representation so an intermediary compression layer
		// cannot rewrite or remove that validator.
		"Accept-Encoding": []string{"identity"},
	}, true, &headers)
	if err != nil {
		return PreviewLease{}, err
	}
	createOperationID := strings.TrimSpace(headers.Get("X-Paperboat-Operation-ID"))
	if !validPreviewID(createOperationID) {
		return PreviewLease{}, fmt.Errorf("%w: create response has no valid operation ID", ErrPreviewLeaseInvalid)
	}
	if response.Lease != nil {
		response.Lease.CreateOperationID = createOperationID
		response.Lease.ETag = strings.TrimSpace(headers.Get("ETag"))
		if err := validatePreviewLease(*response.Lease); err != nil {
			return PreviewLease{}, err
		}
		if err := validateRequestedPreviewDomains(input.Domains, *response.Lease); err != nil {
			return PreviewLease{}, err
		}
		if _, err := previewLeaseETag(response.Lease.ID, response.Lease.ETag); err != nil {
			return PreviewLease{}, err
		}
		return *response.Lease, nil
	}
	if response.Operation == nil || validatePreviewLeaseOperation(*response.Operation) != nil {
		return PreviewLease{}, fmt.Errorf("%w: create response has neither lease nor resource operation", ErrPreviewLeaseInvalid)
	}
	if response.Operation.ID != createOperationID {
		return PreviewLease{}, fmt.Errorf("%w: create operation header does not match response", ErrPreviewLeaseInvalid)
	}
	if c.machineAuth != nil && c.credentialSource == nil && strings.TrimSpace(c.accessToken) == "" {
		return PreviewLease{}, ErrMachineAuthReadRequiresClientSession
	}
	lease, err := c.GetPreviewLease(ctx, response.Operation.ResourceID)
	if err != nil {
		return PreviewLease{}, fmt.Errorf("fetch preview lease after create operation: %w", err)
	}
	lease.CreateOperationID = response.Operation.ID
	if err := validateRequestedPreviewDomains(input.Domains, lease); err != nil {
		return PreviewLease{}, err
	}
	return lease, nil
}

// GetPreviewLease returns the current safe lease projection and its strong
// ETag, which must be sent on every subsequent mutation.
func (c *Client) GetPreviewLease(ctx context.Context, previewID string) (PreviewLease, error) {
	if strings.TrimSpace(previewID) == "" {
		return PreviewLease{}, fmt.Errorf("%w: preview ID is required", ErrPreviewLeaseInvalid)
	}
	var lease PreviewLease
	var headers http.Header
	path := "/v1/previews/" + url.PathEscape(previewID)
	if err := c.doRequestMeta(ctx, http.MethodGet, path, nil, &lease, http.Header{
		// A strong ETag describes the exact representation used for OCC. Do
		// not let a proxy's response encoder transform this representation.
		"Accept-Encoding": []string{"identity"},
	}, true, &headers); err != nil {
		return PreviewLease{}, err
	}
	lease.ETag = strings.TrimSpace(headers.Get("ETag"))
	if err := validatePreviewLease(lease); err != nil {
		return PreviewLease{}, err
	}
	if lease.ID != previewID {
		return PreviewLease{}, fmt.Errorf("%w: response ID does not match requested preview", ErrPreviewLeaseInvalid)
	}
	if _, err := previewLeaseETag(lease.ID, lease.ETag); err != nil {
		return PreviewLease{}, err
	}
	return lease, nil
}

// GetPreviewDomainDNSInstructions returns the server-owned customer records
// for one preview domain. The response is bound to the exact preview and
// domain IDs in the request before it is exposed to callers.
func (c *Client) GetPreviewDomainDNSInstructions(ctx context.Context, previewID, domainID string) (PreviewDNSInstructions, error) {
	if !validPreviewID(previewID) || !validPreviewID(domainID) {
		return PreviewDNSInstructions{}, fmt.Errorf("%w: preview and domain IDs are required", ErrPreviewLeaseInvalid)
	}
	path := "/v1/previews/" + url.PathEscape(previewID) + "/domains/" + url.PathEscape(domainID) + "/instructions"
	var instructions PreviewDNSInstructions
	if err := c.doStrict(ctx, http.MethodGet, path, nil, &instructions); err != nil {
		return PreviewDNSInstructions{}, err
	}
	hostname, err := NormalizePreviewDomain(instructions.Hostname)
	if err != nil || hostname != instructions.Hostname {
		return PreviewDNSInstructions{}, fmt.Errorf("%w: DNS instructions hostname is invalid", ErrPreviewLeaseInvalid)
	}
	lease := PreviewLease{ID: previewID}
	domain := PreviewDomainSummary{ID: domainID, Hostname: hostname}
	if err := validatePreviewDNSInstructions(lease, domain, instructions); err != nil {
		return PreviewDNSInstructions{}, fmt.Errorf("%w: invalid DNS instructions: %v", ErrPreviewLeaseInvalid, err)
	}
	return instructions, nil
}

// RenewPreviewLease extends a live lease while preserving its immutable
// identity. Endpoint is a server-projected public URL and can change when a
// self-hosted custom domain becomes ready or stops being usable.
func (c *Client) RenewPreviewLease(ctx context.Context, lease PreviewLease, ownerSessionID, idempotencyKey string) (PreviewLease, error) {
	if err := validateMutationLease(lease, idempotencyKey); err != nil {
		return PreviewLease{}, err
	}
	if strings.TrimSpace(ownerSessionID) == "" {
		return PreviewLease{}, fmt.Errorf("%w: owner session is required", ErrPreviewLeaseInvalid)
	}
	var renewed PreviewLease
	var headers http.Header
	path := "/v1/previews/" + url.PathEscape(lease.ID) + "/lease/renew"
	err := c.doRequestMeta(ctx, http.MethodPost, path, struct {
		OwnerSessionID string `json:"owner_session_id"`
	}{OwnerSessionID: ownerSessionID}, &renewed, http.Header{
		"If-Match":        []string{lease.ETag},
		"Idempotency-Key": []string{idempotencyKey},
		"Accept-Encoding": []string{"identity"},
	}, true, &headers)
	if err != nil {
		return PreviewLease{}, err
	}
	renewed.ETag = strings.TrimSpace(headers.Get("ETag"))
	if err := validatePreviewLease(renewed); err != nil {
		return PreviewLease{}, err
	}
	if _, err := previewLeaseETag(renewed.ID, renewed.ETag); err != nil {
		return PreviewLease{}, err
	}
	if !samePreviewLeaseIdentity(lease, renewed) {
		return PreviewLease{}, fmt.Errorf("%w: renewal changed lease identity", ErrPreviewLeaseInvalid)
	}
	return renewed, nil
}

// StopPreviewLease revokes the lease. Stop is intentionally idempotent at the
// server; callers must reuse one idempotency key across retries.
func (c *Client) StopPreviewLease(ctx context.Context, lease PreviewLease, idempotencyKey string) (PreviewLease, error) {
	if err := validateMutationLease(lease, idempotencyKey); err != nil {
		return PreviewLease{}, err
	}
	var stopped PreviewLease
	var headers http.Header
	path := "/v1/previews/" + url.PathEscape(lease.ID)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = c.doRequestMeta(ctx, http.MethodDelete, path, nil, &stopped, http.Header{
			"If-Match": []string{lease.ETag}, "Idempotency-Key": []string{idempotencyKey},
			"Accept-Encoding": []string{"identity"},
		}, true, &headers)
		var conflict *APIError
		if !errors.As(err, &conflict) || conflict.Code != "generation_conflict" || attempt == 2 {
			break
		}
		// Owner renewal can advance the lease between GET and DELETE. Refresh
		// only this resource; reuse the mutation key and preserve its identity.
		current, readErr := c.GetPreviewLease(ctx, lease.ID)
		if readErr != nil {
			return PreviewLease{}, readErr
		}
		if !samePreviewLeaseIdentity(lease, current) {
			return PreviewLease{}, ErrPreviewLeaseInvalid
		}
		lease = current
	}
	if err != nil {
		return PreviewLease{}, err
	}
	stopped.ETag = strings.TrimSpace(headers.Get("ETag"))
	if err := validatePreviewLease(stopped); err != nil {
		return PreviewLease{}, err
	}
	if _, err := previewLeaseETag(stopped.ID, stopped.ETag); err != nil {
		return PreviewLease{}, err
	}
	if !samePreviewLeaseIdentity(lease, stopped) {
		return PreviewLease{}, fmt.Errorf("%w: stop changed lease identity", ErrPreviewLeaseInvalid)
	}
	return stopped, nil
}

// PreviewLeasePage is the keyset-paginated response for GET /v1/previews.
// Items are complete canonical preview_lease resources; the cursor is an
// opaque server value and must only be replayed for the same account.
type PreviewLeasePage struct {
	Items      []PreviewLease `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// ListPreviewLeases returns one bounded page of temporary preview leases.
// The API owns cursor semantics, so the CLI never decodes or manufactures a
// cursor. A zero limit uses the protocol default; callers cannot request an
// unbounded response.
func (c *Client) ListPreviewLeases(ctx context.Context, cursor string, limit int) (PreviewLeasePage, error) {
	return c.ListPreviewLeasesFiltered(ctx, cursor, limit, nil)
}

func (c *Client) ListPreviewLeasesFiltered(ctx context.Context, cursor string, limit int, filters url.Values) (PreviewLeasePage, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 200 {
		return PreviewLeasePage{}, fmt.Errorf("%w: preview page limit must be between 1 and 200", ErrPreviewLeaseInvalid)
	}
	values := url.Values{"limit": []string{strconv.Itoa(limit)}}
	for _, key := range []string{"q", "state", "owner"} {
		if value := filters.Get(key); value != "" {
			values.Set(key, value)
		}
	}
	if strings.TrimSpace(cursor) != "" {
		cursor = strings.TrimSpace(cursor)
		if len(cursor) > 4096 || strings.ContainsAny(cursor, "\r\n") {
			return PreviewLeasePage{}, fmt.Errorf("%w: preview cursor is invalid", ErrPreviewLeaseInvalid)
		}
		values.Set("cursor", cursor)
	}
	var page PreviewLeasePage
	path := "/v1/previews?" + values.Encode()
	if err := c.doStrict(ctx, http.MethodGet, path, nil, &page); err != nil {
		return PreviewLeasePage{}, err
	}
	if len(page.Items) > limit {
		return PreviewLeasePage{}, fmt.Errorf("%w: preview page exceeds requested limit", ErrPreviewLeaseInvalid)
	}
	if len(page.NextCursor) > 4096 || strings.ContainsAny(page.NextCursor, "\r\n") {
		return PreviewLeasePage{}, fmt.Errorf("%w: preview next cursor is invalid", ErrPreviewLeaseInvalid)
	}
	seen := make(map[string]struct{}, len(page.Items))
	for index := range page.Items {
		item := page.Items[index]
		if err := validatePreviewLease(item); err != nil {
			return PreviewLeasePage{}, fmt.Errorf("%w: invalid preview at index %d: %v", ErrPreviewLeaseInvalid, index, err)
		}
		if _, ok := seen[item.ID]; ok {
			return PreviewLeasePage{}, fmt.Errorf("%w: duplicate preview resource", ErrPreviewLeaseInvalid)
		}
		seen[item.ID] = struct{}{}
	}
	return page, nil
}

// NormalizePreviewDomains converts user-supplied exact, apex, and one-label
// wildcard names into the canonical lower-case IDNA ASCII form. It sorts the
// result and rejects duplicate names after normalization, so the same request
// always has the same JSON and idempotency hash.
func NormalizePreviewDomains(domains []string) ([]string, error) {
	if len(domains) == 0 {
		// The v1 wire contract requires an explicit array. Returning a
		// non-nil empty slice prevents ordinary previews from silently
		// omitting the field and being rejected by the server.
		return []string{}, nil
	}
	if len(domains) > MaxPreviewDomains {
		return nil, fmt.Errorf("%w: at most %d domains are allowed", ErrPreviewLeaseInvalid, MaxPreviewDomains)
	}
	result := make([]string, 0, len(domains))
	seen := make(map[string]struct{}, len(domains))
	for _, raw := range domains {
		name, err := NormalizePreviewDomain(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("%w: duplicate domain %q", ErrPreviewLeaseInvalid, name)
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

// NormalizePreviewDomain validates one exact/apex or one-label wildcard
// hostname. A wildcard is allowed only as the complete first label. IP
// literals, ports, URL syntax, recursive wildcards, and local names are not
// preview domains because they cannot be verified as public DNS aliases.
func NormalizePreviewDomain(raw string) (string, error) {
	if strings.TrimSpace(raw) != raw || raw == "" || len(raw) > 253 || strings.ContainsAny(raw, "\r\n\x00 /?#:@") {
		return "", fmt.Errorf("%w: domain %q is invalid", ErrPreviewLeaseInvalid, raw)
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", fmt.Errorf("%w: domain %q contains whitespace or control characters", ErrPreviewLeaseInvalid, raw)
		}
	}
	wildcard := strings.HasPrefix(raw, "*.")
	value := raw
	if wildcard {
		value = strings.TrimPrefix(value, "*.")
		if value == "" || strings.Contains(value, "*") {
			return "", fmt.Errorf("%w: wildcard domain %q is invalid", ErrPreviewLeaseInvalid, raw)
		}
	} else if strings.Contains(raw, "*") {
		return "", fmt.Errorf("%w: wildcard must be the first label in %q", ErrPreviewLeaseInvalid, raw)
	}
	value = strings.TrimSuffix(value, ".")
	if net.ParseIP(value) != nil {
		return "", fmt.Errorf("%w: IP literals are not domains", ErrPreviewLeaseInvalid)
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil || ascii == "" || len(ascii) > 253 || strings.Contains(ascii, "..") {
		return "", fmt.Errorf("%w: domain %q is not valid IDNA", ErrPreviewLeaseInvalid, raw)
	}
	ascii = strings.ToLower(ascii)
	labels := strings.Split(ascii, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%w: domain %q must contain a DNS suffix", ErrPreviewLeaseInvalid, raw)
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("%w: domain %q has an invalid label", ErrPreviewLeaseInvalid, raw)
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return "", fmt.Errorf("%w: domain %q has an invalid label", ErrPreviewLeaseInvalid, raw)
			}
		}
	}
	if wildcard {
		if len(ascii)+2 > 253 {
			return "", fmt.Errorf("%w: wildcard domain %q is too long", ErrPreviewLeaseInvalid, raw)
		}
		return "*." + ascii, nil
	}
	return ascii, nil
}
