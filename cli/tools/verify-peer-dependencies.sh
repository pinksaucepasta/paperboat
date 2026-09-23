#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

command -v rg >/dev/null 2>&1 || {
  echo "peer dependencies: rg is required" >&2
  exit 2
}

require_module() {
  path=$1
  version=$2
  actual=$(go list -m -f '{{.Version}}' "$path")
  [ "$actual" = "$version" ] || {
    echo "peer dependency mismatch: $path is $actual, want $version" >&2
    exit 1
  }
}

require_module github.com/pion/ice/v4 v4.4.0
require_module github.com/pion/stun/v3 v3.1.6
require_module github.com/pion/transport/v4 v4.0.2
require_module github.com/flynn/noise v1.1.0
require_module github.com/adrg/xdg v0.5.3
require_module github.com/coreos/go-systemd/v22 v22.6.0
require_module github.com/google/renameio/v2 v2.0.2
require_module github.com/quic-go/quic-go v0.61.0
require_module github.com/tailscale/peercred v0.0.0-20250107143737-35a0c7bd7edc
require_module github.com/tailscale/squibble v0.0.0-20260411062017-141f5d618bc4
require_module go.uber.org/goleak v1.3.0
require_module golang.org/x/crypto v0.55.0
require_module howett.net/plist v1.0.1
require_module pgregory.net/rapid v1.3.0
require_module tailscale.com v1.103.0-pre.0.20260904030409-31d8badb3bfb
modules=$(go list -m all)
if printf '%s\n' "$modules" | grep -q '^github.com/tailscale/tailcat '; then
  echo "Tailcat module must not reenter the dependency graph" >&2
  exit 1
fi
if rg -n --glob '*.go' '^[[:space:]]*([[:alnum:]_.]+[[:space:]]+)?"github.com/tailscale/tailcat(/[^"]*)?"' internal cmd; then
  echo "Tailcat imports must use the owned mesh integration" >&2
  exit 1
fi

v3_edges=$(go mod graph | awk '$2 ~ /^github.com\/pion\/transport\/v3@/ { print }')
[ "$v3_edges" = "github.com/pion/mdns/v2@v2.1.0 github.com/pion/transport/v3@v3.1.1" ] || {
  echo "unexpected pion/transport/v3 module edge:" >&2
  printf '%s\n' "$v3_edges" >&2
  exit 1
}

compiled_packages=$(go list -deps -test ./...)
if printf '%s\n' "$compiled_packages" | grep -q '^github.com/pion/transport/v3\($\|/\)'; then
  echo "pion/transport/v3 entered the compiled package graph" >&2
  exit 1
fi

if rg -n --glob '*.go' --glob '!upstream/tailscale/**' 'github\.com/pion/(turn|mdns)|github\.com/pion/transport/v3' .; then
  echo "owned source imports a forbidden Pion package" >&2
  exit 1
fi

if rg -n --glob '*.go' --glob '!upstream/tailscale/**' 'github\.com/gorilla/websocket' .; then
  echo "owned source imports forbidden Gorilla WebSocket package" >&2
  exit 1
fi

if rg -n --glob '*.go' --glob '!upstream/tailscale/**' --glob '!**/*_test.go' 'Getsockopt(Ucred|Xucred)|SO_PEERCRED|LOCAL_PEERCRED' internal cmd; then
  echo "owned source bypasses the peercred facade" >&2
  exit 1
fi

if printf '%s\n' "$compiled_packages" | grep -q '^github.com/gorilla/websocket$'; then
  echo "Gorilla WebSocket entered the compiled package graph" >&2
  exit 1
fi

