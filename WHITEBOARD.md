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

## Paperboat operator admin panel — backend implemented; frontend planned

Recorded 2026-09-13 by Codex at user direction. Add an operator-only admin area for
Paperboat staff, backed by capabilities in the existing `paperboat-server`; no separate
admin server. Team owner/admin/member controls remain in the existing team dashboard.
Backend implementation selected 2026-09-19 at user direction; frontend remains future work.
The owning evidence entry is “Operator admin backend” in `plans/TRACKER.md`.

Backend implementation extended 2026-09-19 to include the originally missed region/node,
backup and Sentry browsing scope. The authoritative API reference is
`server/docs/openapi.json`; operational semantics and activation steps are in
`server/docs/runbooks/operator-admin.md`. The frontend should consume these
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

## Tunnel-only mode without a Paperboat machine (and an official container image)

`pb tunnel run --token <token>` for a Docker container, Kubernetes pod or CI job, like
`cloudflared tunnel run --token`: nothing joins the account and nothing installs into
the system; it only forwards a tunnel to the app next to it. Needs a tunnel token with
rotation and one more admission path. Decision (2026-10-20): not for launch; until then
every tunnel runs on an enrolled Paperboat machine.

## Console talks to the local `pb` (browser ↔ local daemon bridge)

When the console runs on a computer that has `pb` signed in to the same account, the page
could use that local `pb` instead of our servers and edges. Candidate uses:
- terminals and exec straight over the peer-to-peer path (lower latency, no edge bandwidth);
- file transfers and Inbox downloads machine to machine, and drag-and-drop uploads from the page;
- private previews and private ports opened through the local `pb` (127.100.x.x) without
  browser sign-in at the edge;
- ENV vault unlock and config-sync conflict compare done by the local `pb` (keys never in the page);
- live machine status, diagnostics and `pb doctor` results shown from the local daemon;
- "Open in terminal / VS Code" buttons that hand off to the local `pb`.

Open questions before building: browser rules for a public page reaching a local port (Chrome
Private Network Access, Safari blocks), pairing so other websites can't use it, origin checks,
fallback to the normal path when no local `pb` is present, and not ending up with two code
paths per feature. Not for launch (user, 2026-10-21).

## Priority relays in more regions — after launch

Recorded 2026-10-23 at the user's direction (a Pro extra, not affordable yet). More managed relay
regions with priority capacity for paid plans, so relay-only paths stay fast far from today's edges.
Revisit once revenue covers the extra servers.

Container mode, recorded 2026-10-23 with the entry above: today `pb` runs in Docker only as a full
systemd container (`--privileged`, systemd as PID 1). The standard shape (Tailscale, cloudflared) is an
official image: `pb daemon` in the foreground as the main process, pairing from a one-time key in an
environment variable, state on a volume, no extra privileges. Covers Docker hosts, Kubernetes and CI.

## Dual-stack edges (IPv6) — after launch (coordinator decision 2026-11-12)

Edges are IPv4-only at launch (no public IPv6, no AAAA for the preview/tunnel domain). IPv6-only visitors still
reach them through their carrier's NAT64/DNS64. Later: public IPv6 on every edge, wildcard AAAA records, owner IP
rules and `--host-header` checked with real IPv6 client addresses (the IPv6 part of cross-platform row X-20).

## Batched integration queue — after launch (user decision 2026-11-12)

