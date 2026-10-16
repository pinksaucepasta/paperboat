# paperboat-server

The control plane for Paperboat. It owns identity, billing and metering, enrolled-machine
lifecycle, config-repository assignment, authorization, and tunnel policy.

Live terminal, upload, and preview traffic does not pass through this service.
`paperboat-server` decides, authorizes, meters, and orchestrates.

## Development

The server is a Go service backed by PostgreSQL. Application SQL is generated with `sqlc` from `internal/db/queries`,
while Goose owns forward-only Postgres migration execution from `internal/db/migrations`.

The server requires real WorkOS credentials for browser authentication. Configure
`PAPERBOAT_WORKOS_CLIENT_ID` and `PAPERBOAT_WORKOS_API_KEY` (or their secret-file
equivalents) before starting it on the designated remote dev server. Fake WorkOS
verification is available only in tests.

Paperboat lowercases email addresses and removes the local part's first `+` suffix
for every domain. Dots and domain names are preserved. Verified provider identities
for these aliases sign into one account; invitations and contact addresses use the
same canonical email. Refresh and logout remain bound to each provider session.

Apply Postgres migrations:

```sh
PAPERBOAT_DATABASE_DSN='postgres://...' go run ./cmd/paperboat-server migrate -config config/local.example.json
```

Useful checks:

Install Go and `ripgrep`; the Makefile selects the pinned Go toolchain and `sqlc`
generator. Run these commands from the repository root.

```sh
curl -i http://127.0.0.1:8080/healthz
curl -i http://127.0.0.1:8080/readyz
go test ./...
go vet ./...
gofmt -w .
make check
```

PostgreSQL integration tests require a separate, disposable PostgreSQL 17 database.
Without `PAPERBOAT_TEST_DATABASE_DSN`, those tests are skipped by `make check`.
Create a database whose name ends in `_test`, then run, for example:

```sh
PAPERBOAT_TEST_DATABASE_DSN='postgres://localhost:5432/paperboat_test?sslmode=disable' make check
```

Use your local test credentials if authentication is required. Never point this variable
at an application database; integration tests can change or remove test data.

## Deployment

The repo includes a Compose-based production stack under `deploy/`. It runs one-shot
migration job and the backend server on loopback. PostgreSQL is
external: set `PAPERBOAT_DATABASE_DSN` to its connection string. Start from
`deploy/.env.example`, provide the required values and mounted secret files, then run
`docker compose --env-file deploy/.env -f deploy/docker-compose.yml up -d`.

Billing products are managed in Postgres through the operator billing API, which
verifies each Stripe price before writing it. Deployments do not import a billing
catalog file.

Release container tags use `YYYY.MM.DD.X`. Run `tools/release-version.sh next`, create
that exact tag without a `v` prefix, and push it.

The server image contains only `paperboat-server`. `PAPERBOAT_RELEASES_ROOT_PATH` is the
host release root and defaults to `/opt/paperboat/releases`. Compose mounts that parent
directory read-only at `/srv/paperboat-releases`; the server reads the active release from
the fixed Compose value `PAPERBOAT_RELEASE_DIRECTORY=/srv/paperboat-releases/current`.
Existing deployments may temporarily keep the legacy variable name
`PAPERBOAT_RELEASES_PATH`, but its value must be changed to the parent
`/opt/paperboat/releases`, not the old `/opt/paperboat/releases/current` child. The
canonical variable is `PAPERBOAT_RELEASES_ROOT_PATH` and it takes precedence.

The release publisher validates the parent mount and atomically exchanges the complete
`current` directory only after staged consumer verification. Because the server resolves
files through `current` for every request, `/install` and `/tuf/` switch
together without restarting the container. Do not bind-mount the `current` child directly:
Docker would keep serving its old inode after an exchange. The server never receives TUF
signing keys or signing state. Release URLs use `PAPERBOAT_RELEASE_BASE_URL` (or the public
base URL when omitted).

The server exposes health/readiness, authentication, machine billing, machine, environment,
and config-repository APIs. See [docs/api.md](docs/api.md) and
[docs/contracts/http-api.md](docs/contracts/http-api.md) for the maintained interface.
P2P control-stream, endpoint-identity, regional-selection, and global-disable recovery is
defined in [docs/runbooks/p2p-control-plane.md](docs/runbooks/p2p-control-plane.md).

For direct server OAuth testing, set the GitHub OAuth app callback URL to:

