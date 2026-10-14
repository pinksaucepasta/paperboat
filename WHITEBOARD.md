# Paperboat whiteboard

Remaining items are post-launch scope (user decision 2026-09-13).

## Generic UDP support — later

Recorded 2026-09-07 by Codex from the user's direction.

Add generic public and private UDP forwarding in a future scoped task. Preserve datagram
boundaries, bounded return-path mappings, expiry and cleanup. Public UDP needs assigned
endpoint ports, owner-authorized publication and abuse/traffic limits; private UDP needs
Paperboat-authorized encrypted native access. Application authentication remains protocol-specific.

UDP is deferred, not part of the current HTTP/HTTPS/WebSocket and raw TCP delivery tasks.
Internal QUIC support does not constitute generic UDP forwarding. Define actual client UX,
limits and acceptance before implementation; this note is not implementation/deployment evidence.

## CLI UX review and fixes — before customizable shortcuts

Recorded 2026-09-08 by Codex from the user's direction.

Review all CLI commands for clear, user-oriented names and consistent workflows before
adding customizable shortcuts. For example, consider `pb device manage` instead of
`pb device capabilities`: users should not need internal terminology to find device
controls. This example is a direction to evaluate, not a finalized command replacement.

Walk through the full command tree, help/discoverability, flags, defaults, output and error
messages. Simplify awkward or redundant commands around what users want to do. Update
affected CLI contracts, docs, completions, examples and agent interfaces together when
changes are selected. Settle the canonical UX first so customization builds on a coherent
interface rather than hiding confusing commands.

This is a future discussion/review item, not implementation authorization or a new launch gate.

## Paperboat SDK — discuss further

Recorded 2026-09-08 by Codex from the user's direction.

Explore a supported developer library with typed APIs, documentation, examples and stable
releases so developers can use Paperboat functionality inside their own products. Their
application provides the UI; users need not run `pb` commands or interact with Paperboat
directly. This means a proper SDK, not a wrapper that shells out to the CLI.

Candidate capabilities include device connectivity, terminal sessions and streaming I/O,
file transfers, previews/tunnels, and device/permission management. Discuss an embedded
connection runtime and management API client as a coherent developer interface, reusing
existing implementations. Decide initial scope, supported languages/platforms, enrollment,
application/user authorization, tenant isolation, key custody, lifecycle and compatibility
before committing to APIs. Invisible integration does not eliminate authorization needs.

This is an open discussion item, not a settled SDK design, implementation task or launch gate.

## Browser terminal over WebTransport — planned direction

Recorded 2026-09-26 at the user's direction. Use WebTransport for the browser terminal,
over HTTP/3 and QUIC. Prefer one WebTransport path over maintaining WebTransport and WSS
paths in parallel unless real browser/network testing shows a fallback is needed. WebTransport
does not have a TCP mode: if HTTP/3/QUIC is unavailable or blocked, the browser cannot use
this path. Any WSS fallback is a separate transport decision, not WebTransport over TCP.

This changes only the browser terminal carrier. Keep terminal rendering in the browser,
the existing one-use access ticket and View/Interact authorization, and end-to-end encryption
between browser and host through the edge. Do not route terminal content through the control
plane. Before selecting a fallback, test real browsers and restrictive networks, plus input,
output, resize, replay, background-tab recovery, reconnect, revocation and cleanup. This is
planning direction only, not implementation or deployment evidence.

## Codex and OpenCode remote command integrations — deferred

Recorded 2026-09-08 by Codex at explicit user direction. Neither `pb codex` nor
`pb opencode` belongs in the current main-branch product or launch acceptance.
Preserve the existing Codex implementation on `archive/codex-integration` in the
Paperboat CLI, server and web repositories, then remove its command, runtime/API
integration, UI entry points and pinned protocol/path-manifest machinery from main.
OpenCode has no current command implementation to preserve.

