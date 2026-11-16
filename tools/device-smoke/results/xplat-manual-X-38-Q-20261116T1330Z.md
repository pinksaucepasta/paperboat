# X-38 lane R-X-38-Q (2026-11-16 IST 15:20-18:30): Windows strict IPv6-only, full-tunnel VPN, NAT rebinding, captive portal, Wi-Fi switch, macOS full-tunnel (machine side)

Build `dev-87d7a5241` (main 87d7a5241, server .30 prod) on every machine below; `pb version` of each is in the logs (x38q-logs/guest.log for W; install JSON for the others):

| Name | OS | Where | pb |
|---|---|---|---|
| rx38o-a | Linux (container on pbvm2, dual-stack, kept from R-X-38-O/P) | pbvm2 | dev-87d7a5241 (upgraded in place) |
| rx38q-w | Windows 11 26100, Windows Sandbox guest, local admin `pbw` | victus (real PC) | dev-87d7a5241 (guest.log line `pb version`) |
| rx38q-mp | macOS 26.7.2 arm64, GitHub macpool-6 (user `runner`, GUI session) | internet | dev-87d7a5241 |
| rx38q-m-5 | macOS 26.6.2 arm64, pbmac2 VM, OS user `pbx38` (no GUI session) | victus LAN | dev-87d7a5241 |

Bed (rebuilt as scripts, `tools/device-smoke/winsandbox-v6only/`): the Windows Sandbox's only route to the world is WireGuard
(official 0.5.3 MSI) to the NAT64/DNS64 bed on pbvm2 (tayga 64:ff9b::/96, unbound DNS64 on fd38:0:0:1::1). **The bed needs TCP MSS clamping on
the WireGuard interface** (tayga adds 20 bytes, the tunnel MTU is 1380): without it HTTPS through NAT64 hangs on the first full-size segment and
`pb install` fails in the machine-guard phase with ENOENT (found and fixed in the bed, not a product bug; documented in the driver header).
Everything below runs by `winsandbox_v6only.sh start|cmd|stop` + `bed-nat.sh` (no hand-built steps).

## 1. W strict IPv6-only vs L and M: PASS (guest.log, cmd-...log)
Guest proof taken at the same moment as the checks (guest.log "proof"): no IPv4 default route (only a /32 route to the tunnel endpoint),
`ping 8.8.8.8` "General failure", ping of the Ethernet gateway lost, `curl -4 https://<api>` code=000, `curl -4 https://1.1.1.1` code=000,
`curl -6` through DNS64/NAT64 code=404 (remote 64:ff9b::…); Windows Firewall default outbound = Block with allow rules only for IPv6 ranges
and WireGuard's UDP to the bed. `pb install` and `pb pair` themselves ran in that state. Then: doctor healthy (all checks pass, `udp_ipv6` pass);
W>L `pb ping` 3/3 path=regional_relay, `pb exec` ok; L>W `pb ping` 3/3, `pb exec cmd.exe /c echo ok` ok; `pb connect rx38q-w` from L reaches the Windows
prompt (cmd, `C:\Users\pbw>`), typed command echoed, clean exit. W<->M: W>rx38q-mp ping 3/3 relay + exec (Darwin), rx38q-mp>W ping 3/3 relay + exec,
W>rx38q-m-5 ping 3/3 + exec, L>rx38q-mp ping/exec, rx38q-mp>L ping 3/3 + exec, doctor healthy on rx38q-mp (mac-gui-checks-*.txt).

## 2. W full-tunnel VPN (WireGuard, AllowedIPs 0.0.0.0/0): PASS
Route table (cmd log): default via the tunnel `x38f` metric 0 next to the Ethernet default metric 30, `curl -4` to the API goes out of the bed
(remote 157.180.74.88 through the tunnel); doctor healthy; W>L ping direct + exec ok; L>W ping direct + exec ok.

## 3. W NAT rebinding (port-changing, bed-nat.sh rebind): PASS
IPv4 NAT of the guest on the bed with the source-port range pinned to 41000-41999; while a 150 s `pb exec` ran in each direction (counter 1..150) the range
was changed (42000, 43000, 44000 in three rounds) and the guest's conntrack entries flushed (x38q-logs/nat-rebind-round2.txt and -round3.txt show the old/new sport pairs and the
UNREPLIED entry that formed). Results: W>L 150/150 lines rc=0 in every round; L>W 150/150 lines (l-to-w-long-exec-round3.txt); `pb ping` path stayed
direct (+12 s, +40 s, +70 s). The first two rounds had harness faults (a `/tmp` path under the service's private tmp, backslashes lost in quoting, PowerShell
execution policy) and are not counted.

## 4. W captive portal: recovers on its own PASS; naming gap found and FIXED (c6d670f93)
Portal = raw TCP listener on 127.0.0.1:80 answering every request with `302 Location: http://portal.example.test/login`, the API host mapped to
loopback in the hosts file (cmd log, `listener job` + `portal probe: code=302`). With the portal up `pb doctor` is unhealthy; 20 s after removal it was
"healthy" in two runs and "degraded: control plane unavailable" for one more probe in the third (backoff after repeated outages), healthy at the next probe 25 s
later; no daemon restart; exec W>L ok right after.
Naming: with the portal up doctor printed `authentication: fail: Paperboat authentication is unavailable … check local credential storage` instead of the
captive-portal message. Cause (Spark trace, verified): the access token had expired 12 minutes after pairing and its refresh hit the portal, so
`auth.Credential()` failed and the captive-portal check lived only in the later `Me()` branch. Not a regression (no change to that classification since
f3f5481be). Fix: one helper `doctorServerFailure` (proxy / outdated / captive portal / network) used by both branches; a `*url.Error` from the
refresh goes through it. Test `TestDoctorServerFailureNamesACaptivePortalAndOnlyThen` (CI run 43864469642, log line `--- PASS: TestDoctorServerFailureNamesACaptivePortalAndOnlyThen`).
Device re-check of the naming on a build with the fix: see "Open" below.

