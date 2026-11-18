# Edge-mode switch, managed-edge terminals and relay pool handover on an isolated stack

Run 2026-11-19 (IST) on an isolated stack on the anvit test host (own Compose project, own PostgreSQL, own DNS names and
ports; no other lane's stack touched). Build under test: commit a3114f1ca (server, edge, relay images and `pb`, all built
from that commit; the later commits on the branch change only this results file).

Stack: server (PostgreSQL 17, regional HTTPS with a Let's Encrypt wildcard), one managed edge (`spark_managed_edge`),
one workspace-owned edge (`spark_own_edge`, `self_hosted: true`, tunnel installation of account `usr_staging`),
two account relays (A = `stg_relay_396350ecbbc0`, B = `stg_relay_91e2c0f5cce7`), two machines (`spark-m1`, `spark-m2`)
each a systemd container running the new `pb` daemon. Hostnames below use `example.test` for the real ones.

## 1. Self-hosted-only workspace: terminals on the managed edge, tunnels on the own edge

`PUT /v1/selfhost/pools/tunnel {"mode":"self-hosted-only","installation_ids":["spark_own_install"]}` on a paired machine:

    runtime_carrier_attachments after the flip (machine spark-m1)
      spark_managed_edge | active=t | ready=t | hostname machine-<id>.rt.stg.example.test
      spark_own_edge     | active=f | ready=t | hostname machine-<id>.rt.stg.example.test
    control_routes: public_host = managed_public_host (t); applied_node_id = spark_managed_edge
    machine_topic_generations: tunnels and previews bumped by the flip (the machine was woken)

- Browser terminal ticket (`POST /v1/terminal-sessions/{id}/browser-ticket`):
  `stream_url = wss://anvit2.stg.example.test/v1/browser-terminal/machine-<id>.rt.stg.example.test`,
  `edge_node_id = spark_managed_edge`: the managed edge and the managed hostname.
- A WebSocket opened with that ticket against the managed edge: `101 Switching Protocols`, then one binary frame from
  the machine's terminal (362 bytes) through the managed edge's carrier; `native_usage_authorities` shows the attachment
  on `spark_managed_edge`. (The client does not speak the end-to-end terminal protocol, so the machine ends it after 10 s;
  every run below ends at the same 10.1-10.2 s.)
- `pb tunnel create tcp1 --from tcp://127.0.0.1:8081` (a public TCP tunnel, port 10000):
  `endpoint_edge_assignments`: only `spark_own_edge` (observed_ready = t); a client inside the stack dialling the own edge
  on port 10000 got `echo:hello-through-edge` from the machine's origin; the managed edge refused the connection.
- `pb preview 8080`: refused with `preview_domain_required` (self-hosted workspaces need a customer-owned preview domain):
  previews follow the edge mode. Not run: a preview or HTTP tunnel on a verified custom domain through the own edge
  (needs a customer DNS zone and certificate; the TCP tunnel above is the data path through the own edge).

## 2. Edge-mode switch with a held terminal (both directions)

Terminal opened at t=0 (attached on `spark_managed_edge`), pool flipped at t=4 s, observed to t=14 s:

    run                               pool flip at t=4 s                      terminal ended by the machine at   active/applied edge after
    baseline (no flip)                none                                    t=10.2 s                          spark_managed_edge
    self-hosted-only -> managed-only  200 mode=managed-only                   t=10.2 s                          spark_managed_edge
    managed-only -> self-hosted-only  200 mode=self-hosted-only (own edge)    t=10.1 s                          spark_managed_edge

The held terminal is not ended by the switch (no revoked/ended close, same end as the baseline) and the route's edge and
hostname do not change. The switch was accepted with the TCP tunnel paused (a running tunnel refuses the flip, as designed).

## 3. Relay pool switch drain, end to end

Direct paths between the two machines blocked (iptables), so `pb exec spark-m2 -- ...` from `spark-m1` is relayed.
A long relayed session (`pb exec` printing a line per second) running; the account's relay pool then set to relay B only
(`PUT /v1/selfhost/pools/relay {"mode":"self-hosted-only","installation_ids":["stg_relay_install_91e2c0f5cce7"]}`):

    02:43:01 start                          A capacity_used=3  B capacity_used=2
    02:43:17 relayed session running        A capacity_used=3  B capacity_used=2
    02:43:18 pool switched to [B]           relay_pool_handovers: node A, close_at = now + 299 s
    02:43:49 (+31 s)                        A capacity_used=0  B capacity_used=3
    ... every 30 s until +430 s             A capacity_used=0  B capacity_used=3

The machines moved their carriers to B within 31 s of the switch (inside the 5-minute window); the relayed session kept
running: 445 consecutive ticks received, largest gap between two ticks 2.7 s (at the move). The handover row for A stays
(account refused on A until its old grants expire); at its close time A had no carrier of the account left to close.
Not shown here: the relay-side forced close of a carrier that does not move (covered by the unit tests
`TestPoolHandoverKeepsCarriersUntilTheDeadlineThenClosesThemNonFatally` and `TestObservationAppliesPoolHandoverList`).

## Stack findings

- The managed edge publishes its HTTPS on a host port here, not 443: the server reads terminal snapshots through the
  edge by its infrastructure name on 443, so the server container got a hosts entry for that name pointing at the managed
  edge (a property of this test stack, not of the change).
- The first server start used the wrong region label and asked for a certificate for another stack's regional name; fixed
  and restarted before anything else ran. No other stack's data was changed.