```text
https://<public-base-url>/v1/github/oauth/callback
```

Run the server with:

```sh
PAPERBOAT_PUBLIC_BASE_URL='https://<public-base-url>' \
PAPERBOAT_GITHUB_BASE_URL='https://api.github.com' \
PAPERBOAT_GITHUB_CLIENT_ID='...' \
PAPERBOAT_GITHUB_CLIENT_SECRET='...' \
go run ./cmd/paperboat-server serve -config config/local.example.json
```

## Configuration

Configuration can come from defaults, a JSON file, environment variables, and secret-file
environment variables. Secret-file variables use the same name with `_FILE`, for example
`PAPERBOAT_ENCRYPTION_KEY_FILE`.

Common environment overrides:

- `PAPERBOAT_ENV`
- `PAPERBOAT_HTTP_ADDRESS`
- `PAPERBOAT_PUBLIC_BASE_URL`
- `PAPERBOAT_ALLOWED_ORIGINS`
- `PAPERBOAT_DATABASE_DRIVER`
- `PAPERBOAT_DATABASE_DSN`
- `PAPERBOAT_GITHUB_OAUTH_AUTHORIZE_URL`
- `PAPERBOAT_GITHUB_OAUTH_TOKEN_URL`
- `PAPERBOAT_GITHUB_OAUTH_SCOPES`
- `PAPERBOAT_GITHUB_CONFIG_REPO_NAME`
- `PAPERBOAT_GITHUB_CONFIG_REPO_BRANCH`
- `PAPERBOAT_SESSION_KEYS` or `PAPERBOAT_SESSION_KEYS_FILE`
- `PAPERBOAT_ENCRYPTION_KEY` or `PAPERBOAT_ENCRYPTION_KEY_FILE`
- `PAPERBOAT_WORKOS_API_KEY` or `PAPERBOAT_WORKOS_API_KEY_FILE` (also the AuthKit code-exchange secret)
- `PAPERBOAT_WORKOS_CLIENT_ID` or `PAPERBOAT_WORKOS_CLIENT_ID_FILE`

Browser sessions require both values and the existing server encryption key. The
server encrypts WorkOS refresh credentials in PostgreSQL, refreshes them before
the WorkOS access token expires, and rejects older sessions that have no refresh
credential. A confirmed WorkOS refresh revocation ends the Paperboat session;
temporary provider failures deny protected requests until retry succeeds.
- `PAPERBOAT_STRIPE_API_KEY` or `PAPERBOAT_STRIPE_API_KEY_FILE`
- `PAPERBOAT_STRIPE_WEBHOOK_SECRET` or `PAPERBOAT_STRIPE_WEBHOOK_SECRET_FILE`
- `PAPERBOAT_GITHUB_CLIENT_ID` or `PAPERBOAT_GITHUB_CLIENT_ID_FILE`
- `PAPERBOAT_GITHUB_CLIENT_SECRET` or `PAPERBOAT_GITHUB_CLIENT_SECRET_FILE`
- `PAPERBOAT_RELEASES_ROOT_PATH` (Compose host release root; legacy `PAPERBOAT_RELEASES_PATH` is a parent-path-only fallback)
- `PAPERBOAT_RELEASE_DIRECTORY` (fixed to `/srv/paperboat-releases/current` by Compose; configurable only for non-Compose deployments)
- `PAPERBOAT_RELEASE_BASE_URL`
- `PAPERBOAT_PREVIEW_BASE_DOMAIN`
- `PAPERBOAT_TUNNEL_BASE_DOMAIN`
- `PAPERBOAT_DIAGNOSTICS_OBJECT_ENDPOINT`
- `PAPERBOAT_DIAGNOSTICS_OBJECT_REGION`
- `PAPERBOAT_DIAGNOSTICS_OBJECT_BUCKET`
- `PAPERBOAT_DIAGNOSTICS_FORCE_PATH_STYLE`
- `PAPERBOAT_DIAGNOSTICS_RETENTION`
- `PAPERBOAT_DIAGNOSTICS_ACCESS_KEY` or `PAPERBOAT_DIAGNOSTICS_ACCESS_KEY_FILE`
- `PAPERBOAT_DIAGNOSTICS_SECRET_KEY` or `PAPERBOAT_DIAGNOSTICS_SECRET_KEY_FILE`
- `PAPERBOAT_RECEIPT_EMAIL_ENDPOINT` — HTTPS endpoint for the existing email delivery service
- `PAPERBOAT_RECEIPT_EMAIL_FROM` — fixed sender address for successful Team Inbox receipts
- `PAPERBOAT_RECEIPT_EMAIL_TOKEN` or `PAPERBOAT_RECEIPT_EMAIL_TOKEN_FILE`

