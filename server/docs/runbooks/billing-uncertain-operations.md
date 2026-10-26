# Billing uncertain operations

Current personal/team billing records operations in `billing_workspace_operations`.
A timeout or provider error can follow a successful mutation. Keep the original
idempotency key, actor and workspace; retries revalidate current authority. Request
and result payloads are encrypted. Never copy credentials, payment data, decrypted
payloads or checkout/portal URLs into incident records.

Checkout/portal retries reuse their original request and key within the 24-hour
safe retry window. Fresh pending operations remain exclusive; stale pending calls
can be recovered after one minute. The dashboard retains opaque keys across reload.
Open checkout sessions resume; verified expiry permits a new checkout. Completed
sessions require authoritative subscription reconciliation. Active and recoverable
subscriptions use the portal rather than a second purchase.

Seat recovery reads Stripe state before retrying. It resolves the original target
quantity before newer membership changes and does not repeat an applied update.
After 24 hours, uncertain mutations require operator reconciliation; a new key
must not bypass them.

## Administrator recovery

Use an active billing or admin operator account with its operator session and CSRF
token. Each endpoint is under
`POST /v1/admin/billing/workspace-operations/{operation_id}/`, where operation_id
is the original operation key. Send `Operator-Target` (the operation ID), an opaque
`Operator-Reason` and a new `Idempotency-Key` for the recovery (8-128 characters), and
reuse the key for exact retries; success is 204. The service rechecks current operator authority.
Evidence references are opaque incident/provider references; only their hashes
are stored and audited. Conflicting requests are rejected.

- `reconcile-checkout`: body `{"provider_session_id":"cs_…","evidence_reference":"case:…"}`.
  The server retrieves and verifies the session's customer, operation, mode, price
  and quantity. Open sessions restore the original encrypted result; complete
  sessions reconcile authoritative subscription state; expired sessions terminate
  the old checkout. An asserted success or supplied redirect URL is never accepted.
- `supersede-portal`: body `{"evidence_reference":"case:…"}`.
  Audits supersession of an expired uncertain portal operation without claiming
  that its creation failed or changing a subscription. The user can create a new
  portal session afterward.
- `retire`: body `{"outcome":"not_applied","evidence_reference":"case:…"}`.
  Only use when provider evidence proves the expired uncertain operation was not
  applied. Retirement permits a fresh operation; seat retirement also advances the
  workspace revision so the worker gets a fresh key.

Never edit operation rows manually or fabricate webhook events. Failed original
keys remain terminal (`billing_operation_failed`); the dashboard clears that key
so a deliberate new attempt can proceed.

These paths have PostgreSQL and controlled-provider coverage. Real Stripe object
reconciliation and connected browser payment recovery remain unverified until test
credentials, products, prices and webhook configuration are available.

References: [Stripe idempotency](https://docs.stripe.com/api/idempotent_requests),
[Checkout retrieval](https://docs.stripe.com/api/checkout/sessions/retrieve),
[low-level errors](https://docs.stripe.com/error-low-level), and
[webhooks](https://docs.stripe.com/webhooks).
