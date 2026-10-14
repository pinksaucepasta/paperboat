# Install a self-hosted node

Ubuntu 22.04/24.04 or Debian 12 on amd64/arm64, systemd and public inbound connectivity are required. Install
first; no account, team, or global scope is assigned by installation.
The public helper installer/package publication is still pending; these one-liners
require that qualified release publication. The local package and Docker supervisor
have been exercised on the configured ARM64 infrastructure.

```sh
curl -fsSL https://get.pprbt.dev/selfhost/install | sh
# Choose tunnel or both instead of the default relay:
curl -fsSL https://get.pprbt.dev/selfhost/install | sh -s -- --enable tunnel
curl -fsSL https://get.pprbt.dev/selfhost/install | sh -s -- --enable both
```

The installer downloads the configured immutable package, verifies its SHA256,
installs `pbh`, installs Docker from the configured official source when missing, and enables a protected system service. The helper runs the configured relay/tunnel images with host networking and a protected state mount. Runtime binaries are not installed on the host.
An optional `--endpoint-host` accepts a public IP or DNS name; otherwise configure `PAPERBOAT_SELFHOST_ADDRESS_URL` with the deployment's
public-address endpoint before running `pbh`. Address detection has no embedded
Paperboat domain. No domain is required to
install or claim. Public browser and runtime routes require customer-owned domain
configuration in the dashboard before publication; do not use a Paperboat-managed
domain for a self-hosted route.

Paste the printed claim address and expiring code into **Dashboard → Network →
Add self-hosted node**, then choose the owning personal workspace or team.
Paperboat global nodes use the separate authenticated operator enrollment API with Operator-Target set to the claim address, an opaque Operator-Reason reference and an Idempotency-Key. The bootstrap TLS key is pinned by the code; ownership
is assigned only by the authenticated control-plane claim. The code lasts fifteen
minutes, is single-use, and contains no account or team identity. Treat it as a
secret. Generate a fresh unclaimed code with `pbh selfhost code`.

Tunnel edge ownership is exclusive per workspace: choose managed-only or
self-hosted-only. There is no mixed tunnel pool or managed fallback. A workspace
may select up to 32 enrolled tunnel installations; Paperboat chooses at most two
ready selected edges for a tunnel publication and one for a preview. Relay pools
remain Personal-scoped and retain mixed or self-hosted-only modes.

Set each self-hosted hostname's A/AAAA records to exactly the verified public
ingress addresses Paperboat selects for that publication. After changing a pool
or placement, explicitly verify DNS again in the dashboard (or run
`pb tunnel domain verify <tunnel> <domain>` for a tunnel) and wait for the
certificate to become ready. Stop active previews and tunnels in that workspace
before switching between managed-only and self-hosted-only; the control plane
rejects a live mode change.

Configure customer DNS and certificates before restarting or recreating those
publications.

For public browser HTTPS on the enrolled endpoint hostname, provide a valid
browser-trusted certificate and owner-only private key during tunnel installation:

```sh
pbh install --enable tunnel --endpoint-host edge.customer.example \
  --https-cert /etc/paperboat/edge-fullchain.pem \
  --https-key /etc/paperboat/edge-key.pem
```

These files configure public browser HTTPS and are independent of the infrastructure
TLS identity pinned by the claim code for the native carrier. On an existing tunnel
installation, update the paths and restart the service with:

```sh
pbh configure-tls --https-cert /etc/paperboat/edge-fullchain.pem \
  --https-key /etc/paperboat/edge-key.pem
```

`configure-tls` validates and atomically saves the new paths before restarting the
service. If the restart fails, it restores the previous configuration and attempts
service recovery. Separately managed `pbh` processes can save the paths with
`--no-service` and `--state-dir /absolute/state`; restart that process to apply
them. Renew the certificate files and rerun `configure-tls` to reload them.
Updating public browser HTTPS does not change the carrier identity or enrollment.

Default inbound ports:

| Components | TCP | UDP |
| --- | --- | --- |
| Claim API (all installations) | 8443 | — |
| Relay only | 443 | 443 (STUN), 27446 (QUIC) |
| Tunnel only | 80, 443, 27443 | 27444 |
| Both | 80, 443, 27443, 27445 | 27444, 27445 (STUN), 27446 |

