package api

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func tunnelPath(parts ...string) (string, error) {
	path := "/v1"
	for _, part := range parts {
		if !validTunnelToken(part) {
			return "", errors.New("invalid tunnel resource identifier")
		}
		path += "/" + url.PathEscape(part)
	}
	return path, nil
}

func validTunnelToken(v string) bool {
	return v != "" && v == strings.TrimSpace(v) && len(v) <= 256 && !strings.ContainsAny(v, "\x00\r\n\t /\\?#%")
}

func containsTunnelControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r >= 0x7f && r <= 0x9f {
			return true
		}
	}
	return false
}

var tunnelNamePatternV1 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

var tunnelRouteNamePatternV1 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$`)

func validTunnelID(v string) bool {
	return len(v) >= 3 && len(v) <= 128 && validTunnelToken(v)
}

func validTunnelETag(v string) bool {
	return len(v) >= 3 && len(v) <= 512 && v[0] == '"' && v[len(v)-1] == '"' && !strings.ContainsAny(v[1:len(v)-1], "\"\r\n\x00")
}

func validateTunnelName(value string) error {
	if !tunnelNamePatternV1.MatchString(value) {
		return errors.New("tunnel name must be 1-63 ASCII letters, digits, '.', '_' or '-' and cannot start with punctuation")
	}
	return nil
}

func validateDomainHostname(value string) error {
	if value != strings.TrimSpace(value) || len(value) < 1 || len(value) > 253 || strings.ContainsAny(value, "\x00\r\n\t /?#@:") {
		return errors.New("invalid hostname")
	}
	value = strings.TrimPrefix(value, "*.")
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return errors.New("hostname must contain a DNS suffix")
	}
	for _, label := range labels {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("invalid hostname")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("invalid hostname")
			}
		}
	}
	return nil
}

func validateTunnelOrigin(scheme, address string) error {
	if address == "" || address != strings.TrimSpace(address) || len(address) > 512 || strings.ContainsAny(address, "\x00\r\n\t") {
		return errors.New("origin address is not canonical")
	}
	switch scheme {
	case "http", "https", "h2c", "tcp":
		host, portText, err := net.SplitHostPort(address)
		if err != nil || host == "" || portText == "" || !validTunnelOriginHost(host) {
			return errors.New("origin address must contain a host and numeric port")
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
			return errors.New("origin port must be between 1 and 65535")
		}
	case "unix":
		if !strings.HasPrefix(address, "/") || path.Clean(address) != address {
			return errors.New("Unix origin must be a clean absolute path")
		}
	default:
		return errors.New("origin scheme must be http, https, h2c, unix, or tcp")
	}
	return nil
}

func validTunnelOriginHost(host string) bool {
	if host == "" || len(host) > 253 || host != strings.TrimSpace(host) || strings.ContainsAny(host, "\x00\r\n\t /?#@") {
		return false
	}
	if net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func validateTunnelCreateInput(in TunnelCreateInput) error {
	if err := validateTunnelName(in.Name); err != nil {
		return err
	}
	if in.AccessMode == "" {
		in.AccessMode = "public"
	}
	if in.AccessMode != "public" && in.AccessMode != "private" && in.AccessMode != "team" {
		return errors.New("access mode must be public, private, or team")
	}
	if err := validateTunnelOrigin(in.Origin.Scheme, in.Origin.Address); err != nil {
		return err
	}
	if in.Origin.HostOverride != nil && !validTunnelOriginHost(*in.Origin.HostOverride) {
		return errors.New("origin host override is invalid")
	}
	if in.ExpiresAt != nil && in.ExpiresAt.IsZero() {
		return errors.New("expiry is invalid")
	}
	return nil
}

func validateHostMatch(match TunnelRouteHostMatch) error {
	switch match.Type {
	case "managed_exact", "exact":
		if match.WildcardLabels != nil || strings.HasPrefix(match.Hostname, "*.") {
			return errors.New("exact host match is invalid")
		}
		return validateDomainHostname(match.Hostname)
	case "one_label_wildcard":
		if match.WildcardLabels == nil || *match.WildcardLabels != 1 || !strings.HasPrefix(match.Hostname, "*.") {
			return errors.New("one-label wildcard match is invalid")
		}
		return validateDomainHostname(match.Hostname)
	case "catch_all":
		if match.Hostname != "" || match.WildcardLabels != nil {
			return errors.New("catch-all match cannot include a hostname")
		}
		return nil
	default:
		return errors.New("host match type is invalid")
	}
}

func validateRouteOrigin(origin TunnelRouteOrigin) error {
	if err := validateTunnelOrigin(origin.Scheme, origin.Address); err != nil {
		return err
	}
	if origin.HostOverride != nil && !validTunnelOriginHost(*origin.HostOverride) {
		return errors.New("host override is invalid")
	}
	if origin.Scheme != "https" {
		if origin.TLS != nil {
			return errors.New("TLS settings require an HTTPS origin")
		}
		return nil
	}
	if origin.TLS == nil {
		return errors.New("HTTPS origins require a TLS verification policy")
	}
	switch origin.TLS.Verification {
	case "system", "custom_ca", "insecure_development":
	default:
		return errors.New("TLS verification is invalid")
	}
	if origin.TLS.Verification == "custom_ca" && (origin.TLS.CAReference == nil || strings.TrimSpace(*origin.TLS.CAReference) == "") {
		return errors.New("custom CA reference is required")
	}
	if origin.TLS.Verification != "custom_ca" && origin.TLS.CAReference != nil {
		return errors.New("CA reference requires custom_ca verification")
	}
	if origin.TLS.ServerName != nil && !validTunnelOriginHost(*origin.TLS.ServerName) {
		return errors.New("TLS server name is invalid")
	}
	for _, reference := range []*string{origin.TLS.CAReference, origin.TLS.ClientCredentialReference} {
		if reference != nil && !validTunnelCredentialReference(*reference) {
			return errors.New("TLS credential reference is invalid")
		}
	}
	return nil
}

func validTunnelCredentialReference(value string) bool {
	if len(value) < 24 || len(value) > 512 || !strings.Contains(value, "://paperboat/") || strings.ContainsAny(value, "\x00\r\n\t ") {
		return false
	}
	prefix := strings.SplitN(value, "://", 2)[0]
	switch prefix {
	case "keychain", "credential-manager", "secret-service", "protected-file", "tpm":
		return true
	default:
		return false
	}
}

func validateTunnelRouteInput(in TunnelRouteInput, allowDefaults bool) error {
	if !tunnelRouteNamePatternV1.MatchString(in.Name) {
		return errors.New("route name is invalid")
	}
	if in.Protocol != "http" && in.Protocol != "tcp" && in.Protocol != "tls" && in.Protocol != "tcp_private" {
		return errors.New("route protocol must be http, tcp, tls, or tcp_private")
	}
	if err := validateHostMatch(in.HostMatch); err != nil {
		return err
	}
	if in.PathPrefix != nil && (*in.PathPrefix == "" || len(*in.PathPrefix) > 2048 || !strings.HasPrefix(*in.PathPrefix, "/") || strings.ContainsAny(*in.PathPrefix, "\x00\r\n")) {
		return errors.New("path prefix is invalid")
	}
	if in.Protocol == "tcp_private" && (in.HostMatch.Type != "catch_all" || in.PathPrefix != nil || in.Origin.Scheme != "tcp") {
		return errors.New("private TCP routes require catch-all matching and a tcp origin")
	}
	if (in.Protocol == "tcp" || in.Protocol == "tls") && (in.HostMatch.Type != "managed_exact" || in.HostMatch.Hostname == "" || in.PathPrefix != nil || in.Origin.Scheme != "tcp") {
		return errors.New("public TCP routes require managed hostname matching and a tcp origin")
	}
	if err := validateRouteOrigin(in.Origin); err != nil {
		return err
	}
	if !allowDefaults && in.Origin.Scheme == "https" && in.Origin.TLS == nil {
		return errors.New("HTTPS route response is missing TLS policy")
	}
	if in.Priority < 0 || in.Priority > 1000000 {
		return errors.New("priority is out of range")
	}
	if allowDefaults {
		if in.ConnectTimeoutMS != 0 && (in.ConnectTimeoutMS < 100 || in.ConnectTimeoutMS > 120000) {
			return errors.New("connect timeout is out of range")
		}
		if in.IdleTimeoutMS != 0 && (in.IdleTimeoutMS < 1000 || in.IdleTimeoutMS > 3600000) {
			return errors.New("idle timeout is out of range")
		}
		if in.MaxConcurrentStreams != 0 && (in.MaxConcurrentStreams < 1 || in.MaxConcurrentStreams > 10000) {
			return errors.New("stream limit is out of range")
		}
		return nil
	}
	if in.ConnectTimeoutMS < 100 || in.ConnectTimeoutMS > 120000 || in.IdleTimeoutMS < 1000 || in.IdleTimeoutMS > 3600000 || in.MaxConcurrentStreams < 1 || in.MaxConcurrentStreams > 10000 {
		return errors.New("route timeout or stream limit is out of range")
	}
	return nil
}

func validateTunnelDomainInput(in TunnelDomainInput) error {
	if err := validateDomainHostname(in.Hostname); err != nil {
		return err
	}
	if !validTunnelID(in.RouteID) {
		return errors.New("invalid route identifier")
	}
	if in.Provider == "" {
		in.Provider = "generic"
	}
	if in.Provider != "generic" && in.Provider != "cloudflare" && in.Provider != "route53" && in.Provider != "google_cloud_dns" && in.Provider != "digitalocean" && in.Provider != "namecheap" {
		return errors.New("invalid DNS provider")
	}
	if len(in.Provider) > 64 || strings.ContainsAny(in.Provider, "\x00\r\n") {
		return errors.New("invalid DNS provider")
	}
	if in.CertificateStrategy != "" && (len(in.CertificateStrategy) > 64 || strings.ContainsAny(in.CertificateStrategy, "\x00\r\n")) {
		return errors.New("invalid certificate strategy")
	}
	return nil
}

func validateOperation(v *TunnelOperation, resourceKind, resourceID string) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "operation" || !validTunnelID(v.ID) || !validTunnelToken(v.ResourceKind) || !validTunnelID(v.ResourceID) || v.ResourceKind != resourceKind || v.ResourceID != resourceID || v.Phase == "" || v.State == "" || v.Progress < 0 || v.Progress > 100 || len(v.CorrelationID) > 256 || containsTunnelControl(v.Phase+v.State+v.CorrelationID) || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() || v.UpdatedAt.Before(v.CreatedAt) {
		return ErrUnsafeTunnelResponse
	}
	switch v.Phase {
	case "validating", "persisting", "waiting_for_dns", "issuing_certificate", "installing_service", "connecting", "checking_origin", "draining", "rolling_back", "ready", "failed":
	default:
		return ErrUnsafeTunnelResponse
	}
	switch v.State {
	case "pending", "running", "succeeded", "failed", "canceled":
	default:
		return ErrUnsafeTunnelResponse
	}
	if v.Error != nil {
		if v.Error.Schema != TunnelV1Schema || v.Error.Kind != "error" || v.Error.Code == "" || len(v.Error.Code) > 128 || len(v.Error.Message) > 1000 || len(v.Error.RepairAction) > 500 || containsTunnelControl(v.Error.Code+v.Error.Component+v.Error.Outcome+v.Error.RequestID+v.Error.CorrelationID+v.Error.Message+v.Error.RepairAction) {
			return ErrUnsafeTunnelResponse
		}
		projectOperationError(v.Error)
	}
	return nil
}

func pageQuery(cursor string, limit int) (string, error) {
	if limit < 1 || limit > 200 || len(cursor) > 4096 || strings.ContainsAny(cursor, "\x00\r\n") {
		return "", errors.New("invalid page request")
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return "?" + q.Encode(), nil
}

func validNextCursor(cursor string) bool {
	return len(cursor) <= 4096 && !strings.ContainsAny(cursor, "\x00\r\n")
}

func mutationHeaders(etag, key string) (http.Header, error) {
	if !validTunnelToken(key) {
		return nil, errors.New("invalid idempotency key")
	}
	h := http.Header{"Idempotency-Key": []string{key}}
	if etag != "" {
		if !validTunnelETag(etag) {
			return nil, errors.New("invalid ETag")
		}
		h.Set("If-Match", etag)
	}
	return h, nil
}

func requiredTunnelMutationHeaders(etag, key string) (http.Header, error) {
	if !validTunnelETag(etag) {
		return nil, errors.New("valid ETag is required for tunnel mutation")
	}
	return mutationHeaders(etag, key)
}

func validateTunnel(v Tunnel) error {
	if v.Schema != TunnelV1Schema || v.Kind != "tunnel" || !validTunnelID(v.ID) || !validTunnelID(v.AccountID) || !validTunnelID(v.CreatedByHostID) || !validTunnelID(v.CreatedByActorID) || !tunnelNamePatternV1.MatchString(v.Name) || (v.AccessMode != "public" && v.AccessMode != "private" && v.AccessMode != "team") || (v.DesiredState != "active" && v.DesiredState != "paused" && v.DesiredState != "deleted") || v.Generation < 1 || !validTunnelETag(v.ETag) || len(v.SummaryCode) > 128 || strings.ContainsAny(v.SummaryCode, "\x00\r\n") {
		return ErrUnsafeTunnelResponse
	}
	if v.StableEndpoint == "" {
		if v.SummaryCode != "custom_domain_required" || !tunnelEndpointUUIDPattern.MatchString(v.StableEndpointID) {
			return ErrUnsafeTunnelResponse
		}
		return nil
	}
	if v.SummaryCode == "custom_domain_required" {
		return ErrUnsafeTunnelResponse
	}
	u, err := url.Parse(v.StableEndpoint)
	if err != nil || u == nil {
		return ErrUnsafeTunnelResponse
	}
	hostLabels := strings.Split(u.Hostname(), ".")
	if !tunnelEndpointUUIDPattern.MatchString(v.StableEndpointID) || u.Scheme != "https" || len(hostLabels) < 2 || validateDomainHostname(u.Hostname()) != nil || u.Hostname() != strings.ToLower(u.Hostname()) || strings.Contains(u.Hostname(), "*") || net.ParseIP(u.Hostname()) != nil || u.User != nil || u.Port() != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return ErrUnsafeTunnelResponse
	}
	return nil
}

func validateRoute(v TunnelRoute) error {
	if v.Schema != TunnelV1Schema || v.Kind != "route" || !validTunnelID(v.ID) || !validTunnelID(v.TunnelID) || v.Generation < 1 || !validTunnelETag(v.ETag) || validateTunnelRouteInput(TunnelRouteInput{Name: v.Name, Protocol: v.Protocol, HostMatch: v.HostMatch, PathPrefix: v.PathPrefix, Origin: v.Origin, Priority: v.Priority, ConnectTimeoutMS: v.ConnectTimeoutMS, IdleTimeoutMS: v.IdleTimeoutMS, MaxConcurrentStreams: v.MaxConcurrentStreams}, false) != nil || (v.DesiredState != "active" && v.DesiredState != "disabled" && v.DesiredState != "deleted") {
		return ErrUnsafeTunnelResponse
	}
	if v.Protocol == "tcp" {
		if !validTunnelID(v.PublicTCPListenerID) || v.PublicTCPPort < 1024 || v.PublicTCPPort > 65535 {
			return ErrUnsafeTunnelResponse
		}
	} else if v.PublicTCPListenerID != "" || v.PublicTCPPort != 0 {
		return ErrUnsafeTunnelResponse
	}
	return nil
}

func validateDomain(v *TunnelDomain) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "domain_binding" || !validTunnelID(v.ID) || !validTunnelID(v.AccountID) || !validTunnelID(v.TunnelID) || !validTunnelID(v.RouteID) || v.Generation < 1 || !validTunnelETag(v.ETag) || validateDomainHostname(v.Hostname) != nil || v.DNS.Target == "" || len(v.DNS.Target) > 512 || len(v.DNS.ObservedRecords) > 64 || v.Certificate.State == "" || len(v.Certificate.Reference) > 512 {
		return ErrUnsafeTunnelResponse
	}
	if v.MatchType == "one_label_wildcard" {
		if v.WildcardLabels == nil || *v.WildcardLabels != 1 || !strings.HasPrefix(v.Hostname, "*.") {
			return ErrUnsafeTunnelResponse
		}
	} else if v.MatchType != "exact" || v.WildcardLabels != nil {
		return ErrUnsafeTunnelResponse
	}
	switch v.State {
	case "requested", "waiting_dns", "verified", "issuing_tls", "ready", "conflict", "dns_error", "tls_error", "expired", "quarantined":
	default:
		return ErrUnsafeTunnelResponse
	}
	switch v.Certificate.State {
	case "not_requested", "issuing", "ready", "renewing", "failed", "expired", "revoked", "not_applicable":
	default:
		return ErrUnsafeTunnelResponse
	}
	if (v.CertificateStrategy == "none") != (v.Certificate.State == "not_applicable") {
		return ErrUnsafeTunnelResponse
	}
	if v.Certificate.Failure != nil {
		clean, err := redactTunnelMetadata(v.Certificate.Failure, 0)
		if err != nil {
			return err
		}
		v.Certificate.Failure = clean
	}
	return nil
}

func validateConnector(v TunnelConnector) error {
	if v.Schema != TunnelV1Schema || v.Kind != "connector" || !validTunnelID(v.ID) || !validTunnelID(v.TunnelID) || !validTunnelID(v.HostID) || v.Generation < 1 || v.RotationGeneration < 1 || !validTunnelETag(v.ETag) || v.ProtocolVersion != "1.0" || !validTunnelCredentialReference(v.CredentialReference) || (v.DesiredState != "active" && v.DesiredState != "draining" && v.DesiredState != "revoked") || (v.DrainState != "accepting" && v.DrainState != "draining" && v.DrainState != "drained" && v.DrainState != "forced_closed") {
		return ErrUnsafeTunnelResponse
	}
	return nil
}

func validateHealth(v *TunnelHealth, tunnelID string) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "health" || v.ResourceKind != "tunnel" || v.ResourceID != tunnelID || v.OverallCode == "" || len(v.Summary) > 1000 || len(v.RepairAction) > 500 || len(v.CorrelationID) > 256 || containsTunnelControl(v.OverallCode+v.Summary+v.RepairAction+v.CorrelationID) {
		return ErrUnsafeTunnelResponse
	}
	for _, dimension := range []TunnelHealthDimension{v.Dimensions.Service, v.Dimensions.Edge, v.Dimensions.Config, v.Dimensions.Route, v.Dimensions.Origin, v.Dimensions.DNS, v.Dimensions.Certificate, v.Dimensions.Access, v.Dimensions.Update} {
		if dimension.Status == "" || len(dimension.Status) > 32 || len(dimension.Code) > 128 || containsTunnelControl(dimension.Status+dimension.Code) {
			return ErrUnsafeTunnelResponse
		}
	}
	v.Summary = redactTunnelText(v.Summary)
	v.RepairAction = redactTunnelText(v.RepairAction)
	return nil
}

func validateLogEntry(v *TunnelLogEntry, tunnelID string) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "log_entry" || !validTunnelToken(v.ID) || v.TunnelID != tunnelID || v.Code == "" || len(v.Code) > 128 || len(v.Message) > 4096 || len(v.Metadata) > 64 || len(v.Cursor) > 4096 || containsTunnelControl(v.Code+v.Level+v.Component+v.Message) {
		return ErrUnsafeTunnelResponse
	}
	metadata, err := redactTunnelMetadata(v.Metadata, 0)
	if err != nil {
		return err
	}
	v.Metadata = metadata
	v.Message = redactTunnelText(v.Message)
	return nil
}

func validateTunnelCapabilities(capabilities []string) error {
	if len(capabilities) < 1 || len(capabilities) > 16 {
		return errors.New("connector capabilities must contain 1-16 values")
	}
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" || len(capability) > 64 || containsTunnelControl(capability) {
			return errors.New("connector capability is invalid")
		}
		if _, ok := seen[capability]; ok {
			return errors.New("connector capabilities must be unique")
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func validateTunnelEnrollmentInput(in TunnelConnectorEnrollmentInput) error {
	if !validTunnelID(in.HostID) {
		return errors.New("invalid connector enrollment host identifier")
	}
	if in.TTLSeconds < 0 || in.TTLSeconds > 900 {
		return errors.New("connector enrollment TTL must be between 0 and 900 seconds")
	}
	return validateTunnelCapabilities(in.Capabilities)
}

func validTunnelBase64URL(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func validateTunnelEnrollmentExchangeInput(in TunnelConnectorEnrollmentExchangeInput) error {
	if in.Token == "" || len(in.Token) > 256 || strings.TrimSpace(in.Token) != in.Token || containsTunnelControl(in.Token) {
		return errors.New("connector enrollment token is invalid")
	}
	if !validTunnelID(in.HostID) {
		return errors.New("invalid connector enrollment host identifier")
	}
	if in.ProtocolVersion == "" {
		in.ProtocolVersion = "1.0"
	}
	if in.ProtocolVersion != "1.0" || len(in.ProtocolVersion) > 32 || containsTunnelControl(in.ProtocolVersion) {
		return errors.New("unsupported connector protocol version")
	}
	if !validTunnelCredentialReference(in.CredentialReference) {
		return errors.New("invalid connector credential reference")
	}
	if !validTunnelBase64URL(in.CredentialThumbprint, 43) || !validTunnelBase64URL(in.CredentialVerifierPublicKey, 43) || !validTunnelBase64URL(in.CredentialProof, 86) {
		return errors.New("invalid connector credential proof material")
	}
	if in.CredentialVerifierAlgorithm != "ed25519" {
		return errors.New("connector credential verifier must use ed25519")
	}
	for _, value := range []*string{in.SoftwareVersion, in.OperatingSystem, in.Architecture} {
		if value != nil && (len(*value) > 128 || containsTunnelControl(*value)) {
			return errors.New("connector metadata is invalid")
		}
	}
	return nil
}

func validateTunnelEnrollment(v *TunnelConnectorEnrollment, tunnel string) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "connector_enrollment" || !validTunnelID(v.ID) || v.TunnelID != tunnel || !validTunnelID(v.HostID) || v.EnrollmentToken == "" || len(v.EnrollmentToken) > 256 || strings.TrimSpace(v.EnrollmentToken) != v.EnrollmentToken || containsTunnelControl(v.EnrollmentToken) || v.ExpiresAt.IsZero() {
		return ErrUnsafeTunnelResponse
	}
	if err := validateTunnelCapabilities(v.Capabilities); err != nil {
		return ErrUnsafeTunnelResponse
	}
	if err := validateOperation(&v.Operation, "connector", v.ID); err != nil {
		return err
	}
	return nil
}

func validateTunnelActivation(v *TunnelConnectorActivation, tunnel string) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "connector_activation" || !validTunnelID(v.AccountID) || v.TunnelID != tunnel || !validTunnelID(v.ConnectorID) || !validTunnelID(v.HostID) || v.CredentialGeneration < 1 || v.ProcessGeneration < 1 {
		return ErrUnsafeTunnelResponse
	}
	return validateOperation(&v.Operation, "connector", v.ConnectorID)
}

func validateTunnelEvent(v *TunnelEvent, tunnel string) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "event" || !validTunnelID(v.ID) || v.Cursor == "" || len(v.Cursor) > 2048 || v.ResourceKind == "" || v.ResourceID == "" || len(v.ResourceID) > 128 || v.OccurredAt.IsZero() || v.CorrelationID == "" || len(v.CorrelationID) > 128 || containsTunnelControl(v.ID+v.Cursor+v.EventType+v.ResourceKind+v.ResourceID+v.CorrelationID) {
		return ErrUnsafeTunnelResponse
	}
	switch v.ResourceKind {
	case "tunnel", "route", "domain_binding", "connector", "config_generation", "operation", "preview_lease":
	default:
		return ErrUnsafeTunnelResponse
	}
	if v.ResourceKind == "tunnel" && v.ResourceID != tunnel {
		return ErrUnsafeTunnelResponse
	}
	switch v.Actor.Type {
	case "user", "host", "system", "edge":
	default:
		return ErrUnsafeTunnelResponse
	}
	if !validTunnelID(v.Actor.ID) {
		return ErrUnsafeTunnelResponse
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_.]*$`).MatchString(v.EventType) {
		return ErrUnsafeTunnelResponse
	}
	metadata, err := redactTunnelMetadata(v.SafeMetadata, 0)
	if err != nil {
		return err
	}
	v.SafeMetadata = metadata
	return nil
}

