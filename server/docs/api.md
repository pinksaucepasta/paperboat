# Paperboat Server API

The API is contract-first. Its schemas are maintained in
[`docs/openapi.json`](openapi.json); CI extracts registered methods and paths from the
router and requires matching OpenAPI operations. Focused contract tests additionally
verify authentication and important request and response schemas.

This API is the control-plane contract for dashboard and CLI clients. It authorizes,
meters, and orchestrates resources; it does not proxy terminal, preview, or WebSocket
data.

## Response Envelope

Success:

```json
{
  "data": {}
}
```

Error:

```json
{
  "error": {
    "code": "machine_not_ready",
    "message": "Machine is not ready for connection.",
    "support_reference": "support_550e8400-e29b-41d4-a716-446655440000",
    "request_id": "req_...",
    "details": {}
  }
}
```

Every response includes a `Request-Id` header. Browser writes use the session cookie
plus CSRF protection. Mutations use the idempotency and expected-generation/ETag
requirements of their current operation in [OpenAPI](openapi.json).

Preview and tunnel v1 responses also include `Correlation-Id`. Their typed errors add
component, outcome certainty, retryability, and a safe repair action. Every create uses
`Idempotency-Key`; mutable-resource writes use a strong ETag and require `If-Match` when
a stale write could overwrite another actor. Lists use bounded keyset pagination and
events resume from an opaque account-and-resource-bound cursor.

`GET /v1/operations/{operation_id}` returns typed long-running progress. Safe pending
work can be canceled with idempotent `DELETE /v1/operations/{operation_id}`. Work that
has crossed its cancellation boundary returns `operation_not_cancellable` without
claiming that its outcome is known.

## Dashboard and CLI access

Machine inventory and enrollment use `/v1/machines` and `/v1/machines/setup`.
Incoming services are governed by `/v1/machines/{machine_id}/capabilities`;
every machine can still initiate authorized outgoing access. Enrollment installs one
runtime with incoming terminal/exec, managed SSH, file receiving and preview/tunnel
capabilities enabled by default. Peer relay is explicitly opt-in.

CLI sign-in uses the device authorization and rotating client-session contract in
[CLI authorization](contracts/cli-authorization.md). Machine reads require
`machines:read`; native connection descriptors and readiness require `machines:connect`.
Browser approval and dashboard installation tokens are approval methods for the same
CLI session authority, token policy, refresh and revocation system. Installation
bootstrap retains its machine setup behavior; interactive CLI login uses browser approval.
Browser cookies are not CLI credentials. Dashboard logout revokes its browser session;
CLI logout uses `/v1/auth/token/revoke`. Account suspension and client revocation fence
current access and propagate revocation to affected runtime sessions.

GitHub linking is optional configuration-repository authorization. Current repository
operations and TOML machine settings are defined in [configuration sync](contracts/config-sync.md).
Personal and team subscriptions use the workspace billing API described below.
The registered operations and request/response schemas are maintained in
[OpenAPI](openapi.json), with [HTTP contracts](contracts/http-api.md) describing resource
ownership, readiness, authorization and recovery.

## Preview and Tunnel Browser Viewer Policies

`GET|PUT /v1/browser-access/policies/{resource_kind}/{resource_id}/{route_id}` manages
the browser audience for one exact preview or tunnel route. Writes require the owner or
authorized resource manager, a browser session, CSRF protection, and `expected_generation`
(use `0` for the first save). Audiences are `owner`, `team`, `specific_emails`,
`email_domain`, or `public`; owners always retain access. Team viewers must be current
direct members of the selected team. Email and domain viewers need a verified current
WorkOS email; domain matching is exact after `@`. The policy grants browser viewing only
and does not change native access.

