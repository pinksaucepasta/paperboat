# FIX-RELAYSTUCK repro attempts (failures.md L356/L413/L389), 2026-11-17 01:50-03:30 IST

Machines: frs-a (pbvm2 container, home CGNAT 49.36.124.90), frs-m (macpool-1 hosted macOS 26.7.2, Azure SNAT pool 13.105.117.80/.81), frs-w (winpool-2 hosted Windows).
Builds: dev-ad6a6b17d = main a624c8e25 + magicsock debug logging (not pushed); dev-05823f125 = 8014d59eb^ + same logging. pb ping both ways, 2 samples each.
Note: the Mac's UDP egress IP (.80) differs from its TCP egress IP (.81), so a peer-UDP block must cover the pool /24.

| scenario | build | result |
|---|---|---|
| fresh pair A<->M | ad6a6b17d | M>A relay first ~90 s, then direct both ways |
| A-side UDP block 3 min (/24) | ad6a6b17d | direct both ways 5 s after unblock |
| A-side UDP block 10 min | ad6a6b17d | direct both ways 5 s after unblock |
| M-side pf UDP block 6 min | ad6a6b17d | direct both ways 5 s after unblock |
| pb service restart M, 6 min watch | ad6a6b17d | direct both ways throughout |
| pb service restart A, 6 min watch | ad6a6b17d | direct both ways throughout |
| rename churn 7 min (46 renames) | ad6a6b17d | 46/46 samples direct both ways |
| rename churn 8 min, client refetches config every ~37 s | 05823f125 (pre-8014d59eb) | 43/44 direct both ways (first sample right after install) |
| fresh pair W with A, M | ad6a6b17d | A>W, M>W relay first ~40 s, then all 4 directions direct |
| W-side firewall UDP block 3 min | ad6a6b17d | all 4 directions direct 10 s after unblock |
| W-side firewall UDP block 10 min | ad6a6b17d | all 4 directions direct 11 s after unblock; during the block W>A/W>M relay but A>W/M>W pings reported path=direct (open question: the Windows Block rules did not stop the listener's inbound UDP, or the path label is stale) |

Disco logs (Mac carrier + daemon): CallMeMaybe is re-sent every ~5-6 s both ways while not direct (magicsock sendDiscoPingsLocked, same as upstream ~/ref/tailscale wgengine/magicsock/endpoint.go:1428-1464 and heartbeat :874-945); pongs arrive from the advertised STUN address.
Spark reads: (a) disco relearning (rx-ping addCandidateEndpoint, handleCallMeMaybe) equals upstream; (b) a signed-config refetch for an unchanged peer keeps the magicsock endpoint (no bestAddr loss); (c) dialer and listener use distinct node keys per endpoint, each with its own signed endpoints, no wrong-endpoint pings.

Conclusion: L356/L413/L389 do not reproduce on current main (a624c8e25) with hosted Mac and Windows runners in 11 scenarios; no code change. The originals were on dev-cab625433/dev-72c817bfc (11-13), before the lossy-path hold-down (fork c543eba25), the netmap cache (5cf648eb9), the Mac carrier wedge fix (2830fb517) and the relay-fence fixes.