func validateTunnelPrivateAccessAdmission(v TunnelPrivateAccessAdmission) error {
	if v.Schema != TunnelV1Schema || v.Kind != "private_access_carrier_admission" {
		return ErrUnsafeTunnelResponse
	}
	for _, value := range []string{v.AccountID, v.MachineID, v.ResourceID, v.CarrierSessionID, v.RouteID, v.AssignmentID, v.EdgeNodeID, v.TunnelID, v.CarrierConnectorID} {
		if !validTunnelID(value) {
			return ErrUnsafeTunnelResponse
		}
	}
	if v.InstallationGeneration == 0 || v.RouteGeneration == 0 || v.SessionGeneration == 0 || v.ProcessGeneration == 0 || v.ConfigGeneration == 0 || v.AssignmentGeneration == 0 || v.ExpiresAt.IsZero() || !validTunnelBase64URL(v.AccessorPublicKey, 43) || !validTunnelBase64URL(v.AccessorThumbprint, 43) || len(v.EdgeEndpoints) != 2 || len(v.EdgeCarrierServerCertificateChainPEM) == 0 || len(v.EdgeCarrierServerCertificateChainPEM) > 65536 || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(v.ConfigContentHash) || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(v.EdgeCarrierServerSPKISHA256) {
		return ErrUnsafeTunnelResponse
	}
	switch v.ResourceKind {
	case "preview":
		if v.ResourceID == "" || v.TunnelName != "" || v.RouteName != "" || v.OperationID != "" || v.ConnectorID != "" || v.Protocol != "http" {
			return ErrUnsafeTunnelResponse
		}
	case "tunnel":
		if !tunnelNamePatternV1.MatchString(v.TunnelName) || !tunnelRouteNamePatternV1.MatchString(v.RouteName) || !validTunnelID(v.ConnectorID) || v.OperationID != "" {
			return ErrUnsafeTunnelResponse
		}
	default:
		return ErrUnsafeTunnelResponse
	}
	if v.Protocol == "private_tcp" {
		if v.Hostname != "" || v.MatchType != "catch_all" || v.WildcardSuffix != "" {
			return ErrUnsafeTunnelResponse
		}
	} else if v.Protocol != "http" || v.Hostname == "" {
		return ErrUnsafeTunnelResponse
	}
	if len(v.EdgeProcessEpoch) < 8 || len(v.EdgeProcessEpoch) > 128 || containsTunnelControl(v.EdgeProcessEpoch) || !v.ExpiresAt.After(time.Now().UTC()) {
		return ErrUnsafeTunnelResponse
	}
	for _, endpoint := range v.EdgeEndpoints {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "tls" && u.Scheme != "quic") || u.Hostname() == "" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return ErrUnsafeTunnelResponse
		}
	}
	return nil
}

func validateTunnelPrivateAccessSnapshot(v *TunnelPrivateAccessSnapshot) error {
	if v == nil || v.Schema != TunnelV1Schema || v.Kind != "private_access_carrier_snapshot" || !v.Complete || len(v.Admissions) > 4096 {
		return ErrUnsafeTunnelResponse
	}
	for _, admission := range v.Admissions {
		if err := validateTunnelPrivateAccessAdmission(admission); err != nil {
			return err
		}
	}
	return nil
}