Changes revoke existing browser sessions. Native-public HTTP tunnel routes can switch
between public and restricted browser audiences. The saved tunnel policy reports
`serving_state`: `fencing` while an old public edge grant can still be used, `pending`
while the replacement edge assignment awaits readiness, and `ready` once the exact
mode and policy generation are serving. `PUT` returns `202` with `Retry-After: 1` while
serving is not ready; clients should reload the policy until it reports `ready`.
Preview policies report `not_applicable` because they do not use tunnel edge assignments.
Unsupported edge transitions return `409 edge_policy_transition_required` without saving.
Other stale-generation or owner changes also return `409` and require reloading the policy.

## Terminal Sessions

Each terminal-capable user machine has durable terminal sessions. Automatic names use
three independently random words from the shared disjoint 64-word lists and skip
retained names across the owner’s machines. Names and opaque session IDs are stable
identifiers. 
`POST /v1/machines/{machine_id}/connection-descriptor` accepts
`source_machine_id` plus either `terminal_session_id` or
`create_session: {name, idempotency_key, cwd?}`. Create-and-connect returns the
created durable session and descriptor together. Optional `cwd` is a canonical
absolute target-platform path of at most 1024 UTF-8 bytes; explicit CWD requires
an owned source machine matching the target. Omission uses the enrolled workspace
root. Replay returns the original session and directory, even if the retry supplies
another directory. Invalid paths return `400 invalid_terminal_session_cwd` before
creation. The host validates directory existence at launch and never substitutes
another directory on failure. Authorized source and target may be the same enrolled
machine; normal CLI authentication and exact resource authorization still apply.

`GET /v1/machines/{machine_id}/connection-readiness` accepts the same optional
`terminal_session_id` query parameter. CLI readiness polls and the final descriptor
re-broker therefore retain the selected session rather than reverting to `default`.
When any terminal close or closed-record deletion is awaiting Helper reconciliation, the endpoint
returns the retryable `helper_starting` / `terminal_session_operation_pending` state until
the cleanup is complete.

- `GET /v1/machines/{machine_id}/terminal-sessions` requires `machines:read` and accepts
  `limit` (1-200) and `offset`.
- `POST /v1/machines/{machine_id}/terminal-sessions` requires `machines:connect`, an
  `Idempotency-Key`, and an optional `{ "name": "api" }` body.
- `PATCH /v1/machines/{machine_id}/terminal-sessions/{session_id}` accepts
  `{ "name": "api-debugging" }`; only the session owner may rename it. The reserved
  default name cannot change. Renaming preserves the session ID and running process.
- `POST /v1/machines/{machine_id}/terminal-sessions/{session_id}/close` and `DELETE ...`
  apply helper work when the runtime is reachable. Delete is allowed only after the server
  has confirmed the process is closed; otherwise it returns `409 terminal_session_not_closed`
  and asks the owner to close and retry. `200` means helper work applied immediately; `202`
  means it remains pending.

Owner terminal inventories count and select durable records in one database snapshot,
so concurrent creation or deletion cannot produce contradictory pagination. Live runtime
metadata is fetched afterward and may be unavailable without hiding the selected record.

Session list records contain `id`, editable `name`, automatic application `title`, terminal-host `machine`,
stable launch directory `started_in`, `state`, nullable `attached_count`, terminal-I/O-only
`last_active_at`, `created_at`, `updated_at`, and optional `expires_at`. Open sessions omit
`expires_at`; once a non-default close is confirmed, it is 30 days after that close. Expired
closed rows are hidden, their server metadata is scrubbed, and a host record-deletion
operation is queued in bounded worker batches. The operation remains pending while the host
is offline. State is
`attached` or `detached` after a successful live observation, `closed` for an ended process,
and `unavailable` when live metadata cannot be established; unavailable data is never guessed.

