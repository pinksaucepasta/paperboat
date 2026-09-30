package splitdns

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// BrowserSuffix is the public DNS namespace for locally terminated browser
// access. Its DNS-only wildcard points to BrowserGatewayIP, never a public edge.
const BrowserSuffix = "local.pprbt.dev"
const BrowserGatewayIP = "127.100.0.1"
const BrowserGatewayHostname = "gateway." + BrowserSuffix

func validateBrowserSuffix(suffix string) (string, error) {
	if suffix == BrowserSuffix {
		return suffix, nil
	}
	return ValidateSuffix(suffix)
}

// ValidateTrustSuffix permits native private suffixes and only the selected
// public browser namespace. It never admits an entire public TLD as CA scope.
func ValidateTrustSuffix(suffix string) (string, error) {
	return validateBrowserSuffix(suffix)
}

// IsPublicBrowserHostname admits only a canonical port and device label in the
// selected browser namespace. Reachability never supplies service authorization.
func IsPublicBrowserHostname(host string) bool {
	if host != strings.ToLower(host) || !strings.HasSuffix(host, "."+BrowserSuffix) {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(host, "."+BrowserSuffix), ".")
	return len(parts) == 2 && validBrowserHost(host, BrowserSuffix)
}

// BrowserRoute binds one browser hostname to an explicitly authorized service.
type BrowserRoute struct {
	Address   netip.Addr
	Port      int
	MachineID string
}

// BrowserHostname identifies an explicit browser service as <port>.<alias>.<suffix>.
// Services on one device share a registrable browser site.
func BrowserHostname(aliasOrMachine string, port int, suffix string) (string, error) {
	if _, err := validateBrowserSuffix(suffix); err != nil {
		return "", err
	}
	alias := strings.ToLower(strings.TrimSpace(aliasOrMachine))
	if !validLabel(alias) || port < 1 || port > 65535 {
		return "", errors.New("invalid browser service identity")
	}

	return strconv.Itoa(port) + "." + alias + "." + suffix, nil
}

func validBrowserHost(host, suffix string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !strings.HasSuffix(host, "."+suffix) {
		return false
	}
	prefix := strings.TrimSuffix(host, "."+suffix)
	if prefix == "" {
		return false
	}
	parts := strings.Split(prefix, ".")
	if len(parts) == 1 {
		return validLabel(parts[0])
	} else if len(parts) == 2 {
		port, err := strconv.Atoi(parts[0])
		return err == nil && port >= 1 && port <= 65535 && strconv.Itoa(port) == parts[0] && validLabel(parts[1])
	}
	return false
}

func validLabel(label string) bool {
	if len(label) < 1 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
