package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// Network supplies optional discovery-only endpoints. These do not authorize relays.
type Network struct {
	STUNServers []string `json:"stun_servers,omitempty"`
}

func (n Network) Validate() error {
	if len(n.STUNServers) > 8 {
		return fmt.Errorf("network.stun_servers permits at most 8 endpoints")
	}
	seen := make(map[string]bool, len(n.STUNServers))
	for i, endpoint := range n.STUNServers {
		invalid := func() error {
			return fmt.Errorf("network.stun_servers[%d] must be a unique public IP or DNS host with a canonical port (1–65535)", i)
		}
		if strings.ContainsFunc(endpoint, unicode.IsSpace) {
			return invalid()
		}
		host, port, err := net.SplitHostPort(endpoint)
		if err != nil || host == "" {
			return invalid()
		}
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
			return invalid()
		}
		canonical := strings.ToLower(host)
		if ip, err := netip.ParseAddr(host); err == nil {
			ip = ip.Unmap()
			if ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				return invalid()
			}
			canonical = ip.String()
		} else {
			if canonical != host || !strings.Contains(host, ".") || len(host) > 253 || strings.ContainsAny(host, ":/\\@?#%[]") {
				return invalid()
			}
			for _, label := range strings.Split(host, ".") {
				if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
					return invalid()
				}
				for _, c := range label {
					if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
						return invalid()
					}
				}
			}
		}
		key := net.JoinHostPort(canonical, port)
		if seen[key] {
			return invalid()
		}
		seen[key] = true
	}
	return nil
}