`title` comes from the running application's OSC 0/2 output. Optional `foreground_process`
provides a process-name fallback when the application has not supplied a title. Optional
`current_directory` is a live host observation, never the launch directory relabeled.
The dashboard shows the editable name first, activity second, and the live directory;
when live directory observation is unavailable it explicitly says `Started in` instead.
These fields are not persisted as user-authored metadata. Missing/offline observations
return an empty title and omit the live directory/process. Display metadata strips control
and bidirectional-control characters and bounds title/process to 128 characters and paths
to 1024. The same fields and authorization rules apply to the shared-session catalog.

Recent replay output is persisted and byte-bounded by the paired CLI host while a terminal runs.
The updated host clears that output when the process closes or exits, and on recovery of an
already ended process. `expires_at` is the server metadata expiry; output is not intentionally
kept until that date. An offline or older host may retain its prior output until the updated
agent reconnects. The reserved default session has no `expires_at` and keeps its stable ID;
closing it clears output without deleting that identity.

Terminal browser access is configured separately for one exact terminal session. The default
`owner` audience keeps access with its owner. `team` gives current members of the already-bound,
entitled team browser viewing; this policy cannot move a terminal to another team. `specific_emails`
and `email_domain` require the current verified WorkOS email on the signed-in Paperboat account.
Domain matching is exact after `@`, so `person@company.com` matches `company.com`, but neither
`person@sub.company.com` nor `person@notcompany.com` does. Terminal policies do not support public
access or wildcard addresses. These browser grants do not create team membership, machine grants,
ENV access, SSH, or native terminal access.

View is the default role. `interactive_emails` grants terminal input only to those exact verified
addresses; it cannot be granted to a whole email domain. Adding an interactive address requires
`confirm_interactive_access: true`. A browser policy generation advances on changes so gateways
can revoke existing attachments; the browser gateway rechecks authorization every two seconds.

`GET /v1/terminal-sessions` and the exact session detail include terminals shared through
current browser email, domain, or team policy. `browser_only: true` means the reported role
applies only to the browser console; it grants no native join or machine access. Catalog
entries use the same current verified-email decision as browser stream admission.

- `GET /v1/terminal-sessions/{session_id}/browser-access-policy` requires an owner browser session.
- `PUT /v1/terminal-sessions/{session_id}/browser-access-policy` requires an owner browser session
  and CSRF token. It replaces the full policy and requires `expected_generation`; stale updates
  return `409 terminal_access_policy_changed`. Email lists are limited to 100 exact addresses.

`POST /v1/terminal-sessions/{session_id}/browser-ticket` requires a WorkOS-backed browser
session, CSRF token, an allowed Origin, and a canonical SHA-256 SPKI digest of the
browser's new in-memory Ed25519 TLS key. It returns one single-use ticket, the browser
stream URL, current role, machine and owner IDs, and the machine's current public
transport signing key. The authenticated control plane authorizes this key; the
browser verifies its fingerprint locally before opening the encrypted stream.

The returned `wss://.../v1/browser-terminal` URL points to the selected runtime
edge. The browser offers `paperboat.browser-terminal.e2ee.v1` and the ticket
subprotocol. The edge sends that ticket over its node-authenticated control
connection; the control plane consumes it once, checks the exact edge node and
process, current policy and host process generation, and returns a host credential
bound to the browser's TLS key. Only the edge forwards terminal bytes to the host.
The browser verifies the machine certificate under the authorized machine key
and negotiates inner TLS 1.3 with the machine. Commands, output, resize,
acknowledgements, and replay stay encrypted across the edge. The control plane
checks the browser session and resource policy throughout the attachment; loss of
fresh authorization or a changed grant disconnects it. The host validates the
browser certificate key and enforces View versus Interact permissions. A new
attachment uses a new browser key and ticket.

