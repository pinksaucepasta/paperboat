# BW throttle staging measurements (branch t3/unlimited-edge-traffic-throttle, commit 8f4a3782c)

Date: 2026-11-18/19 UTC. Staging API: `https://anvit.stg.example.test:18443`
(loopback 127.0.0.1:18091 used for API checks). Public base: `*.pub.example.test`
(host port 18446). Containers on pbvm2: `bw-a` (172.17.0.6,
machine_d8e937ba-f171-4c1f-9168-9ff726af5f92), `bw-b` (172.17.0.8,
machine_818ba89c-2ef0-4b44-8af4-40ca41cf1f12), pb build dev-8f4a3782c
(linux/amd64, CGO_ENABLED=0, from `git archive 8f4a3782c`). Direct UDP A↔B
blocked inside both containers (iptables OUTPUT/INPUT DROP udp to/from peer;
removed TCP test rules afterwards). Both records revoked by exact id at the
end; both containers removed. No prod touched. pbvm2 was on a mobile hotspot
the whole run: absolute Mbit/s numbers below are link-influenced; ratios are
the robust signal.

Staging stack (all images built from 8f4a3782c, labels verified):
- paperboat-server:stg-8f4a3782c (migrated DB 400→422; goose 419 marked
  applied, its table+function already matched 419 exactly)
- paperboat-edge:stg-8f4a3782c (ready, fresh heartbeats)
- paperboat-relay:stg-8f4a3782c (ready; node stg_relay_68c3a58984e9)
- server runs with `-config billing.json`: personal_allowance_bytes=30000000,
  fair_use={edge_free 64000, edge 125000, team_per_seat 100000000000,
  relay_mbit 10, relay_free_included 30000000, relay_included 100000000000,
  relay_free_slow 0.512, relay_slow 1}; operator policy personal set to
  30000000 the same way.
- Relay reclassified self-hosted→managed for usr_staging (managed_node_authority
  added, selfhost row deleted, relay pool mixed, allowed={usr_staging}); both
  machines appear in its subject sets.
- Stale usage-verification key replaced with the live file key (reports were
  400 credential_invalid → dead-lettered before the fix).
- Expired then deleted the nonstandard paid-grant workspace `bw_staging`
  (id breaks notice prefix parsing; prod uses `personal:<user>`); next debit
  auto-created `personal:usr_staging` (free, 30 MB calendar-month period).

## 1. Relayed exec and send — FAIL on absolute rates, PASS on behavior

(a) `pb send` 20 MB A→B (managed, counted +20,002,600
hosting=managed bytes; hash-verified on B): ELAPSED 37,776 ms =
4.24 Mbit/s. Expected ~10: not reached — the path is link-limited below the
cap (hotspot), so the 10 Mbit/s cap is non-binding here. Completes, counted.
Top-up 10 MB: 18,912 ms (4.23 Mbit/s); 1 MB probe: 2,642 ms. Managed month
total reached 31,017,271 bytes (> 30,000,000) before phase 2.
(b) After crossing: `pb send` 3 MB A→B: 135,802 ms = 176.7 kbit/s,
delivered+verified, never refused. Repeat B→A 3 MB: 132,841 ms =
180.6 kbit/s. Expected ~512 kbit/s: measured ~180 (3x low; stable across both
directions — 24x below the fast-phase rate, so the throttle clearly engages,
but the absolute value misses 512; possibly send-protocol overhead at low
rates, hotspot variance, or a sub-configured enforcement — needs code follow-up).
(c) `pb exec` prints ~1 MB: fast phase 549 ms (14.6 Mbit/s), slow phase
507 ms (15.8 Mbit/s). Completes in both phases, but NOT slower (1 MB fits the
relay burst). Extra: 10 MB exec in slow phase took 2,133 ms (37.5 Mbit/s);
with TCP+UDP direct fully blocked it still took 488 ms for 1 MB, so exec bulk
is relayed yet not throttled — exec output bypasses the per-account throttle
(coverage gap for the parent lane).

## 2. Preview and tunnel through the staging edge — PASS

Origin: `python3 -m http.server` on A:18081. Preview
`https://bold-blue-lookout-XXXX.pub.example.test:18446`, durable tunnel
`bwtun` at `https://graceful-violet-camp-XXXX.pub.example.test:18446`
(both deleted/stopped at cleanup).
- Fast: 20 MB preview fetch from outside: HTTP 200, 20,000,000 bytes,
  5,059 ms = 31.6 Mbit/s, hash-identical to origin.
- Crossing: second 20 MB: 27,019 ms = 5.9 Mbit/s avg (mixed fast/slow).
- Slow, parallel: preview 10 MB at 27,709 B/s (221.7 kbit/s) + tunnel 10 MB
  at 31,016 B/s (248.1 kbit/s); sum = 58,725 B/s = 469.8 kbit/s ≈ 512.
  Both HTTP 200 with full bodies; no 429/403/5xx anywhere in the run.
- Slow, single: 3 MB preview fetch: HTTP 200, 3,000,000 bytes, 48,500 ms =
  495 kbit/s.
- `GET /v1/billing/workspaces/personal`: throttled:true, usage 60 MB /
  allowance 30 MB, relay_included 30 MB, relay_mbit 10, relay_slow 0.512,
  throttle_mbit 0.512, state free, id personal:usr_staging.
- `GET /v1/announcements`: bell holds both "80% of included traffic used"
  and "Public traffic reduced to 512 kbit/s".

## 3. Browser terminal while slowed — NOT RUN

No staging console exists (stack is server+edge+relay only; no console
service or test sign-in), and the repo has no ready protocol-level terminal
tool (only live-console e2e + unit tests). Hand-rolling a bridge client
would be a new tool, out of scope for this check.

## Staging leftovers for the parent lane

Stack stays up on stg-8f4a3782c with the config above (compose backup at
/tmp/compose.yaml.bw-bak on anvit). Re-grant paid test data with
`deploy/staging.py grant` if needed (was expired+deleted for the Free tiers).
Old draining stg_relay_* rows/installs from other runs untouched. Account
relay month total ≈ 35 MB managed; edge observed ≈ 60+ MB vs 30 MB cap.
