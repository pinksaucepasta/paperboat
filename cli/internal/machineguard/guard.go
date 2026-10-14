// Package machineguard owns protected local TCP listeners and their DNS projection.
// Network access still requires a fresh exact-resource grant in the user daemon.
package machineguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

var ErrUnsupported = errors.New("protected machine-name access requires the Linux Paperboat machine guard service; this platform is not qualified")

var ErrPortInUse = errors.New("protected port is already in use")

func responseError(out response) error {
	if out.ErrorCode == "port_in_use" {
		return ErrPortInUse
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	if out.ErrorCode != "" {
		return errors.New("unknown machine guard error code")
	}
	return nil
}

type Config struct {
	Socket, StateDir       string
	LoopbackCIDR           string
	ProtectedLoopbackCIDRs []string
	hostsPath              string
	legacyDNSInterfacePath string
}
type request struct {
	Operation    string            `json:"operation"`
	Hostname     string            `json:"hostname,omitempty"`
	IP           string            `json:"ip,omitempty"`
	Port         int               `json:"port,omitempty"`
	Names        map[string]string `json:"names,omitempty"`
	LoopbackCIDR string            `json:"loopback_cidr,omitempty"`
}
type response struct {
	Certificate *CertificateBundle `json:"certificate,omitempty"`
	Status      *RuntimeStatus     `json:"status,omitempty"`
	Socket      []byte             `json:"socket,omitempty"`
	ErrorCode   string             `json:"error_code,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// RuntimeStatus is reported by the privileged guard itself, so callers can
// distinguish desired configuration from the firewall/resolver configuration
// that is actually active.
type RuntimeStatus struct {
	BrowserDomain          string   `json:"browser_domain"`
	RootRenewalRequired    bool     `json:"root_renewal_required"`
	TrustReady             bool     `json:"trust_ready"`
	Version                string   `json:"version"`
	LoopbackCIDR           string   `json:"loopback_cidr"`
	Ready                  bool     `json:"ready"`
	ActiveLeases           int      `json:"active_leases"`
	ActiveOwners           int      `json:"active_owners"`
	ProtectedLoopbackCIDRs []string `json:"protected_loopback_cidrs,omitempty"`
}
type NamePublisher interface {
	ReplaceNames(context.Context, map[string]netip.Addr) error
	Close() error
}
type ListenerOwner interface {
	Acquire(context.Context, string, netip.Addr, int) (net.Listener, error)
}

// CertificateBundle contains only a leaf private key; the privileged CA key is never returned.
type CertificateBundle struct {
	CertificatePEM    []byte `json:"certificate_pem"`
	PrivateKeyPEM     []byte `json:"private_key_pem"`
	RootCAPEM         []byte `json:"root_ca_pem"`
	RevocationListDER []byte `json:"revocation_list_der"`
}
