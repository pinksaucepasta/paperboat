package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func validateRequestedPreviewDomains(requested []string, lease PreviewLease) error {
	canonical, err := NormalizePreviewDomains(requested)
	if err != nil {
		return err
	}
	if len(canonical) == 0 {
		return nil
	}
	if len(lease.Domains) != len(canonical) {
		return fmt.Errorf("%w: server returned %d domains for %d requested domains", ErrPreviewLeaseInvalid, len(lease.Domains), len(canonical))
	}
	seen := make(map[string]struct{}, len(lease.Domains))
	for _, domain := range lease.Domains {
		if _, ok := seen[domain.Hostname]; ok {
			return fmt.Errorf("%w: server returned duplicate domain", ErrPreviewLeaseInvalid)
		}
		seen[domain.Hostname] = struct{}{}
	}
	for _, want := range canonical {
		found := false
		for _, domain := range lease.Domains {
			if domain.Hostname == want {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: server omitted requested domain %q", ErrPreviewLeaseInvalid, want)
		}
	}
	return nil
}

func validatePreviewDomainSummaries(lease PreviewLease) error {
	if len(lease.Domains) > MaxPreviewDomains {
		return fmt.Errorf("%w: lease contains too many domains", ErrPreviewLeaseInvalid)
	}
	seenIDs := make(map[string]struct{}, len(lease.Domains))
	seenHosts := make(map[string]struct{}, len(lease.Domains))
	for index, domain := range lease.Domains {
		if err := validatePreviewDomainSummary(lease, domain); err != nil {
			return fmt.Errorf("%w: invalid domain at index %d: %v", ErrPreviewLeaseInvalid, index, err)
		}
		if _, ok := seenIDs[domain.ID]; ok {
			return fmt.Errorf("%w: duplicate domain ID", ErrPreviewLeaseInvalid)
		}
		if _, ok := seenHosts[domain.Hostname]; ok {
			return fmt.Errorf("%w: duplicate domain hostname", ErrPreviewLeaseInvalid)
		}
		seenIDs[domain.ID] = struct{}{}
		seenHosts[domain.Hostname] = struct{}{}
	}
	return nil
}

func validatePreviewDomainSummary(lease PreviewLease, domain PreviewDomainSummary) error {
	if !validPreviewID(domain.ID) || domain.TargetKind != "preview_lease" || domain.PreviewID != lease.ID {
		return errors.New("domain target binding is invalid")
	}
	canonical, err := NormalizePreviewDomain(domain.Hostname)
	if err != nil || canonical != domain.Hostname {
		return errors.New("domain hostname is not canonical")
	}
	switch domain.MatchType {
	case "exact":
		if strings.HasPrefix(domain.Hostname, "*.") || domain.WildcardLabels != nil {
			return errors.New("exact domain cannot have wildcard labels")
		}
	case "one_label_wildcard":
		if !strings.HasPrefix(domain.Hostname, "*.") || domain.WildcardLabels == nil || *domain.WildcardLabels != 1 {
			return errors.New("one-label wildcard domain is invalid")
		}
	default:
		return errors.New("domain match type is invalid")
	}
	if !validatePreviewDomainState(domain.State) || domain.Generation < 1 {
		return errors.New("domain state or generation is invalid")
	}
	if err := validatePreviewDomainDNS(domain.DNS); err != nil {
		return err
	}
	if err := validatePreviewDomainCertificate(domain.Certificate); err != nil {
		return err
	}
	if err := validatePreviewResourceETag(domain.ETag); err != nil {
		return err
	}
	if domain.Instructions != nil {
		if err := validatePreviewDNSInstructions(lease, domain, *domain.Instructions); err != nil {
			return err
		}
	}
	return nil
}

func validatePreviewDomainDNS(dns PreviewDomainDNS) error {
	if strings.TrimSpace(dns.Target) == "" || len(dns.Target) > 253 || strings.ContainsAny(dns.Target, "\r\n\x00") {
		return errors.New("domain DNS target is invalid")
	}
	if len(dns.ObservedRecords) > 32 {
		return errors.New("domain DNS observations exceed the limit")
	}
	for _, record := range dns.ObservedRecords {
		if strings.TrimSpace(record) == "" || len(record) > 253 || strings.ContainsAny(record, "\r\n\x00") {
			return errors.New("domain DNS observation is invalid")
		}
	}
	if dns.LastCheckedAt != nil && dns.LastCheckedAt.IsZero() {
		return errors.New("domain DNS check time is invalid")
	}
	return nil
}

func validatePreviewDomainCertificate(certificate PreviewDomainCertificate) error {
	switch certificate.State {
	case "not_requested", "issuing", "ready", "renewing", "failed", "expired", "revoked":
	default:
		return errors.New("domain certificate state is invalid")
	}
	if certificate.Reference != "" && (len(certificate.Reference) > 512 || strings.ContainsAny(certificate.Reference, "\r\n\x00") || strings.Contains(certificate.Reference, "-----BEGIN")) {
		return errors.New("domain certificate reference is invalid")
	}
	if certificate.ExpiresAt != nil && certificate.ExpiresAt.IsZero() {
		return errors.New("domain certificate expiry is invalid")
	}
	return validatePreviewSafeObject(certificate.Failure, 0)
}

func validatePreviewDNSInstructions(lease PreviewLease, domain PreviewDomainSummary, instructions PreviewDNSInstructions) error {
	if instructions.Schema != PreviewTunnelSchemaV1 || instructions.Kind != "dns_instructions" || instructions.TargetKind != "preview_lease" || instructions.PreviewID != lease.ID || instructions.DomainID != domain.ID || instructions.Hostname != domain.Hostname || strings.TrimSpace(instructions.Provider) == "" || len(instructions.Provider) > 64 || strings.TrimSpace(instructions.Note) == "" || len(instructions.Note) > 512 || strings.ContainsAny(instructions.Note, "\r\n\x00") {
		return errors.New("domain DNS instructions binding is invalid")
	}
	switch instructions.VerificationState {
	case "requested", "waiting_dns", "verified", "issuing_tls", "ready", "conflict", "dns_error", "tls_error", "expired", "quarantined":
	default:
		return errors.New("domain DNS instruction state is invalid")
	}
	if len(instructions.Records) == 0 || len(instructions.Records) > 8 {
		return errors.New("domain DNS instruction records are invalid")
	}
	addressRecords := 0
	aliasRecords := 0
	seenRecords := make(map[string]struct{}, len(instructions.Records))
	challengeName := "_acme-challenge." + strings.TrimPrefix(domain.Hostname, "*.")
	for _, record := range instructions.Records {
		if record.TTL < 30 || record.TTL > 86400 || len(record.Name) == 0 || len(record.Name) > 253 || len(record.Value) == 0 || len(record.Value) > 253 || strings.ContainsAny(record.Name+record.Value, "\r\n\x00") {
			return errors.New("domain DNS instruction record is invalid")
		}
		key := record.Name + "\x00" + record.Type + "\x00" + record.Value
		if _, exists := seenRecords[key]; exists {
			return errors.New("domain DNS instruction record is duplicated")
		}
		seenRecords[key] = struct{}{}

		switch record.Type {
		case "A", "AAAA":
			addressRecords++
			ip := net.ParseIP(record.Value)
			if record.Name != domain.Hostname || ip == nil || ip.String() != record.Value || (record.Type == "A" && ip.To4() == nil) || (record.Type == "AAAA" && (ip.To4() != nil || ip.To16() == nil)) {
				return errors.New("domain DNS address record is invalid")
			}
		case "ANAME", "ALIAS":
			aliasRecords++
			if record.Name != domain.Hostname || !validPreviewDNSAliasTarget(record.Value) {
				return errors.New("domain DNS alias record is invalid")
			}
		case "CNAME":
			aliasRecords++
			if (record.Name != domain.Hostname && record.Name != challengeName) || !validPreviewDNSAliasTarget(record.Value) {
				return errors.New("domain DNS CNAME record is invalid")
			}
		default:
			return errors.New("domain DNS instruction record type is invalid")
		}
	}
	// At most two selected self-hosted edges contribute IPv4 and IPv6
	// addresses, followed by one primary alias and one ACME delegation.
	if addressRecords > 4 || aliasRecords > 2 {
		return errors.New("domain DNS instruction records exceed the placement limit")
	}
	return nil
}

func validPreviewDNSAliasTarget(value string) bool {
	if strings.Contains(value, "*") {
		return false
	}
	normalized, err := NormalizePreviewDomain(value)
	return err == nil && normalized == value
}

func validatePreviewResourceETag(etag string) error {
	if len(etag) < 3 || len(etag) > 512 || etag[0] != '"' || etag[len(etag)-1] != '"' || strings.ContainsAny(etag[1:len(etag)-1], "\r\n\x00\"") {
		return errors.New("domain ETag is not strong")
	}
	return nil
}

func validatePreviewDomainState(value string) bool {
	switch value {
	case "requested", "waiting_dns", "verified", "issuing_tls", "ready", "conflict", "dns_error", "tls_error", "expired", "quarantined", "released":
		return true
	default:
		return false
	}
}

func validatePreviewSafeObject(value map[string]any, depth int) error {
	if value == nil {
		return nil
	}
	if depth > 4 || len(value) > 32 {
		return errors.New("domain failure metadata exceeds the limit")
	}
	for key, item := range value {
		if strings.TrimSpace(key) == "" || len(key) > 64 || strings.ContainsAny(key, "\r\n\x00") || strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "secret") || strings.Contains(strings.ToLower(key), "private") || strings.Contains(strings.ToLower(key), "authorization") || strings.Contains(strings.ToLower(key), "password") || strings.Contains(strings.ToLower(key), "cookie") {
			return errors.New("domain failure metadata contains a secret field")
		}
		switch typed := item.(type) {
		case string:
			if len(typed) > 500 || strings.ContainsAny(typed, "\r\n\x00") {
				return errors.New("domain failure metadata string is invalid")
			}
		case map[string]any:
			if err := validatePreviewSafeObject(typed, depth+1); err != nil {
				return err
			}
		case []any:
			if len(typed) > 32 {
				return errors.New("domain failure metadata array exceeds the limit")
			}
			for _, child := range typed {
				if nested, ok := child.(map[string]any); ok {
					if err := validatePreviewSafeObject(nested, depth+1); err != nil {
						return err
					}
				} else if text, ok := child.(string); ok && (len(text) > 500 || strings.ContainsAny(text, "\r\n\x00")) {
					return errors.New("domain failure metadata string is invalid")
				}
			}
		case nil, bool, float64:
		default:
			return errors.New("domain failure metadata type is invalid")
		}
	}
	return nil
}

