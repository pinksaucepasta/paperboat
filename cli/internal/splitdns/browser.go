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
	// HTTPS makes the gateway speak TLS to the service (port 443, or a front desk set to https):
	// it sends the browser's hostname as SNI and skips certificate verification, because the hop
	// is inside Paperboat's encrypted connection (as tunnel origins do).
	HTTPS bool
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

// ResolveBrowserRoute keeps numeric services and exact named aliases authoritative, including a top-level
// name (<name>.<domain>, no service label). A machine proxy accepts only a single nonnumeric application label.
func ResolveBrowserRoute(routes map[string]BrowserRoute, host, domain string) (BrowserRoute, bool) {
	machine, label, err := ParseBrowserHostname(host, domain)
	if err != nil {
		return BrowserRoute{}, false
	}
	if route, ok := routes[host]; ok {
		return route, true
	}
	if label == "" || NumericBrowserLabel(label) {
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
