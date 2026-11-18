# Release flow and launch cutover

How a build reaches production, and the one-time cutover to the launch layout. It names the real tools and
their flags; it does not repeat the runbooks it links. Hosts are described by role: addresses and host names
live in the operators' local notes, never in this repository.

Related: [deploy/README.md](../../../deploy/README.md) (every deploy step and the gate in detail),
[glitchtip-errors.md](glitchtip-errors.md), [operator-admin.md](operator-admin.md),
[preview-tunnel-control-plane.md](preview-tunnel-control-plane.md), [control-plane-alerts.md](control-plane-alerts.md),
[control-plane-credential-rotation.md](control-plane-credential-rotation.md), [sentry-operations.md](sentry-operations.md),
[helper/README.md](../../../helper/README.md).

## Layout

| Role | Runs | Notes |
| --- | --- | --- |
| Server VPS | the control plane only: Compose project in `/opt/paperboat` (server, proxy, diagnostics store) | one deploy lock file, `deploy.lock`, lives here |
| Database | PlanetScale Postgres, same region as the server VPS | two connections, see below |
| Console VPS | the web console (`tools/console-prod.sh`) | small VPS of its own |
| Relay + edge nodes | 5–6 VPS across regions, each one relay and one edge, installed and upgraded with `pbh` | no Compose; `paperboat-selfhost.service` |
| Staging | a full stack on the ARM build host: server, its own Postgres, console, one edge, one relay, staging domains | `deploy/staging.py`; own error-tracking environment |
| Dev host | development and image builds (amd64 server images are built on an x86_64 Docker host, arm64 edge/relay images on the ARM build host) | never the server VPS: a build there slowed `/v1/edge/*` during a gate |
| Probes | the production canary (`tools/probe/canary.py`, user unit `paperboat-canary`) runs from two non-production places (target) | today it runs from the dev host only, and the gate reads its `status.json` there; the second place is not set up yet (see the gaps). Neither is the server VPS or a production node, so a production outage cannot take the prober down with it |

### Database connections (server configuration)

| Variable | Launch value |
| --- | --- |
| `PAPERBOAT_DATABASE_DSN` | the **pooled** PlanetScale connection (all ordinary queries) |
| `PAPERBOAT_DATABASE_DIRECT_DSN` | the **direct** connection: session advisory locks, `LISTEN`/`NOTIFY` (machine events) and the migrations run on it; the direct pool is small (6 connections by default) |
| `PAPERBOAT_DATABASE_REPLICA_DSN` | **empty** at launch. A regional read replica later is this one value per region, nothing else |
| `PAPERBOAT_DATABASE_STARTUP_PARAMS` | `false` only if the pooler rejects startup parameters; then run `paperboat-server db-role-defaults` once as the application role first |

Operator guidance, not enforced by the code: use TLS verification (`sslmode=verify-full`) in both DSNs. The values are secrets: they go in the protected
`server.env` / `secrets/`, never in the repository or a log. Why two connections:
`server/internal/db/db.go` (`Direct()`): behind a transaction pooler an unlock or `LISTEN` would run on another backend.

## Staging

`deploy/staging.py` (run from the dev host; the stack is on the ARM build host) keeps its own Compose project, Postgres volume,
images, ports and secrets; it never reads or writes production user or machine data (`init` only copies the identity-provider and DNS credentials from the production environment, in memory). Subcommands: `build <commit>`, `build-edge <commit>`, `init`, `up <image>`, `grant`, `seed`, `dns`,
`enroll <name>`, `edge <image>`, `relay <image>`, `reset`, `status`, `logs`, `down`. Staging runs under staging domains and is told apart in error tracking by its own region and instance
(`PAPERBOAT_SENTRY_REGION`, `PAPERBOAT_SENTRY_INSTANCE`; `staging.py` sets no `PAPERBOAT_SENTRY_ENVIRONMENT`, and the monitors accept only the names in [sentry-operations.md](sentry-operations.md)), so its errors never mix with
production. Staging checks, in order: the staging canary (`deploy/staging-canary.sh`: exec both ways and ssh),
the end-to-end suite and the probes against staging. A build is not offered to production until all three are green.

## Release flow

Order, with what each step is and what stops it:

1. **Build.** Clean checkout of the commit, never a working tree. Server image: `docker build --build-arg PAPERBOAT_VERSION=<YYYY.MM.DD.N> --build-arg PAPERBOAT_COMMIT=<40-hex> -f server/deploy/Dockerfile .`
   (deploy/README.md, "Building and deploying images"). Edge and relay images build natively for each architecture and are moved with `docker save | ssh <host> docker load`.
