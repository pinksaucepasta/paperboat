# Managed control-plane deployment

Run this stack only on the managed VPS. Set immutable image references in the
owner-only `.env`: `PAPERBOAT_POSTGRES_IMAGE` (PostgreSQL 17),
`PAPERBOAT_SERVER_IMAGE`, `PAPERBOAT_DIAGNOSTICS_IMAGE` (MinIO), and
`PAPERBOAT_PROXY_IMAGE` (Caddy). Configure `PAPERBOAT_API_HOST`,
`PAPERBOAT_DOWNLOAD_HOST`, and `PAPERBOAT_DIAGNOSTICS_HOST` there too.

Restore owner-only secret files into `secrets/`; keep `server.env` owner-only.
Configure the server's database DSN with the `postgres` service hostname and
`paperboat` database, every public domain, browser origin, provider secret file,
and certificate reference. Set its listener to `0.0.0.0:8080`. Its diagnostic
object endpoint uses the configured diagnostics HTTPS host. Create the configured
private diagnostics bucket before admitting uploads. Never print Compose's resolved
environment or server secrets.

`docker compose config --quiet` validates composition without printing values.
For initial bootstrap only, `docker compose up -d` starts PostgreSQL, migrates from the repository schema, and
starts the server only after migrations succeed. Caddy owns API/download/object-store
TLS; the server certificate coordinator owns the public/runtime edge wildcards.
Configure DNS before starting the proxy. The release workflow publishes the three
native-qualified CLI binaries to public GitHub Releases, then atomically activates
signed TUF metadata and the product-pinned installer in `releases/current/`.
Installer and metadata requests reach the server; binary URLs in the signed metadata
point to GitHub. The origin bundle keeps an empty `tuf/targets/` directory. Pairing
custom builds is independent of this publication and needs no server release record.

PostgreSQL is reachable only on VPS loopback and the private Compose network.
Preserve the named volumes and protected secret files during upgrades; do not use
`down --volumes` for ordinary teardown. Back up PostgreSQL and test a restore before
calling this deployment complete. The self-host installer manages relay/tunnel
nodes separately; this stack does not provide a self-hostable server.

Install `paperboat-backup.service` and `paperboat-backup.timer` into systemd on
the VPS and enable the timer. `backup-database.sh` publishes a mode-0600 custom
PostgreSQL dump atomically into protected `backups/`, preserving previous backups
on failure. Monitor disk capacity and retain backups according to your policy;
the script does not silently delete existing backups.

Run `verify-backup.sh <dump-file>` on the VPS to restore into a new
`paperboat_restore_test`, verify schema count and migration version, and remove
only the scratch database it created. It refuses an existing scratch database.

Build the diagnostics image from its official source using `diagnostics.Dockerfile`:
set `PAPERBOAT_DIAGNOSTICS_MODULE` to the upstream module and
`PAPERBOAT_DIAGNOSTICS_COMMIT` to its reviewed 40-character commit, then supply both
as Docker build arguments. The pinned builder/runtime images and upstream license
are included; no unavailable upstream container tag is required. Set the resulting
image reference in the protected deployment environment. The entrypoint reads the
mounted credential files and refuses missing or empty credentials.

Install the distribution's `python3-boto3` on the managed VPS and run
`verify-diagnostics.py --endpoint <configured-HTTPS-endpoint> --region <region>
--bucket <bucket> --secrets-directory <protected-directory>`. It creates the private
bucket if absent, checks upload/download, verifies anonymous reads are denied, and
removes its random disposable object. It never prints credentials or payloads.

## Public HTTPS for enrolled IP edges

Relay/edge installations live on their own hosts. An installation advertising a
public IP needs a publicly trusted IP certificate in addition to its enrolled
carrier certificate. These identities serve different connections; keep the
carrier key and runtime credentials unchanged.

Install `edge-ip-certificate.py` as
`/usr/local/lib/paperboat-selfhost/edge-ip-certificate.py` (mode 0755), and install
`paperboat-edge-certificate.service` and `.timer` in `/etc/systemd/system/`.
Create mode-0600 `/etc/paperboat-edge-certificate.env` with
`PAPERBOAT_CERTIFICATE_EMAIL` set to the operator's certificate contact address.
Run `systemctl daemon-reload`, start `paperboat-edge-certificate.service`, then
`systemctl enable --now paperboat-edge-certificate.timer`.

