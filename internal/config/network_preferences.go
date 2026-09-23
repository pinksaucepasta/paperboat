package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// NetworkPreferences stores local overrides separately from the applied daemon
// configuration. Null removes an override; it never means "use this default".
type NetworkPreferences struct {
	Revision           string  `json:"revision"`
	DeviceSuffix       *string `json:"device_suffix"`
	DeviceLoopbackCIDR *string `json:"device_loopback_cidr"`
}

var ErrNetworkPreferencesChanged = errors.New("network preferences changed; reload before saving")

func (c *Config) NetworkPreferencesPath() string { return c.Path() + ".network-preferences.json" }

func (c *Config) LoadNetworkPreferences() (NetworkPreferences, error) {
	path := c.NetworkPreferencesPath()
	var out NetworkPreferences
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		// Preserve existing deliberate local settings on first use.
		if c.DeviceSuffix != "" && c.DeviceSuffix != "pprbt" {
			value := c.DeviceSuffix
			out.DeviceSuffix = &value
		}
		if c.DeviceLoopbackCIDR != "" && c.DeviceLoopbackCIDR != "127.100.0.0/16" {
			value := c.DeviceLoopbackCIDR
			out.DeviceLoopbackCIDR = &value
		}
		out.Revision = "absent"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return out, errors.New("local network preferences must be a regular file of at most 4096 bytes")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("read local network preferences: %w", err)
	}
	if err = ValidateNetworkPreferences(out); err != nil {
		return out, err
	}
	digest := sha256.Sum256(data)
	out.Revision = hex.EncodeToString(digest[:])
	return out, nil
}

func ValidateNetworkPreferences(value NetworkPreferences) error {
	if value.DeviceSuffix != nil {
		if _, err := splitdns.ValidateSuffix(*value.DeviceSuffix); err != nil {
			return err
		}
	}
	if value.DeviceLoopbackCIDR != nil {
		if _, err := NormalizeDeviceLoopbackCIDR(*value.DeviceLoopbackCIDR); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) SaveNetworkPreferences(value NetworkPreferences, expected string) (out NetworkPreferences, resultErr error) {
	if err := ValidateNetworkPreferences(value); err != nil {
		return out, err
	}
	path, err := filepath.Abs(c.NetworkPreferencesPath())
	if err != nil {
		return out, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return out, err
	}
	lock := newSharedLock(path + ".lock")
	if err = lock.Lock(); err != nil {
		return out, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	current, err := c.LoadNetworkPreferences()
	if err != nil {
		return out, err
	}
	if current.Revision != expected {
		return out, ErrNetworkPreferencesChanged
	}
	value.Revision = ""
	data, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	if err = atomicfile.Write(path, data, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		return out, err
	}
	return c.LoadNetworkPreferences()
}
