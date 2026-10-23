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
| `billing` | Billing admin: plan, seats, top-ups and Manage billing (invoices, billing details). |
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
- `billing.manage`: plan, seats, top-ups, Manage billing (Stripe portal).
- `team.own`: delete the team, transfer ownership.

The matrix is served by the API at `GET /v1/team-roles`. Change a role with
`pb team role <team> <account> <admin|billing|auditor|member>` or in the console Members list.
In the workspace policy, `role:billing` and `role:auditor` name the people holding those roles;
`role:member` is every active member, whatever their role.

## Single sign-on (SAML and OIDC)

Teams with at least `business.sso_min_seats` seats (10) can sign their members in through their
company identity provider: Okta, Microsoft Entra ID, Google Workspace, OneLogin, JumpCloud or any
SAML 2.0 or OpenID Connect provider. Paperboat uses WorkOS for this; the company's IT admin sets
it up in the WorkOS Admin Portal, which has a step-by-step guide for every provider. Paperboat
never sees SAML metadata or provider credentials.

**Set up** (owners and admins, console team **Security**, or `pb team sso setup`):

1. **Verify your domain.** Open the Admin Portal link for domain verification and add the DNS TXT
   record it shows (e.g. for `example.com`). A domain belongs to one team.
2. **Connect your identity provider.** Open the SSO link and follow the guide for your provider.
3. **Sign in through SSO.** From then on, anyone signing in with an email in a verified domain is
   sent to your provider, from the console and from `pb login` alike. Signing in from the
   provider's app launcher (IdP-initiated) works too.

**Require SSO.** When turned on, members reach the team (console, API, CLI, API tokens, new
connections) only after an SSO sign-in within the team's **SSO session length** (default 7 days,
1–30). Without one, requests get `403 team_sso_required`; the member signs in again with their
work email. Connections already open keep running. While Require SSO is on, invitations go only
to addresses in verified domains. Before turning it on, Paperboat lists the members who would lose
access until they sign in through SSO; you confirm that list.

**Break-glass.** The owner, and up to `business.sso_exempt_max` (5) members marked **SSO exempt**,
keep access without SSO, so a broken provider never locks the team out; the owner can always turn
Require SSO off. Each access without SSO is recorded (`team.sso_bypassed`, once a day per person).

**JIT provisioning** (off by default): people with an email in a verified domain who sign in
through the team's SSO become members automatically (a seat is added).

## Directory sync (SCIM)

Connect your directory from the same Security page (`pb team scim setup`). Paperboat then follows
it within seconds:

- Active directory users become members. Someone without a Paperboat account gets a seat held
  for them as a directory invitation (role from their groups) and becomes a member at their
  first sign-in with that email. The invitation has the normal invitation lifetime; every sync
  renews it while they stay in the directory and updates its role when their groups change.
- **Group → role:** map directory groups to `admin`, `billing`, `auditor` or `member`
  (`pb team scim map <group> <role>`). Someone in several mapped groups gets the highest role
  (admin > billing > auditor > member); everyone else is a member, or is left out when
  **Only provision mapped groups** is on.
- Members the directory provisioned are **managed by the directory**: their role and membership
  change only there (`409 directory_managed` otherwise).
- **Deprovisioning:** a user deleted or suspended in the directory (or removed from every mapped
  group, with Only provision mapped groups) is removed from the team exactly like `pb team
  remove`: their open sessions end at once, their team API tokens stop working and their shared
  machines leave the team. Their own Paperboat account and personal workspace stay theirs. The
  owner is never removed by the directory.
- Seats are added automatically when the directory adds people (a normal seat change).
- Disconnecting the directory keeps everyone; they become ordinary members.

Every provisioning step is in the audit log with the actor `directory`.

### WorkOS dashboard settings (Paperboat operators)

Done once in the WorkOS dashboard, never by code: SSO and Directory Sync enabled for the
environment; AuthKit Single Sign-On enabled; Admin Portal domain verification allowed and its
return URL set to the console's `/team` page; the server's API key may use organizations, Admin
Portal, SSO, Directory Sync and Events.

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

### Machine access

Every start of terminal, exec, managed SSH and file access is `machine.access_started` (details:
`capability`, `capabilities`, `access_session_id`), and every refusal is `machine.access_denied`
with `result: denied` and a `reason`, at most once a minute per account, machine, capability and
reason. Both go to the log of the workspace that decided the access: a member's access to a
machine shared into a team is in the team's log, not in the machine owner's personal log. Reasons
use the names of `pb policy explain`, checked in this order:

| Reason | Why |
|---|---|
| `outside_token_scope` | The API token used is limited to other machines or capabilities. |
| `team_sso_required` | The team requires SSO and the member has no recent SSO sign-in. |
| `machine_not_in_workspace` | Not a member, the team is suspended or unpaid, or the machine is not in the workspace. |
| `policy` | The workspace policy does not allow this capability; `details.policy_version` is the version that decided (`pb policy explain --version`). |
| `not_found` | No such machine. |

Private access (ports) is `private_access.allowed` / `private_access.denied`.

### Event catalog

Every action is `<namespace>.<verb>`. The namespaces, which a test keeps equal to the server:

<!-- event-catalog:start -->
| Namespace | Events about |
|---|---|
| `api_token` | API tokens created and revoked. |
| `auth` | Sign-in, sign-out, CLI device approval and revocation. |
| `billing` | Plan, seats, top-ups, Customer Portal sessions, bandwidth exhaustion. |
| `client_update_policy` | Paperboat operations: the CLI update policy. |
| `config` | Config sync: assignments, consents, conflicts, credentials. |
| `connector` | Tunnel connectors: enrollment, credentials, draining. |
| `control` | Paperboat operations: recovered control operations. |
| `backup` | Paperboat operations: database backup reports. |
| `diagnostic_upload` | Support diagnostics uploaded by a user. |
| `domain` | Custom domains: created, verification, deleted. |
| `edge` | Paperboat operations: edge nodes and usage keys. |
| `environment_manifest` | ENV manifests (never values). |
| `github` | GitHub connection and config repository. |
| `helper` | Self-hosted helper enrollment and replacement. |
| `machine` | Machine setup and access: `machine.access_started`, `machine.access_denied`. |
| `managed_ssh` | Managed SSH targets and host keys. |
| `mint` | Paperboat operations: credential signing keys. |
| `operation` | Preview and tunnel operations completed or cancelled. |
| `operator` | Paperboat operations: staff actions on an account. |
| `operator_reporting` | Paperboat operations: staff reports read. |
| `peer_endpoint` | Peer certificates and enrollments of machines and the CLI. |
| `peer_network` | Peer network keys and certificates. |
| `personal` | The personal workspace policy (`personal.policy.updated`, …). |
| `preview` | Previews created, ready, renewed, stopped. |
| `private_access` | Private access to machine ports: grants, allowed, denied. |
| `release_authority` | Paperboat operations: release signing authority. |
| `route` | Tunnel routes and their intents. |
| `support` | Support references looked up. |
| `team` | Team membership, invitations, roles, sharing, grants, policy, SSO, directory sync, audit export and streaming. |
| `terminal` | Terminal browser access and access codes. |
| `transfer_destination` | Default and per-session file transfer destinations. |
| `tunnel` | Tunnels: created, updated, paused, domains, connectors. |
| `user_machine` | Machines: enrollment, approval, renames, availability, removal. |
<!-- event-catalog:end -->

Namespaces marked "Paperboat operations" are about running the service and are never in a
workspace log.

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

### Streaming to a SIEM (Team plan)

Owners and admins (`audit.stream`) send the team audit log to their SIEM as it happens; at most
one destination of each kind (`GET/PUT/DELETE /v1/teams/<team>/audit-streams/{https|s3}`,
console Activity → Streaming). A new stream starts with events after it was set up. Events are
delivered at least once and in order (use `id` to drop duplicates), within a few seconds and at
most 30 seconds after they happen.

**HTTPS** (`url`, optional `header_name` / `header_value`): Paperboat POSTs batches of up to
`business.audit_stream_batch_bytes` (1 MiB) as NDJSON, one event per line,
`Content-Type: application/x-ndjson`. The optional header carries your SIEM's credential, for
example `Authorization: Splunk <HEC token>` (Splunk HEC raw endpoint), `DD-API-KEY: <key>`
(Datadog logs), `Authorization: Bearer <token>` (Axiom, Cribl, Panther, Elastic). Any 2xx answer
counts as delivered.

Every request carries `Pb-Signature: t=<unix seconds>,v1=<hex>` where `v1` is
HMAC-SHA256 with your signing secret (shown once when you create the stream) over
`<t>.<request body>`. To verify: recompute it, compare in constant time, and reject a `t` older
than 5 minutes.

**S3-compatible** (`url` = endpoint such as `https://s3.eu-central-1.amazonaws.com` or your
Cloudflare R2, Backblaze B2 or MinIO endpoint, `region`, `bucket`, optional `prefix`,
`access_key_id`, `secret_access_key`): each batch is one gzip NDJSON object at
`<prefix>/team-<team>/YYYY/MM/DD/<first>-<last>.ndjson.gz` (path-style URL, SigV4). The key only
needs permission to put objects into that prefix.

Destinations must be on the public internet over https. On failure Paperboat retries with
exponential backoff up to `business.audit_stream_max_backoff` (1 hour); events wait until
delivered or until their retention ends. The stream status shows the last delivery, the last
error and how many events wait. After `business.audit_stream_alert_after` (1 hour) of failures
the owners and admins get one email, and `team.activity_stream_failing` is recorded. **Send test
event** (`POST .../test`) delivers one `team.activity_stream_test` event right away.

## Billing

The owner and billing admins (`billing.manage`) of a team, and the owner of a personal workspace,
open **Manage billing** in the console Billing page. It is the Stripe Customer Portal: invoices
and receipts (view, download, pay), payment methods, and the billing email, company name, address
and tax ID printed on every later invoice. Stripe stores all of it; Paperboat keeps no copy.

- **Tax:** Checkout asks companies for their tax ID, name and address. Stripe Tax computes VAT and
  sales tax only when the server setting `business.stripe_tax` (environment
  `PAPERBOAT_BILLING_STRIPE_TAX=true`) is on; it is turned on once the business has tax
  registrations.
- **Pay by invoice:** set per customer in the Stripe dashboard (Customer → subscription → payment
  method "Email invoice"), not in Paperboat. Stripe emails each invoice with a page to pay by card
  or bank transfer. An invoice not paid by its due date starts the same 7-day grace as a failed
  card payment, after which the team's paid features pause until it is paid.