## 5. W Wi-Fi switch (the real victus PC): PASS
victus joined its saved house Wi-Fi profile (Intel AX211, DHCP 192.168.31.241) next to the Ethernet (External vEthernet 192.168.31.250); the Sandbox's traffic leaves
through victus's best default route, so moving the route changes the egress interface and source address of the guest's pb connections. By interface
metric the default moved External -> Wi-Fi (45 s) -> External while a 150 s `pb exec` ran each way; probe log (wifi-switch-victus-flip.log): route-via,
`ping 192.168.31.146` (pbvm2) and internet at every step all OK (baseline, +5 s, +50 s, back, settled). Both execs finished 150/150 (wifi-switch-run.txt,
wifi-switch-l-to-w-exec.txt); path after: regional_relay, 3/3. victus restored to the exact prior state (Wi-Fi disconnected, DHCP enabled, auto metric
30; External static .250, auto metric 25; one default via 192.168.31.1 metric 281; no .241, no Wi-Fi default route; DNS 192.168.31.1,1.1.1.1), checked by me and FLEET4.
No second Wi-Fi network exists for hp (one SSID, the hotspot idea was dropped by the coordinator), so Wi-Fi switch is proven on W only.

## 6. W dual-stack and WinHTTP system proxy on current main: PASS
IPv4 default route back via the sandbox NAT, outbound allowed, IPv6 tunnel kept: `curl -4` code=404, `curl -6` code=404 via 64:ff9b::, doctor healthy,
ping/exec to L (relay), rx38q-mp (direct), rx38q-m-5 (direct). Dead WinHTTP proxy (`netsh winhttp set proxy 127.0.0.1:9`): doctor "authentication: fail: The Paperboat
server is unreachable through the configured proxy." + the daemon health names it; after `netsh winhttp reset proxy` doctor healthy within 60 s.

## 7. macOS full-tunnel VPN (pbmac2, user pbx38, machine side): PARTIAL
WireGuard (brew wireguard-tools, `wg-quick up`) with AllowedIPs 0.0.0.0/0 on the pbmac2 VM: routes `0/1` and `128.0.0.0/1` via utun4 (macos-fulltunnel-pbmac2.txt),
egress to the API through the bed (remote 157.180.74.88), the rx38q-m-5 daemon stayed up: L>M ping 3/3 direct + `pb exec` ok, W>M ping 3/3 direct + `pb exec` ok
(Darwin) while the VPN was up; `wg-quick down` restored default via 192.168.31.1 on en0. Not covered: Mac-INITIATED checks under the VPN and a healthy doctor under the
VPN: an OS user without a GUI session cannot unlock its login keychain over SSH (`security unlock-keychain` rc 51 even with the right password through `sudo -u`),
so its CLI and local daemon are degraded ("account credentials unavailable": `pb pair` printed "installed local daemon is degraded" although the machine enrolled and
serves exec). The GitHub Mac (GUI session, full CLI) cannot reach the bed (LAN) and the only public host available as a WireGuard endpoint is the prod edge host, which I did
not touch. Finding for the product: `pb pair` on macOS in a user session without a login keychain returns an error although enrollment succeeded.

## Open (why X-38 stays [~])
- macOS full-tunnel VPN with the Mac as initiator and doctor healthy under the VPN (needs a GUI Mac on the LAN: pbmac2's admin session is another lane's, or a second public endpoint).
- Wi-Fi switch on L and M (only W proven; hp has one SSID).
- Device check of the captive-portal naming fix on a build containing c6d670f93 (build queued behind other lanes at the time of writing).
- Every part above ran on dev-87d7a5241 or earlier builds; L, M, W parts proven in earlier lanes (UDP-block forcing relay, firewall defaults per OS, system proxy on L and M, NAT rebind on L and M) were not re-run here.

## Cleanup
All removed and checked: Windows Sandbox stopped, staging folder, scheduled tasks (rx38q-*, the predecessor's rx38o-job) and firewall rules gone on victus;
victus network restored to the exact prior state (section 5). pbvm2: tayga/unbound units (x38o-*), wg interface x38ow, NAT64 interface, every iptables/ip6tables rule
(x38o-w, x38q), unbound configs, /tmp files and tokens of this and the previous X-38 lanes removed, container rx38o-a removed. pbmac2: pb uninstalled, user pbx38 and
its sudoers file deleted, brew wireguard-tools/wireguard-go removed. macpool-6: pb uninstalled, runner stopped. hp: pb uninstalled, OS user x38q and its sudoers file deleted.
Machine records revoked by exact id: rx38q-m, -m-2, -m-3, -m-4 (stale enrollments from the macOS keychain attempts), rx38q-m-5, rx38q-mp, rx38q-hp, rx38q-w;
rx38o-a's revoke was issued last from its own CLI and could not be re-listed afterwards (no other CLI in this account was left).

## Findings handed on
- Fixed and pushed: pb doctor naming a captive portal when the token refresh fails (c6d670f93, test above).
- `pb install` over an existing install, run in the Sandbox as `pbw` through an elevated scheduled task on the doctor-fix build, failed with "Windows installation failed while installing runtime services"
  (daemon diagnostics: control request deadline exceeded around the upgrade); not investigated, the first install in the same guest on the same build passed.
- macOS: `pb pair` for an OS user without a GUI session (SSH-only, login keychain not unlockable) returns an error though enrollment succeeds and the machine serves exec.
- Windows `pb pair` from a non-interactive elevated task prints "Still working on the administrator step" for about 2 minutes before returning (same as R-X-38-O).

