# Edge and relay capacity runs (one 2 vCPU / 4 GB node)

What one Hetzner CX23-sized node (2 vCPU, 4 GB) running our edge or our relay holds. The real `paperboat-edge` and `paperboat-relay` images (built from
`edge/deploy/Dockerfile`, `relay/deploy/Dockerfile`) run the way pbh starts them (`--network host --cap-drop ALL --read-only`, plus `--cpus=2 --cpuset-cpus=0,1
--memory=4g`) on a GitHub-hosted runner (`tools/device-smoke/linuxpool.sh start`); the load generator runs on the runner's other two cores. Two runners cannot
reach each other (no inbound path; only the reverse tunnels to the jump host), so there is one node per runner: the numbers carry no NIC limit.

| Piece | Where | What |
|---|---|---|
| `loadctl` | `edge/tools/loadctl` | stand-in control plane: `init` writes the edge's config, certificates and trust files; `serve` answers the edge's and the relay's control requests and holds the admissions the load generator posts |
| `loadsim machines` | `cli/internal/loadsim` | N simulated machines on the agent's own carrier, hub and origin forwarder: one runtime route and R endpoint routes (previews/tunnels) each; `-inspector` records every request on the machine |
| `loadsim http` | same | open-loop request ladder, or large downloads, through the edge's public listener; each connection gets its own loopback source address because the edge limits requests, handshakes and streams per client address (`edgehttp/ip_limits.go`) |
| `relayload` | `relay/tools/relayload` | N idle peers, or P relayed pairs pacing a rate, with grants signed by loadctl's issuer key |

Runs, in order: `loadctl init`, `loadctl serve`, start the edge or relay container, `loadsim machines` or `relayload`, then the request ladder. One profile
(`go tool pprof`) per bottleneck: a pprof listener is added to a runner-only copy of `main.go`, never committed.

## Results (2026-11-20, main e74b52ff1, 1,000 carriers with one endpoint route each)

| | before the fixes | after |
|---|---|---|
| Idle carriers | 270 KB and 0.8% of a core per 100 | same |
| Carriers per node | 1,024 (fixed cap) | `node_capacity`, at most 4,096 admissions in all |
| Requests/s at p99 < 200 ms | 200 | 800 |
| New TLS connections/s at p99 < 250 ms | 200 | 400 |
| Large downloads | 260 Mbit/s, edge 34% CPU: the account byte limit (32 MiB/s), not the node | same |
| Inspector capture on | no edge cost (capture is on the machine) | |
| Relay idle connections | refused from ~6,000 total at 35% CPU | 16,000 held, 9% CPU, 2.2 GB |
| Relay forwarding | ~640 Mbit/s at 171% CPU, load generator on the same box: a lower bound | |
