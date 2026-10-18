# Console notes

Open findings about control-plane contracts the console depends on.

## `shared` on machines does not mean "shared with you"

`GET /v1/machines` items carry `shared: boolean`, but the server sets it when the machine is
bound to a team (`team_resource_bindings`, or `owner_team_id` set), not when someone else
owns it.

- Source: `paperboat-server/internal/usermachines/shared_access.go` (`machine.Shared = …` from
  the `team_resource_bindings` query) and `service.go` (`m.Shared = true` for team-owned rows).
- Effect: the first console build labeled the user's own team-bound machines "Shared with you".
- Console fix: ownership is derived from `owner_account` vs. `GET /v1/me` `id`, and
  `owner_team_id` for team machines (`machineOwner` in `lib/api.ts`). `shared` is not read.
- Server follow-up: rename the field to what it means (for example `team_bound`) or add an
  explicit viewer-relative field, and update producers, consumers and tests together.

## Other contract gaps

- **Team `suspended` is not in OpenAPI.** `GET /v1/teams/{id}` returns `suspended`, but `TeamTeam` omits it, so
  `lib/teams.ts` adds it by hand; the Team page shows a recovery notice from it.

- **Unknown `/v1/` paths answer 401, not 404.** The control plane's `/v1/` fallback is the
  edge-control handler (`router.go`), so a path it doesn't serve looks like an expired session
  (`unauthenticated`). The console now confirms with `/v1/me` before sending anyone to sign in
  (`lib/api.ts`); unknown routes should return 404 `not_found`.

- **OpenAPI is missing response schemas** for `GET /v1/me`, `GET /v1/machines` and
  `GET`/`PATCH /v1/machines/{machine_id}` (only a description), and `/v1/me` returns a generic
  `SuccessResponse`. The console's `Me` and `Machine` types are therefore hand-written in
  `lib/api.ts`; everything else comes from `lib/api-schema.d.ts`. Adding these schemas on the
  server lets the console drop its copies.

- **No bandwidth limit.** `GET /v1/usage` reports bytes but no plan cap exists, so the
  Bandwidth card shows usage only.
- **Machine limit is not shown.** Free accounts allow 100 machines, so the home card shows the
  machine count and how many are online instead of "of N" (`machine_limit` stays unused).
- **Adding machines to a team** (interim, see "Teams" below) shares one of your personal
  machines: `POST /v1/teams/{team}/machines` `{operation_id, expected_generation, machine_id,
  action: "share"}`, fenced by `generation` from `GET /v1/teams/{team}`. Sharing alone grants
  no access (`team_machine_grants` decide it), so this dialog is not the final flow.
- **Sign-in return path.** The server only signs `return_to` values under `/dashboard`
  (`validDashboardReturnTo` in `internal/auth/auth.go`). The console keeps its own
  `pb-return` cookie instead.
- **CLI approval pages.** `pb login` machine approval and logout-return pages lived in the old
  `paperboat-dashboard` and do not exist in the console yet.
- **Machine list paging.** The console requests one page of 200 machines; past that the table
  says "Showing 200 of N" and typing searches the whole workspace on the server (`?q=`).
- **Machine hostname.** The payload carries no hostname; the console builds `<alias>.<domain>` from
  `local_access_base_domain` in `GET /v1/client-configuration` (`clientConfigQuery`, no domain is
  hardcoded). The same response gives `installer_base_url` for the self-host install command.
- **Updates are personal only.** `GET /v1/machines/update-summary` uses the personal workspace,
  so team machines show no update state. Update = request + approve a maintenance approval; if
  the approve call fails, the pending request remains until it expires.
- **Custom machine domain.** Domains lists machine domains (browser-local for now). Storing them
  needs the server to keep them per account and the CLI/daemon to resolve them.
- **Hide my email.** Replaces the address with random letters of the same shape. A per-browser preference (`lib/privacy.ts`) until the control plane stores
  it so the CLI and other clients can honor it too.
- **Previews and tunnels.** One tunnel resource underneath (`pb preview` = `pb tunnel
  --ephemeral`), but two products for users: Previews (temporary, ngrok-like) and Tunnels
  (durable, Cloudflare-Tunnel-like). The console keeps two sections over one shared
  implementation filtered by lifecycle. The API still has separate `/v1/previews` and
  `/v1/tunnels`.
