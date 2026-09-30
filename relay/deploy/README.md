# Install a self-hosted node

A Linux machine with systemd and public inbound connectivity is required. Install
first; no account, team, or global scope is assigned by installation.

```sh
curl -fsSL https://get.pprbt.dev/selfhost/install | sh
# Choose tunnel or both instead of the default relay:
curl -fsSL https://get.pprbt.dev/selfhost/install | sh -s -- --enable tunnel
curl -fsSL https://get.pprbt.dev/selfhost/install | sh -s -- --enable both
```

The installer downloads the configured immutable package, verifies its SHA256,
installs `pbh` and the runtime binaries, and enables a protected system service.
An optional `--endpoint-host` accepts a public IP or DNS name; otherwise the
installation service detects the request's public IP. No domain is required to
install or claim. Browser/public routes need their own domain configuration in
the dashboard before publication.

Paste the printed claim address and expiring code into **Dashboard → Network →
Add self-hosted node**, then choose the owning personal workspace, team, or
operator global pool there. The bootstrap TLS key is pinned by the code; ownership
is assigned only by the authenticated control-plane claim. The code lasts fifteen
minutes, is single-use, and contains no account or team identity. Treat it as a
secret. Generate a fresh unclaimed code with `pbh selfhost code`.

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
crashes. `pbh status` shows service status. `pbh uninstall` stops the service and
removes protected local state; remove the node in Network to revoke its registry
entry. The helper executable stays available for a future install.

Release owners build one required artifact with
`relay/deploy/package-selfhost.sh VERSION amd64 ABSOLUTE_IGNORED_BUILD_DIRECTORY`.
The install endpoint renders `install.sh` with the configured immutable
`PACKAGE_URL`, `PACKAGE_SHA256`, and `PACKAGE_ARCH`. An unconfigured endpoint must
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
Infrastructure certificates carry a one-year validity. The helper checks at startup
and every day, renews within thirty days of expiry using the same protected key,
and restarts its runtime components to reload the replacement. Failed renewal
preparation leaves the previous certificate untouched and schedules a retry. Runtime JWKS is loaded at
startup; issuer-key updates also require a controlled service restart. Existing
streams close on restart and are not replayed by the data-plane node.

The package builder has no embedded Sentry DSN. Runtime reporting is disabled unless
explicitly configured with `PAPERBOAT_SENTRY_ENABLED`, `PAPERBOAT_SENTRY_DSN`, and
`PAPERBOAT_SENTRY_RELEASE` in service environment. Set
`PAPERBOAT_SENTRY_ENABLED=false` to disable reporting. Runtime reporting excludes
private traffic, credential values, headers, URLs, raw errors, locals, and command
arguments; public browser traffic still has the edge trust boundary described above.
