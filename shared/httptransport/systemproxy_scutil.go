package httptransport

import (
	"bufio"
	"hash/fnv"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

const scutilWarnInterval = time.Minute

var scutilWarn = struct {
	sync.Mutex
	at  time.Time
	log func(string, ...any)
}{log: diagnosticlog.TryWarn}

// snapshotFromScutil turns one `scutil --proxy` run into a snapshot. A failed run is "no proxy" (direct), logged
// at most once a minute, like Tailscale's system-proxy lookup (ref/tailscale/net/tshttpproxy/tshttpproxy.go):
// a scutil hiccup must not fail every API call. A successful run with an unsupported config (PACOnly) still
// refuses, through the snapshot.
func snapshotFromScutil(output []byte, err error) ProxySnapshot {
	if err == nil {
		return parseScutilProxy(string(output))
	}
	scutilWarn.Lock()
	defer scutilWarn.Unlock()
	if scutilWarn.at.IsZero() || time.Since(scutilWarn.at) >= scutilWarnInterval {
		scutilWarn.at = time.Now()
		scutilWarn.log("macOS system proxy unreadable, connecting directly", "error", err.Error())
	}
	return ProxySnapshot{}
}

// parseScutilProxy reads `scutil --proxy` output (a CF dictionary dump), the
// cgo-free way to read macOS network proxy settings; Tailscale also shells out
// to scutil (ref/tailscale/net/dns/manager_darwin.go). It is untagged so it is
// tested on every OS.
func parseScutilProxy(output string) ProxySnapshot {
	values := map[string]string{}
	var exceptions []string
	// Only top-level keys and the top-level ExceptionsList count: scutil also dumps a nested __SCOPED__ block
	// with per-interface settings that must not overwrite them.
	var blocks []string
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "}" {
			if len(blocks) > 0 {
				blocks = blocks[:len(blocks)-1]
			}
			continue
		}
		key, value, ok := strings.Cut(line, " : ")
		if !ok {
			if strings.HasSuffix(line, "{") { // the root "<dictionary> {"
				blocks = append(blocks, "")
			}
			continue
		}
		switch {
		case strings.HasSuffix(value, "{"):
			blocks = append(blocks, key)
		case len(blocks) == 1:
			values[key] = strings.TrimSpace(value)
		case len(blocks) == 2 && blocks[1] == "ExceptionsList":
			exceptions = append(exceptions, value)
		}
	}
	result := ProxySnapshot{
		NoProxy:            normalizeNativeExceptions(strings.Join(exceptions, ",")),
		ExcludeSimpleHosts: values["ExcludeSimpleHostnames"] == "1",
		PACOnly:            (values["ProxyAutoConfigEnable"] == "1" && !loopbackPAC(values["ProxyAutoConfigURLString"])) || values["ProxyAutoDiscoveryEnable"] == "1",
	}
	if values["HTTPEnable"] == "1" {
		result.HTTPProxy = nativeProxyURL(values["HTTPProxy"], values["HTTPPort"])
	}
	if values["HTTPSEnable"] == "1" {
		result.HTTPSProxy = nativeProxyURL(values["HTTPSProxy"], values["HTTPSPort"])
	}
	if result.HTTPProxy != "" || result.HTTPSProxy != "" {
		result.PACOnly = false
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(result.HTTPProxy + "\x00" + result.HTTPSProxy + "\x00" + result.NoProxy + "\x00" + strconv.FormatBool(result.ExcludeSimpleHosts) + "\x00" + strconv.FormatBool(result.PACOnly)))
	result.Generation = hash.Sum64()
	return result
}

// loopbackPAC reports a PAC served from this computer: Paperboat's own machine guard installs one for private
// previews (cli/internal/machineguard/preview_pac.go) and it must never make pb refuse its own traffic.
func loopbackPAC(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
}

func normalizeNativeExceptions(value string) string {
	items := strings.Split(value, ",")
	result := items[:0]
	for _, item := range items {
		item = strings.TrimSpace(item)
		if strings.HasPrefix(item, "*.") {
			item = "." + strings.TrimPrefix(item, "*.")
		}
		if item != "" && item != "<local>" {
			result = append(result, item)
		}
	}
	return strings.Join(result, ",")
}

func nativeProxyURL(host, portText string) string {
	port, err := strconv.Atoi(portText)
	if host == "" || err != nil || port < 1 || port > 65535 {
		return ""
	}
	return "http://" + net.JoinHostPort(host, portText)
}
