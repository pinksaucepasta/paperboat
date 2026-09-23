package config

import (
	"net/netip"

	"github.com/pinksaucepasta/paperboat/internal/deviceloopback"
)

const DefaultDeviceLoopbackCIDR = deviceloopback.DefaultCIDR

func NormalizeDeviceLoopbackCIDR(value string) (string, error) {
	return deviceloopback.NormalizeCIDR(value)
}

func MapCanonicalDeviceAddress(address netip.Addr, cidr string) (netip.Addr, error) {
	return deviceloopback.MapCanonical(address, cidr)
}