Consider a bors-style queue that tests an actual combined batch once and bisects failed
batches, if native GitHub per-PR queue throughput proves insufficient. Before launch keep
GitHub's native queue with five concurrent speculative builds, rebase merges and the fast-PR /
heavy-merge-group split. Native merge limits do not amortize CI across PRs. Prior art:
[bors-ng batching](https://bors.tech/documentation/) and
[GitHub merge queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue).

## Exec round trips: what is left — after launch

Measured 2026-11-12: one `pb exec` over a 290 ms path took ~12 dependent round trips. Fixed 2026-11-16 (1f9007a39): the per-exec
network refresh (2-5 round trips) is gone. Fixed 2026-11-16 (FIX-EXECRTT): exec runs on one stream whose credential, preface, hello and
start go out in one flight, and the peer association lingers 5 s after its last user; 290 ms path 2.57 s -> 1.05-1.3 s
(private smoke receipt 20261116T-execrtt.json). Left: the daemon's per-exec descriptor fetch from the server
(`MachineExecDescriptor`, one control-server round trip per exec).

- Socket-based agent that Git-for-Windows ssh can reach (after launch): the managed agent is a Windows named pipe, which MSYS OpenSSH cannot use; until then Git runs through `git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe` (pb ssh doctor prints it).

## Remove the reliance on the Tailscale fork — after launch (user decision 2026-11-13)

The CLI's peer data plane builds on our fork github.com/pinksaucepasta/tailscale (WireGuard/magicsock/DERP-style relay, netstack). Carrying a fork means rebasing on every upstream release, owning its security fixes, and a large dependency surface for what we use. Goal: depend only on upstream libraries (wireguard-go, gvisor netstack, quic-go) or our own small packages, with no fork in go.mod. Steps to plan: inventory every fork package we import (cli/internal/peertransport/*, mesh, DERP map, tsnet-like pieces), classify each as "upstream library exists", "our code already replaces it", or "needs a small own implementation"; replace one area at a time behind the existing peer transport interfaces with device checks per area (direct path, relay, NAT rebind, update handover). Keep wire compatibility only with the current main (zero-users rule). Related: post-launch year plan (shed the fork).

### Findings 2026-11-13 (fork 2d32e5f0 vs its upstream base 610b05c5)

The fork is 4 commits, 20 files, +1110/-113 (about 400 non-test lines), 340 commits behind upstream main.
`cli/internal/peertransport` (92 files) and `relay/` (7) import ~43 `tailscale.com` packages; none of the
Tailscale control plane. Dropping Tailscale itself is a rewrite; dropping the fork is small. Per patch:

| Patch | Plan |
|---|---|
| Underlay packet trace (magicsock, ~60) | Delete; `mesh/underlay_diagnostics.go` reads it, so drop that too or use upstream `Conn.InstallCaptureHook` |
| Cloud metadata probe skip (cloudenv, -7) | Delete; call `cloudenv.Get()` in a goroutine at daemon start (cached, takes the 2 s off the connect path) |
| STUN-only regions never home (netcheck, derp.go, `HasDERP`, ~90) | Server never publishes a STUN-only region; put STUN-only nodes inside a region with a relay node, then delete |
| Per-family local endpoints (~40), relay re-discovery after rebind (3) | Upstream PRs with their tests; drop when merged |
| Peer relay policy (udprelay, ~230) | Upstream authorizes once, before `AllocateEndpoint`, via `PeerCapabilityRelay`/`RelayTarget` (`feature/relayserver/relayserver.go:136`) and revokes by stock lifetimes (30 s unbound, 5 min idle; `net/udprelay/server.go:60`). Copy that: drop the per-packet authorizer and policy deadline; keep `RevokeEndpoint` only if instant revocation is a product promise and the limiter only for per-plan bandwidth (USER DECIDES). ~230 -> ~60 or 0 |
| DERP carrier factory (~350) | Keep. Upstream relays are TCP only; Tailscale documents DERP head-of-line blocking and TCP meltdown under load/loss and moved to UDP peer relays. Our fallback, QUIC datagrams (`relay/derpquic`), is the better design there |

Also: the derpquic relay drops on its 64-slot per-peer queue (`server.go` select/default); a loaded loopback
benchmark lost up to 1.5 %. Tune before measuring under loss (two windows-test VMs, forced relay, `tc netem` 1/3/5 %
loss + 100 ms: transfer rate, terminal echo, network switch). `cli/upstream/PATCHES.md` names fork commit
ac1095ce while both go.mod files pin 2d32e5f0.

## macOS Developer ID signing — after launch

Release builds are ad-hoc signed only (`.github/workflows/release.yml`), so macOS treats every `pb` update as a new app: the local-network permission prompt returns after each update and the identity macOS keys permissions on is not stable. Needs an Apple Developer Program membership (paid, user decision 2026-11-16: not now), a Developer ID Application certificate in CI secrets, `codesign --options runtime` with that identity and notarization (`notarytool`) in the release job, and a check that an update keeps the local-network grant.

## SDK, direct API and MCP — after launch (user 2026-11-16)

We need a Go SDK (sdk/go exists in part), a direct token-authenticated HTTP API that ships with it (scoped like a recipe,
attributed in Activity; API tokens were replaced by recipes), and an MCP server / agent skill so AI agents can use Paperboat.
Design each when it is picked up; earlier F24 design notes are in git history (WHITEBOARD before 2026-11-16).

## Terminal recordings: asciicast v3 and player features — after launch (user 2026-11-17)

Today recordings are asciicast v2 segments, `pb recordings export` writes v2, and the console replays with the same
wterm + Ghostty renderer as live sessions (seek, 1–8× speed; the CLI also has `--idle-limit`). Decision: keep v2 and our
own player for launch (v2 is what most tools read; one renderer keeps replay identical to live). Later, in order:
idle limit in the console player; markers and keyboard shortcuts in our player; optional
`pb recordings export --format v3` (asciicast v3: relative intervals, marker/exit events, header term metadata) with v2
staying the default. Not planned: switching to asciinema-player (it brings a second terminal emulator).

## Tunnel redirect and rewrite rules like Cloudflare — after launch (user 2026-11-18)

Today a tunnel has simple path redirects plus header and URL rewrite rules. After launch, research what Cloudflare
offers (Redirect Rules, Bulk Redirects, Page Rules, Transform Rules: www ↔ apex, HTTP → HTTPS, wildcard and regex
matches, query-string handling, status codes 301/302/307/308, conditions on host/path/header/country) and implement
the useful set for tunnels and custom domains.

## Held for after launch — decided 2026-11-18

- **Server breach readiness** (decided 2026-11-18): one break-glass operator action that rotates every account's
  SSH CA, the machine-trusted signing keys and session keys and signs out every browser, CLI, API and recipe session,
  with progress in the admin panel; key rollover that machines accept without a reinstall; a timed staging drill.
  Later, when revenue allows: CA and signing keys in a KMS/HSM so a breach cannot copy them. Before launch we only
  check that release signing cannot be done from the prod server and write the private runbook.
- **Package repositories (apt/yum/Homebrew with GPG, like Tailscale)** (decided 2026-11-18): after launch, publish signed
  apt, yum/dnf and Homebrew repositories so `apt install paperboat` and `brew install paperboat` work and update with the
  system. At launch first install is the fixed installer verified with the release key (`cli/tools/install.sh`).
  Prior art: Tailscale `scripts/installer.sh` (GPG-signed apt/yum repos) and NetBird `release_files/install.sh`.
- **KMS/HSM for CA and signing keys** (decided 2026-11-18): when revenue allows, hold the CA and signing keys in a KMS/HSM so a
  breach cannot copy them.
- **Status page:** move to a hosted provider (incident.io / Atlassian Statuspage) when revenue allows; swap the adapter.
- **Status page: move to a hosted provider when revenue allows; swap the adapter.** Ours (`status/`) stays until then. The seams: probes post through
  one client (`tools/probe/status_report.py`), and the admin panel reaches incidents and maintenance through one server-side interface
  (`admin/lib/server/status-provider.ts`); a hosted provider (incident.io, Atlassian Statuspage) means one new adapter there plus publishing
  component states from `status/lib/tick.ts`.
- **Shared contracts not merged yet** (skipped on purpose by the dedup worktree; code health only, no user-visible gain):
  release index (move release policy to `shared/`, validate at the release gate and in the client), terminal attach
  descriptor (an optional field becomes required; client and server deploy together), tunnel config snapshot
  (hash-relevant field tags and two diverging validators must become one). Also still to check: inspector, tunnel view
  types, config assignment.
- **Crash-loop restart backoff and candidate-rendered service units** — landing before launch if the device proof
  passes; otherwise first item after launch (branches `restart-backoff-reapply`, `t3/d55d53e1-candidate-units`).
- **Errors back to Sentry on 2026-12-02** when the free quota resets (private ops runbook),
  then remove GlitchTip. Paid Sentry plan once Paperboat earns money. Until then GlitchTip takes errors only; Sentry logs,
  traces, release health and cron monitors are paused.
- **Release safety still post-launch by design:** upgrade matrix (old/new client × server × edge) and release channels
  (dev, beta, stable).
- **Admin panel data:** device/browser and country insights need the console and site to send `$browser` /
  `$device_type` with their events; retention needs new explicit events (privacy decision first).
- **TUI:** `huh` forms only if a real multi-field form appears (e.g. `pb env set` name + hidden value); a real Windows
  console check of the narrow-terminal hint (headless runs are fixed at 120 columns).
- **Edge error pages:** four 401 answers (bad/expired token, duplicate cookies, malformed machine token, anonymous
  non-navigation) — keep only if they never differ between an existing private name and a missing one.

## Admin panel: actions with no backend yet — after launch (2026-11-18)

Support, Sentry and abuse dispositions; backup and maintenance writes; selfhost verify-ingress and pools as an operator;
usage-key and mint-key lists (the panel asks for ids today); release-authority approve/reject and staged rollout; SSH CA
list and revoke; a server API to pre-mint or cancel a `pbh` enrolment code (today `pbh selfhost code` mints it on the
node, 15 min). Launch-day operator needs (operator management and MFA reset, session revoke, plan grants, server-node
resume) are being built before launch.

## Plan grants in the customer console — after launch (2026-11-18)

A support plan grant (admin panel, `plan_grant`) is a workspace row with a product but no Stripe customer or subscription, and the
customer billing API does not say so. The console therefore shows a granted workspace like a paid one: Manage billing (the portal
needs a Stripe customer), seat changes (need a subscription) and top-ups (need a customer) are offered and fail, and Upgrade is
hidden while the product code is set. Add a `granted` flag to the customer workspace answer, hide those controls and show "Plan
granted until <date>" with Upgrade still available (a checkout replaces the grant). Revoking a grant on a workspace that never had a
customer resets it to Free, so only the running grant is affected.

Decided for plan grants (2026-11-18): a grant keeps the workspace's `bandwidth_anchor`, as any cancelled-and-repurchased Stripe
workspace does; seats are capped at the team member maximum (128) with no floor against current members; a revoke that races a
succeeded checkout whose webhook has not arrived loses to the payment.

## DMCA designated agent — after launch (user 2026-11-19)

- Register the agent at dmca.copyright.gov ($6, 3 years), using the address already on the legal pages and abuse@mail.paperboat.cloud.
- The form needs a phone number: use a forwarding number (e.g. Google Voice), never a personal mobile. The site shows none.
- Stripe requires a public support phone (shown on receipts) and will not remove it: swap it for the same forwarding number.
- Amend the designation when the company is incorporated.

## Self-hosted control server through pbh — after launch (user 2026-11-19)

- `pbh` already installs self-hosted relays and edges. Extend it to install the whole server (API, database, console), so a company can run Paperboat entirely on its own machines.
- Needs real work first: database setup and upgrades, sign-in without our WorkOS account, email, TLS and domains, backups, and updates through the same signed release path.

## Multi-region control servers — after launch (user 2026-11-19)

- Route the one API address to the nearest healthy region with a DNS load balancer (e.g. Cloudflare geo steering), like Tailscale's single control address. `pb` and the console stay unchanged.
- Each region's server already reads from its own replica once it has caught up with the client's commit position (`PAPERBOAT_DATABASE_REPLICA_DSN`); writes go to the primary.
- Server test speed: split the PostgreSQL `-race` suite across hosts and cache unchanged packages (about 25–30 min today).

## Scale: the peer-network config is rebuilt for the whole account on every change (O(N²) fan-out)
Measured on prod 2026-11-19 (server log "peer network configuration slow"): 2,187 slow builds in 2 hours, each 300–400 ms for an account with 88–169 peers (`build_ms` 281–382, `sign_ms` 5–12), every peer re-fetching the full document when any peer changes (a new machine, a `home_relay` change). That fan-out is why a busy account amplifies Postgres predicate-lock pressure: on 2026-11-19 08:48–08:56Z the pool (`max_pred_locks_per_transaction` 64 × 100 connections) filled and a revoke hit its 10 s deadline. Direction: send per-peer deltas (the mesh log already has `mesh_seq`) instead of the whole document, cap build cost per account, and build under a read-only snapshot (`InReadOnlySnapshot`) so a build takes no SIREAD locks. Prior art: Tailscale/headscale send map deltas (`PeersChanged`/`PeersRemoved`), not full maps.

## Drop control_tunnel_nodes.capacity_limit
drop control_tunnel_nodes.capacity_limit one deploy after nothing reads it

## Next server deploy after this release: drop recipes.source_format
Migration 447 made `recipes.source_format` optional (the server no longer writes it: one HuJSON file format). The server of the deploy that ships 447 no longer reads the column either; the migration that drops the column ships one deploy later (AGENTS.md: a column the previous server reads is dropped one deploy after the code stops reading it): `SET LOCAL lock_timeout = '5s'; ALTER TABLE paperboat.recipes DROP COLUMN source_format;` (forward-only Down).

## Edge and relay regions by where machines are — after launch (user 2026-11-20)

- Launch with one edge+relay region in the US and one in the EU; add the next region (likely Singapore for Asia/India) when machines there reach about 10–15% of active machines.
- Record a country per machine and per sign-up: GeoIP of the connecting public IP at enrollment/attach (store the country only, never the address); admin shows active machines and sign-ups by country. Today nothing stores a location (`users`, `user_machines` have no country; PostHog GeoIP is on for the marketing site only).
- Relays need no change: machines already measure every relay and pick the best (`cli/internal/peertransport/tailnet/regional_selection.go`).
- Edges do: an account's edge pair is chosen when its first machine attaches and kept while healthy (`server/internal/edgeset/reconcile.go` `EnsureTx`, `select.go` `choose`), so a new closer edge only gets new accounts. Move existing accounts make-before-break: the machine opens a carrier to the new edge, the new edge serves once its previews/tunnels are ready, the old one drains its open requests and is dropped. Prior art: cloudflared keeps several connections to different colos and replaces them one at a time. Test: a preview keeps serving during the move.
