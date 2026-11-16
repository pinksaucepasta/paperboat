# Preview create latency on prod after the one-readiness rule (lane FLEET4, 2026-11-17 05:50 IST)

lanex2-l2 (Linux container on anvit, fleet build dev-b112b2a4a) against prod server .50 (5792d94bb, migration 377) and edges .30. 22 sequential `pb preview <port> --background --json --ttl 30m` (public), each to its own local origin; time = CLI call until it returns with state ready. Then both edge IPs of the preview host (152.67.0.60, 140.245.11.41; DNS returns both) polled with `curl --resolve` until each served the preview's marker (seconds after the CLI returned). Script: scratchpad prevlat.py (not committed).

| | p50 | p90 | max |
|---|---|---|---|
| create until CLI ready | 1.69 s | 2.22 s | 3.46 s |
| both edges serving after ready | 0.34 s | | 0.45 s |

22 of 22 ready, 0 of 44 edge polls unserved (every edge served on the first or second poll, i.e. at once). Target p50 <= 2 s: met. Slow ones: #6 3.37 s, #9 3.46 s (others 1.61-2.22 s). Previews stopped and deleted afterwards.

```
#0 rc=0 create 1.79s state=ready edges={'152.67.0.60': 0.36, '140.245.11.41': 0.42}
#1 rc=0 create 1.74s state=ready edges={'140.245.11.41': 0.29, '152.67.0.60': 0.39}
#2 rc=0 create 2.22s state=ready edges={'152.67.0.60': 0.29, '140.245.11.41': 0.39}
#3 rc=0 create 1.67s state=ready edges={'152.67.0.60': 0.32, '140.245.11.41': 0.32}
#4 rc=0 create 2.14s state=ready edges={'152.67.0.60': 0.31, '140.245.11.41': 0.32}
#5 rc=0 create 2.18s state=ready edges={'140.245.11.41': 0.33, '152.67.0.60': 0.34}
#6 rc=0 create 3.37s state=ready edges={'152.67.0.60': 0.27, '140.245.11.41': 0.3}
#7 rc=0 create 1.73s state=ready edges={'140.245.11.41': 0.31, '152.67.0.60': 0.44}
#8 rc=0 create 1.63s state=ready edges={'140.245.11.41': 0.3, '152.67.0.60': 0.32}
#9 rc=0 create 3.46s state=ready edges={'140.245.11.41': 0.32, '152.67.0.60': 0.33}
#10 rc=0 create 1.65s state=ready edges={'140.245.11.41': 0.36, '152.67.0.60': 0.38}
#11 rc=0 create 1.70s state=ready edges={'152.67.0.60': 0.29, '140.245.11.41': 0.31}
#12 rc=0 create 1.63s state=ready edges={'140.245.11.41': 0.28, '152.67.0.60': 0.3}
#13 rc=0 create 1.67s state=ready edges={'152.67.0.60': 0.37, '140.245.11.41': 0.41}
#14 rc=0 create 1.74s state=ready edges={'152.67.0.60': 0.3, '140.245.11.41': 0.33}
#15 rc=0 create 1.62s state=ready edges={'152.67.0.60': 0.34, '140.245.11.41': 0.37}
#16 rc=0 create 1.61s state=ready edges={'152.67.0.60': 0.36, '140.245.11.41': 0.37}
#17 rc=0 create 1.68s state=ready edges={'140.245.11.41': 0.35, '152.67.0.60': 0.35}
#18 rc=0 create 1.70s state=ready edges={'152.67.0.60': 0.33, '140.245.11.41': 0.36}
#19 rc=0 create 1.64s state=ready edges={'152.67.0.60': 0.33, '140.245.11.41': 0.34}
#20 rc=0 create 1.65s state=ready edges={'152.67.0.60': 0.34, '140.245.11.41': 0.34}
#21 rc=0 create 1.69s state=ready edges={'140.245.11.41': 0.38, '152.67.0.60': 0.45}
SUMMARY {"n": 22, "ok": 22, "create_p50": 1.69, "create_p90": 2.22, "create_max": 3.46, "edge_unserved": 0, "edge_serve_after_ready_max": 0.45, "edge_serve_after_ready_p50": 0.34, "edges_per_preview": [2]}
```
