# F21-38 L: idle edge with 50 seeded previews, versions vs full pulls (REL-F2138B, 2026-11-13 19:31-19:42 IST)

Staging on anvit: server `paperboat-server:stg-6ca6b6982`, edges `pb-rel-edge-1`/`-2` (`paperboat-edge:2026.11.13.19`) and `pb-stg-edge`
(`paperboat-tunnel:stg-c88f5d563`). All three carry the delta code (9e6f85d64, 20688cf9f); later commits to those files
(1b847729f) change carrier-miss waiting, not the cursor, interval or version. Main at the time: 90b8ceddf. No `pb` machine is in this
check (edges and server only), so there is no `pb version` to record.

## Seed (staging test data, removed afterwards)
50 live preview leases + endpoints + routes (`preview_relf2138b_01..50`, public http, lease 3 h) with 2 active edge assignments each
(100 assignments, edges `stg_node_79de6713a81c` and `stg_node_ced24ff9c85e`), attached to machine `machine_fd08a028-...`, the one
machine with a valid `endpoints` carrier on both edges. Cloned from existing staging rows with INSERT ... SELECT; the lease owner
heartbeat was refreshed every 30 s (the reaper marks a lease `owner_lost` after 5 min: the first attempt at 13:56-14:01 UTC lost
all 100 assignments at 13:59:26-56 and was discarded). Result: 100 of 100 assignments active at start and at end of the clean window.

## Measured idle window, 300 s (14:01:35-14:06:35 UTC), 100 assignments active throughout
Real edges (versioned pulls, as shipped), tcpdump on docker0, TLS payload bytes both ways, per edge:

| edge | requests / s (all paths) | edge to server | server to edge |
|---|---|---|---|
| pb-rel-edge-1 | 3.23 | 241 KB/min | 174 KB/min |
| pb-rel-edge-2 | 3.05 | 242 KB/min | 180 KB/min |
| pb-stg-edge | 2.87 | 168 KB/min | 203 KB/min |

(Requests counted as bursts of client packets, not from a log: the server log does not name the edge. The 500 ms endpoint pull is 2 of them,
plus a full pull every 2 min per edge.) These are the whole edge, all control paths, with 50 previews held.

The "before" measured on the real wire, not computed: a replay of an edge's own endpoint pull (rel-edge-1's credential and process
epoch, never printed) with an empty `since` every 500 ms, which is exactly what the pre-delta edge (796a942cd) sent. It ran in the same
300 s window, against the same seeded set, from the host (one extra puller on rel-edge-1's identity):

| 300 s, same 50-preview set, 0.5 s cadence | requests | answer body each | answer bodies in 300 s |
|---|---|---|---|
| before: empty `since` every pull (replay) | 600 ok, 0 errors | 10,719 B (full set, 214 B per preview) | 6,431,342 B |
| now: cursor (replay, separate 120 s run) | 240 ok, 0 errors | 166 B unchanged (first pull 10,719 B) | 50,356 B for 240 = 126 KB per 300 s |

Wire cost of the same stream, 40 s each, 80 pulls, TLS payload both ways (includes one TLS handshake and, for the cursor run, the first full set):

| | edge to server | server to edge | per pull, steady |
|---|---|---|---|
| before (empty `since`) | 79,208 B | 928,772 B | about 1.0 KB up, 11.6 KB down |
| now (cursor) | 88,224 B | 97,553 B | about 1.1 KB up, about 1.0 KB down |

So at 2 pulls/s the endpoint pull alone went from about 1.39 MB/min down (50 previews) to about 0.12 MB/min, and the answer body from 10,719 B to 166 B.
The full answer grows with the set (214 B per preview, 1,000 previews = 214 KB per pull, about 25 MB/min); the unchanged answer stays 166 B.
The remaining idle cost is headers: about 1 KB each way per request whatever the body, so an idle edge stays at roughly 3 requests/s.

Other paths with versions (unchanged from REL-F2138, 2026-11-13 19:19 IST, same staging): ingress desired-state about every 6 s and routes
desired-state about every 15 s; both ask with their version (TestIngressAuthorityAsksForAVersionAndIdleBytesStaySmall,
TestHTTPClientRouteSnapshotAsksForAVersion).

## Cleanup
Seeded rows deleted by id prefix (0 endpoints, 0 leases, 0 routes left), temp files and scripts on anvit removed. Staging edges and server
untouched. No machines, containers or deployments created.

## Seed and replay, for repeating it
```sql
-- search_path=paperboat,public; template row: preview_2f2d8129-... (a stopped staging preview)
INSERT INTO preview_leases(...) SELECT 'preview_relf2138b_'||n, 'https://relf2138b-'||n||'.pub.stg.pprbt.dev', ... 'active', now()+interval '3 hours' ... FROM preview_leases t, generate_series(1,50) n WHERE t.id='preview_2f2d8129-...';
-- same shape for endpoints, endpoint_routes (http) and endpoint_edge_assignments (edge_node_id, edge_process_epoch, carrier_session_id,
-- generation copied from the tunnel assignments of the same machine, desired='active')
```
Replay: POST /v1/edge/runtime/endpoint-admissions with `Authorization: Bearer <edge runtime.credential>`, `X-Paperboat-Edge-Node-ID`,
`X-Paperboat-Edge-Process-Epoch`, body `{"edge_node_id","process_epoch","capabilities":["endpoints","tunnel_endpoints","endpoint_deltas"],"since":<last cursor or empty>}`.
