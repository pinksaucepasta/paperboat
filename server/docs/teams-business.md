# Teams for business: admin reference

Reference for team owners and admins, and for agents working on Paperboat (PLAN.md F19). It is
written from the code; tests keep the tables below equal to it. Sections are added as each part
ships.

## Roles

Every team member has exactly one role. Roles are **administrative permissions only**: using
machines, ENV, previews and tunnels is decided by the workspace policy (`pb policy`, console
Access), never by a role. Every role takes a seat.

| Role | Who it is for |
|---|---|
| `owner` | The one person who owns the team. Transfer with `pb team transfer`. |
| `admin` | Runs the team: members, settings, machines, policy, security, audit log. |
| `billing` | Billing admin: plan, seats, top-ups, invoices, billing details. |
| `auditor` | Read-only: sees everything an admin sees and reads/exports the audit log. |
| `member` | Everyone else. |

### Permission matrix

<!-- roles-matrix:start -->
| Permission | owner | admin | billing | auditor | member |
|---|---|---|---|---|---|
| `team.read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `members.manage` | ✓ | ✓ |  |  |  |
| `settings.manage` | ✓ | ✓ |  |  |  |
| `security.manage` | ✓ | ✓ |  |  |  |
| `audit.read` | ✓ | ✓ |  | ✓ |  |
| `audit.stream` | ✓ | ✓ |  |  |  |
| `billing.manage` | ✓ |  | ✓ |  |  |
| `team.own` | ✓ |  |  |  |  |
<!-- roles-matrix:end -->

- `team.read`: see the team, members, machines, settings and policy.
- `members.manage`: invite and remove members, change roles. Admins manage only people
  without `members.manage` and never make someone an admin; only the owner makes or removes admins.
- `settings.manage`: team name, machine approval, private access, team machines, resource grants.
- `security.manage`: SSO, verified domains, Require SSO, SSO exemptions, directory sync.
- `audit.read`: search and export the audit log, see streaming status.
- `audit.stream`: configure audit log streaming.
- `billing.manage`: plan, seats, top-ups, invoices, billing details, pay by invoice.
- `team.own`: delete the team, transfer ownership.

The matrix is served by the API at `GET /v1/team-roles`. Change a role with
`pb team role <team> <account> <admin|billing|auditor|member>` or in the console Members list.
In the workspace policy, `role:billing` and `role:auditor` name the people holding those roles;
`role:member` is every active member, whatever their role.

## Audit log

Every workspace has an audit log: console **Activity**, `pb team activity`, and the API
`GET /v1/activity?workspace=<personal|team>`. In a team, the owner, admins and auditors read it
(`audit.read`); in a personal workspace, its owner.

### Event

The same shape in the API, the export and the stream:

| Field | Meaning |
|---|---|
| `id` | Stable event ID. |
| `occurred_at` | When it happened (UTC, RFC 3339). |
| `workspace` | `personal:<account>` or `team:<team>`. |
| `action` | What happened, `<area>.<verb>`, e.g. `team.role`, `user_machine.renamed`, `private_access.denied`. |
| `actor.kind` | `user`, `api_token` (a user acting through an API token), `directory` (directory sync), `operator` (Paperboat staff), `system`. |
| `actor.id`, `actor.email` | The account and its email when the event happened. |
| `actor.token_id` | The API token used, when one was. |
| `target.kind`, `target.id`, `target.name` | What it happened to, with its name at that time. |
| `machine` | The machine involved, if any, with its name at that time. |
| `result` | `success` or `denied`. |
| `reason` | For denials, a stable code (e.g. `team_sso_required`). |
| `ip`, `user_agent` | Where a signed-in request came from. |
| `request_id` | Matches the `Request-Id` response header and support references. |
| `details` | Documented keys only (role, permission, capabilities, format, …). Never contents, ENV values or secrets. |

### Retention

Events are kept by the workspace's plan: Free 30 days, Pro 90 days, Team 365 days (server config
`business.audit_retention_*`). Nothing can change an event, and nothing but the retention job can
delete one; no event younger than 30 days can be deleted at all.

### Search and filter

Query parameters (all optional, each at most once): `actor` (account ID, email or API token ID),
`action` (exact, or a prefix: `team` matches `team.*`), `target` (resource or machine ID),
`result` (`success`/`denied`), `since` and `until` (RFC 3339), `q` (text in the action, target
name or actor email), `area`, `machine_id`, `limit` (1–200) and `cursor` (from `next_cursor`).

### Export (Team plan)

`GET /v1/activity/export?workspace=<team>&format=csv|json` with the same filters downloads every
matching event, newest first, up to `business.audit_export_max_rows`. CSV columns: `id,
occurred_at, workspace, action, result, reason, actor_kind, actor_id, actor_email,
actor_token_id, target_kind, target_id, target_name, machine_id, machine_name, ip, user_agent,
request_id, details` (details as JSON; cells starting with `= + - @` are prefixed with `'` so
spreadsheets never run them). JSON is one array of events. Each export is itself recorded as
`team.activity_exported`.

## Invoices and billing details

The owner and billing admins (`billing.manage`) of a team, and the owner of a personal workspace,
see and download invoices and edit billing details in the console Billing page. Stripe stores all
of it; Paperboat keeps no copy.

- **Invoices:** `GET /v1/billing/workspaces/{personal|teams/<team>}/invoices` lists finalized
  invoices newest first (24 per page, `cursor` = `next_cursor`), each with its number, period,
  total, tax, status, due date, a page to view and pay it (`hosted_url`) and the PDF (`pdf_url`).
- **Billing details:** `GET`/`PUT .../details` with `email` (where invoices and receipts go),
  `name` (company or person), `address` (country as a two-letter code, required with any address
  line) and one `tax_id` (`type` is a Stripe tax ID type such as `eu_vat`, `gb_vat`, `us_ein`;
  `null` removes it). Stripe checks the tax ID and prints the name, address and tax ID on every
  later invoice. Changes are recorded as `billing.details_updated` (without the values).
- **Tax:** Stripe Tax computes VAT and sales tax only when the server setting
  `business.stripe_tax` (environment `PAPERBOAT_BILLING_STRIPE_TAX=true`) is on; it is turned on
  once the business has tax registrations.

## Pay by invoice

Teams on the yearly Team plan with at least `business.invoice_min_seats` seats (10) can pay by
invoice instead of card: `GET`/`PUT /v1/billing/workspaces/teams/<team>/pay-by-invoice`
(`{"pay_by_invoice": true}`), or the console Billing page. The first purchase is by card; the
switch applies from the next invoice. Stripe then emails each invoice to the billing email, due
`business.invoice_days_until_due` days (30) after it is issued, with a page to pay by card or bank
transfer; seat changes are invoiced the same way. An invoice not paid by its due date starts the
same 7-day grace as a failed card payment, after which the team's paid features pause until it is
paid. Turning pay by invoice off returns to automatic card payment. Changes are recorded as
`billing.pay_by_invoice_changed`.