- **Install commands.** `/v1/machine-enrollments` returns a token and a POSIX template; the
  console extracts the install URL from `bootstrap_command` and builds both shells' commands
  (`lib/enrollment.ts`). The server should return the per-shell commands itself. Machine names
  are validated against a mirror of `machinealias.Valid`; descriptions are applied with
  `PATCH /v1/machines/{id}` after pairing because the installers cannot carry them.
- **Docs.** The sidebar Docs button is a placeholder until a docs site exists.

## Sections waiting on server APIs

The sidebar has these sections; each needs the listed control-plane support.

- **Network** (self-hosted relays and edges). `GET /v1/edges` lists hosted edges for reading.
  Missing: registering, health-checking and removing a self-hosted relay or edge per account,
  and choosing it per machine or workspace.
- **Domains**: see "Domains page" below for the account-level API the console calls.
- **Usage** (detailed bandwidth). `GET /v1/usage` already filters by `machine_id`,
  `resource_kind`, `mode` (direct/relayed), `hosting` (self-hosted/managed) and returns a time
  `series`, which covers per-machine, per-path and over-time views. Missing: a quota/plan cap.
- **Activity**: see "Usage and Activity" below for the read API the console calls.

## Edges and relays

- **Edge selection.** Today each account is served by two edges chosen by the server
  (`GET /v1/edges`). Planned: let users pin edges themselves: one, both, or neither (automatic),
  per account or workspace. The machine page's Network section lists the account's edges with
  place name, hosting (Paperboat or Self-hosted), status and latency.
- **Self-hosted edges** are excluded from `GET /v1/edges` (`edgeplacement` skips
  `selfhost_installations`); the list needs a hosting field so they can appear marked
  "Self-hosted".
- **Latency** from each machine to each edge and relay is measured by the daemon but not
  reported (below); the Network section shows "—" until it is.
- **Development samples.** Under `next dev` only, when a machine reports no relays, the Network
  section fills in sample relays and edge latencies (one of each marked self-hosted) so the layout
  can be reviewed (`sampleRelays` in `components/machine-overview.tsx`). The UI does not label
  them; production builds show only reported data. Remove them once machines report latency.

## Machine domains (dummy UI)

Every machine answers on `<name>.<domain>` for each of the account's machine domains. The Domains
page lists them (with tunnel and preview domains, below): Paperboat's own (`local_access_base_domain` from the client configuration)
cannot be removed; custom ones can be added and removed; exactly one is the default, shown
wherever an address appears. The machines table shows the default-domain address (click to copy);
machine details show it with "+N", and clicking it opens every address to copy, as does the header's IP menu. The list starts as Paperboat's domain alone, as default.

Limitations, all from there being no server support yet:

- Stored per browser in `localStorage` (`lib/machine-domains.ts`); nothing reaches the server and
  other browsers see the default list. Only that module changes when an API exists.
- How a custom domain works: the user adds a wildcard A record `*.example.com → 127.100.0.1`
  and adds `example.com` here; `name.example.com` then reaches the machine from any computer
  running Paperboat (Add domain says this).
- DNS check (built, console only): `GET /api/domain-check?domain=` in the console's own server
  confirms the caller is signed in (`/v1/me`), then resolves `pb-check-<random>.<domain>` and
  reports ready / wrong address / no record / couldn't check. A random label proves the record is
  a wildcard. Domains shows the result on each row, "Make default" stays off until it is
  ready, and "Check again" re-runs it (results are cached 5 minutes). It checks public DNS
  from the console server, not from each machine.
- Setup instructions (built): a domain that isn't set up shows one line under it, "Add record
  A `*.home` → 127.100.0.1" (Name relative to the DNS zone the check route finds from the first
  parent with an SOA record; each part copies on click), what it points to now, and "Check again".
- Removing a custom domain has no confirmation, and nothing checks whether anything (bookmarks,
  configs) still uses it.
- Server side, `machineservices.BrowserSuffix` is a single fixed suffix; per-account domains and
  a default are missing.
- On machines, `pb` serves one browser domain per install: `local_access.domain` in its config,
  approved by an administrator because it installs a local HTTPS certificate authority limited
  to that domain (`splitdns.NormalizeBrowserDomain`, `machineguard`, `browser-domains.json`).
  A list of account domains needs the daemon to fetch them and get each one approved per machine.