func validatePreviewLeaseCreateInput(input PreviewLeaseCreateRequest) error {
	if !validPreviewID(input.OwnerMachineID) || !validPreviewID(input.OwnerSessionID) || (input.OwnerSessionKind != "local_lease" && input.OwnerSessionKind != "foreground") {
		return fmt.Errorf("%w: owner machine and session are required", ErrPreviewLeaseInvalid)
	}
	mode := strings.ToLower(strings.TrimSpace(input.AccessMode))
	if mode != "" && mode != "public" && mode != "private" && mode != "team" {
		return fmt.Errorf("%w: access mode must be public, private, or team", ErrPreviewLeaseInvalid)
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("%w: expiration must be in the future", ErrPreviewLeaseInvalid)
	}
	scheme := strings.ToLower(strings.TrimSpace(input.Target.Scheme))
	if scheme != "http" && scheme != "https" && scheme != "h2c" && scheme != "unix" && scheme != "tcp" {
		return fmt.Errorf("%w: target scheme is unsupported", ErrPreviewLeaseInvalid)
	}
	if strings.TrimSpace(input.Target.Address) == "" || len(input.Target.Address) > 512 || strings.ContainsAny(input.Target.Address, "\r\n") {
		return fmt.Errorf("%w: target address is invalid", ErrPreviewLeaseInvalid)
	}
	if _, err := NormalizePreviewDomains(input.Domains); err != nil {
		return err
	}
	return nil
}

