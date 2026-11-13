//go:build windows

package httptransport

import (
	"context"

	"golang.org/x/sys/windows/registry"
)

const internetSettings = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

type NativeSystemProxySource struct{}

// Snapshot reads the per-user WinINET proxy, then the machine WinHTTP default for a service whose user has none
// (Tailscale reads the same two places through winhttp.dll, ref/tailscale/net/tshttpproxy/tshttpproxy_windows.go;
// pb does not run PAC/WPAD). A registry read that fails is "no proxy": it must not fail every API call.
func (NativeSystemProxySource) Snapshot(context.Context) (ProxySnapshot, error) {
	if key, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.QUERY_VALUE); err == nil {
		defer key.Close()
		enabled, _, _ := key.GetIntegerValue("ProxyEnable")
		server, _, _ := key.GetStringValue("ProxyServer")
		override, _, _ := key.GetStringValue("ProxyOverride")
		pac, _, _ := key.GetStringValue("AutoConfigURL")
		if snapshot := snapshotFromWinINET(enabled == 1, server, override, pac); snapshot.HTTPProxy != "" || snapshot.HTTPSProxy != "" || snapshot.PACOnly {
			return snapshot, nil
		}
	}
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, internetSettings+`\Connections`, registry.QUERY_VALUE); err == nil {
		defer key.Close()
		if blob, _, err := key.GetBinaryValue("WinHttpSettings"); err == nil {
			return parseWinHTTPSettings(blob), nil
		}
	}
	return ProxySnapshot{}, nil
}