- Backend work needed for custom domains to actually work, in order:
  1. Server: store an account's (and team's) domains and default, with an API to list, add,
     remove and set default; re-run the DNS check server-side when saving.
  2. Server → machines: deliver the domain list to each machine (machine policy or heartbeat).
  3. `pb`: accept several browser domains instead of one, and ask once per machine for
     administrator approval of each new domain's local HTTPS certificate authority; report per
     domain whether it is approved so the console can show "waiting for approval on homelab".
- Team workspaces will need their own list (who may change it: owner/admin).

## Bandwidth card

Shows all traffic Paperboat carried for the workspace's machines in the chosen period (24 hours,
7 or 30 days): direct, relayed and via edges, managed and self-hosted,
from `GET /v1/usage`, with the server's series as a trend line and a Direct / Relay / Edge split
summed from `resources[].paths`. The split is hidden when the response is paged (`next_offset`),
since totals include everything but the per-path detail would be partial. Limitations: the
server says these are application byte observations, separate from billing, so the card is not
a bill; direct (peer-to-peer) bytes depend on machines reporting them (`coverage.native`), which
the card does not surface yet.

- **Source menu** (Combined / Managed / Self-hosted) filters by the `hosting` of each
  path in `resources[].paths`. Direct traffic has no relay or edge (`hosting: none`), so it
  counts only for Combined. Managed and self-hosted totals come from the path detail, so they
  are unavailable when that detail is paged; there is no series per hosting either, so outside
  development the chart shows only for Combined.
- **Backend gap: no series per path.** `series` is one combined line; the direct / relay / edge
  split exists only as period totals in `resources[].paths`. Overlaid lines per path (wanted on
  this card) need `series` entries with a `mode` (or per-mode series) from `GET /v1/usage`.
  Until then, under `next dev` only, the chart draws sample Direct / Relay / Edge lines
  (`samplePathSeries` in `components/usage-limits.tsx`, shaped from the real combined series);
  production draws the single combined line. Path colours are `--path-direct/relay/edge`.

## Running column

The machines table's Running column says what each machine is running on one muted line,
abbreviated with every kind always shown ("3 term · 0 tun · 1 prev", full words on hover); "—" only when the counts could not be
loaded.

Limitations, all from the missing per-machine count API:

- The console groups three inventories by machine: `GET /v1/terminal-sessions?state=running`
  (`target.machine_id`, account-wide, first 200 only since it pages by offset),
  `GET /v1/tunnels?state=active` (`created_by_host_id`, the creating machine's ID) and
  `GET /v1/previews?state=active` (`owner_machine_id`), following cursors up to 10 pages of 200.
  Large accounts can undercount. A counts field on the machine list would replace all three.
- A tunnel can have connectors on several machines; it is counted on the machine that created it.
- The three lists refresh every 30 seconds, separately from the machine list (15 seconds).

## Teams (agreed direction, not built)

Product decisions so far, and what the console will show:

- **Creating a team is paying first.** Create team → name → number of seats ($15 per seat per
  month, the owner takes one) → Stripe Checkout → the team exists once payment succeeds.
- **Seats are bought, invites use them.** Billing shows seats used / bought, the next invoice
  date and amount, payment method and invoices (Stripe portal). Inviting with no free seat asks
  "Add a seat: $15/month, prorated, charged now" before sending. A pending invite holds a seat;
  cancelling or expiry frees it. Removing a member frees a seat but does not lower the bill until
  an owner removes seats.
- **One billing date.** The cycle never moves. Seats added mid-cycle are charged at once for the
  days left until the billing date (Stripe proration, `always_invoice`); from the next date the
  bill covers all seats. Seats removed stay paid until the billing date, with no refund.
  Example: 5 seats on 12 May = $75; 3 more on 18 May ≈ 3 × $15 × 25/31 = $36.29 that day;
  12 June = 8 × $15 = $120.
- **Invites are always by email**, including people without an account yet.
- **Roles:** owner (billing, delete, transfer ownership), admin (members, machines, access),
  member (uses what they are granted).
- **Adding a machine** in a team enrolls it straight into the team (team-owned). Owners and admins
  add directly; a member's new machine waits for approval (team setting, on by default), like
  Tailscale's device approval.
- **Default access is shown while enrolling:** "Who can use it: everyone in the team" with what
  they can do (terminal, files, SSH, previews, tunnels), changeable there and later on the
  machine's Access section.
- **Moving machines:** personal → team by the machine's owner (team member; needs approval when a
  member does it); team → personal by an owner/admin, into their own Personal. Moving ends live
  sessions, removes the machine's machine-scoped environment values (they belong to the old
  owner's vault; the confirmation says how many), switches config sync to the new owner's
  default, revokes old access grants, and asks to rename on a name clash. Public previews and
  tunnels are stopped as part of the move (listed in the confirmation) instead of blocking it.