func validateMutationLease(lease PreviewLease, idempotencyKey string) error {
	if err := validatePreviewLeaseIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	if err := validatePreviewLease(lease); err != nil {
		return err
	}
	if strings.TrimSpace(lease.ETag) == "" {
		return ErrPreviewLeaseETagRequired
	}
	if _, err := previewLeaseETag(lease.ID, lease.ETag); err != nil {
		return err
	}
	return nil
}

// validatePreviewLeaseIdempotencyKey keeps mutation keys stable across the
// client/server boundary. Header values containing whitespace, controls, or
// non-ASCII text are rejected instead of being silently trimmed or rewritten,
// which would make the server's idempotency fingerprint ambiguous.
func validatePreviewLeaseIdempotencyKey(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
		return errors.New("preview lease idempotency key is required")
	}
	for _, r := range value {
		if r > unicode.MaxASCII || unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("preview lease idempotency key is invalid")
		}
	}
	return nil
}

func validatePreviewLease(lease PreviewLease) error {
	if lease.Schema != PreviewTunnelSchemaV1 || lease.Kind != "preview_lease" || !validPreviewID(lease.ID) || !validPreviewID(lease.AccountID) || !validPreviewID(lease.ActorID) || !validPreviewID(lease.OwnerMachineID) || !validPreviewID(lease.OwnerSessionID) || !validPreviewOwnerSessionKind(lease.OwnerSessionKind) {
		return fmt.Errorf("%w: required identity fields are missing", ErrPreviewLeaseInvalid)
	}
	if lease.RouteID != "" && !validPreviewID(lease.RouteID) {
		return fmt.Errorf("%w: carrier route identity is invalid", ErrPreviewLeaseInvalid)
	}
	if lease.AccessMode != "public" && lease.AccessMode != "private" && lease.AccessMode != "team" {
		return fmt.Errorf("%w: unsupported access mode", ErrPreviewLeaseInvalid)
	}
	if lease.Persistent {
		return fmt.Errorf("%w: preview leases cannot be persistent", ErrPreviewLeaseInvalid)
	}
	if err := validatePreviewLeaseTarget(lease.Target); err != nil {
		return err
	}
	switch lease.EndpointState {
	case "ready":
		if !validPreviewHTTPSURL(lease.Endpoint) {
			return fmt.Errorf("%w: ready endpoint must be an HTTPS URL", ErrPreviewLeaseInvalid)
		}
	case "ended":
		if lease.Endpoint != "" || lease.State != "owner_disconnected" && lease.State != "expired" && lease.State != "stopped" {
			return fmt.Errorf("%w: an ended lease must have no endpoint and a terminal state", ErrPreviewLeaseInvalid)
		}
	case "custom_domain_not_ready":
		if lease.Endpoint != "" || lease.State == "ready" {
			return fmt.Errorf("%w: unavailable custom domain must have no endpoint and cannot be ready", ErrPreviewLeaseInvalid)
		}
	default:
		return fmt.Errorf("%w: endpoint state is invalid", ErrPreviewLeaseInvalid)
	}
	if lease.LeaseDeadline.IsZero() || lease.CreatedAt.IsZero() || lease.LastRenewedAt.IsZero() {
		return fmt.Errorf("%w: lease deadline is missing", ErrPreviewLeaseInvalid)
	}
	if lease.UserDeadline != nil && lease.UserDeadline.IsZero() {
		return fmt.Errorf("%w: user deadline is invalid", ErrPreviewLeaseInvalid)
	}
	if !validPreviewLeaseState(lease.State) || !validPreviewAllocationState(lease.AllocationState) || !validPreviewEdgeState(lease.EdgeState) || !validPreviewOriginState(lease.OriginState) {
		return fmt.Errorf("%w: lease state is invalid", ErrPreviewLeaseInvalid)
	}
	if lease.ETag != "" {
		if _, err := previewLeaseETag(lease.ID, lease.ETag); err != nil {
			return err
		}
	}
	if err := validatePreviewDomainSummaries(lease); err != nil {
		return err
	}
	return nil
}

