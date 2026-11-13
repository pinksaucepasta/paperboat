package httptransport

import (
	"encoding/binary"
	"strings"
)

// snapshotFromWinINET turns the per-user WinINET settings (Internet Options: ProxyEnable, ProxyServer,
// ProxyOverride, AutoConfigURL) into a snapshot. It is untagged so it is tested on every OS. Like macOS, a PAC
// URL without an explicit proxy is PACOnly (pb never runs PAC scripts); WPAD auto-detect is not read because it is
// on by default and would make every Windows machine refuse.
func snapshotFromWinINET(enabled bool, server, override, autoConfigURL string) ProxySnapshot {
	result := ProxySnapshot{
		NoProxy:            normalizeNativeExceptions(strings.ReplaceAll(override, ";", ",")),
		ExcludeSimpleHosts: strings.Contains(override, "<local>"),
	}
	if enabled {
		result.HTTPProxy, result.HTTPSProxy = winProxyServers(server)
	}
	result.PACOnly = result.HTTPProxy == "" && result.HTTPSProxy == "" && autoConfigURL != "" && !loopbackPAC(autoConfigURL)
	return withGeneration(result)
}

// winProxyServers reads "host:port" (every scheme) or "http=host:port;https=host:port;socks=..." (per scheme; a
// scheme that is not listed goes direct, as in Windows).
func winProxyServers(server string) (httpProxy, httpsProxy string) {
	if !strings.Contains(server, "=") {
		proxy := winProxyURL(server)
		return proxy, proxy
	}
	for _, entry := range strings.Split(server, ";") {
		scheme, value, _ := strings.Cut(strings.TrimSpace(entry), "=")
		switch strings.ToLower(scheme) {
		case "http":
			httpProxy = winProxyURL(value)
		case "https":
			httpsProxy = winProxyURL(value)
		}
	}
	return httpProxy, httpsProxy
}

func winProxyURL(value string) string {
	value = strings.TrimSpace(value)
	for _, scheme := range []string{"http://", "https://"} {
		value = strings.TrimPrefix(value, scheme)
	}
	host, port, ok := strings.Cut(value, ":")
	if !ok {
		return ""
	}
	return nativeProxyURL(host, port)
}

// parseWinHTTPSettings reads the machine default of `netsh winhttp set proxy` (the WinHttpSettings blob): version,
// counter, flags (0x02 = proxy server), then the proxy and bypass strings, each with a 32-bit length.
func parseWinHTTPSettings(blob []byte) ProxySnapshot {
	if len(blob) < 16 || blob[8]&0x02 == 0 {
		return ProxySnapshot{}
	}
	proxy, rest, ok := winHTTPString(blob[12:])
	if !ok {
		return ProxySnapshot{}
	}
	bypass, _, _ := winHTTPString(rest)
	return snapshotFromWinINET(true, proxy, bypass, "")
}

func winHTTPString(blob []byte) (value string, rest []byte, ok bool) {
	if len(blob) < 4 {
		return "", nil, false
	}
	size := int(binary.LittleEndian.Uint32(blob))
	if size < 0 || size > len(blob)-4 {
		return "", nil, false
	}
	return string(blob[4 : 4+size]), blob[4+size:], true
}
