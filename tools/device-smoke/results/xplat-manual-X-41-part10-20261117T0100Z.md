# X-41 part 10 (R-X-41-N): the nine removal flows with the real pb ssh step, all PASS, 2026-11-17 IST 00:00-00:31

Own fresh trio, one fresh account: rx41n-l = Linux container on hp (pbtest-systemd, user tester), rx41n-m = macpool-4 (hosted macOS, user runner), rx41n-w = winpool-5 (hosted Windows, runneradmin). `pb version` of all three = dev-65dddfdfe (origin/main, contains 4b75e3ac1 certificate cache fix, 5193f87f3 event rate limit, 1b144db5d urgent refresh on scope refusal). Server with SSH certificates.

| Flavour | L target | M target | W target |
|---|---|---|---|
| revoke (other two OSes: old machine gone, ssh alias removed, new identity works with exec and pb ssh) | PASS 342 s | PASS 315 s | PASS 337 s |
| reset | PASS 249 s | PASS 257 s | PASS 252 s |
| uninstall | PASS 279 s | PASS 261 s | PASS 286 s |

File: xplat-removal-20261116T190029Z.json (and xplat-removal-20261117-nine-flows-rx41n.out). Revoked targets wrote 17, 14 and 21 events in the minute after the revoke (bound 45; was 80-98 before 5193f87f3), observers 1-6 events/min. After every flow both observers `pb exec` and `pb ssh` the machine enrolled again within 1-8 s, no trust-host needed.

uninstall:L passed: the Mac (observer) executed on the Linux machine installed and paired again within seconds. That is the case that lost M>L exec for 49 minutes on dev-64faf9466 (part 8); it did not repeat on this build (fix 1b144db5d). R-X-41-P runs its own rerun.

Earlier parts still stand for the other X-41 content: part 7 (ENV after removal 6/6, config sync entries 6/6, local names 3/3, tunnels/previews/shared terminal/long execs), part 8 (event burst data).
Incidents of this lane (told to the coordinator): ci-test.sh watchers of other lanes killed by pattern (15:40 IST), the fleet Mac used by mistake 22:25-23:15 IST (FLEET4 re-paired it), both fixed in the harness (xplat_fleet.py refuses unset XPLAT_MAC_SSH / SMOKE_WINDOWS_SSH / XPLAT_L_CT).
