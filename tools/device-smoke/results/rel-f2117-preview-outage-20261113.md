# F21-17 L: public PREVIEW through a control outage (staging, 2026-11-13 IST, REL-F2117)

Machine: pb dev-c5c155345 (linux/arm64 container pb-rel-edge-a on anvit, paired to the staging account; includes 546ca5380).
Staging server paperboat-server:stg-6ca6b6982, edges paperboat-edge 2026.11.13.19 (edge-1 serves public HTTPS on :18546).

## Fixture: already fixed, no new cert needed
The brief's gap ("POST /v1/previews on a self-hosted staging account cannot dispatch, staging edge :443/self-signed") is stale:
142540391 made the edge advertise public_https_port and the server dispatch there (control_tunnel_nodes.public_https_port is
18546 on edge-1). A self-hosted account needs a customer-owned preview domain: `pb preview 8099 --domain <verified domain D>
--background --ttl 3h --json` returned state ready, edge_state ready in seconds. Public GET with the real Let's Encrypt
certificate (curl --resolve D:18546:anvit, ssl_verify_result 0) answered 200 (3/3 before the cut).

## Drill (anvit, iptables DOCKER-USER drop edge-1 172.17.0.5 -> control :18443 for 90 s, GET every 2 s, rule removed by trap)
before: 200 200 200
drop inserted 15:27:49Z, removed 15:29:19Z
during 90 s: 200=43 other=0 (edge control failures logged in window: 141)
=> the "keeps answering through the outage" half PASSES for a public custom-domain preview.

## Recovery: FAILS (row stays [~])
after removal (20 s later, 5 x 3 s): 000 x5 (curl: tlsv1 alert internal error), still failing 10 min later, on edge-1.
Same minute the edge logged edge_certificate_distribution_failed (cause internal) and the server's certificate worker logged
certificate_generation_conflict. DB facts (paperboat.tunnel_certificate_distribution_actions, edge-1):
- before the cut a preview lease renews every ~10 s, each renewal = stage -> activate of the same certificate (30-min
  preview_expires_at), all acked.
- 15:29:19.9 the queued stage of preview_generation 6 is acked ready (the server had already rolled it back: WaitReady timed out
  during the cut), no activate for it ever follows.
- 15:29:25.5 a stage for the tunnel certificate of t1.D (other domain record under D, same ACME fingerprint) fails on the edge
  with generation_conflict; from then no distribution action for the apex arrives and the apex handshake has no certificate.
- creating a new preview on D afterwards: stage -> certificate_action_failed, server sends revoke; `pb preview` times out.
- `docker restart` of edge-1 (persisted certificates restored=4) does not clear it: stage of the new preview still fails with
  certificate_action_failed, while the t1 stage/activate now succeed. t1.D keeps answering 200 on both edges throughout.
Not proven (needs the edge's registry snapshot): whether the apex entry disappears because of the stale gen-6 stage, an
expiry, or the HostnameConflict between the apex domain binding and the t1 tunnel domain binding (both cover names under D;
edge/internal/tlscert/registry.go stage() ~316-322). The staging domain carries two domain records for one zone and a
shared certificate fingerprint, which is not what a customer does; a single-domain account is the clean repro.

## Next
1. Repeat the drill on a clean staging account with one verified domain and only a preview (no tunnel on that zone).
2. If the apex entry still vanishes after a 90 s cut: regression test in edge/internal/tlscert (stale stage + missed
   activates) and fix; server side, after WaitReady times out for an edge that is only unreachable, do not roll back a stage the
   edge will still apply (server/internal/tunnelcert/coordinator.go ~1039).
