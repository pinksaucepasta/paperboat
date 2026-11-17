package tlscertwire

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

// ErrInvalid is the one error both modules wrap for a malformed target, hostname or identifier.
var ErrInvalid = errors.New("invalid TLS certificate binding")

// TargetKind identifies the owner of a domain/certificate binding. A certificate is always bound to exactly
// one durable route or one foreground preview lease; the discriminator keeps a preview identifier from being
// read as a durable tunnel route (or vice versa) when a binding is retried or distributed.
type TargetKind string

const (
	TargetDurableRoute TargetKind = "durable_route"
	TargetPreviewLease TargetKind = "preview_lease"
	// TargetPlatformWildcard is a Paperboat-owned wildcard bundle (preview or tunnel). It is distributed to
	// every eligible edge process, so its binding carries no route or lease identity.
	TargetPlatformWildcard TargetKind = "platform_wildcard"
	// TargetWorkspaceDomain is a workspace domain's own wildcard order, known to the server only: its
	// certificate is never distributed under this kind (each hostname under the domain is its own target), so
	// an edge refuses it.
	TargetWorkspaceDomain TargetKind = "workspace_domain"
)

// CertificateTarget is the immutable target identity shared by issuance, persistence, distribution and
// readiness checks. Exactly one of RouteID or PreviewID is populated according to Kind; preview
// generations fence lease reuse.
type CertificateTarget struct {
	Kind              TargetKind
	DomainID          string
	AccountID         string
	TunnelID          string
	RouteID           string
	PreviewID         string
	PreviewGeneration uint64
	PreviewState      string
	PreviewExpiresAt  time.Time
}

// KindValue is the canonical discriminator; an unset Kind is inferred from the preview identity.
func (t CertificateTarget) KindValue() TargetKind {
	if t.Kind != "" {
		return t.Kind
	}
	if t.PreviewID != "" || t.PreviewGeneration != 0 {
		return TargetPreviewLease
	}
	return TargetDurableRoute
}

func (t CertificateTarget) hasNoOwner() bool {
	return t.TunnelID == "" && t.RouteID == "" && t.PreviewID == "" && t.PreviewGeneration == 0 && t.PreviewState == "" && t.PreviewExpiresAt.IsZero()
}

// Validate checks the target's shape. Whether a preview lease is still active is the caller's policy
// (PreviewActive), as is refusing TargetWorkspaceDomain.
func (t CertificateTarget) Validate() error {
	if !ValidIdentifier(t.DomainID) || !ValidIdentifier(t.AccountID) {
		return fmt.Errorf("%w: target identity is invalid", ErrInvalid)
	}
	switch t.KindValue() {
	case TargetDurableRoute:
		if !ValidIdentifier(t.TunnelID) {
			return fmt.Errorf("%w: durable target tunnel is invalid", ErrInvalid)
		}
		if t.Kind != "" && !ValidIdentifier(t.RouteID) {
			return fmt.Errorf("%w: durable target route is invalid", ErrInvalid)
		}
		if t.PreviewID != "" || t.PreviewGeneration != 0 || t.PreviewState != "" || !t.PreviewExpiresAt.IsZero() {
			return fmt.Errorf("%w: durable target contains preview identity", ErrInvalid)
		}
	case TargetPreviewLease:
		if t.TunnelID != "" || t.RouteID != "" || !ValidIdentifier(t.PreviewID) || t.PreviewGeneration == 0 {
			return fmt.Errorf("%w: preview target identity is invalid", ErrInvalid)
		}
	case TargetPlatformWildcard, TargetWorkspaceDomain:
		if !t.hasNoOwner() {
			return fmt.Errorf("%w: wildcard target contains route or lease identity", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: target kind is invalid", ErrInvalid)
	}
	return nil
}

// PreviewActive reports whether a preview-lease target carries an active state and an expiry.
func (t CertificateTarget) PreviewActive() bool {
	return t.KindValue() == TargetPreviewLease && t.PreviewState == "active" && !t.PreviewExpiresAt.IsZero()
}

// Key is the target's replay and storage key.
func (t CertificateTarget) Key() string {
	return strings.Join([]string{string(t.KindValue()), t.DomainID, t.AccountID, t.TunnelID, t.RouteID, t.PreviewID, fmt.Sprint(t.PreviewGeneration)}, "\x00")
}

var dnsNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// NormalizeHostname returns the canonical DNS wire-form (ASCII IDNA) hostname and whether it is a leading-label
// wildcard. Domains are stored, signed and matched in this one form.
func NormalizeHostname(raw string) (string, bool, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/:@?#\r\n\x00") {
		return "", false, fmt.Errorf("%w: hostname is invalid", ErrInvalid)
	}
	wildcard := strings.HasPrefix(host, "*.")
	base := host
	if wildcard {
		base = host[2:]
	}
	ascii, err := idna.Lookup.ToASCII(base)
	if err != nil {
		return "", false, fmt.Errorf("%w: hostname is invalid: %w", ErrInvalid, err)
	}
	base = strings.ToLower(strings.TrimSuffix(ascii, "."))
	host = base
	if wildcard {
		host = "*." + base
	}
	if host == "" || len(host) > 253 || strings.Contains(base, "*") || net.ParseIP(base) != nil || !dnsNamePattern.MatchString(base) {
		return "", false, fmt.Errorf("%w: hostname is invalid", ErrInvalid)
	}
	return host, wildcard, nil
}

// ValidIdentifier is the shape of every account, domain, tunnel, route and preview identifier in a target.
func ValidIdentifier(value string) bool {
	if len(value) < 3 || len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// ValidEpoch is the shape of an edge process epoch.
func ValidEpoch(value string) bool {
	return len(value) >= 8 && len(value) <= 128 && strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == ""
}
