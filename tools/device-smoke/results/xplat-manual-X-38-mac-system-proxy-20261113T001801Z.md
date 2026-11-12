# X-38 M: macOS system proxy honoured by a CGO_ENABLED=0 build (FIX-MPX)

Build: dev-0b52398c7 (CGO_ENABLED=0 darwin/arm64, cut from the fix commit). Machine: pbmac VM (macOS 26.6.2 arm64), run
standalone from /tmp as the VM's existing user (the VM's installed pb, a fleet machine, was not touched or replaced).

| Part | Command | Result |
|---|---|---|
| Root cause | macpool runner after `pb install` + `pb pair` | `pb pair` failed with a ProxyError: `scutil --proxy` showed the runner's system PAC `http://127.0.0.1:17391/proxy.pac` (Paperboat's own machine-guard PAC, cli/internal/machineguard/preview_pac.go); the first parser treated it as an unsupported PAC and refused all requests. Fixed: a loopback PAC is ignored (`TestParseScutilProxyLoopbackPACIsIgnored`) |
| Baseline | `scutil --proxy` | only the guard's loopback PAC, no HTTP(S) proxy |
| Set proxy | `networksetup -setwebproxy/-setsecurewebproxy Ethernet 127.0.0.1 8899` (local CONNECT proxy, python, logs each request) | `scutil --proxy` HTTPEnable 1, HTTPSEnable 1 |
| Control | installed old build (dev-94eb504aa, nocgo): `pb doctor` | healthy, proxy log empty (proxy ignored: the bug) |
| Fixed build | `/tmp/mpx-pb doctor` | `authentication: pass`, proxy log: `CONNECT api.pprbt.dev:443` |
| Restore | proxies off, proxy process and files removed | `scutil --proxy` back to baseline |
| Pair check (Linux, same build) | two throwaway containers on pbvm2, `pb exec` a->b and b->a, `pb ping` | `mpx-ab`, `mpx-ba`, ping 4/4 path=direct, loss 0% |

Not covered: the daemon (hostd/`pb exec` into a Linux container) reaching the API through the proxy: that needs this build installed
as a paired Mac machine, and the pbmac VM carries a fleet machine and the macpool queue was not available.