The script reads the enrolled IP from protected deployment configuration. It uses
a pinned native Certbot image and the short-lived IP certificate profile, persists
ACME state under the installation's `public-acme/`, and applies certificate paths
through `pbh configure-tls`. It checks daily and renews only within two days of
expiry. HTTP validation briefly stops the helper and its exact owned edge container;
the service is restored on failure. A current certificate leaves the service running.
The protected `public-acme/renew.log` contains the latest attempt. Verify both current
node readiness and the server's authenticated public ingress proof after configuring
or renewing a certificate.

## Managed server upgrades

Use `python3 rollout-server.py --directory /opt/paperboat --image <qualified-image>`
for upgrades. Do not replace the serving server with `compose up` or restart the
proxy. The controller runs additive migrations and starts a separate candidate,
checks `/readyz`, then reloads only Caddy's API/download upstreams. Database,
configuration, secrets, certificate state and existing server processes remain.
A failed candidate leaves the serving process untouched; interrupted switching
restores the previous route before retiring anything.

The controller records the serving container in protected `server-rollout.json`.
Old containers stay alive while their namespace has accepted live TCP connections,
including long HTTP responses and control streams. Run `--reap` periodically and
before the next upgrade; it removes an old container only after those connections
finish. Install `paperboat-server-reap.service` and `.timer` in `/etc/systemd/system/`
and enable the timer to perform this check every minute. Each check is bounded;
it retains live connections for the next check. There is no session-killing
retirement deadline. Two retained generations
block the next rollout with an actionable message instead of accumulating processes.
Caddy retains in-flight HTTP responses during reload; server browser WebSocket
endpoints would additionally need explicit proxy stream preservation before use.
Use `server-rollout.json`'s `active` container for logs/exec after the first rollout,
rather than assuming the bootstrap Compose container still exists.

Changes that remove schema or change wire contracts must preserve the prior serving
version for the drain window. Back up the database before rollout as described above.
This controller upgrades the control plane; edge carrier rolling replacement is a
separate lifecycle and must also retain its admitted streams.

## Building and deploying images

Every image builds from the repository root as its context (the Go modules reference each other
through `go.work`), taken from a clean checkout of the commit being deployed:

```sh
git archive HEAD -- . ':!console' ':!marketing' ':!tools' | tar x -C "$dir"
cd "$dir" && docker build --build-arg PAPERBOAT_VERSION=<YYYY.MM.DD.N> -t <name>:<tag> -f <service>/deploy/Dockerfile .
```

The server also needs `--build-arg PAPERBOAT_COMMIT=<40-hex HEAD>`; it is the commit that
`GET /v1/server-version` reports and the image build refuses anything else. Take a database backup
(`./backup-database.sh`) and rehearse any new migration against a restored copy in the isolated
test PostgreSQL before rolling out. Never run test suites against the production database.

Edges are `pbh`-managed. Copy the image to the host (`docker save | ssh host docker load`) and run,
one host at a time, `/usr/local/lib/paperboat-selfhost/pbh install -relay-image <relay> -tunnel-image
<tunnel>`: both images must be named, and the installation keeps its ownership and configuration.
After each host, check that its node row heartbeats (`control_tunnel_nodes.last_heartbeat_at`) and
the edge logs show no `route_invalid`/`tunnel_route_reconcile_failed` before touching the next.
Rolling back is the same command with the previous image tags.

## Deploy procedure for any lane (server and edges)

Any lane may deploy its own server or edge change with this procedure. The order is fixed; skip
nothing. Hosts: the control plane runs on `ssh root@coolify` (`/opt/paperboat`); the two edges are
oracle (`node_fce5a4b8`, the machine the lanes run on) and anvit (`ssh anvit`, `node_e8a853d5`).

**1. Gate.** The change is pushed to `main` after `make gate` (or the Go/PostgreSQL tests of the
packages you touched) passed. Database tests run only in the isolated test PostgreSQL on oracle
(`~/bin/lanebtest.sh <srcdir> <go test args>`, a container on `127.0.0.1:55433`), never on the
production database.

