# X-38 lane R-X-38-P (2026-11-16 IST 14:05-15:30): IPv6-only on macOS vs Linux and Windows, Mac NAT rebind, Mac captive portal

Build dev-64faf9466 (= main 64faf9466, has 098b62f32, 4f1387ff7, 140592f4d) on every machine in this file; server .30 prod.
Bed (the predecessor's, left in place): NAT64 (tayga 64:ff9b::/96) + DNS64 (unbound) on pbvm2 at 2409:4090:105d:d00a:215:5dff:fe1f:1b06,
Linux peer rx38o-a (container on pbvm2, dual-stack), macOS 26.6.2 = the pbmac2 VM bridged on the home LAN (native IPv6 by SLAAC),
Windows = a winpool runner (rx38p-w, Windows Server 2025, dual-stack, started and stopped by this lane).
Every Mac command runs under fleet-mac2.lock; an ssh session to macOS runs `security unlock-keychain` first (login keychain is locked otherwise).

## 1. macOS strict IPv6-only vs Linux and Windows: PASS (5 rounds, all 4 directions, ping + exec)
Fresh pair of the Mac as rx38p-m (dev-64faf9466), dual-stack control first: M>L ping 2/2, M>W ping 2/2, M>W exec ok.
Then on the Mac (detached script, run3 files): `networksetup -setv4off Ethernet`, DNS = the DNS64 address, `route add -inet6 64:ff9b::/96 <bed>`.
State proof at every round (Mac side): en0 has no inet address, `curl -4 https://api.pprbt.dev` -> 000, `curl -6` -> 404 (the API through DNS64/NAT64).
Rounds at t+10 s, 60 s, 120 s, 200 s, 280 s after IPv4 off (the 200 s settle round and one after it are the settled rounds):
doctor healthy on the Mac every round; M>L, M>W, L>M, W>M `pb ping` 3/3 and `pb exec` ok in all five rounds (10 of 10 per direction),
path regional_relay every time (no common IPv4). No transient failure, including the first 10 s, the window where both runs of R-X-38-O had one.
Files (x38p-logs/): run3-m.log (Mac side: M>L, M>W), run3-l.log (L>M from rx38o-a, W>M from the runner).
The two transients of R-X-38-O (L>M exec "did not open the connection", M>L ping "machine is offline" inside two minutes of IPv4 off) did not repeat
on this build in this run: it is not proven fixed, 098b62f32/4f1387ff7/140592f4d touched exactly relay dialing and carrier renewal.

## 2. macOS IPv6-only with 464XLAT (the state macOS reaches by itself after a reboot on a DNS64 network): PASS (3 rounds each way)
The first IPv4-off run was cut short when another lane restarted the VM (its mistake, it admitted it); after the restart the VM came up IPv6-only
(IPv4 off persisted in the network service) and macOS had started its own CLAT: en0 inet 192.0.0.2, default via 192.0.0.1, `curl -4` -> 404 through the translator.
Doctor healthy; M>L and L>M `pb ping` 3/3 and `pb exec` ok in three rounds (x38p-logs/round1-3.m/.l). Recorded as a separate realistic case; the strict run above is the main proof.
Also run (log files were overwritten by the next run, results read from the output at the time, not kept): a transition run from dual-stack with the same script on the Mac that was already IPv6-only after the restart: M>L and L>M ping + exec ok at t+10 s, 60 s, 120 s, 200 s, 260 s, 330 s.

## 3. macOS NAT rebinding: PASS (x38p-logs/reb.log)
The Mac (IPv6 off) routed through pbvm2 as its IPv4 NAT: default gateway 192.168.31.146, `iptables MASQUERADE` for the Mac's address with UDP source ports 41000-41999.
Both directions started a 150 s `pb exec` (counter loop) first; at that moment the NAT mapping was changed (rule replaced by ports 42000-42999 and `conntrack -D` of the Mac's UDP flows,
conntrack shows the Mac's flows to the relays and to the peer with new sport 42xxx). Path M>L relay at t+10 s, direct again at t+30 s; L>M direct throughout; both long execs finished rc=0 with all 150 lines.
Rules and routes removed afterwards (the Mac default gateway is 192.168.31.1 again, IPv6 automatic).

## 4. macOS captive portal: PASS (x38p-logs/cap.log)
`/etc/hosts`: api host -> 127.0.0.1, a python HTTP server on 127.0.0.1:80 answering every GET with a 302 to a foreign host.
75 s after it started `pb doctor`: "authentication: fail: A captive portal (Wi-Fi sign-in page) is blocking the network; open a browser to sign in."
Recovery "Complete the network sign-in in a browser, then run pb doctor again."; hosts line and server removed: `pb doctor` healthy 20 s later without a restart; exec M>L and L>M ok.

## Cleaned up
rx38p-m (Mac uninstalled with pb uninstall, record gone), rx38p-w revoked and the runner stopped, Mac VM idle, pbmac2.owner free, hosts and temp files removed, iptables x38p rules removed.
Left in place on purpose: the NAT64/DNS64 bed on pbvm2 (x38o-* units, x38ow) and rx38o-a, for the Windows run below. Remove: stop and disable x38o-* units, `wg-quick`/`ip link del x38ow`, docker rm rx38o-a, revoke rx38o-a by id.

## Gap
Windows strict IPv6-only on this build: the bed is a Windows Sandbox on victus with WireGuard to pbvm2 (R-X-38-O-W report); its scripts, .wsb and keys were never saved
(the Sandbox is ephemeral) and need a logged-in desktop on victus, about 1-1.5 h to rebuild. Windows IPv6-only has one pass on dev-432169341 (L<->W both ways, ping+exec, doctor healthy);
commits after it touched relay dialing, carrier renewal and outage recovery, which are OS-independent and pass on the Mac here. L<->W IPv6-only on dev-64faf9466 stays open.