default_http=$(rg -n --glob '*.go' --glob '!upstream/tailscale/**' --glob '!**/*_test.go' 'http\.(DefaultClient|DefaultTransport|Get|Post)\b' internal cmd || true)
unexpected_default_http=$(printf '%s\n' "$default_http" | grep -Ev '^internal/hostruntime/preview/proxy\.go:[0-9]+:[[:space:]]*config\.Transport = http\.DefaultTransport$' || true)
if [ -n "$unexpected_default_http" ]; then
  echo "owned external HTTP path bypasses the shared transport:" >&2
  printf '%s\n' "$unexpected_default_http" >&2
  exit 1
fi

if rg -n --glob '*.go' --glob '!upstream/tailscale/**' '\.(UnsafeKey|Cipher|SetNonce)\(' internal/peertransport; then
  echo "owned E2EE source uses a forbidden Noise cipher API" >&2
  exit 1
fi

for import in $(rg -o --no-filename --glob '*.go' --glob '!upstream/tailscale/**' --glob '!internal/peertransport/mesh/**' 'tailscale\.com/[^"[:space:]]+' . | sort -u); do
  case "$import" in
    tailscale.com/disco|tailscale.com/types/nettype|tailscale.com/wgengine/magicsock|tailscale.com/tailcfg|tailscale.com/types/key|tailscale.com/wgengine/filter|tailscale.com/tstest/integration|tailscale.com/net/netmon|tailscale.com/net/portmapper|tailscale.com/net/portmapper/portmappertype|tailscale.com/net/wsconn|tailscale.com/util/eventbus|tailscale.com/util/winutil|tailscale.com/util/winutil/conpty) ;;
    *)
      echo "owned source imports forbidden Tailscale package: $import" >&2
      exit 1
      ;;
  esac
done

unexpected_winutil=$(rg -n --glob '*.go' --glob '!upstream/tailscale/**' --glob '!internal/peertransport/mesh/**' 'tailscale\.com/util/winutil' . || true)
if [ -n "$unexpected_winutil" ]; then
  echo "owned source imports Tailscale winutil outside the Windows ConPTY parity test:" >&2
  printf '%s\n' "$unexpected_winutil" >&2
  exit 1
fi

unexpected_eventbus=$(rg -n --glob '*.go' --glob '!upstream/tailscale/**' --glob '!internal/peertransport/mesh/**' 'tailscale\.com/util/eventbus' . | grep -Ev '^\./internal/peertransport/networkmonitor/(monitor|renewal)(_test)?\.go:[0-9]+:' || true)
if [ -n "$unexpected_eventbus" ]; then
	echo "owned source imports Tailscale eventbus outside the network-monitor facade:" >&2
  printf '%s\n' "$unexpected_eventbus" >&2
	exit 1
fi

unexpected_portmappertype=$(rg -n --glob '*.go' --glob '!upstream/tailscale/**' --glob '!internal/peertransport/mesh/**' 'tailscale\.com/net/portmapper/portmappertype' . | grep -Ev '^\./internal/peertransport/networkmonitor/renewal(_test)?\.go:[0-9]+:' || true)
if [ -n "$unexpected_portmappertype" ]; then
	echo "owned source imports Tailscale port-mapping event types outside the renewal facade:" >&2
	printf '%s\n' "$unexpected_portmappertype" >&2
	exit 1
fi

unexpected_tailnet=$(rg -n --glob '*.go' --glob '!upstream/tailscale/**' --glob '!internal/peertransport/mesh/**' '"tailscale\.com/(tailcfg|types/key|wgengine/filter|tstest/integration)"' . | grep -Ev '^\./(internal/peertransport/(tailnet|native|peerquic)/[^:]+\.go|internal/hostruntime/runtime/native_peer_unix\.go):[0-9]+:' || true)
if [ -n "$unexpected_tailnet" ]; then
  echo "owned source imports Tailcat support packages outside the virtual UDP boundary:" >&2
  printf '%s\n' "$unexpected_tailnet" >&2
  exit 1
fi

# Extracted assembly imports are deliberate and must retain ownership annotations.
# Other source-policy rules apply to mesh too; it is production source now.
"$root/tools/verify-source-policy.sh"
echo "peer dependencies: valid"