func validPreviewHTTPSURL(value string) bool {
	endpoint, err := url.Parse(value)
	return err == nil && endpoint.Scheme == "https" && endpoint.Hostname() != "" && endpoint.User == nil && endpoint.Opaque == "" && endpoint.RawQuery == "" && endpoint.Fragment == "" && (endpoint.Path == "" || endpoint.Path == "/") && (endpoint.RawPath == "" || endpoint.RawPath == "/")
}

func samePreviewLeaseIdentity(left, right PreviewLease) bool {
	return left.Schema == right.Schema && left.Kind == right.Kind && left.ID == right.ID &&
		left.AccountID == right.AccountID && left.ActorID == right.ActorID &&
		left.OwnerMachineID == right.OwnerMachineID && left.OwnerSessionID == right.OwnerSessionID && left.OwnerSessionKind == right.OwnerSessionKind &&
		left.Target == right.Target && left.AccessMode == right.AccessMode && left.Persistent == right.Persistent &&
		left.CreatedAt.Equal(right.CreatedAt) && equalPreviewOptionalTime(left.UserDeadline, right.UserDeadline)
}

func equalPreviewOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validatePreviewLeaseTarget(target PreviewLeaseTarget) error {
	scheme := strings.ToLower(strings.TrimSpace(target.Scheme))
	if scheme != "http" && scheme != "https" && scheme != "h2c" && scheme != "unix" && scheme != "tcp" {
		return fmt.Errorf("%w: target scheme is unsupported", ErrPreviewLeaseInvalid)
	}
	if strings.TrimSpace(target.Address) == "" || len(target.Address) > 512 || strings.ContainsAny(target.Address, "\r\n") {
		return fmt.Errorf("%w: target address is invalid", ErrPreviewLeaseInvalid)
	}
	return nil
}