**2. Take the deploy lock.** One deploy at a time, server or edge. The lock is a file on coolify
that names its owner; `noclobber` makes taking it atomic:

```sh
ssh root@coolify 'set -o noclobber; echo "<lane> <what> $(date -u +%FT%TZ)" > /opt/paperboat/deploy.lock' \
  || ssh root@coolify cat /opt/paperboat/deploy.lock     # someone else deploys: wait, do not remove it
# ... deploy ...
ssh root@coolify rm /opt/paperboat/deploy.lock             # always release it, also after a rollback
```

A lock older than 45 minutes whose owner lane is not answering may be removed after checking
`docker ps` on coolify and `python3 /opt/paperboat/rollout-server.py --directory /opt/paperboat --reap`
show no rollout in progress. `rollout-server.py` also holds its own `server-rollout.lock`, so two
rollouts cannot switch traffic together even if this file is ignored.

**3. Server.** On coolify, from a clean checkout of the exact pushed commit (never the working tree):

```sh
V=2026.10.22.N                                   # next free tag: docker images paperboat-server
C=$(git rev-parse origin/main)                   # the commit you deploy
D=$(mktemp -d); git -C <your clone> archive $C -- . ':!console' ':!marketing' ':!tools' | tar x -C $D
docker build --build-arg PAPERBOAT_VERSION=$V --build-arg PAPERBOAT_COMMIT=$C \
  -t paperboat-server:$V -f $D/server/deploy/Dockerfile $D        # or copy the archive to coolify first
cd /opt/paperboat && ./backup-database.sh                          # always, before the migration runs
# new migration? rehearse it first in the isolated test PostgreSQL (goose up on oracle's lane-b-test-pg)
docker inspect --format '{{.Config.Image}}' $(python3 -c "import json;print(json.load(open('server-rollout.json'))['active'])")   # PREVIOUS image: note it
python3 rollout-server.py --directory /opt/paperboat --image paperboat-server:$V
python3 rollout-server.py --directory /opt/paperboat --reap        # before the next deploy
```

The rollout runs additive migrations, starts a candidate, waits for `/readyz` and only then switches
Caddy; a failed candidate leaves the serving process untouched. Check the result:
`curl -fsS https://api.pprbt.dev/v1/server-version` must report `$C`, and `/readyz` must be 200.

**4. Edges** (only when `tunnel/`, `relay/`, `helper/` or shared edge code changed). Oracle builds
natively (arm64); anvit takes the image by `docker save | ssh anvit docker load`. One edge at a time,
oracle first, and wait for its node row to heartbeat before the next:

```sh
sudo docker build --build-arg PAPERBOAT_VERSION=$V -t paperboat-tunnel:$V -f tunnel/deploy/Dockerfile $D
sudo /usr/local/lib/paperboat-selfhost/pbh install --relay-image paperboat-relay:<current> --tunnel-image paperboat-tunnel:$V
# check: select node_id,last_heartbeat_at from control_tunnel_nodes; edge logs show no route_invalid
```

If `pbh` itself changed, install the new binary to `/usr/local/lib/paperboat-selfhost/pbh` and
`/usr/local/bin/pbh` first: the old binary regenerates `tunnel/deployment.json` from its own config on
start and drops new fields. The current tags are in `docker ps` on each edge and in the PLAN Log.

**5. Roll back** at once, without asking, when the new build breaks anything that worked before:
`python3 rollout-server.py --directory /opt/paperboat --image paperboat-server:<previous>` (migrations are
additive, so the previous server still runs on the new schema; a migration that removes or renames
anything must not be deployed in the same step as the code that needs it). Edges: the same `pbh install`
with the previous tags. Release the lock, then tell the lane that was waiting what happened.

