# X-38 lane R-X-38-O (2026-11-16 IST 08:00-13:00): IPv6-only and dual-stack on macOS and Windows, captive portal on Windows

Bed (own, all removable): a NAT64 (tayga 64:ff9b::/96) + DNS64 (unbound) box on pbvm2 at its LAN IPv6 2409:4090:105d:d00a:215:5dff:fe1f:1b06
(units x38o-tayga, x38o-unbound2, config /etc/unbound/x38o.conf, tun x38onat64, iptables MASQUERADE for 192.168.254.0/24);
Linux dual-stack peer rx38o-a (container on pbvm2); macOS 26.6.2 = the pbmac2 VM on the Mac, restarted with `tart run --net-bridged=en0`
so it has its own native IPv6 (SLAAC) and DHCP IPv4 on the home LAN; Windows 11 (26100) = a Windows Sandbox on victus with a WireGuard
tunnel to pbvm2 (helper report: xplat-manual-X-38-O-W-v6only-20261116T025427Z.md) and a winpool runner (rx38o-w2) for the captive portal.
IPv6-only on the Mac: `networksetup -setv4off Ethernet`, DNS = the DNS64 address, `route add -inet6 64:ff9b::/96 <bed address>`;
a 14-minute timer inside the VM restores DHCP/DNS/route by itself.

## macOS dual-stack (dev-432169341, M = rx38o-m-2): PASS
`pb doctor` healthy; M>L `pb ping` path=direct 3/3, `pb exec rx38o-a -- echo ok-m-to-l` ok; L>M `pb ping` path=direct 3/3, exec ok (3 times).
Interface: en0 inet 192.168.31.134 and inet6 2409:4090:105d:d00a:182b:72e5:7fd5:5a12 (autoconf secured).

## macOS IPv6-only on the home LAN (the same Mac, IPv4 off): ping and exec pass in both directions, never all four in one run
State proof (every run): no inet address on en0, `netstat -rn -f inet` empty, `curl -4 https://api.pprbt.dev` -> could not resolve, `curl -6` -> http 404
(the API through DNS64/NAT64, 64:ff9b::9db4:4a58), resolv.conf = the DNS64 address. Paths are regional_relay both ways (no common IPv4).
Run dev-e90b01b80 (04:20 UTC), keychain unlocked in the same ssh session: doctor healthy, M>L ping 3/3, M>L exec ok, L>M ping 3/3, L>M exec FAIL once
("The target machine was reached but did not open the connection"), the same exec passed 3 of 3 minutes later and 3 of 3 in dual-stack.
Run dev-e90b01b80 (04:36 UTC): doctor healthy, M>L ping FAIL once ("the machine is offline", rx38o-a was online), M>L exec ok, L>M ping 3/3, L>M exec ok 3 of 3.
So each of the four directions passed, two of two runs had one transient failure inside the first two minutes after IPv4 was removed.
The longer-settle run (200 s, two rounds) could not run: the Mac host moved to a mobile hotspot (see Blockers).
Harness artifact found and fixed: a new ssh session to macOS has the login keychain locked ("security: SecKeychainCopySettings ... User interaction is not allowed"),
so `pb doctor` there says "authentication unavailable" and `pb exec` fails with "could not finish the command" although the network is fine; every ssh command now
runs `security unlock-keychain` first (the earlier two v6-only runs, 03:00 and 03:08 UTC, and the 04:06 run were this and are not counted).

## Windows (rx38o-w2, winpool-12, dev-f3f5481be = main f3f5481be, has cde871446): captive portal PASS
Fake portal: hosts line `127.0.0.1 api.pprbt.dev`, HTTP.sys stopped, a scheduled task (SYSTEM) answering every GET on :80 with a 302 to http://portal.example.test/login.
- With the portal (75 s after it started): `pb doctor`: "authentication: fail: A captive portal (Wi-Fi sign-in page) is blocking the network; open a browser to sign in."
  Recovery "Complete the network sign-in in a browser, then run pb doctor again." (before the fix it said "did not accept the authenticated health check").
- Portal removed (hosts line deleted, task deleted): `pb doctor` healthy 20 s later, no restart.
Not run on the Mac: no Mac was reachable (see Blockers).

## Windows dual-stack and IPv6-only (dev-432169341, Sandbox + WireGuard + the NAT64 bed): PASS (own file, commit 6e53cdad9/5f111b19d)
doctor healthy, ping + exec both ways in both phases; IPv6-only guest has no IPv4 default route and no IPv4 path to the internet (`curl -4` 000, `curl -6` through 64:ff9b::),
but keeps its on-link IPv4 address on the Sandbox NIC (the helper's own caveat: not an "endpoint-only" firewall).

## Blockers (2026-11-16 11:30-13:00 IST)
- The Mac (apple) and victus moved to the user's mobile hotspot after a power cut (coordinator note): pbmac2 got 10.175.186.x, sshd banners time out, pbvm2 (bed + rx38o-a) is behind victus and unreachable.
  The bed is LAN-local, so IPv6-only on M cannot be re-run until the home network is back.
- macpool has five Macs, all claimed by other lanes (no free runner for the Mac captive portal and NAT rebind).
- Builds: the IPv6-only runs are on dev-432169341/dev-e90b01b80; commits since then touched relay dialing, carrier renewal and outage recovery (098b62f32, 4f1387ff7, 140592f4d), so by the speed rule they need a rerun on a newer build (f3f5481be is built: /home/ubuntu/pb-builds/dev-f3f5481be).
