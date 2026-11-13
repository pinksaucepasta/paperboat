# F21-28 L: a blackholed QUIC carrier is found within the 15 s idle limit (staging, 2026-11-13 IST, REL-F2128)

Build: machine pb dev-c5c155345 (linux/arm64, container pb-rel-edge-a on anvit, `pb version`), staging edges pb-rel-edge-1/-2 on
paperboat-edge:2026.11.13.19 (contains f189d8f21, the edge QUIC 10 s idle / 5 s keepalive config), edge main at 8cf17c4b0 for the tests.

## Edge side (test)
The edge already ran endpointQUICConfig (idle 10 s, keepalive 5 s, same as the client's defaultQUICConfig 15 s/5 s); the earlier gap note
("edge idle 2 min / keepalive 30 s") was stale. The production carrier is HTTP/3 CONNECT (edge/internal/datacarrier/http_carrier.go), which
uses the same config. Tests, both through a UDP front that goes silent while the peer advertises a 2 min idle timeout:
- TestEdgeQUICCarrierDetectsABlackholedPath (native QUIC listener, existing)
- TestHTTPServiceHTTP3CarrierDetectsABlackholedPath (new, the production HTTP/3 path)
Client side: TestBlackholedQUICCarrierClosesAfterTheIdleTimeout (cli connector, existing). CI run 43434050957 at 34e37568f: both edge tests PASS (edge detected after 14.17 s each).

## Live (staging)
Setup needs: docker UDP to the published edge ports does not work from a container on the same bridge, so a temporary host nat
PREROUTING DNAT for this container only (removed after) let the machine's worker open QUIC carriers to both edges (two UDP sockets,
no TCP carriers). A first drill with an nft DROP in the container's OUTPUT hook was not a blackhole: sendto fails with EPERM and
quic-go closes at +2 s; the old "3-4 s" figures of round 29 were most likely that. Real drill: host `iptables -t raw PREROUTING -j DROP`
for both directions of container <-> edge-1 :18548 (silent path), machine runtime worker freshly restarted.
- t0 = 13:30:41Z drop inserted. The machine's QUIC carrier to edge-1 gave up and the pool replaced it over TCP (ESTAB to edge-1 :18545)
  at **+16.98 s** (15 s idle timeout + fallback dial; old behaviour was the 2 min QUIC idle). One QUIC redial attempt at +10.9 s also saw no answer.
- Edge side live: edge-1 logs nothing for a quiet idle close, so the live run does not show it; the edge side is the unit tests above.
Raw sampled output: +0.0 s udp sockets 3, +1.8 s 2 (peer-net socket), +10.9 s 3 (redial), +16.86 s TCP carrier up.
Cleanup: nat and raw rules and the nft table removed, scratch scripts removed; pb-rel-edge-a left running on dev-c5c155345.

## Other finding
Machine on anvit bridge cannot reach the staging edge QUIC ports without the temporary DNAT: staging machines on the bridge always run on TCP carriers.
