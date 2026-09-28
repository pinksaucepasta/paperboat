package config

import (
	"github.com/pinksaucepasta/paperboat/internal/deviceloopback"
)

const DefaultDeviceLoopbackCIDR = deviceloopback.DefaultCIDR

func NormalizeDeviceLoopbackCIDR(value string) (string, error) {
	return deviceloopback.NormalizeCIDR(value)
}