Backend gaps against this (current behaviour in brackets):

- Team creation tied to checkout [team is created free; checkout is separate on
  `/v1/billing/workspaces/teams/{id}/checkout`].
- Bought seats with free-seat accounting and "add a seat" at invite [Stripe quantity is reconciled
  to the active member count, `ReconcileSeats`]; seat price readable for the UI; proration preview.
- Invites by email, for people without accounts, with a longer expiry [account ID only,
  `InvitationLifetime` 24 h, everyone joins as member].
- Enroll straight into a team [`POST /v1/machine-enrollments` takes only `shell`].
- Machine approval state and a team setting for it [none].
- A default grant applied when a machine joins a team, settable at enrollment [grants are
  separate `POST /v1/teams/{id}/machine-grants`; a bound machine without one is unusable].
- Team → personal transfer [only `share`, `unshare`, `transfer_to_team`, `remove`].
- Moving wipes machine-scoped env values, ends sessions, stops public publications [the move is
  rejected while public previews/tunnels exist; env handling unspecified].
- Members leaving: what happens to machines they shared [unspecified].

## Environment

The Environment page and a machine's Environment tab are a Vercel-style list: names, set, remove,
never a value. Record ids are hashed and names live inside the encrypted record, so the vault is
opened in the browser with its password (`lib/envvault/`): the CLI's own Go crypto
(`cli/cmd/environmentvault-wasm`, built by `scripts/build-environment-vault-wasm.sh`) runs in a
worker, keys exist only in its memory (terminating the worker is the lock; 10 minutes idle or page
leave), and the page makes the same `/v1/environment-vault`, scope, record and layer calls as
`pb env set`. After each set or remove it seals host layers for machines that have no key for the
scope yet (the CLI's `refreshENVRecordScope` step), applying stricter recipient checks than the CLI.

- Scopes: Personal, the selected Team workspace's shared set, and a machine's own override (its tab).
- Not in the browser: vault init, recovery code, password change, reset, Team key grants. The page
  points to `pb env vault init` when no vault exists.
- Delivery status per machine (`/v1/environment/layers`) is not shown yet.
- Browser check: `scripts/check-environment.js` (F10-06).

## Machine capabilities

Capabilities (terminal, sessions, file transfer, previews, environment) are not user-toggleable.
The console shows only what affects use, and does not offer switches to turn them off.
**Keep awake** and **Allow sleep** are the two values of one availability setting
(`desired_mode` is `keep_awake` or `allow_sleep`); the machine page shows it as "Keep awake: On/Off"; those who manage the machine change it from a
caret menu on the value. A muted note follows while `availability.status` is pending ("Waiting for
<machine>"), error or unsupported (no menu). The server reports `offline` while the machine is
offline. Limitation: machines confirm through the privileged host service, so a machine whose
host service never reports stays pending indefinitely, and the console cannot tell that apart
from "not yet"; the server needs to report whether a host service is present.

## Terminals

The terminal is real: `lib/terminal/use-terminal-connection.ts` is paperboat-dashboard's browser
console engine (WTerm + Ghostty, end-to-end encrypted through the WASM worker in
`public/terminal/`) with its protocol handling unchanged. It renders at `/terminals/{id}`
(its own tab, outside the console sidebar: top bar, terminal, People panel, Share) and in the
machine page's quick terminal. Next: several terminals in one tab (tabs or splits) that everyone
in it can see. Not yet exercised
against a live machine from this console; that needs a signed-in browser on the console origin.

Server gaps the UI works around:

- **Participant identity.** `participants[]` carries `account_id` only, so people other than you
  and the owner show as "Participant 9f3a". Needed: display name and email (owner-visible), or
  an endpoint that resolves account ids the viewer is allowed to see.
- **Changing someone's role while they're connected.** Typing is granted by email
  (`interactive_emails`) and participants have no email, so the People panel can only remove
  people. Needed: set viewer/interactive by account id on a live terminal.
- **Who is typing.** No input-attribution event, so the panel cannot show who is typing now.
- **The list knows little about open terminals.** `GET /v1/terminal-sessions` returns no
  participants (`participants_available: false`) and no start time, and `last_active_at` is often
  empty while a terminal is open. So the list can't show who's in a terminal or when it started;
  open terminals are listed as current, and only closed ones group by date. Needed: participant
  count (or ids) and `created_at` in the catalog.
- **Live presence pushes.** No event stream for joins and leaves; the page polls `sharing`
  every 4 s.
- **Guests in the People panel.** Code access lists admitted guests, not who is connected;
  `participants[]` does not say whether guests appear there.
- **Reading the join code back.** The code comes back only in the response that creates it, so
  the owner's browser keeps it until it expires (`lib/terminal/invite.ts`); another browser can
  only show it by making a new one, which disconnects guests who used the old one. Needed: an
  owner-only read of the live code, as Meet and Zoom show theirs.
- **Joining with a code** works without an account: `/terminal/{id}` sends people with access to the
  terminal and asks everyone else for the code and a name. The console relay lets exactly
  `code-join` and `guest-browser-ticket` through without a session (the control plane checks the
  origin and the terminal's guest cookie). Guests see no participants: `browser-session` returns
  none for them. Invites and the Share link carry `?join=code`, which only picks the first screen
  (code form vs. sign in); the server alone decides access and never says whether codes are on.
- **Signed-in people without access join with a code as guests.** `code-join` ignores the account,
  so they show up under the name they type, not their identity. Linking the guest to the account
  (and showing their email to the owner) needs a server change.
- **Email and code invites are browser-only.** The server marks those terminals `browser_only`
  ("native join, machine and team grants do not follow from it"), so `pb session join` only works
  for team grants and the list hides "Continue in local terminal" for them. Letting invitees join
  from the CLI needs the server's join check and the CLI to accept browser-policy access.
- **`browser-session` reports the raw runtime state** (`running`) where the other endpoints say
  `attached`/`detached`; `terminalCanAttach` accepts both. The server should use one vocabulary.
- **OpenAPI.** `POST /v1/machines/{id}/terminal-sessions` (body `{ name? }`, returns the session)
  and `POST …/browser-ticket` have no schemas; `Machine.permissions` is typed by hand.
- **Runtime build target.** `paperboat-oss/cli/cmd/browserterminal-wasm/build-dashboard-assets.sh`
  still writes to `paperboat-dashboard/public/terminal`; copy into the console's
  `public/terminal` when the WASM changes, or point the script here once the old dashboard retires.

## Previews and tunnels

One server resource (`paperboat.preview-tunnel/v1`) with two lifecycles. The console lists,
inspects, changes and stops them (`lib/tunnels.ts`, `components/tunnels/`). Gaps:

- **Previews are created from the console**: `POST /v1/previews` with the browser session and a
  `foreground` owner (a fresh `console_<random>` session ID), which the server dispatches to the machine's
  daemon (`preview_launch`); the dialog polls `GET /v1/previews/{id}` until `ready`. `local_lease` sessions are only for
  the CLI's own hostd lease; the console must send `foreground`.
- **Tunnels are created with `pb tunnel create` on the machine.** The control plane requires the
  machine's signed identity and local connector, so New tunnel builds the exact command (name, port,
  `--private`, `--workspace` for a team) with a copy button instead of a form; the tunnel then appears
  in the list. Previews are created from the console (`owner_session_kind: "foreground"`, a fresh
  `console_<random>` session ID).
- **Tunnel address is not editable.** The server patches only `name`, `access_mode` and `expires_at`
  (`TunnelPatchRequest`); the address is generated, so the console renames the tunnel only.
- **Share a file (drop.new-style) is UI-only for now.** Previews has "Share a file" and a page-wide
  drop target. The browser sends `POST /v1/previews/files?workspace=&machine=&name=&access_mode=&expires_at=`
  with the raw file as the body (streamed through the relay, Idempotency-Key) and expects a
  `PreviewLease` or operation like `POST /v1/previews`, then polls the preview until `ready`.
  Needed: the server streams the body to the machine's Paperboat Inbox without storing it (no
  store-and-forward); the daemon serves that one file as a preview with target
  `{ scheme: "file", address: <inbox path> }`, inline for HTML, Markdown (rendered), PDF, images and
  text, `Content-Disposition: attachment` for everything else; the file is deleted from the Inbox
  when the preview ends; a size limit per workspace. The console labels `file` targets by file name.
- **Public hostnames** are the console's name for a route plus, for your own domain, its domain
  binding; adding/removing one does both calls. Removing a domain then a route is two requests,
  so a failure in between leaves the route; the row stays and can be removed again.
- **Preview leases carry no ETag in the body** (tunnels, routes and domains do), so stopping one
  reads it first for the `ETag` header. Put `etag` on `PreviewLease`.
- **No stop time on previews.** Ended previews show `last_renewed_at` (or their deadline) as
  "Ended". Add `stopped_at`/`ended_at`.
- **`team` access.** The server accepts `access_mode: team` and the CLI has `--team`, but the
  OpenAPI enum is `public | private`, so the console offers only those two.
- **Tunnel list filters.** `GET /v1/tunnels` ignores `state` and returns deleted tombstones; the
  console drops `deleted`. `summary_code` values (`ready`, `pending`, `route_unavailable`,
  `custom_domain_required`, `expired`) aren't enumerated in the OpenAPI document.
- **Route and domain endpoints** are registered only when the server has `TunnelResources`
  configured; elsewhere they 404 and the sections show the error.
- **Relay** forwards `If-Match` now (tunnel, route and domain changes need it).
- Not yet exercised against real previews/tunnels; checked with stubbed API data only.

## Files (machine Inboxes)

The Files section lists each online machine's Paperboat Inbox (`lib/files.ts`, `components/files/`).
Privacy: Paperboat stores no Inbox file names, sizes or contents. Every call goes through the server
to the machine, live, on the same server-to-daemon HTTP boundary previews use (a 90-second single-use
credential per call, bound to the machine, the operation and the file):

- `GET /v1/machines/{id}/inbox?workspace=` → `{ total, items: [{ id, name, size, received_at }] }`,
  newest 200. 409 `machine_offline` when the machine is offline, so the page says "Machine offline —
  Inbox unavailable"; 404 for machines you may not manage (owner, or team owner/admin only).
- `GET /v1/machines/{id}/inbox/{file_id}/content` → the file as an attachment, streamed through
  (2 GiB limit, 30 s idle limit). `DELETE /v1/machines/{id}/inbox/{file_id}` needs an
  Idempotency-Key and the CSRF token. Errors: 404 `inbox_file_not_found`, 409 `machine_offline`,
  413 `file_too_large`, 503 `machine_unreachable`.

Download and Delete are behind `inboxLive` in `lib/files.ts` until verified against a real machine
after the server and daemon deploys. Not in the contract (so off): sending files from the console,
Send to another machine, and Share link. Machines record no sender, so there is no From column.
Team Inbox requests use the existing `GET /v1/team-inbox/requests` and `POST …/{id}/approve|decline`
(`expected_generation`); they carry account IDs only, so a request reads "A teammate wants to send…".

## Config sync

Read-only (`lib/config-sync.ts`, `components/config/`), from the existing
`GET /v1/config-sync/status`: per machine the repository, direction (`bidirectional`, `pull_only`,
`push_only`), managed file count, state, and the `conflicts`, `review` and `skipped` paths with
their reasons. By decision, conflicts are only reported; they're resolved on the machine
(`pb config conflict`), and incoming changes are approved there (`pb config approve`). Path
`reason` and `recovery_actions` values aren't enumerated in the OpenAPI document, so the console
shows them humanized (`changed_both_sides` → "Changed both sides").

## Domains page

Where the workspace is reachable by name (`lib/domains.ts`, `components/domains/`): Paperboat's machine
domain (`local_access_base_domain`), machine domains you add (browser-local, a wildcard A record to
the local gateway, DNS check by the console's own route), and the custom hostnames tunnels publish
(from `GET /v1/tunnels/{id}/domains`, with the state from the server: requested, waiting DNS,
verified, ...). A tunnel hostname's DNS record and verification live on its tunnel page ("Add public
hostname", instructions, verify); the Domains page only lists them and links there. There is no
account-level domain API (`/v1/domains` was never built; the first console build assumed it), and no
"verified once, used everywhere" step: ownership is proven per hostname. Machine domains still come
from `lib/machine-domains.ts` until the server stores them per domain.

## Network (self-hosted relays and tunnel servers)

`components/network/network-page.tsx`, `lib/network.ts`, on the existing `/v1/selfhost/...` API:
servers paired to the workspace (installations grouped by node: relay, tunnel server or both),
Add server (the installer command for relay/tunnel/both, then the claim address and one-time
code it prints, `POST /v1/selfhost/enrollments` with the workspace scope), Check public access
(`verify-ingress`), Remove (deletes each installation of the node), and the two pools: relays
`mixed | self-hosted-only` (account scope always), tunnel servers `managed-only | self-hosted-only`
(workspace scope). Paperboat's servers come from `GET /v1/edges`, grouped by region. Gaps:

- Team scope sends the workspace ID (the team slug) as `scope_id`; confirm the server's team scope
  takes the slug, not an internal ID.
- Installations carry no version, last-seen time or region, so the list can't show them; `ready`
  is the only health signal. Tunnel readiness doesn't say whether public access was verified.
- Per-tunnel placement (`/v1/selfhost/placements/...`) isn't in the console yet; it belongs on
  the tunnel's page once a workspace uses its own tunnel servers.
- The server rejects switching tunnel servers while previews or tunnels run; the console shows
  that error rather than listing what to stop.

## Usage and Activity

**Usage** (`components/usage/usage-page.tsx`) is on the existing `GET /v1/usage`: period (24 hours,
7 or 30 days), `machine_id` for a machine (header picker or its tab), totals with up/down, the
direct / relay / edge split summed from `resources[].paths` (shown as "—" when the server pages
that detail), the combined series as a chart with hover, and a table by resource with each one's
path mix. It notes when `coverage.native` is false. Same server gaps as the bandwidth card: one
combined series (no per-path lines), and these are measurements, not billing.

**Activity** (`components/activity/activity-page.tsx`, `lib/activity.ts`) is UI-only for now. The
server writes audit events but has no read API; the console calls
`GET /v1/activity?workspace=&machine_id=&area=&q=&cursor=&limit=50` →
`{ items: [{ id, occurred_at, action: "area.verb", actor: { kind: user|machine|system, id, name?,
email? }, target?: { kind, id, name? }, machine?: { id, name }, source?: console|cli|machine|api }],
next_cursor? }`. `area` is a comma list of action prefixes (machine, terminal, preview, tunnel,
file, environment, config, team, share, account). Events render as sentences from a table of known
actions (`describe`); unknown ones show their action name. Needed on the server: the read API with
team visibility rules (members see team events, not each other's personal ones), names as they
were at the time, and the same action vocabulary.

## Machine details worth reporting in the heartbeat

The machine payload carries capabilities, availability, runtime diagnostics and versions. A
Tailscale-grade machine page would also need, reported by the daemon and stored per machine:

- **Connectivity:** the daemon already measures per-peer latency and connection mode
  (`PeerStatus.LatencyMs` / `ConnectionMode` in `cli/internal/daemonrpc/backend.go`, shown by
  `pb status`) and probes edge latency for placement (`internal/edgeplacement`), but none of it
  is sent to the control plane. Reporting it needs a bounded, privacy-reviewed heartbeat field
  (peer graph and endpoints are sensitive). Also wanted: preferred relay
  region, relay latencies, NAT type, UDP reachability, IPv6, port-mapping support (UPnP, NAT-PMP,
  PCP), and local endpoints.
- **System:** OS name and version (kernel, macOS or Windows build), hostname, uptime, CPU and
  memory, disk free for file transfers.
- **Client:** release channel, auto-update on/off, last update result, daemon start time.
- **Security:** identity key expiry or rotation date, SSH host key fingerprints (already served
  by `/v1/machines/{id}/ssh-host-keys`).
