package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNetworkPreferencesCASResetAndValidation(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := cfg.LoadNetworkPreferences()
	if err != nil {
		t.Fatal(err)
	}
	suffix, cidr := "mydevices", "127.42.0.0/16"
	saved, err := cfg.SaveNetworkPreferences(NetworkPreferences{DeviceSuffix: &suffix, DeviceLoopbackCIDR: &cidr}, initial.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision == initial.Revision || *saved.DeviceSuffix != suffix {
		t.Fatal("saved override was not persisted")
	}
	if _, err = cfg.SaveNetworkPreferences(NetworkPreferences{}, initial.Revision); !errors.Is(err, ErrNetworkPreferencesChanged) {
		t.Fatalf("stale write: %v", err)
	}
	invalid := "192.168.0.0/16"
	if _, err = cfg.SaveNetworkPreferences(NetworkPreferences{DeviceLoopbackCIDR: &invalid}, saved.Revision); err == nil {
		t.Fatal("nonloopback range accepted")
	}
	current, err := cfg.LoadNetworkPreferences()
	if err != nil || current.Revision != saved.Revision {
		t.Fatal("rejected write changed data", err)
	}
	reset, err := cfg.SaveNetworkPreferences(NetworkPreferences{}, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if reset.DeviceSuffix != nil || reset.DeviceLoopbackCIDR != nil {
		t.Fatal("reset must restore inheritance")
	}
}

func TestNetworkPreferencesPreserveExistingLocalOverride(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DeviceSuffix = "mynetwork"
	value, err := cfg.LoadNetworkPreferences()
	if err != nil || value.DeviceSuffix == nil || *value.DeviceSuffix != "mynetwork" {
		t.Fatal(value, err)
	}
	if _, err = cfg.SaveNetworkPreferences(NetworkPreferences{}, value.Revision); err != nil {
		t.Fatal(err)
	}
	value, err = cfg.LoadNetworkPreferences()
	if err != nil || value.DeviceSuffix != nil {
		t.Fatal("explicit reset resurrected applied config", value, err)
	}
}

func TestNetworkPreferencesRejectOversizedAndSymlinkFiles(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.NetworkPreferencesPath(), make([]byte, 4097), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = cfg.LoadNetworkPreferences(); err == nil {
		t.Fatal("oversized file accepted")
	}
	if err = os.Remove(cfg.NetworkPreferencesPath()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "other.json")
	if err = os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, cfg.NetworkPreferencesPath()); err != nil {
		t.Skip("symlink creation unavailable")
	}
	if _, err = cfg.LoadNetworkPreferences(); err == nil {
		t.Fatal("symlink preferences accepted")
	}
}