**6. Recheck and smoke.** After every deploy: public health (`/healthz`, `/readyz`), the `server-version`
commit, one preview created and fetched through each edge, and `docker logs --since 5m` of the active
server container with no new `level=ERROR` family. Then ask Lane A (thread "Lane A — CLI & Linux
devices") for one smoke run against the deployed server, naming the version and what changed. A check that
passed before and fails now is a regression: roll back first, investigate second. Add one PLAN Log
line (rows, commit, image tag) and keep the previous image tag until the smoke run is green.

## Edge failover

Every workspace is served by an edge set of two edges in distinct failure domains (managed pool, or
its own self-hosted servers; never mixed). What the system does without help:

- an edge whose heartbeat stops is no longer offered for new placement and its DNS addresses drop
  out within the record TTL (60 s);
- after 10 minutes without it, the workspace's set replaces it: a managed workspace takes another
  managed edge, a self-hosted one another selected installation, otherwise it keeps the remaining
  edge (`worker_edge_sets`).

Checking by hand: `control_tunnel_nodes` (state, `last_heartbeat_at`), `workspace_edge_sets`
(the set per workspace), `edge_dns_publications` (what DNS holds; `failed` rows carry a
`failure_code`). The server trusts forwarded client addresses only from `trusted_proxy_cidrs`; if the
proxy's Docker network address changes (a new Compose network), update that CIDR and redeploy the
server with a changed image label so the container is actually replaced.

## Shared endpoint engine: rehearsal plan and chaos-test spec (PLAN.md 3a; no code yet)

**Migration rehearsal (every migration of the series).** Run `./backup-database.sh`; restore the newest dump into a new
database in the isolated test PostgreSQL (`pg_restore --no-owner --no-privileges`); run `db.Migrate` against it and assert:
goose version, row counts of `tunnels`, `tunnel_routes`, `tunnel_domains`, `tunnel_config_generations` equal the
`endpoints`/`endpoint_routes`/`endpoint_hostnames` counts for kind=tunnel, ids unchanged, every live
`edge_dns_publications` hostname still maps to exactly one endpoint, no orphan certificate distribution, live preview leases
ended with `terminal_state` set. Take the lock-heavy statements (`ALTER`/`DROP` on `user_machines`, `tunnels`,
`preview_leases`) from the 115 experience: a DROP COLUMN on a busy table deadlocked once, so give each such step a
`lock_timeout` and retry the rollout; migrations stay transactional. Keep the previous image running as the rollback target.

**Chaos test (kill one edge mid-traffic).** Fixture: an account edge set of two edges, one agent with 2 carriers per edge,
one preview and one tunnel with an HTTP route and a private_tcp route, a client holding 20 requests per second and one long
stream on each. Steps: (1) stop edge A's process; assert within 15 s the DNS record lists only B, in-flight streams on A
reset (no hang), new requests succeed on B with zero failures after the record change plus TTL; (2) keep A down 10 min
(fake clock): the set replaces A with C, the agent opens carriers to C, assignments appear on C, the record lists B and C;
(3) A returns with a new process epoch: it is not in the set, serves nothing, and its stale assignments are removed on its
first snapshot; (4) kill B instead and repeat; (5) kill the agent's active carrier on one edge: the warm carrier takes the
streams, no route change, no DNS change. Pass criteria: no request fails after convergence, no endpoint is ever listed on
an edge without a ready assignment, no goroutine/stream leak on the edge (leak test), observed state matches desired
within one pull interval. Runs in the isolated test PostgreSQL with in-process edges; the device smoke suite repeats (1) once
against the real pair.

## Public Suffix List entry for the preview/tunnel domain (filed by the owner after the landing page is live)

Browser access works without the listing (host-only `__Host-` cookies plus the edge's same-site request checks);
the listing adds browser-level isolation between previews and needs no code change.

1. Prove control of the domain: add a TXT record `_psl.pprbt.app` whose value is the URL of the pull request
   (`https://github.com/publicsuffix/list/pull/<number>`), in the zone that serves `pprbt.app`.
2. Open a pull request against `publicsuffix/list`, adding `pprbt.app` in the PRIVATE DOMAINS section, in
   alphabetical position, with the block comment:
   ```
   // Paperboat : https://paperboat.cloud
   // Submitted by Paperboat <contact address of the owner>
   pprbt.app
   ```
   Description: what the domain serves (user-controlled preview and tunnel subdomains of one service, so every
   label is a separate security boundary), the organisation's site, that the domain is registered for at least two
   years, and a link to the `_psl` TXT record check (`dig +short TXT _psl.pprbt.app`).
3. Publication takes weeks; browsers pick it up with their releases. Nothing in Paperboat waits for it.