A connected process is not readiness. Dashboard node status reports actual
control-plane registration, health, and tunnel ingress verification. The service
starts component runtimes after durable claim configuration and retries runtime
crashes. `pbh status` shows service status. `pbh uninstall` stops the service,
removes its owned containers and helper executables, and retains protected local
identity, credentials and configuration for reinstall. To permanently delete that
data, explicitly run `pbh uninstall --remove-data --confirm-remove-data`. Remove
the node in Network to revoke its registry entry. Docker and unrelated containers
are retained.

Release owners set `PAPERBOAT_SELFHOST_ADDRESS_URL` to the configured HTTPS
`/selfhost/address` endpoint before packaging. The helper binary embeds that
initial endpoint, so the installer preserves automatic public-address detection
without a domain literal in source. Runtime configuration can override it.
Build one required artifact with
`helper/deploy/package-selfhost.sh VERSION amd64 ABSOLUTE_IGNORED_BUILD_DIRECTORY`.
The release publisher must render `install.sh` with the configured immutable
architecture-specific `PACKAGE_URL_AMD64`, `PACKAGE_SHA256_AMD64`,
`PACKAGE_URL_ARM64`, and `PACKAGE_SHA256_ARM64` values. An unconfigured endpoint must
not claim an installation is available. Packages include Paperboat and upstream
Tailscale license notices.

Relay payloads remain end-to-end encrypted between endpoints. Public/browser HTTP
terminates TLS at the tunnel edge: its machine administrator can inspect plaintext
and connection metadata there. TLS pass-through preserves origin TLS and opaque
payloads but exposes SNI and connection metadata. Infrastructure TLS pins never
replace the certificates selected for published browser domains.

Keep encrypted backups of `/var/lib/paperboat-selfhost`; it contains the persistent
installation TLS key, single-use-code verifier, scope assignment, component runtime
credentials, and tunnel usage-signing key. Never start two machines from one backup.
The helper preserves identity and ownership on reinstall with the same settings.
Claim/carrier infrastructure certificates carry one-year validity. The helper
checks at startup and every day, renews within thirty days of expiry using the same
protected key, and restarts its runtime components to reload the replacement.
Failed renewal preparation leaves the previous certificate untouched and schedules
a retry. Runtime JWKS is loaded at
startup; issuer-key updates also require a controlled service restart. Existing
streams close on restart and are not replayed by the data-plane node.

The package builder has no embedded Sentry DSN. Runtime reporting is disabled unless
explicitly configured with `PAPERBOAT_SENTRY_ENABLED`, `PAPERBOAT_SENTRY_DSN`, and
`PAPERBOAT_SENTRY_RELEASE` in service environment. Set
`PAPERBOAT_SENTRY_ENABLED=false` to disable reporting. Runtime reporting excludes
private traffic, credential values, headers, URLs, raw errors, locals, and command
arguments; public browser traffic still has the edge trust boundary described above.

Container image references are supplied by the package build using
`PAPERBOAT_SELFHOST_RELAY_IMAGE` and `PAPERBOAT_SELFHOST_TUNNEL_IMAGE`; the
initial public-address API uses `PAPERBOAT_SELFHOST_ADDRESS_URL`. The installer
renderer supplies `DOCKER_INSTALL_URL` alongside package URLs and SHA256 values.
An operator can explicitly pass `--relay-image` and `--tunnel-image` to `pbh install`.
Image configuration is atomic and owner-only; the claim and certificate identity
remain owned by the shared installation supervisor.

After a claim, the launcher waits for the exact runtime credential's node to
report a new process epoch and a fresh ready heartbeat at
`GET /v1/selfhost/runtime-readiness`. A running container alone is insufficient.
Shutdown drains and removes only containers labelled for that installation and
keeps its protected state. A foreign container with a colliding name is refused.
No Docker daemon network configuration is changed when Docker is already present.

Re-run the installer to upgrade images. Existing roles, custom ports, domains,
region, name, TLS paths, claim and private identity are read from the protected
installation; they do not need to be repeated. Conflicting settings flags are
refused before the upgrade. Externally managed trust and certificate files are
mounted at their original absolute paths with read-only access.

For a claimed node, installation succeeds only after every owned container has a
new process epoch and a fresh authenticated ready heartbeat. A failed image
startup restores the previous image configuration and restarts it; the package
installer also restores its previous helper and unit on failure. Data is retained
for recovery. Successful Docker migration removes the retired host runtime
executables. The initial unclaimed installation verifies its pinned claim TLS
listener; runtimes start after authenticated enrollment.
