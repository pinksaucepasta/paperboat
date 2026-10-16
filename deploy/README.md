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
