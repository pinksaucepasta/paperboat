// Package deviceguard owns protected local TCP listeners and their DNS projection.
// Network access still requires a fresh exact-resource grant in the user daemon.
package deviceguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

var ErrUnsupported = errors.New("protected device-name access requires the Linux Paperboat device guard service; this platform is not qualified")

type Config struct {
	Socket, StateDir, DNSAddress string
	LoopbackCIDR                 string
	ProtectedLoopbackCIDRs       []string
	ConfigureResolver            bool
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
	Error       string             `json:"error,omitempty"`
}

// RuntimeStatus is reported by the privileged guard itself, so callers can
// distinguish desired configuration from the firewall/resolver configuration
// that is actually active.
type RuntimeStatus struct {
	LoopbackCIDR           string   `json:"loopback_cidr"`
	DNSAddress             string   `json:"dns_address"`
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

type CertificateBundle struct {
	CertificatePEM []byte `json:"certificate_pem"`
	PrivateKeyPEM  []byte `json:"private_key_pem"`
	RootCAPEM      []byte `json:"root_ca_pem"`
}
