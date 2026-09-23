// Package deviceloopback defines the local projection of canonical device
// addresses assigned by the control plane.
package deviceloopback

import (
	"errors"
	"net/netip"
	"strings"
)

const (
	DefaultCIDR   = "127.100.0.0/16"
	CanonicalCIDR = "127.100.0.0/16"
)

var canonicalPrefix = netip.MustParsePrefix(CanonicalCIDR)

// NormalizeCIDR accepts one complete, normalized 127.N/16 projection. 127.0/16
// remains reserved for conventional localhost use and 127.255/16 is excluded
// so the configurable range has unambiguous inclusive bounds.
func NormalizeCIDR(value string) (string, error) {
	value = strings.TrimSpace(value)
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 16 || prefix != prefix.Masked() {
		return "", errors.New("must be a normalized IPv4 prefix in the form 127.N.0.0/16")
	}
	octets := prefix.Addr().As4()
	if octets[0] != 127 || octets[1] < 1 || octets[1] > 254 {
		return "", errors.New("must use 127.1.0.0/16 through 127.254.0.0/16")
	}
	return prefix.String(), nil
}

func Prefix(value string) (netip.Prefix, error) {
	clean, err := NormalizeCIDR(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.ParsePrefix(clean)
}

// MapCanonical preserves the low 16 host bits of a server-assigned
// 127.100.x.y address while selecting the configured local /16.
func MapCanonical(address netip.Addr, cidr string) (netip.Addr, error) {
	prefix, err := Prefix(cidr)
	if err != nil {
		return netip.Addr{}, err
	}
	address = address.Unmap()
	if !address.Is4() || !canonicalPrefix.Contains(address) {
		return netip.Addr{}, errors.New("device address is outside the canonical 127.100.0.0/16 range")
	}
	source := address.As4()
	base := prefix.Addr().As4()
	return netip.AddrFrom4([4]byte{base[0], base[1], source[2], source[3]}), nil
}

func DNSAddress(cidr string) (netip.Addr, error) {
	prefix, err := Prefix(cidr)
	if err != nil {
		return netip.Addr{}, err
	}
	base := prefix.Addr().As4()
	return netip.AddrFrom4([4]byte{base[0], base[1], 0, 1}), nil
}
