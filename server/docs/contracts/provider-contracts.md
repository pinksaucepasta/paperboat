# Provider and Catalog Contracts

Status: implemented contract baseline, pending production provider values.

## Dynamic Catalog Rule

Billing products, endpoint URLs, domain names, webhook mappings, and feature switches
must be dynamic configuration or database seed records.
They must not be hardcoded into production logic.

## WorkOS

Purpose:

- Dashboard authentication.
- Mapping WorkOS identities to Paperboat users.
- Session creation for dashboard-origin browser access.

Approved baseline:

- WorkOS organization/environment identifiers are deployment configuration.
- Redirect/callback URLs for local, preview, staging, and production are provider
  configuration and must point to the configured Paperboat public base URL.
- Dashboard origin allowlist is `http.allowed_origins`; production must set only the
  deployed dashboard origins.
- Session idle expiry and absolute expiry are dynamic session configuration values.

Implementation constraints:

- Store WorkOS subject and identity metadata needed for idempotent user lookup.
- Never log identity provider tokens.
- Callback handling writes audit events.

## Stripe workspace subscriptions (Task 36.9)

Stripe replaces Polar for the new personal/team subscription model. Every personal
account and team has its own Stripe customer. An initiating administrator is not
the owner of a team's subscription. Current Owner/Admin authority is checked for
checkout and portal access, including operation replays; ordinary members can read
their workspace's subscription state. Personal Pro is never included in a team seat.

Checkout uses Stripe subscription quantities: one personal subscription or the
team's authoritative active membership count. A bounded recurring reconciliation
updates team quantity after membership changes; the client cannot supply quantities.
Checkout operation metadata associates a subscription with its authorized workspace.
Signed webhooks trigger a fresh provider read, rather than applying arrival order
as subscription order. Duplicate receipts do not mutate another subscription.

The adapter pins Stripe API `2026-09-30.endive`. Configure test credentials through
`PAPERBOAT_STRIPE_API_KEY_FILE` and `PAPERBOAT_STRIPE_WEBHOOK_SECRET_FILE`; the default
endpoint is `https://api.stripe.com`. Live keys are rejected before an HTTP request.
Portal sessions require `PAPERBOAT_STRIPE_PERSONAL_PORTAL_CONFIGURATION_ID` and
`PAPERBOAT_STRIPE_TEAM_PORTAL_CONFIGURATION_ID`. Each scope uses its own product
and monthly/yearly prices, immediate proration, and no portal quantity changes.
Retries retain the original configuration from the encrypted operation request.
The webhook is `POST /v1/webhooks/stripe`. Prices come from configured Stripe price
IDs; the example catalog is inactive until configured. Free and Pro have the same personal features. Allowance checks, usage accounting
and quota enforcement remain implemented. Configure optional nonnegative
`billing.personal_allowance_bytes`, `billing.team_allowance_bytes`, and
`billing.personal_machine_limit` through the server config; omission is unlimited,
not zero. An optional catalog `allowance_bytes` overrides the default for a paid
product. Period allowance snapshots apply to each new accounting period; changing
configuration does not rewrite already recorded period allowances. Admission creates
the period before checking it, so an explicit zero blocks the first request. Personal
machine quota checks serialize concurrent enrollment. Bandwidth exhaustion stops new
admissions and closes affected active streams when signed usage receipts confirm it;
asynchronous reporting permits in-flight overshoot, not an exact byte ceiling. Usage
still records all observed bytes, and a new period permits fresh admission. Real Stripe
acceptance still requires provider test setup.

See [Stripe quantities](https://docs.stripe.com/billing/subscriptions/quantities),
[webhook delivery](https://docs.stripe.com/webhooks), and
[billing tests](https://docs.stripe.com/billing/testing).

## GitHub

Purpose:

- Optional user OAuth connection for authorized GitHub configuration repositories.
- Private per-user config repo provisioning.

Approved baseline:

- OAuth app identity is deployment/provider configuration.
- OAuth scopes are `github.oauth_scopes`; production must use the minimum scope set that
  supports private configuration repository provisioning and sync.
- Config repo name is `github.config_repo_name`.
- Token material is encrypted, validated before sensitive GitHub operations, and marked
  unusable when provider validation/refresh fails.
- Config-sync repository access uses short-lived credentials bound to the current
  repository, assignment, environment, machine installation generation and operation.
  User-facing APIs never return provider credentials.

Implementation constraints:

- Token material is encrypted.
- Private config repo provisioning is idempotent.
- The `pb` config worker owns ongoing config-sync pull/apply/commit/push behavior and obtains
  scoped repository access through its authenticated control-plane identity.
- The server owns repository authorization, assignment eligibility, scoped-access
  issuance/revocation, and writer leases; it never proxies configuration bytes.

## Dynamic Catalogs

These catalogs contain environment-specific product and provider data:

- `regions`
- `billing_products`
- `feature_flags`

Billing products are managed in Postgres through the operator API. The API
verifies Stripe prices before activation; deployment files must not rewrite
product mappings. Other provider-backed catalog sources must be environment-specific
where values differ across local, staging, and production.
