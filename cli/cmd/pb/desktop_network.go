package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/deviceguard"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"github.com/spf13/cobra"
)

func desktopEffectiveNetwork(c *cobra.Command, cfg *config.Config, client *api.Client, apply bool) (any, error) {
	remote, err := client.EffectiveNetworkPreferences(c.Context())
	if err != nil {
		return nil, err
	}
	local, err := cfg.LoadNetworkPreferences()
	if err != nil {
		return nil, err
	}
	values, sources := resolveDesktopNetwork(local, remote)
	if _, err = splitdns.ValidateSuffix(values.DeviceSuffix); err != nil {
		return nil, fmt.Errorf("invalid effective device suffix: %w", err)
	}
	if _, err = config.NormalizeDeviceLoopbackCIDR(values.DeviceLoopbackCIDR); err != nil {
		return nil, fmt.Errorf("invalid effective loopback range: %w", err)
	}
	guard, guardErr := desktopGuardStatus(c.Context())
	if apply {
		if runtime.GOOS == "windows" {
			active := desktopAppliedNetwork(c.Context())
			if active["device_loopback_cidr"] == "" || active["device_loopback_cidr"] == nil {
				return nil, errors.New("the installed Windows Paperboat runtime must be updated before applying network settings; use Update this device, then retry Apply. No network settings were applied")
			}
		}
		if guardErr != nil || !guard.Ready || guard.LoopbackCIDR != values.DeviceLoopbackCIDR {
			return map[string]any{"requires_authorization": true, "device_loopback_cidr": values.DeviceLoopbackCIDR, "message": "System authorization is required to configure protected device addresses. Other active device-access users may need to disconnect before changing the range."}, nil
		}
		// Freeze migrated local overrides before updating the applied projection.
		if local.Revision == "absent" {
			local, err = cfg.SaveNetworkPreferences(local, "absent")
			if err != nil {
				return nil, err
			}
		}
		fresh, err := config.Load(cfg.Path())
		if err != nil {
			return nil, err
		}
		fresh.DeviceSuffix = values.DeviceSuffix
		fresh.DeviceLoopbackCIDR = values.DeviceLoopbackCIDR
		if err = fresh.Save(); err != nil {
			return nil, err
		}
		serviceArgs := []string{"daemon", "service", "restart", "--json"}
		if runtime.GOOS != "windows" {
			// The current-user service must use the bundled runtime whose applied
			// configuration contract this app understands, not an older CLI on PATH.
			serviceArgs = []string{"daemon", "service", "install", "--config", fresh.Path(), "--json"}
		}
		if _, err = desktopCLI(c, serviceArgs); err != nil {
			return nil, fmt.Errorf("network settings were saved, but the daemon did not become ready; retry Apply on this device: %w", err)
		}
		cfg = fresh
	}
	active := desktopAppliedNetwork(c.Context())
	pending := guardErr != nil || !guard.Ready || guard.LoopbackCIDR != values.DeviceLoopbackCIDR || active["device_suffix"] != values.DeviceSuffix || active["device_loopback_cidr"] != values.DeviceLoopbackCIDR || active["daemon_state"] != "ready"
	message := "The running daemon and protected address service match these settings."
	if pending {
		message = "Saved defaults are not fully applied on this device. Apply on this device to configure protected addresses and restart its Paperboat service."
	}
	if apply && pending {
		return nil, errors.New("settings were saved and the daemon restarted, but its applied settings could not be confirmed; refresh status and retry Apply")
	}
	return map[string]any{"local": local, "account": remote.Account, "selected_team": remote.SelectedTeam, "selected_team_unavailable": remote.SelectedTeamUnavailable, "effective": values, "sources": sources, "application": map[string]any{"pending": pending, "message": message, "daemon": active, "guard": guard}}, nil
}

func resolveDesktopNetwork(local config.NetworkPreferences, remote api.EffectiveNetworkPreferences) (api.NetworkPreferenceValues, api.NetworkPreferenceValues) {
	values, sources := remote.Effective, remote.Sources
	if local.DeviceSuffix != nil {
		values.DeviceSuffix = *local.DeviceSuffix
		sources.DeviceSuffix = "local"
	}
	if local.DeviceLoopbackCIDR != nil {
		values.DeviceLoopbackCIDR = *local.DeviceLoopbackCIDR
		sources.DeviceLoopbackCIDR = "local"
	}
	return values, sources
}

func desktopGuardStatus(ctx context.Context) (deviceguard.RuntimeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	guard, err := deviceguard.Connect(ctx, deviceguard.DefaultSocket)
	if err != nil {
		return deviceguard.RuntimeStatus{}, err
	}
	defer guard.Close()
	return guard.Status(ctx)
}

func desktopAppliedNetwork(ctx context.Context) map[string]any {
	out := map[string]any{}
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		return out
	}
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return out
	}
	data, err := json.Marshal(snapshot)
	if err == nil {
		_ = json.Unmarshal(data, &out)
	}
	// Only applied configuration/state is exposed, never runtime request grants.
	return map[string]any{"daemon_state": out["daemon_state"], "device_suffix": out["device_suffix"], "device_loopback_cidr": out["device_loopback_cidr"]}
}
