package splitdns

import (
	"errors"
	"golang.org/x/net/publicsuffix"
	"net"
	"strconv"
	"strings"
)

// NormalizeBrowserDomain validates the administrator-approved scope of a local
// CA. A registrable domain (or its subdomain) is required: a CA for a public
// suffix would trust unrelated websites. DNS provisioning is a separate action.
func NormalizeBrowserDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		domain = BrowserSuffix
	}
	if len(domain) > 253 || strings.ContainsAny(domain, "*:/@\\\x00") || net.ParseIP(domain) != nil {
		return "", errors.New("browser domain must be a DNS domain, without wildcard, scheme, port, or IP")
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", errors.New("browser domain must include a registrable domain")
	}
	for _, label := range labels {
		if !validLabel(label) {
			return "", errors.New("browser domain has an invalid DNS label")
		}
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(domain); err != nil {
		return "", errors.New("browser domain cannot be a public suffix")
	}
	return domain, nil
}

// ParseBrowserHostname accepts exactly one machine label and at most one
// service label beneath domain. An empty service denotes the machine index.
func ParseBrowserHostname(host, domain string) (machine, label string, err error) {
	clean, err := NormalizeBrowserDomain(domain)
	if err != nil {
		return "", "", err
	}
	if host != strings.ToLower(host) || len(host) > 253 || !strings.HasSuffix(host, "."+clean) {
		return "", "", errors.New("browser hostname is outside the configured domain")
	}
	labels := strings.Split(strings.TrimSuffix(host, "."+clean), ".")
	if len(labels) < 1 || len(labels) > 2 {
		return "", "", errors.New("browser hostname requires a machine and optional service")
	}
	for _, part := range labels {
		if !validLabel(part) {
			return "", "", errors.New("invalid browser hostname label")
		}
	}
	machine = labels[len(labels)-1]
	if len(labels) == 2 {
		label = labels[0]
		numeric := true
		for _, c := range label {
			if c < '0' || c > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			port, e := strconv.Atoi(label)
			if e != nil || port < 1 || port > 65535 || label != strconv.Itoa(port) {
				return "", "", errors.New("invalid browser port label")
			}
		}
	}
	return machine, label, nil
}