2. **Staging.** `deploy/staging.py up <image>` then the staging canary, e2e and probes. Green on all three or stop.
3. **Server, through the gate.** `deploy/gated-deploy.py` takes the deploy lock, notes the serving images, runs the deploy command
   (`deploy/check-edges.sh --deploy '<rollout-server.py ...>'`, which keeps the 5xx, preview and offline checks), and waits for one canary
   pass that started after the switch. A failed deploy command or a red canary rolls back everything it changed (server first,
   `rollout-server.py --rollback --image <previous>`) and reports. Use `--stage '<staging command>'` to run staging first (exit 5 refuses before production is touched).
   Exit codes: 0 kept, 1 rolled back and green, 2 rolled back and still red (or a rollback step failed), 3 refused (canary red before), 4 lock held, 5 staging failed. Back up first (see the gaps below).
4. **Watch 20 minutes.** Keep the canary and [control-plane-alerts.md](control-plane-alerts.md) in view. The gate itself only waits up to `--wait` seconds (default 300) for the first green run after the switch;
   after it returns "kept" a later red is rolled back by hand on the server host: `python3 rollout-server.py --directory /opt/paperboat --reap`, then `python3 rollout-server.py --directory /opt/paperboat --rollback --image <previous>` (the order the gate uses; `--rollback` skips the two-draining-servers limit).
5. **One low-traffic edge/relay node as the canary node, 30–60 minutes.** Only when `edge/`, `relay/`, `helper/` or shared edge code changed. Run the same gate with only that node: `gated-deploy.py --what '<lane and change>' --server-host <server VPS> --edge <ssh target of that node> --deploy "PROBE='...' SQL='...' deploy/check-edges.sh --deploy '<node command>' <ssh target>=<node ip>"`
   (`--edge` takes an ssh target or `local`; `check-edges.sh` needs `PROBE` and `SQL` and `<ssh target>=<ip>`; see deploy/README.md step 6). The node command runs on the node:
   `sudo /usr/local/lib/paperboat-selfhost/pbh install --relay-image <relay> --edge-image <edge>`; both images must be named. Roll an edge back only after the server is back on the matching version.
6. **Remaining nodes, one at a time.** After each: its node row heartbeats (`control_tunnel_nodes.last_heartbeat_at`) and its edge logs show no `route_invalid` / `edge_route_reconcile_failed` before the next.
7. **Console.** `tools/console-prod.sh` builds `origin/main`, ships it to the console host (`PB_CONSOLE_PROD_HOST` overrides the ssh target), starts it in the slot it is not serving from (`.next-a` / `.next-b`) and
   returns to the old slot if the new one does not answer in 30 s: that is the slot swap. The operator panel is deployed the same way with `tools/admin-prod.sh` (`PB_ADMIN_PROD_HOST`).
8. **Clients.** Server first, then client: the release workflow refuses a release whose server and shared commits are not live (`tools/check_server_deployed.py`, from `.github/workflows/release.yml`).
   Upgrade our own fleet first (every canary machine on the same build, together: `pb install` of the new binary), then push the release tag `YYYY.MM.DD.N` (`tools/release-version.sh next`).
   The Release workflow qualifies five products (Linux amd64/arm64, Windows amd64/arm64, macOS arm64), publishes the GitHub release, signs the TUF metadata and marks it Latest last. Release channels come after launch; at launch there is one stream.
   A failed publish is resumed with `workflow_dispatch` (`release_tag` and `qualification_run`), never by re-tagging.

Never deploy two things at once; the lock is shared by server, edge and gate. A server change that removes or renames schema is never shipped in the same step as the code that needs it
(migrations run while the previous server is still serving).

## Launch cutover

Do these in order. Each step ends with its check; do not start the next one on a red check.

1. **PlanetScale.** Create the Postgres database in the server's region; create the application role; note the pooled and the direct connection. If the pooler rejects startup parameters, run `paperboat-server db-role-defaults` as the application role and set `PAPERBOAT_DATABASE_STARTUP_PARAMS=false` before the first `serve`.
   Move the data once: freeze writes (stop the server), `pg_dump --format=custom` the old database, `pg_restore` into PlanetScale over the **direct** connection, then start `paperboat-server migrate` against it and compare the migration version
   (`goose_db_version`) and the table count, as `deploy/verify-backup.sh` does, and diff row counts of the large tables by hand. Rehearse the whole move on staging first; no repository script does it. Check: `migrate` is a no-op, `/readyz` is 200 on a test server pointed at it.
2. **Server VPS wipe and install.** Before wiping, copy off the host: `.env`, `server.env`, `secrets/`, `Caddyfile`, `releases/` (the signed TUF metadata and installers; the release workflow reads `releases/current/tuf`), and the diagnostics volume if it holds reports.
   Install a fresh host, restore those, set the database variables above, and edit the Compose file first: `deploy/compose.yaml` still starts a bundled `postgres` and makes `migrate` wait for it (see the gaps). Then bootstrap Compose (`docker compose up -d`, once; afterwards `rollout-server.py` only). The server comes up on the same image the staging run proved.
   Point the repository variables the release workflow reads (`PAPERBOAT_DEFAULT_SERVER_URL`, `PAPERBOAT_DEFAULT_RELEASE_URL`, `PAPERBOAT_RELEASE_HOST`) and the release ssh secrets at the new host. Check: `GET /v1/server-version` reports the deployed commit, `/healthz` and `/readyz` are 200.
