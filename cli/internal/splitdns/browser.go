package splitdns

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// BrowserGatewayIP is the loopback address the browser gateway listens on. The
// local access domain's DNS wildcard points here, never at a public edge.
const BrowserGatewayIP = "127.100.0.1"

// BrowserGatewayHostname keys the gateway's protected listener lease. It is not
// a DNS name: the browser namespace is the deployment's configured domain.
const BrowserGatewayHostname = "gateway.paperboat.invalid"

// BrowserRoute binds one browser hostname to an explicitly authorized service.
type BrowserRoute struct {
	Address   netip.Addr
	Port      int
	MachineID string
}

// BrowserHostname identifies an explicit browser service as <port>.<alias>.<suffix>.
// Services on one machine share a registrable browser site.
func BrowserHostname(aliasOrMachine string, port int, suffix string) (string, error) {
	var err error
	suffix, err = NormalizeBrowserDomain(suffix)
	if err != nil {
		return "", err
	}
	alias := strings.ToLower(strings.TrimSpace(aliasOrMachine))
	if !validLabel(alias) || port < 1 || port > 65535 {
		return "", errors.New("invalid browser service identity")
	}

	hostname := strconv.Itoa(port) + "." + alias + "." + suffix
	if _, _, err := ParseBrowserHostname(hostname, suffix); err != nil {
		return "", err
	}
	return hostname, nil
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

// ValidBrowserLabel reports whether label is one lowercase DNS label.
func ValidBrowserLabel(label string) bool { return validLabel(label) }

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

// BrowserWildcardPattern reserves one machine's application namespace.
func BrowserWildcardPattern(machine, domain string) (string, error) {
	base := machine + "." + domain
	parsed, label, err := ParseBrowserHostname(base, domain)
	if err != nil || label != "" || parsed == "gateway" {
		return "", errors.New("invalid browser proxy machine")
	}
	return "*." + base, nil
}

// ResolveBrowserRoute keeps numeric services and exact named aliases authoritative.
// A machine proxy accepts only a single nonnumeric application label.
func ResolveBrowserRoute(routes map[string]BrowserRoute, host, domain string) (BrowserRoute, bool) {
	machine, label, err := ParseBrowserHostname(host, domain)
	if err != nil || label == "" {
		return BrowserRoute{}, false
	}
	if route, ok := routes[host]; ok {
		return route, true
	}
	if NumericBrowserLabel(label) {
		return BrowserRoute{}, false
	}
	route, ok := routes["*."+machine+"."+domain]
	return route, ok
}

func NumericBrowserLabel(label string) bool {
	if label == "" {
		return false
	}
	for _, c := range label {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
