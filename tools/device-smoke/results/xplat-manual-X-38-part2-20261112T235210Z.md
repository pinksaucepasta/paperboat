# X-38 part 2 by hand on my own pair (Linux container on anvit, hosted Windows runner), UTC 20261112 20:50 - 23:10 (IST 02:20 - 04:40 13 Nov)

## NAT rebinding: PASS
conntrack entries of the container deleted on the host and its source UDP port forced into another range (iptables MASQUERADE --to-ports, removed afterwards): a 45 s `pb exec sleep 45; echo survived` from W to L running across it ended "survived"; `pb ping` went direct -> regional_relay for the first 36 s and was direct again within about 1-2 minutes.
## Full-tunnel VPN on the Linux side: PASS
A WireGuard tunnel to a second container (NAT gateway) and the Linux container's default route through wg0 (the endpoint kept on eth0): `pb ping` L>W stayed direct, W>L regional_relay for the 77 s it was held, `pb exec` worked both ways, `pb doctor` healthy, the API answered through wg0; after removing the route W>L was direct at once.
## Not producible here
IPv6-only and dual-stack (the control plane and relays are IPv4 only: an IPv6-only client has no way to reach them; needs NAT64 and DNS64), a captive portal, a Wi-Fi switch. The earlier by-hand parts (UDP blocked, host firewall defaults, proxy) are in the first transcript.
