# Temporary error tracker: GlitchTip until 2026-12-02

Sentry's free error quota is used up until 2026-12-02. Until then errors go to a self-hosted
GlitchTip (MIT, Sentry-SDK compatible) on the production host. On 2026-12-02 every DSN goes back to
Sentry. The Sentry admin readback (`PAPERBOAT_SENTRY_API_*`, `sentry-operations.md`) is unchanged
and reads Sentry again after 2026-12-02; while on GlitchTip it shows nothing new.

## What runs (production host, `ssh root@coolify`)

| Item | Where |
| --- | --- |
| Compose project `glitchtip` (`web` 750 MB limit, `gt-db` Postgres 380 MB limit, own volumes, no Valkey) | `/opt/glitchtip/compose.yaml`, secrets in `/opt/glitchtip/.env` (0600) |
| HTTPS host, DNS-only A record to the host (`errors.<dev zone>`) | `errors.pprbt.dev` block appended to `/opt/paperboat/Caddyfile` (Caddy reload, no restart) |
| Admin login (`admin@errors.pprbt.dev`), registration and organisation creation closed | password in `/root/glitchtip-admin.password` (0600) |
| Projects `go` (server, relay, edge, CLI) and `console`, organisation `paperboat` | DSNs in `/opt/paperboat/secrets/paperboat_glitchtip_dsn_go` and `..._console` (0600) |

GlitchTip has its own Postgres: never point it at the production database. The Paperboat compose
file and containers are untouched. A server rollback that restores an old `Caddyfile` record drops
the `errors.` block; re-append it and `docker exec paperboat-proxy-1 caddy reload --config /etc/caddy/Caddyfile`.

## What GlitchTip accepts

Errors and transactions: yes. `trace_metric`, `span`, `session`, `client_report`, replay and
profile items are dropped by design; logs are off in this install (`GLITCHTIP_ENABLE_LOGS=false`).
So while on GlitchTip set `PAPERBOAT_SENTRY_LOGS_ENABLED=false` and
`PAPERBOAT_SENTRY_METRICS_ENABLED=false` next to the DSN (server, relay, edge).

## Switch to GlitchTip (one line plus the signal switches)

Server (needs the normal server rollout so the container is recreated; the file is read at start):

```sh
cd /opt/paperboat && cp server.env server.env.bak-glitchtip-switch \
 && sed -i 's#^PAPERBOAT_SENTRY_DSN_FILE=.*#PAPERBOAT_SENTRY_DSN_FILE=/run/secrets/paperboat_glitchtip_dsn_go#' server.env \
 && printf 'PAPERBOAT_SENTRY_LOGS_ENABLED=false\nPAPERBOAT_SENTRY_METRICS_ENABLED=false\n' >> server.env
```

Then roll out the server as usual. Check: a test error appears in `https://errors.pprbt.dev`
(project `go`), and `docker stats` shows `glitchtip-web-1` under 750 MB.

Edge and relay (self-hosted by `pbh`): write the `go` DSN into `/var/lib/paperboat-selfhost/sentry-dsn`
(root-owned, 0600) and recreate the containers with the same image.
Console: build with `NEXT_PUBLIC_SENTRY_DSN` set to the `console` DSN (it is inlined into the browser bundle).
Official CLI builds embed the DSN at build time (`SENTRY_DSN` repository variable); a running CLI can
use `PB_SENTRY_DSN`.

## Switch back on 2026-12-02

```sh
cd /opt/paperboat && cp server.env.bak-glitchtip-switch server.env
```

Restores the Sentry DSN file line and drops the two signal switches (or delete those two lines if
`server.env` changed since). Roll out the server, restore the previous DSN in
`/var/lib/paperboat-selfhost/sentry-dsn`, rebuild the console with the Sentry DSN, and set the
repository variables back. Then remove GlitchTip:

```sh
cd /opt/glitchtip && docker compose down -v
sed -i '/^# TEMPORARY until 2026-12-02: GlitchTip/,/^}/d' /opt/paperboat/Caddyfile
docker exec paperboat-proxy-1 caddy reload --config /etc/caddy/Caddyfile
rm -rf /opt/glitchtip /root/glitchtip-admin.password /opt/paperboat/secrets/paperboat_glitchtip_dsn_*
```

Delete the `errors.` DNS record in the dev zone.