- `GET /.well-known/jwks.json`
- `GET /v1/client-configuration` returns server-owned URLs used by unauthenticated clients.
- `POST /v1/auth/device/authorize`
- `POST /v1/auth/device/token`
- `POST /v1/auth/token/refresh`
- `POST /v1/auth/token/revoke`
- `GET /v1/auth/cli-client-sessions`
- `DELETE /v1/auth/cli-client-sessions/{cli_client_session_id}`
- `POST /v1/machines/{machine_id}/connection-descriptor`
- `GET /v1/machines/{machine_id}/connection-readiness`
- `GET /v1/terminal-sessions/{session_id}/browser-access-policy`
- `PUT /v1/terminal-sessions/{session_id}/browser-access-policy`

Native connection descriptors are short-lived and bound to the authorized machine,
resource and current installation generation. Readiness reports a usable target;
pending reconciliation is retryable and does not authorize an unrelated target.

## Error Codes

Documented public codes currently emitted by the handlers include:

- `unauthenticated`
- `forbidden`
- `payment_required`
- `github_required`
- `validation_failed`
- `invalid_request`
- `idempotency_key_required`
- `idempotency_key_conflict`
- `machine_not_ready`
- `terminal_session_not_found` (404; only "not yours or doesn't exist")
- `terminal_session_closed` (409; the owner's terminal has ended)
- `machine_edge_unavailable` (409; the machine has no connected edge route; it reconnects on its own)
- `too_many_pending_tickets` (429 with `Retry-After`; the browser holds the most unredeemed terminal tickets it may)
- `terminal_session_name_conflict`
- `terminal_session_reserved`
- `terminal_session_not_closed`
- `terminal_session_operation_pending`
- `invalid_terminal_access_policy`
- `interactive_confirmation_required`
- `terminal_access_policy_changed`
- `provider_unavailable`
- `provider_outcome_unknown`
- `version_conflict`
- `machine_limit_reached`, `preview_limit_reached`, `browser_terminal_limit_reached` (409)
- `internal_error`

Adding, removing, or renaming public error codes requires contract approval.

## Private Connector Admission

`POST /v1/connectors/admission` is a machine-runtime-to-control-plane endpoint, not a browser or CLI
API. It requires a machine-control bearer credential plus an unpadded base64url
`X-Paperboat-Machine-Proof` signed by the enrolled machine key. The proof covers the exact
method, path, body hash, operation ID, machine/environment binding, and a maximum one-minute
lifetime.

The strict JSON request contains `operation_id`, `environment_id`, `machine_id`, `connector_id`, `edge_pool`,
and `protocol_version: "1.0"`. A successful response follows the canonical
`testdata/contracts/schemas/edge/admission.schema.json` shape and includes one assigned endpoint,
at least one revisioned route handoff, and a short-lived connector credential. Alternate
node ports, credential expiry internals, provider values, and reusable edge-control secrets
are never returned.
## Self-hosted HTTP topology

Relay and tunnel pool policies are independent. Direct peer connectivity is
unchanged. Browser edge pools use `managed-only` or `self-hosted-only`, for either
Personal or a Team workspace. The default is `managed-only`. Browser publications
never mix the two kinds or fall back across modes. Personal relay pools retain
their separate mixed policy.

A self-hosted pool explicitly selects enrolled installations. More than two may
be enrolled and selected (up to 32 selected); each durable browser publication
uses at most two eligible nodes and each preview uses one. Selection does not
transfer installation administration. Changing browser modes requires stopping
active publications first. Team owners and admins manage the Team pool; members
may read it.

Paperboat-managed preview and durable HTTP hostnames cannot be served by a
self-hosted edge. Self-hosted browser publications require a customer domain,
verified DNS for the selected ingress addresses, and a ready certificate.
Infrastructure public HTTPS uses its own trusted certificate, separately from
the pinned carrier identity. Node or address changes require explicit DNS
verification before the new placement is eligible.

A new self-hosted tunnel may exist before its domain is ready, with an empty
`stable_endpoint` and `summary_code: custom_domain_required`. A preview similarly
has `endpoint_state: custom_domain_not_ready` and an empty `endpoint`. Domain
instructions expose all required address and ACME delegation records before
readiness waits. Ready resources expose only the verified customer URL. Native
internal activation identities are not public URLs.

Inventory follows the selected browser mode. An unavailable self-hosted pool
returns no eligible managed fallback. Account removal and operator revocation
fence subsequent authorization. TCP and origin TLS pass-through retain their
own protocol-specific eligibility and trust boundaries.

Self-hosted usage reports must be signed by that installation's distinct usage key and
match its assigned account, environment, route and generation. They retain idempotent
traffic counters, but cannot debit Paperboat bandwidth quota: an independent operator's
assertion of bytes is not billing authority. Managed-node accounting and account/machine
entitlements keep their existing behavior. The operator pays its infrastructure costs.
# Personal and team subscriptions

`GET /v1/billing/workspaces/personal` returns your personal subscription.
`GET /v1/billing/workspaces/teams/{team_id}` returns a current team's subscription;
membership is required. Each workspace exposes `/products`, `/topups`, `/checkout`, and
`/portal`. Checkout and portal require browser authentication, CSRF protection and
an `Idempotency-Key`; team mutations additionally require the owner or a billing admin
(`billing.manage`).
Return URLs must use a configured dashboard origin.

Team billing exposes `seats_bought` and `seats_used` (active members including the
owner plus pending, unexpired invitations). Only the owner and billing admins manage billing.
`GET /v1/billing/workspaces/teams/{team_id}/seats/preview?quantity=N` previews a
quantity change; `POST /v1/billing/workspaces/teams/{team_id}/seats` with
`{"quantity":N}`, CSRF and `Idempotency-Key` commits it. Both return
`{amount_due_now,currency,next_invoice_amount,next_invoice_at}` in `data`;
amounts are currency minor units, the date is RFC3339.

Increases use Stripe `always_invoice` and require immediate payment, without
moving the renewal date. Reductions schedule a lower quantity at renewal, with
no immediate credit or refund; `scheduled_seats` and `scheduled_seats_at` show
that intent while `seats_bought` remains the paid current quantity. The requested
quantity must cover used seats (`409 seats_in_use`). Invitations reserve a seat;
full teams get `409 seat_required`. Acceptance uses the reservation. Removing a
member or cancelling/expiring an invite changes usage only. A scheduled reduction
also bounds new reservations. A pending payment operation must be retried with
the same key (`409 billing_pending` / `503 provider_outcome_unknown`); membership
reconciliation never changes purchased quantity. Other role, resource and
personal subscription boundaries remain independent of seats.

Stripe test setup is pending. Free and Pro provide the same personal features and differ in count limits
(`billing.limits`): machines and concurrent previews plus tunnels 100 and 1000, console browser
terminals 10 and 100; Teams are advertised as unlimited, with hidden ceilings of 10000, 10000 and
1000. A new resource past its limit is refused and existing ones keep working. Bandwidth allowances
remain configurable, with unset values imposing no limit. Prices remain unset. An uncertain checkout must retain its operation ID; a
successful overview refresh does not prove that a purchase failed. Superseded Polar runtime, routes and configuration have been removed; historical migrations remain.

Machine metadata uses `alias` as its only name and selector. Machine setup accepts
`alias`; `PATCH /v1/machines/{machine_id}` replaces it. Machines have no description.
Machine IDs and existing aliases survive migration. Shared environment descriptors and configuration status expose the same `alias`.
Aliases are unique within a personal owner or owning team. Sharing does not rename
machines; consumers reject ambiguous aliases across visible machines. Existing team
collisions remain diagnosable; new conflicting renames and ownership transfers fail.

Machine services use `GET /v1/machine-services` for the caller-authorized, short-lived
port snapshot (CLI bearer or authenticated dashboard session). Each authorized port
includes `browser_url` as `https://<port>.<alias>.<local access domain>` when its alias is
unambiguous in the caller's account namespace. Listing allocates no public hostname
or activation policy. The URL requires the signed-in native daemon, a ready local
TLS certificate and an exact current private-access grant; it has no hosted fallback.
Ambiguous shared aliases retain service metadata but omit the browser URL.
`GET`/`PUT /v1/machines/{machine_id}/private-access-policy` provides
metadata-manage-authorized personal automatic-access settings (it starts enabled and
applies only to personal ownership). Team members reach the ports of a machine shared into
a team only when the machine's `private_access` grant (member or all-members) applies to them
and the team setting `allow_private_access` is on (default off; a team owner or admin changes it
with the `private_access` team mutation, which moves the team generation and so ends the sessions
already admitted). With the setting off the admission request answers 403 `team_private_access_off`.
Administrative membership alone never grants use. Explicit lazy-port policies
and private TCP routes remain independent allowances, including ports below 1024.

The returned discovery-policy generation is persisted independently of native source
policy generations. It advances when the derived enablement or explicit-port set
changes, including audience changes; unchanged refreshes preserve it.

Signed runtime observations carry `machine_services` (`paperboat.machine-services/v1`),
at most 64 distinct ports with a selected `ipv4` or `ipv6` loopback family, installation,
boot, and announcement generation. The signed `sampled_at` bounds the snapshot lease
to 30 seconds, preventing replay from renewing availability. Responses carry
`machine_service_policy` with discovery generation, automatic-discovery enablement,
and sorted explicit ports. No process names or payloads are announced.

Native `machine_service` grants bind machine ID and `tcp:<port>` to the selected
installation, boot, authorization source, and announcement generations. Grant expiry
is an admission deadline; an admitted stream must renew its exact authority through
signed `POST /v1/runtime-machine-services/authorize` at least every 10 seconds. Each
renewal lasts at most 30 seconds; current caller/session/source revocation or a changed
boot/announcement stops renewal. Native bytes remain end-to-end encrypted.
Browser `pPORT-environmentlabel` hostnames use the configured preview domain and
existing lazy activation only after exact authorization. Browser TLS terminates at
the edge. HTTP is the default target scheme; an explicit lazy policy selects another
supported scheme. Machine-service listing includes the stable environment label.
Team IDs used as hostname labels contain lowercase letters, digits and interior
hyphens only, up to 63 characters; opaque account/resource IDs retain their own rules.

## Operation support reference

User-facing failures use one `support_reference` (`support_` plus a canonical lowercase UUIDv4).
Clients carry it in `Support-Reference` on PB control requests and reuse it through retries;
the server returns it in the response header and error envelope. Request IDs and Sentry
event IDs remain internal mappings. The audited staff lookup and account-safe status APIs,
privacy policy, reporting configuration and retention are documented in
[support references](runbooks/support-references.md).

# Resource bandwidth history

`GET /v1/usage?start=<RFC3339>&end=<RFC3339>&workspace=<personal-or-team>`
returns accepted upload/download measurements for the authenticated active workspace.
Optional `actor_account_id`, `resource_id`, `mode` (`direct`, `relay`, `edge`, `unknown`) and `hosting`
(`managed`, `self_hosted`, `none`, `unknown`) filter both totals and details. Windows
span at most 90 days; history retains the past 90 days. Measurement interval ends
fall in the half-open `[start,end)` window. Series use UTC hours through 48 hours
and UTC days thereafter. Detail limits are 200 resources and 32 paths per resource;
`truncated` exposes omissions while totals and series retain all matching bytes.
`resource_route_id` distinguishes machine service ports without inventing resource IDs.
`actor_account_id` selects the retained member responsible for traffic inside that exact workspace;
it filters totals, series and both detail pages together. Native reporting never accepts an actor
from the machine: the server snapshots it from admitted authority. Earlier unattributed team traffic
remains in workspace totals and is omitted from member-filtered results.
`updated_at` is the latest actual workspace report/measurement (year 0001 when absent).

`POST /v1/usage/native` uses the current signed machine identity and exact-body
machine proof. The body contains at most 64 per-stream/path/minute absolute
successful application byte counters, plus optional cumulative `unrecorded_bytes`,
`expired_bytes`, and `recording_incomplete` quality signals. Upload means host read;
download means host write. Retained server issuance determines the exact resource,
workspace, machine generation, and browser attachment node. Replays and lower
counters do not inflate history. Delivery is limited to seven days, counters to
1 TiB per direction per stream/path/minute, and storage admission to 8192 records
per machine/minute. `accepted` and `rejected_indices` acknowledge entries separately.
An empty report array sends a quality heartbeat. Reporting preserves historical
authority after revocation without authorizing new access. Coverage exposes missing,
stale, dropped, or incomplete native reporting; unknown relay nodes remain unknown.

The history projection reuses accepted edge receipt deltas and snapshots resource
and node labels. Canonical public, private, and team preview HTTP/WebSocket traffic
uses the admitted preview's exact route, generation, workspace, and edge node.
The edge records application traffic once, excluding admission messages and internal
carrier legs, through its existing signed usage receipts and bandwidth allowance checks.
Private previews opened through the native PAC proxy refresh their exact signed-grant
accounting authority per HTTP request while retaining the proxy connection lifetime.
Old receipts without a retained workspace binding and native
sessions issued before reporting authority existed are unavailable. These product
observations do not add or duplicate billing debits and contain no file, terminal,
environment, or target payloads. See [OpenAPI](openapi.json) for the response schema.


### Workspace activity

`GET /v1/activity?workspace=personal` returns `{data:{items,next_cursor?}}`.
Cookie sessions or `account:read` bearer access are accepted; team activity
requires a member holding `audit.read` (owner, admin or auditor). Optional `machine_id`, comma-separated
`area`, substring `q` (200 bytes), opaque `cursor` and `limit` (1–200, default 50)
filter the newest-first page. Events include `id`, `occurred_at`, `action`,
`actor:{kind,id}`, `target:{kind,id,name?}` and optional `machine:{id,name}`.
Names and exact workspace ownership are retained at audit insertion, never inferred
from a resource's later owner. Raw metadata is never returned. Earlier events with
no retained scope and unsupported/internal resource kinds are omitted. The feed
covers scoped machine, terminal, preview, tunnel, team, user and machine-environment
audits within the workspace plan's retention (Free 30, Pro 90, Team 365 days). Empty matches
return an empty array. Invalid filters or cursor return 400 `invalid_activity_query`, missing
auth 401, unavailable workspace or insufficient team role 403, and storage failure 500
`activity_unavailable`.

`totals.by_path` and each series point's `by_path` contain
`{mode, hosting, upload_bytes, download_bytes}` aggregates over all matching
measurements, independently of resource/path pagination. Modes are `direct`,
`relay`, `edge`; hosting is `managed`, `self_hosted`, `none`. Unknown attribution
is omitted from these breakdowns and identified in `coverage.note`; combined
upload/download totals still include it. Empty breakdowns are `[]`. Current
machine reporting coverage excludes retired installations and former workspaces.

## Team invitations

`POST /v1/teams/{team_id}/invitations` takes `operation_id`,
`expected_generation`, and exactly one of `email` or `account_id`. An email
invitation can precede signup; the address is normalized to lowercase and does
not create an account. The response and invitation inventory include `email`
when addressed by email, with empty `account_id` until the recipient acts.
Only a current active account with that verified email can discover, accept or
decline it. Acceptance and decline bind the invitation to that account, so a
later email change does not transfer its history or replay authority.

Existing accept, decline, cancellation and inventory endpoints are unchanged.
Malformed recipient inputs return `400 invalid_request`; stale generations or
an existing pending invitation return `409 team_changed`. Administrators keep
team-scoped inventory access; unrelated accounts cannot discover invitations.
