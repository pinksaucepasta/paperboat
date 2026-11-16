# F21-18 no per-second client refetch: request counter over open native streams and private TCP sessions (2026-11-16)

Staging server (anvit, `deploy/staging.py`, image 2d5e26b19, has the private_routes topic, migrations 215-217). pb built from
main e37835bfc plus the two F21-18 commits (e1aaaac20, 7dcb6b554; pushed as cc189eb07, 140592f4d), version
`dev-7dcb6b554-f2118` on every machine (`pb version` read after install): stg-a and stg-b (linux/arm64, containers on anvit),
f2118-w2 (windows/amd64, winpool-11), f2118-m2 (darwin/arm64, macpool-1). Each client held one native stream
(`pb exec stg-b -- sleep 960`) and one private TCP session (`pb access machine stg-b --port 9090`, a keep-alive HTTP
connection open through it) for the whole window.

Counter: `tools/loadtest/request_mix.py --machines 4 --seconds 600 --budget stream_open --version dev-7dcb6b554-f2118`
(server access log of the staging stack, one build's machines only), started 3 min after the streams were up
(02:19:14-02:29:14 UTC). `peer_network` generation of the account read every 2 s: 890 at 02:19:17 and unchanged to the end.

| path (requests per machine per 10 min) | before (main e37835bfc, 2 machines) | after both fixes, strict window |
|---|---|---|
| /v1/machine-peer-network/config | 34.0 | 0 |
| /v1/peer-network/config | 56.5 | 1.0 (4 requests over 4 machines, none from a topic move) |
| /v1/jwks | 0 | 0 |
| status | FAIL (budget 1) | PASS, no budget failure |

Other paths in the window are not F21-18 (inventory lists 2, usage/native 15, stored-transfers 10, events wait 11).

Causes and fixes (this lane): a bound relay pair counted as active recovery and renewed the signed network every 4-12 s
(refresh.go nextRefresh, cc189eb07); the carrier heard only machine_access and renewed every minute (production_carrier.go,
140592f4d). An intermediate window with only the first fix: 10.7 + 6.0 per machine, bursts of all machines within 4 s of each
real peer_network move and one request a minute per carrier before the second fix. Tests: TestBoundPairWithAWorkingPathRenewsOnTheIdleCadence,
TestCarrierRenewalWakesOnAccessAndPeerNetworkChangesNotInventory (GitHub runs 43783954463, 43787110442, -race, `--- PASS` in the log).

Revoke closes the stream within one push (one revoke from stg-a per machine, preview then confirm; times from the revoke command start):

| platform | the revoked machine's own stream to stg-b ends | stg-a's stream to the revoked machine ends |
|---|---|---|
| Windows (f2118-w2) | 3.0 s | 5.1 s (earlier f2118-w record: 6.1 s) |
| macOS (f2118-m2) | 6.1 s | 7.5 s |
| Linux (stg-a, the peer of the revoked machines) | n/a | 5.1-7.5 s as above |

Caveats: the counter averages over machines (a per-machine split needs the machine id in the access log); the staging account
is shared with other lanes, so the strict window is only valid because the topic did not move in it (stated above).
Records f2118-w, f2118-w2, f2118-m, f2118-m2 revoked; macpool-1 and winpool-11 stopped; stg-a and stg-b stay on the build.