Revisit thin wrappers around the tools' standard remote commands, retaining useful
machine/project/folder selection and authenticated transport/process cleanup. Avoid
maintaining upstream protocol schemas or path-rewrite manifests. Verify supported
remote/platform behavior before choosing an implementation; archived Windows workarounds
are reference evidence, not a requirement to restore that design. Normal use of these
tools through a PB terminal or managed SSH is unaffected.

## Remote desktop beta — deferred former Task 38

Moved from TRACKER.md on 2026-09-08 by Codex at user direction. Not scheduled,
not a launch requirement and not a Task 45 acceptance dependency.

When selected, integrate one existing third-party implementation and an explicitly
authorized native TCP/UDP adapter. Reuse Task 20/23 evidence and resolve current
prerequisites at activation. Define supported OS/provider/protocol versions, a named
owner and bounded effort before implementation. Do not build encoders, display/audio/input
stacks, drivers or a streaming engine, or expand core transport support implicitly.

Require actual pairing, per-resource control authorization, account/device isolation,
revocation, audit, cleanup, latency and degraded relay behavior on the declared matrix.
Userspace WireGuard alone does not prove compatibility. Preserve security and publish
honest support limitations; the beta must not block native SSH/editor workflows.

## PostHog product analytics — selected for future planning

Recorded 2026-09-13 by Codex at user direction. Add PostHog alongside Sentry: Sentry
explains failures; PostHog explains onboarding completion, feature adoption and where
users abandon workflows. This records the requested capability, not an SDK deployment
or a new launch gate. Schedule implementation explicitly when selecting this work.

Start with explicit dashboard events for onboarding steps, device enrollment completion,
tunnel/preview creation, team invitation acceptance and checkout completion. Track
success/failure outcomes and conversion funnels with small, documented event schemas.
Confirm successful actions from authoritative results rather than button clicks; payment
success must come from confirmed billing state. Avoid duplicate counting across browser
and server events. Collect only metadata needed for a named product question.

Use pseudonymous account/workspace identifiers where necessary to distinguish personal
and team usage; do not capture emails, device aliases/descriptions, command arguments,
terminal content, ENV values, credentials, file names/paths/payloads, HTTP request bodies,
inspector data or full URLs/query strings. Disable broad autocapture and session replay
by default; neither is part of this initial scope. Do not instrument user applications
served through PB tunnels. CLI/daemon analytics are not implicitly included in dashboard
instrumentation and require an explicit later decision.

Define reporting consent/settings, retention/deletion, access controls and event-volume
budgets before implementation. Honor applicable consent requirements; dashboard behavior
must remain functional when tracking is blocked, disabled or unavailable. Do not make
analytics an authorization, billing or availability dependency. Reuse existing user/team
identity boundaries without treating analytics identifiers as access credentials.

Verify event accuracy, deduplication, personal/team attribution, secret exclusion and
opt-out behavior against representative real dashboard workflows. Keep Sentry and PostHog
responsibilities distinct; do not build a duplicate analytics/reporting platform in PB.

## Official Paperboat agent skill — future implementation

Recorded 2026-09-13 by Codex at user direction. Provide an installable agent skill so
AI coding assistants can use Paperboat correctly through its supported CLI. This is
selected future planning, not implementation or a new launch gate; it does not require
the deferred embedded SDK or Codex/OpenCode remote-command integrations.

Teach device discovery and selection, capability/readiness checks, remote terminal/exec
and managed SSH, file transfers, previews/tunnels, ENV workflows and configuration sync.
Explain personal/team scope, exact resource selection, structured output, exit codes,
reconnect/retry behavior and actionable recovery. Follow the final CLI UX and published
help; avoid stale command examples or maintaining a second command/schema specification.
Keep initial instructions concise, with focused reference material for less common flows.

Agents use the user's existing authenticated PB identity and grants; the skill supplies
no extra authority. Require exact targets and respect user intent for remote execution,
public exposure, file replacement/deletion, sharing and team administration. Never expose
credentials, decrypted ENV values, terminal/file payloads or recovery material in logs or
reports. Handle unavailable devices and unsupported capabilities honestly; do not bypass
authentication or infer success from request submission alone.

