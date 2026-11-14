# X-19 closing pass, lane R-X-19-N, 2026-11-13 22:00 to 2026-11-14 00:30 UTC

Own machines only (no fleet change, no apple): `rx19v-l` (Linux container on pbvm2, amd64), `rx19n-m` (macpool-2, macOS arm64
hosted runner), `rx19n-w` (winpool-6, Windows amd64 runner), all paired into the fleet account as throwaway records.
Build for the matrix: **dev-900bff252** (main 900bff252, `pb version` of each: rx19v-l, rx19n-m, rx19n-w = `dev-900bff252`).
Sub-agent parts ran on dev-ac409910b (main at 03:08 IST); 900bff252 only adds the macOS guard fix below.

## 1. Pairs, all six directions (plain network): PASS
`results/xplat-access-x19n-plain-20261113T235203Z.json` (name resolves to 127.100.0.1 through the public wildcard, 1 MiB byte-exact with
half-close through `pb access machine` and through the 127.100.x.y private address, non-loopback `--listen` refused, `https://<port>.<machine>.<domain>`
and `https://<machine>.<domain>:720` through each OS's own trust store, port 720 below 1024):

| Pair | Result |
|---|---|
| L>M rx19v-l>rx19n-m | PASS 39 s |
| M>L | PASS 59 s |
| L>W | PASS 39 s |
| W>L | PASS 47 s |
| M>W | PASS 44 s |
| W>M | PASS 40 s |

## 2. Full-tunnel VPN with an exit node: PASS
Client `rx19v-l` goes through a WireGuard full tunnel (container `wg0` 10.99.0.2, `ip route replace default dev wg0`, DNS 1.1.1.1 through the
tunnel; exit node = the pbvm2 host `rx19vx` 10.99.0.1 with ip_forward and MASQUERADE; `wg show`: handshake + traffic, `ip route get 1.1.1.1` = `dev wg0`).
Names kept resolving to 127.100.0.1 (`getent hosts dns-check.ppr.boats`), loopback traffic never entered the tunnel.
- `results/xplat-access-x19n-vpn-20261113T235545Z.json`: M>L, L>W, W>L PASS; L>M failed once: raw `ConnectionRefused` on the forward, first pair after the tunnel came up, no cause found.
- `results/xplat-access-x19n-vpn-VM-20261114T000029Z.json`: the same V>M pair three times in a row on a fresh tunnel (first pair right after bringing it up included): 3 of 3 PASS.
- Earlier runs on dev-ac409910b with the fleet (`results/xplat-access-vpn-20261113T215302Z.json`, rerun `...T215415Z.json`): L>lanex2-l2, lanex2-l2>L, W>L PASS; L>W reset once, rerun PASS (failures.md).
Left as one logged unexplained transient (V>M first pair after tunnel start, 1 of 5 attempts on this build).

## 3. Failures found on the way, causes, fixes
1. **macOS: port stays bound after the last gateway route is released (real product bug, fixed 900bff252).** After any peer's port-720 policy was
   created and revoked, the guard on a Mac kept root-owned LISTEN `127.100.0.1:720` for 7+ minutes (`netstat`, `lsof` pid of the guard, `pb resolve` of
   every peer showed no 720). A user service on `0.0.0.0:720` then failed `EADDRINUSE` and `https://<machine>.<domain>:720` to that Mac failed
   (L>M, W>M, VPN client>M on the fleet Mac: results/xplat-access-20261113T221531Z.json). Cause: `server_darwin.go` `shutdownListener` lacked the
   `*gatewayRoute` branch of `server_linux.go`, so the shared socket was never closed. Test `TestReleasingTheLastGatewayRouteClosesTheSharedListener`
   (cli CI run 43524840214: PASS under -race and under sudo). After the fix: Mac listeners after every pair = 0 (matrix output) and V>M passes.
2. **Windows client, fresh runner: browser URLs off because HTTP.sys holds port 80/443.** W>L and W>M failed only at the browser step (`curl.exe: Failed to
   connect to 25225.rx19v-l.ppr.boats:443 after 2051 ms`; the private-access part passed: `forward: True`, `private_address: True`,
   results/xplat-access-x19n-plain-20261113T233435Z.json). pb names it in `pb doctor`:
   `daemon: warning: The local daemon is ready. Browser URLs are off because another program is using port 80/443 (Windows HTTP service (HTTP.sys, for example IIS), PID 4). Recovery: Stop that program, and Paperboat turns browser URLs back on by itself.`
   (`BrowserURLs()` publishes only live routes, `pb status` raises `browser_urls_unavailable`, cli/internal/localdaemon/inventory.go:330.) After `net stop http /y`
   the pairs pass (results/xplat-access-x19n2-plain-20261113T234308Z.json, W>L and W>M PASS; VPN W>L PASS in ...vpn-20261113T234601Z.json).
3. **Harness orphans (fixed, xplat_access.py).** A Windows driver ends `pb exec` with TerminateProcess; the port-720 test origin kept running on the Mac for
   minutes (the host reaped it later) and blocked the next run. The origin now answers `/__quit` and ends itself after 120 s; the driver calls it in `finally`.
4. Fleet `lanex2-l2` (anvit container) showed server-side offline from 22:01Z with a healthy local daemon (failures.md, FLEET3), so L-target pairs used rx19v-l.

## 4. Other open parts, by the sub-agents (own machines, dev-ac409910b)
- Doctor on macOS and Windows with a real rebind-filtering resolver, and real browsers with DNS-over-HTTPS (Chrome Secure DNS + Safari on the Mac, Edge Secure DNS on Windows, both URL forms, OS trust store, net-log shows the DoH request): `xplat-manual-X-19-B-20261113T2200Z.md`, commits 4564ba1ef, f6d08a615.
- Revoking a team grant and turning the team setting off end held private TCP flows on all six pairs (2.7 to 11 s): `xplat-manual-X-19-team-20261113T220302Z.md`, harness commits up to de44c7516, a2616ad57, 8b4a8e21a.
- macOS loopback aliases are removed on uninstall: `ifconfig lo0 | grep -c "inet 127.100"` = 7 before `pb uninstall` on rx19n-m, 0 after. Windows and Linux showed 0 before and after (no alias needed).

## Cleanup
Records revoked by exact id: rx19v-l machine_a92fdb32-bfa8-46e3-b3f8-a55b35f83d3b, rx19n-m machine_847b2f88-175d-4d19-b428-cf78952cea16,
rx19n-w machine_0335c15b-c906-4b30-9d7b-1bbc38ab21e8; pb uninstalled on all three; WireGuard interfaces and iptables rules removed; container rx19v-l and
pbvm2 temp files removed; macpool-2 released; winpool-6 stop requested (runner did not answer, ends by itself).