Paperboat delivery is the default. A team owner or admin can instead configure that
team's SMTP host, encrypted connection (STARTTLS on 587 or implicit TLS on 465), username,
password, and From address through the Team Inbox dashboard/API. SMTP passwords are
encrypted with `PAPERBOAT_ENCRYPTION_KEY`, never returned by reads, and removing the team
configuration immediately restores Paperboat delivery. Only public DNS SMTP endpoints are
accepted at delivery time; private, loopback, link-local, and plaintext endpoints are denied.

Receipt delivery posts only `from`, `to`, a fixed subject, and a file count; it never
sends file names, paths, hashes, contents, machine labels, credentials, or download
authority. The request ID is sent as `Idempotency-Key`, and the delivery service must
return `{"message_id":"..."}`. Failed notification delivery is retried at most five
times and does not roll back or duplicate the completed file transfer.
- `PAPERBOAT_MINT_ACTIVE_KEY_ID`
- `PAPERBOAT_MINT_SIGNING_KEYS` or `PAPERBOAT_MINT_SIGNING_KEYS_FILE` as comma-separated
  `kid:base64url` Ed25519 seeds/private keys. Publish the previous key alongside the active
  key for the configured `PAPERBOAT_MINT_JWKS_MAX_AGE` rotation overlap.


Postgres tables live in the dedicated `paperboat` schema. The migration policy is
forward-only for production releases. Pre-release databases start from the single
`001_baseline.sql` migration. The runner refuses an older, nonempty schema; create a
fresh database before using this migration set. The former dev database was backed
up before its authorized reset, and the release-readiness plan records recovery.

## License

MIT. See [LICENSE](LICENSE).

### Browser session policy

The `browser_sessions` object in `config/local.example.json` contains startup session
policy; JSON durations use nanoseconds, like the existing duration fields. Environment
overrides accept Go duration strings:

| Environment variable | Default |
| --- | --- |
| `PAPERBOAT_BROWSER_SESSIONS_LOGIN_LIFETIME` | `720h` |
| `PAPERBOAT_BROWSER_SESSIONS_LOGIN_ROTATE_BEFORE` | `168h` |
| `PAPERBOAT_BROWSER_SESSIONS_RESOURCE_ABSOLUTE_LIFETIME` | `12h` |
| `PAPERBOAT_BROWSER_SESSIONS_RESOURCE_IDLE_LIFETIME` | `30m` |

Durations must be positive whole seconds. Rotation must precede login expiry; resource
idle expiry cannot exceed its absolute lifetime. Restart to apply policy to subsequent
issuance/renewal. Existing stored absolute deadlines remain intact; revoke sessions
explicitly when immediate invalidation is required. Resource access remains capped by
the actual parent login session and current resource authority.

Team activity retains its fixed 90-day audit-integrity contract. Billing usage feedback
and seat-worker cadence remain internal policies rather than exposed tuning knobs.

## Support references

Each operation has one user-facing PB support reference. The server provides audited
staff lookup and account-safe status APIs, with Sentry reporting disabled by default.
See [configuration, privacy, retention and rollout](docs/runbooks/support-references.md).

### Supplemental STUN discovery

`network.stun_servers` (or comma-separated `PAPERBOAT_NETWORK_STUN_SERVERS`)
optionally configures up to eight distinct public `host:port` destinations.
It defaults to empty. The control plane signs this discovery list in the
endpoint-bound `paperboat.regional-candidates.v1` document. Endpoints remove
it when that document expires; it never grants relay or application access.
The registered Paperboat relays continue supplying STUN and relay fallback.

Multiple discovery destinations can expose NAT mappings missed by one server.
Use operator-supported services or owned STUN listeners; do not pin addresses
from another service's relay inventory. Public services see the requesting
public IP and STUN metadata, but carry no terminal, file, or application traffic.
Cloudflare documents `stun.cloudflare.com:3478` as public STUN at
https://developers.cloudflare.com/realtime/sfu/get-started/connection-patterns/.
Configure destinations explicitly according to the deployment's privacy policy;
empty or unreachable supplemental discovery does not authorize public relays.