3. **Console.** Provision the console VPS (service user, `/opt/paperboat-console`, `/etc/paperboat-console/console.env` with the server URL and WorkOS values, a TLS proxy of its own), then `PB_CONSOLE_PROD_HOST=<console host> tools/console-prod.sh`. The script's health URL and the unit's bind address are the server box's Compose bridge, so adjust both for the console VPS first (see the gaps). Check: the console loads and signs in.
4. **New edge/relay VPS enrolment, one at a time, from the operator panel.** On the panel's Infrastructure page open the self-hosted node enrolment form (the dashboard calls it Network → Add self-hosted node): enter the node's endpoint host, region and kind, and it prints the install command. Run it on the new host: `sudo pbh install --enable both --endpoint-host <name> --region <region>`
   (add `--relay-image` / `--edge-image` for custom images). Then `pbh selfhost code` prints the claim address and a fresh single-use code (valid 15 minutes). Paste both into the panel; the server connects to the claim address, verifies the node and registers it as global fleet infrastructure.
   Put the host's cloud firewall in order (inbound TCP 443 and 10000-59999, [preview-tunnel-control-plane.md](preview-tunnel-control-plane.md)), run `pbh doctor`, add the node to the canary's `CANARY_EDGES` list, and add its address to the managed wildcards
   (the wildcards list every edge and are static). Check: the node row heartbeats and the canary `edges` step is green for it. Install `edge-ip-certificate.py` and its timer if the node advertises a bare IP (deploy/README.md).
5. **Retire the old dev edges** from the dev hosts: remove their A records from both wildcards first, then remove each installation from the panel and `pbh uninstall` on the host. Check: `pbh`-managed containers gone, canary `edges` still green.
6. **DNS.** Point the API, download, diagnostics and console names at their new hosts and the regional/public wildcards at the new edge set. Check: from outside, the API and console resolve to the new hosts; the canary stays green through the change.
7. **Operator TOTP enrolment.** Set `PAPERBOAT_OPERATOR_EMAIL`, sign in once to the panel to create the first operator, then enrol the authenticator and save the ten recovery codes ([operator-admin.md](operator-admin.md)). Add the other operators from the panel's Operators page. `PAPERBOAT_ENCRYPTION_KEY` must be set or no operator can finish signing in.
8. **Error tracking: GlitchTip until 2026-12-02.** Follow [glitchtip-errors.md](glitchtip-errors.md): server DSN file plus `PAPERBOAT_SENTRY_LOGS_ENABLED=false` and `PAPERBOAT_SENTRY_METRICS_ENABLED=false`, the edge/relay `sentry-dsn` file on every node (written before `pbh install`),
   the console build's DSN, and the repository variable the release build embeds. Roll the server out for the new environment. Switch back on 2026-12-02 by the same runbook. Check: a test error appears in GlitchTip.

### Post-deploy checks (after the cutover and after every server or edge deploy)

- **Canary green** on every step, including `edges` for each node and the hourly account probe. A red step rolls back (the gate) or is rolled back by hand.
- **Idle per-route request rate.** With no user traffic, the per-route counters (`paperboat_server_http_route_requests_total`, `paperboat_edge_route_requests_total`) settle at the probes' own baseline within minutes; a route that stays high is a client or worker loop and is investigated before the next step.
- **Private-app sign-in on every edge.** Open a private preview or durable tunnel URL through each edge's own address (not DNS round-robin) and complete the browser sign-in. This is a manual check today: no script does it.
- `docker logs --since 5m` of the active server container shows no new `level=ERROR` family; a preview is created and fetched through each edge.

## Gaps in the tooling (found while writing this; close them or work around them)

- `deploy/compose.yaml` still runs a bundled `postgres` service, `migrate` waits on it, and `deploy/backup-database.sh` / `verify-backup.sh` use `docker compose exec postgres`. The launch server VPS needs a Compose file without it and database backups from PlanetScale; until then take the move-time dump with `pg_dump` over the direct connection.
- `tools/console-prod.sh` and `deploy/paperboat-console.service` serve the console on the Compose bridge (`172.18.0.1:3100`) for the server box's proxy, and `tools/admin-prod.sh` likewise (`:3101`). On a separate console VPS the unit's bind address, the post-deploy health URL and a TLS proxy are host-specific and not in the repository; the operator panel's home is not decided here.
- `deploy/gated-deploy.py` stops watching at the first green canary run after the switch. A continuous 20-minute watch with automatic rollback is not implemented; the watch in step 4 is the operator's.
- The second probe place does not exist yet: `paperboat-canary` and the account probe run from the dev host only, and `gated-deploy.py` reads one `status.json`.
- `deploy/staging.py` has no console subcommand: the staging console and its end-to-end suite are run by hand.
- The managed-edge wildcard records are static and edited by hand, for adding as well as removing a node ([preview-tunnel-control-plane.md](preview-tunnel-control-plane.md)).
