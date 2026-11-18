package tunnelv1

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
)

var (
	// ErrSubdomainInvalid: the label breaks the DNS-label rules; the message names the rule.
	ErrSubdomainInvalid = errors.New("subdomain is invalid")
	// ErrSubdomainReserved: the label is one of the configured reserved names.
	ErrSubdomainReserved = errors.New("subdomain is reserved")
	// ErrSubdomainTaken: another resource holds (or is blocked on) that address.
	ErrSubdomainTaken = errors.New("subdomain is already taken")
	// ErrAddressBlocked: an operator blocked this tunnel's address; renaming would evade the block.
	ErrAddressBlocked = errors.New("tunnel address is blocked")
	// ErrAddressChangePending: the tunnel is already moving to another address; it finishes or fails first.
	ErrAddressChangePending = errors.New("an address change is already in progress")
)

// subdomainPattern is the strictest union of the reference products' public-name rules (zrok util/names.go,
// tailscale dnsname.go, cloudflared quicktunnelauth/state.go): lowercase ASCII, 3-63, no edge hyphens.
var subdomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])$`)

// validateSubdomain rejects (never rewrites) a label that cannot be a tunnel address. reserved holds lowercase labels.
func validateSubdomain(label string, reserved map[string]struct{}) error {
	switch {
	case !subdomainPattern.MatchString(label):
		return fmt.Errorf("%w: use 3 to 63 lowercase letters, digits or hyphens, not starting or ending with a hyphen", ErrSubdomainInvalid)
	case strings.Trim(label, "0123456789") == "":
		return fmt.Errorf("%w: a name of only digits is not allowed", ErrSubdomainInvalid)
	case strings.HasPrefix(label, "xn--"):
		return fmt.Errorf("%w: names starting with xn-- are not allowed", ErrSubdomainInvalid)
	}
	if _, ok := reserved[label]; ok {
		return ErrSubdomainReserved
	}
	return nil
}

func reservedSet(labels []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if !subdomainPattern.MatchString(label) {
			return nil, fmt.Errorf("reserved subdomain %q is not a valid label", label)
		}
		set[label] = struct{}{}
	}
	return set, nil
}

// SubdomainCheck answers whether a tunnel could take an address now. Reason is empty when Available;
// otherwise one of invalid, reserved, taken or current.
type SubdomainCheck struct {
	Subdomain string `json:"subdomain"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// addressHost is label under the base domain of a tunnel's current stable endpoint (https://<label>.<base>).
func addressHost(stableEndpoint, label string) (string, error) {
	host := strings.TrimPrefix(stableEndpoint, "https://")
	_, base, ok := strings.Cut(host, ".")
	if !ok || base == "" {
		return "", fmt.Errorf("%w: stable endpoint has no base domain", ErrInvalidInput)
	}
	return label + "." + base, nil
}

// CheckSubdomain says whether the tunnel could take label as its address now. A label that fails validation is
// an answer (Available false with the reason), not an error; an unknown tunnel or a viewer who may not manage it is.
func (s *Service) CheckSubdomain(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, label string) (SubdomainCheck, error) {
	if err := s.authorize(request, false, "write"); err != nil {
		return SubdomainCheck{}, err
	}
	ctx, request, shared, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return SubdomainCheck{}, scopeErr
	}
	if shared {
		return SubdomainCheck{}, previewtunnelapi.ErrForbidden
	}
	result := SubdomainCheck{Subdomain: label}
	switch err := validateSubdomain(label, s.reserved); {
	case errors.Is(err, ErrSubdomainReserved):
		result.Reason, result.Message = "reserved", "This name is reserved."
		return result, nil
	case err != nil:
		result.Reason, result.Message = "invalid", strings.TrimPrefix(err.Error(), ErrSubdomainInvalid.Error()+": ")
		return result, nil
	}
	check, err := s.repository.CheckAddress(ctx, request.Actor.AccountID, tunnelID, label)
	switch {
	case err != nil:
		return SubdomainCheck{}, err
	case check.Current:
		result.Reason, result.Message = "current", "This is already the tunnel's address."
	case !check.Free:
		result.Reason, result.Message = "taken", "This name is already taken."
	default:
		result.Available = true
	}
	return result, nil
}
