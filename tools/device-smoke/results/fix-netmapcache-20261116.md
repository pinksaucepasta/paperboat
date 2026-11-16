# FIX-NETMAPCACHE: daemon restart while the API is unreachable (2026-11-16)

Product rule: a restart behaves like a sleep; machines keep their last peer state until revoked (Tailscale model).
Cut = iptables DROP of the API address (hp DOCKER-USER for containers, Windows outbound firewall rule).

## Before (dev-e336199c6 / dev-7cf438909, partial fixes), hp containers nmc-a, nmc-b, nmc-a restarted during the cut
- pb exec nmc-a -> nmc-b: FAIL "Paperboat is unreachable" 30.2 s (machine list empty after restart)
- then FAIL "could not reach the target machine" 40.2 s (local identity waited for /v1/peer-keys, 10 s budget)
- nmc-b -> nmc-a: PASS (30.4 s, ENV outage wait, unrelated)

## After, Linux (dev-614973f2a; tree of the pushed commit minus relay restore), API cut on both, nmc-a restarted
| check | result | time |
|---|---|---|
| pb exec nmc-a -> nmc-b | PASS x2 | 33.9 s, 30.9 s (ENV outage wait) |
| pb exec nmc-b -> nmc-a | PASS x2 | 30.4 s, 30.4 s (ENV outage wait) |
| pb connect nmc-a -> nmc-b | PASS (echo marker) | |
| pb connect nmc-b -> nmc-a | PASS (echo marker) | |

## After, Windows restarted side (dev-92456af4b), winpool-2 machine nmc-w2 + hp container nmc-a, API cut on both
Before the relay restore (dev-614973f2a on winpool-10): Linux->Windows PASS, Windows->Linux FAIL at carrier_dial (no relay).
| check | result | time |
|---|---|---|
| pb exec nmc-w2 -> nmc-a | PASS x2 | 4.3 s, 0.7 s |
| pb exec nmc-a -> nmc-w2 | PASS x2 | 0.9 s, 0.6 s |
| pb connect nmc-a -> nmc-w2 | PASS (cmd echo NMC_%COMPUTERNAME%_OK expanded) | |
| pb connect nmc-w2 -> nmc-a | PASS (echo marker, ssh -tt ConPTY) | prompt 5.2 s |

pb version: nmc-a, nmc-b dev-92456af4b (Linux check above ran on dev-614973f2a); nmc-w2 dev-92456af4b.

## Rate (server log, quiet 5 min after the checks, per machine)
dev-92456af4b 143.5 requests / machine / 5 min, statuses 200/202/204 only, no 429. Same window, other builds:
dev-bd6620efe 151.3, dev-4188ece01 121.3, dev-f0ac66fb3 103.2, dev-e7a0aa89a 254.0. No extra requests.

## Mac
Pending: macpool runner queued behind the 5-Mac GitHub cap; recorded in a follow-up commit.