func validatePreviewLeaseOperation(operation PreviewLeaseOperation) error {
	if operation.Schema != PreviewTunnelSchemaV1 || operation.Kind != "operation" || !validPreviewID(operation.ID) || operation.ResourceKind != "preview_lease" || !validPreviewID(operation.ResourceID) || !validPreviewOperationPhase(operation.Phase) || operation.Progress < 0 || operation.Progress > 100 || !validPreviewID(operation.CorrelationID) || operation.CreatedAt.IsZero() || operation.UpdatedAt.IsZero() || operation.NextRetryAt != nil && operation.NextRetryAt.IsZero() {
		return ErrPreviewLeaseInvalid
	}
	projectOperationError(operation.Error)
	switch operation.State {
	case "pending", "running", "succeeded", "failed", "canceled":
		return nil
	default:
		return ErrPreviewLeaseInvalid
	}
}

func validPreviewOperationPhase(value string) bool {
	switch value {
	case "validating", "persisting", "waiting_for_dns", "issuing_certificate", "installing_service", "connecting", "checking_origin", "draining", "rolling_back", "ready", "failed":
		return true
	default:
		return false
	}
}

func validPreviewID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validPreviewLeaseState(value string) bool {
	switch value {
	case "allocating", "connecting", "ready", "origin_unavailable", "owner_disconnected", "expired", "stopped":
		return true
	default:
		return false
	}
}

func validPreviewAllocationState(value string) bool {
	switch value {
	case "pending", "ready", "failed", "released":
		return true
	default:
		return false
	}
}

func validPreviewEdgeState(value string) bool {
	switch value {
	case "pending", "ready", "degraded", "down":
		return true
	default:
		return false
	}
}

func validPreviewOriginState(value string) bool {
	switch value {
	case "unknown", "ready", "unavailable":
		return true
	default:
		return false
	}
}

func previewLeaseETag(id, etag string) (int64, error) {
	value := strings.TrimSpace(etag)
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return 0, fmt.Errorf("%w: lease ETag is not strong", ErrPreviewLeaseInvalid)
	}
	parts := strings.Split(strings.Trim(value, `"`), ":")
	if len(parts) != 4 || parts[0] != "ptv1" || parts[1] != "preview_lease" {
		return 0, fmt.Errorf("%w: lease ETag has the wrong resource", ErrPreviewLeaseInvalid)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || string(decoded) != id || base64.RawURLEncoding.EncodeToString(decoded) != parts[2] {
		return 0, fmt.Errorf("%w: lease ETag does not match the resource", ErrPreviewLeaseInvalid)
	}
	generation, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || generation < 1 {
		return 0, fmt.Errorf("%w: lease ETag generation is invalid", ErrPreviewLeaseInvalid)
	}
	return generation, nil
}

// ValidatePreviewLeaseETag validates the strong preview lease precondition
// without exposing its generation parser. Host-runtime subpackages use this
// before signing requests that carry If-Match.
func ValidatePreviewLeaseETag(id, etag string) error {
	_, err := previewLeaseETag(id, etag)
	return err
}

func validPreviewOwnerSessionKind(kind string) bool {
	return kind == "local_lease" || kind == "foreground" || kind == "lazy_runtime"
}