Define distribution, installation, version compatibility and updates for supported agent
skill formats. Verify representative real workflows with an agent, including failures,
ambiguous device selection, permission denial and cleanup. Reuse the CLI implementation
and existing product evidence; no separate agent-only backend or bespoke protocol needed.

## Paperboat operator admin panel — backend implemented; frontend planned

Recorded 2026-09-13 by Codex at user direction. Add an operator-only admin area for
Paperboat staff, backed by capabilities in the existing `paperboat-server`; no separate
admin server. Team owner/admin/member controls remain in the existing team dashboard.
Backend implementation selected 2026-09-19 at user direction; frontend remains future work.
The owning evidence entry is “Operator admin backend” in `plans/TRACKER.md`.

Backend implementation extended 2026-09-19 to include the originally missed region/node,
backup and Sentry browsing scope. The authoritative API reference is
`paperboat-server/docs/openapi.json`; operational semantics and activation steps are in
`paperboat-server/docs/runbooks/operator-admin.md`. The frontend should consume these
APIs rather than adding authorization or state ownership in the browser:

- Full admin role; separate support, billing and operations staff capabilities.
  Session-bound WorkOS passkey reauthentication, fresh for every staff request.
- Paginated account/team/workspace/operation lists, exact email/account lookup,
  aggregate status and resource-specific audits; one PB reference for issue lookup.
- Account ban/unban with credential revocation; reversible team suspension with
  generation, paid-access, ENV and SMTP enforcement. Suspension preserves subscriptions;
  show unchanged billing status and the need for fresh authority after recovery.
- Relay drain/resume and tunnel disable/enable with explicit revision, target,
  evidence and retry key. Show accepted desired changes separately from runtime
  convergence, and show connector/edge/DNS/certificate observations per layer.
- Verified existing Stripe price registration, catalog activation/deactivation and
  immutable price/scope bindings; shared runtime bandwidth/device policy with revision
  conflicts. Stripe price creation, refunds and automatic plan migration are separate
  provider operations, not fabricated by these APIs. Deployment secrets remain outside
  editable runtime settings.
- Create/update/disable/enable hosted regions and enroll/update/drain/resume/retire
  nodes. Provision and rotate node-scoped runtime credentials; require explicit
  failure domains and revisions. Show runtime generation, last observation and registry
  expiry. Region disablement fences selection and runtime authority; enabling does not
  silently resume nodes. Enrollment manages existing machines, not cloud VM purchasing.
- Backup dashboard: newest run/failure, history, freshness, archive retention and exact
  backup-to-restore verification. Missing producer means not configured/unknown. Consume
  dedicated authenticated deployment evidence, never infer protection from database uptime.
  The Linux producer supports real dump/isolated restore, durable reporting retries,
  interruption cleanup and bounded retention; storage and scheduling are deployment-owned.
- Local reference feed and multi-project Sentry issue/detail/log/trace/metric views, with exact owner/reference
  resolution, project isolation, bounded safe fields and explicit provider errors. Keep
  ingestion enablement separate from read-only Sentry API access. Credentials remain in
  backend deployment secrets. These APIs browse evidence; issue triage remains in Sentry.
- Sentry cron monitors, per-environment check-ins and active incidents; metric detectors
  and their latest issues, plus accepted/filtered/rate-limited ingestion outcomes.
  Display disabled, missing, unsupported and stale evidence explicitly. Backend routes:
  `/v1/admin/sentry/monitors`, `/monitors/{slug}/checkins`, `/detectors` and `/outcomes`
  under the same Sentry prefix, with explicit allowlisted project selection.
- Hosted metric views distinguish a configured region/slot from population maxima;
  do not sum sampled gauges into fleet totals. Show observed bucket times, backup/restore
  state and age, real worker progress/stalls, SDK drop counters and signal enablement.
  Include immutable release/commit association and verified deployment status; publication
  alone is not proof of rollout. Native endpoints remain anonymous in fleet attribution.
