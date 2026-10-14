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
`docker compose up -d` starts PostgreSQL, migrates from the repository schema, and
starts the server only after migrations succeed. Caddy owns API/download/object-store
TLS; the server certificate coordinator owns the public/runtime edge wildcards.
Configure DNS before starting the proxy. Keep releases in `releases/` and publish
through its atomic `current` directory. Missing release/signing decisions do not
make an installer artifact available.

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