- Billing/control recovery, signing/usage-key lifecycle and signed release authority
  retain the owning service's integrity and idempotency rules. Every operator action
  records intent/outcome and all mutation effects commit with their success audit.

Frontend work remains unimplemented: staff login/callback, permission-aware navigation,
search/status views, explicit impact/target confirmation, conflicts/retries, and the
Sentry/reference workflow. Live WorkOS passkeys, Stripe integration, backup storage/scheduling
and deployed runtime convergence still require deployment qualification. Real Sentry
SDK receipt and protected admin read-back were exercised with isolated PostgreSQL and
an identity-provider fixture; this does not qualify live WorkOS or the deployed admin UI.
An independent availability watchdog is deployed; in-process fleet rollout remains
separate. Exact evidence and remaining prerequisites are recorded in the tracker.


Cover account/team support, subscription and billing status/recovery, managed infrastructure
health, relay/tunnel status and authorized operational actions. Use the selected support API: `GET /v1/admin/support/{reference}?owner_id=...`
for audited, bounded failure timelines grouped by account and Sentry links, and
`GET /v1/support/{reference}` for account-owned safe status. The admin workflow accepts
one PB reference, resolves matching owners (with optional account filtering and a separate
unattributed group), and shows the initiating stage/source location, later failures, release, retention and
honest reporting status. Deep debugging stays in Sentry. Distinguish unconfirmed SDK
capture from confirmed delivery; expose unavailable/expired evidence without implying
success. Include reporting configuration/disablement, retention/deletion procedures,
dropped-report health and source-map/release readiness in admin operations scope.
Official native and hosted releases inject a default ingestion DSN and component
release; ordinary source builds have no Paperboat destination and require explicit
opt-in with their own Sentry configuration. Explicit disable remains available.
A public binary can be modified to copy its DSN, so this is a distribution default,
not proof of executable origin. Client-originated Sentry evidence is diagnostic input, never authorization evidence. Use the
implemented capability matrix and documented operational scope; ordinary team admins
do not become Paperboat operators. Self-hosted deployment-local operator authority
remains separate and is not granted by account pairing or a PB staff UI.

Enforce operator authorization on every backend request, strong authentication and audited
sensitive actions. Use least-privilege roles, explicit targets and clear impact/confirmation
for destructive actions. Record actor, action, target and outcome without secret content.
The admin URL or hidden navigation is not a security boundary. Operator status must not
automatically grant terminal/file access, user impersonation, decrypted ENV secrets,
private keys or recovery material. Avoid exposing credentials through configuration views.

Verify non-operator denial, role separation, cross-account support access boundaries,
audit completeness and safe retry/recovery for state-changing actions. Reuse authoritative
account/team, billing and infrastructure services; no alternate state store or bypass of
normal authorization/integrity rules. Scope any future emergency access explicitly.

## Open base server with private extensions — post-launch exploration

Recorded 2026-09-23 by Codex at the user's request. Planning only; the current server
and dashboards remain closed-source for launch.

Explore an independently usable open-source base server for personal self-hosting,
without teams, billing or enterprise features. Retain personal accounts, devices,
authentication, authorization and core Paperboat workflows; barebones must not mean
weakened security or dependence on the private server.

The closed-source server would import the base as packages and add teams and other
commercial/enterprise features in the same process. Share the core implementation;
avoid a separate service deployment or general plugin framework merely for this split.
The dashboard should discover server-provided capabilities and show only supported
features, while the server independently enforces all permissions and entitlements.

Before implementation, define the exact base feature set and license, inspect dependency
boundaries, and plan separation of ownership, authorization, database queries/migrations,
API composition and dashboard consumers. Verify both standalone base operation and the
complete private edition, including security and lifecycle behavior. This is substantial
architecture work requiring a scoped task, not a feature-flag-only packaging change.
