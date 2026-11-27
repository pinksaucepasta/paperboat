# Paperboat whiteboard

Everything here is after launch unless its group says otherwise (user decision 2026-09-13).

## Soon: right after the smoke

### Drop control_tunnel_nodes.capacity_limit
Until the commit that lists columns explicitly, the generated `RETURNING *` / `SELECT *` queries (edge register, heartbeat, stale list) read `control_tunnel_nodes.capacity_limit`, so dropping it under such a server fails every edge heartbeat. Only one deploy after that commit ships: `SET LOCAL lock_timeout = '5s'; ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN capacity_limit;` (forward-only Down, `-- contract-after:` the newest migration in that shipped deploy), then `make generate`.

### Scale: peer-network config fan-out
Machines get deltas since the signed configuration delta landed (`ConfigurationDelta`). Left: check in production that "peer network configuration
slow" builds and predicate-lock pressure on a 100+ machine account are really gone; if not, cap the build cost per account.

### From the final launch batch (26 Nov): recorded, after launch
Measured during the 22-26 Nov fix batch; none makes first use feel broken, so they wait.
- **Team revocation** 4.8 s at 1,000 machines (119 ms at 10, 561 ms at 100); branch `fix/revoke-set`.
- **Parallel opens group commit:** worse on a slow disk (4/20 vs 19/20 at 120 s), parked.
- **Slow or stalled disk:** daemon syncs ~1/s at idle; cold pair on a slow disk > 100 s to first open; closing a terminal 5-9 s on a stalled disk; a new ssh session waits on its audit write.
- **exec/ssh right after a daemon restart:** limits 5 s / 10 s; Mac peer exec once missed 5 s.
- **Managed SSH:** a stopped sshd reaches the machine list after up to ~82 s by design (2 probes 30-35 s apart + 10 s poll); `pb ssh` itself refuses in ~2 s.
- **Config builds:** cap per account at 4 (p99 114 -> 26 ms measured). Cached peer document up to 9 min stale for another owner's host. Team change notices 22 ms at 1,000 members.
- **Edge with permanently refused credentials** keeps its 5 s control loop (~45 % CPU for 12 days on a stale staging edge); `edge` control loop has no backoff or exit on 401.
- **100-session typing run** (lag fix proven at 22 sessions).
- **Not reproduced, watch for:** `pb ssh` stalled ~115 s with the store locked (Windows not checked); 4 min "offline" after a same-version install.
- **DNS cache for relay/edge QUIC dials** and the carrier TCP fallback (`connector/data_carrier_endpoint.go:164`).
- **Unreliable test:** `TestForwardExecInputSwitchesConnectionsAndReplaysTheJournal` fails in a 20x run on main.
- **Failed restore leaves a mixed set:** when the native-installation restore fails (a revoked machine whose previous carrier cannot start), `LifecycleManager.rollbackKeeping` (`cli/internal/hostruntime/service/transaction.go`, the `nativeInstallation.Restore` return) stops before the component restores, so the host service keeps the new build until the journal retry succeeds. The message says the restore did not finish (388e7b487 + cause-text fix). Idea: keep restoring components after a failed native restore and join the errors; test with an unrestorable `NativeInstallationState` and assert every component's `Restore` ran.
- **Security review (week of 27 Nov):** elevated PowerShell on Windows loads modules via the user's PSModulePath; the machine guard takes its install dir from `%ProgramFiles%`. Proposed: clean the environment once where pb elevates.

### From the smoke-gate triage (27 Nov): recorded, after launch
User rule: before launch, fix what a user would notice or feel; the rest waits here.
- **Path display:** `pb status` / the client can briefly show "direct" while traffic goes through the relay (seen on a self-hosted relay). The smoke's strict path check reports it.
- **`pb relay list`** shows only the region ("eu Europe"), no node name: decide whether that is intended.
- **Relay admission:** a relay logged a transient "relay admission refused ... fence generation 7" after a resume.
- **Windows SSH setup time:** about 200 ms over budget (smoke `ssh-setup-time`: 1322 ms, allowed 1142 ms on 26 Nov).
- **Update gate logging:** the hostd journal has no start/end line per update gate operation, only unclassified diagnostic events.
- **Self-host enrolment (operator only):** send the edge domains and browser access in the claim (node reports inspect version 2, optional `edge_config`, server sends it only to v2). Open review finding: a stopped or restarted `pbh` can leave the runtime handoff behind, so an old tunnel runs unsupervised. Work kept unpushed on local branch `fix/enrol-claim-edge-config`.
- **release.yml:** `always()` ignores cancellation in two jobs.
- **Test hygiene:** a deploy test may still expect the removed postgres service (fix now only if CI fails); some device-smoke scripts still SSH to the old deployment-host setting; the paste tests append env overrides after the user's environment, so TERM/INPUTRC/ZDOTDIR can affect the bash/zsh subtests.
- **`pb pair` on HTTP 429:** the CLI does not honor Retry-After or retry, and says "invalid machine bootstrap" (`local_io`) instead of rate limited (`cli/internal/hostruntime/bootstrap/bootstrap.go`).
- **Self-host first platform wildcard is slow with a straggler edge:** `Coordinator.Ensure` makes every first certificate all-or-nothing (`requireAll`, `server/internal/tunnelcert/coordinator.go`, `domain.CertificateReference == ""`), and that also covers a platform wildcard's first issue, whose targets are every ready edge. One slow edge on a fresh install delays it through retries; it succeeds once every edge is ready. Fix: scope that branch to durable routes (`TargetDurableRoute`, no leaf).

## Before launch: decide

### DMCA designated agent — after launch (user 2026-11-19)

- Register the agent at dmca.copyright.gov ($6, 3 years), using the address already on the legal pages and abuse@mail.paperboat.cloud.
- The form needs a phone number: use a forwarding number (e.g. Google Voice), never a personal mobile. The site shows none.
- Stripe requires a public support phone (shown on receipts) and will not remove it: swap it for the same forwarding number.
- Amend the designation when the company is incorporated.

### Flood protection (2026-11-20)
Plan bandwidth limits stop big transfers, not floods of tiny requests; per-address limits stop one attacker, not a flood from many addresses (DDoS) against a preview or tunnel. Later: DDoS protection in front of the edges (e.g. Cloudflare Spectrum or an anycast scrubbing provider) and per-route request shedding.

## Workspaces, plans and admin panel

### Per-feature switches for a whole workspace (user 2026-11-24)

A switch per feature that turns it fully on or off for a whole workspace (for example terminals, exec, SSH, file transfer, previews,
tunnels, ENV, recordings). Off means off everywhere: the server refuses it, every `pb` command and console page says it is turned off,
and what is already running stops. Open: who flips it (the workspace's owners and admins, Paperboat's operators in the admin panel, or
both), whether a team can also lock a feature on, and how it sits next to the existing team access rules.

### Every plan limit editable in the admin panel (user 2026-11-24)

Today the panel changes the Free monthly allowance and a product's allowance, and overrides counts for one workspace. Plan-wide speed
caps (slow speed after the allowance, relay speed, terminal cap), count limits (machines, previews and tunnels, browser terminals,
public TCP, viewers) and the team per-seat allowance are server configuration and need a restart. Make all of them editable per plan
in the panel, audited, and picked up by servers, edges and relays without a restart. Check first whether the panel's team allowance
field changes a paid team's pool at all (the pool is TeamPerSeatBytes times seats).

### Regions per plan (user 2026-11-24)

Let operators choose, per plan, which managed regions a workspace may be placed in (for example Free only in one region), in the
admin panel. Today a region can only be created, changed, enabled, disabled or replaced for everyone. Needs: the rule applied when a
workspace's primary and fallback regions are chosen, a defined move when a plan changes (upgrade, downgrade, grant ending) or the rule
changes, what the fallback is when only one region is allowed, and relays decided too (they carry traffic when machines cannot
connect directly). Self-hosted regions are never limited.

### More global preview/tunnel domains from the admin panel — after launch (user 2026-11-24)

One global domain serves every preview and tunnel today (server setting; its wildcard certificate is issued, renewed and sent to every ready edge
by the server). Later: a list of global domains managed in the admin panel (add, verify, pause, remove), one wildcard certificate and wildcard
DNS record per domain on every edge, and a rule for which domain a new name gets (a default, or the user's choice).

### Admin panel: actions with no backend yet — after launch (2026-11-18)

Support, Sentry and abuse dispositions; backup and maintenance writes; selfhost verify-ingress and pools as an operator;
usage-key and mint-key lists (the panel asks for ids today); release-authority approve/reject and staged rollout; SSH CA
list and revoke; a server API to pre-mint or cancel a `pbh` enrolment code (today `pbh selfhost code` mints it on the
node, 15 min). Launch-day operator needs (operator management and MFA reset, session revoke, plan grants, server-node
resume) are being built before launch.

## Previews, tunnels and network

### Generic UDP support — later

Recorded 2026-09-07 by Codex from the user's direction.

Add generic public and private UDP forwarding in a future scoped task. Preserve datagram
boundaries, bounded return-path mappings, expiry and cleanup. Public UDP needs assigned
endpoint ports, owner-authorized publication and abuse/traffic limits; private UDP needs
Paperboat-authorized encrypted native access. Application authentication remains protocol-specific.

UDP is deferred, not part of the current HTTP/HTTPS/WebSocket and raw TCP delivery tasks.
Internal QUIC support does not constitute generic UDP forwarding. Define actual client UX,
limits and acceptance before implementation; this note is not implementation/deployment evidence.

### Tunnel redirect and rewrite rules like Cloudflare — after launch (user 2026-11-18)

Today a tunnel has simple path redirects plus header and URL rewrite rules. After launch, research what Cloudflare
offers (Redirect Rules, Bulk Redirects, Page Rules, Transform Rules: www ↔ apex, HTTP → HTTPS, wildcard and regex
matches, query-string handling, status codes 301/302/307/308, conditions on host/path/header/country) and implement
the useful set for tunnels and custom domains.

### Priority relays in more regions — after launch

Recorded 2026-10-23 at the user's direction (a Pro extra, not affordable yet). More managed relay
regions with priority capacity for paid plans, so relay-only paths stay fast far from today's edges.
Revisit once revenue covers the extra servers.

Container mode, recorded 2026-10-23 with the entry above: today `pb` runs in Docker only as a full
systemd container (`--privileged`, systemd as PID 1). The standard shape (Tailscale, cloudflared) is an
official image: `pb daemon` in the foreground as the main process, pairing from a one-time key in an
environment variable, state on a volume, no extra privileges. Covers Docker hosts, Kubernetes and CI.

### Dual-stack edges (IPv6) — after launch (coordinator decision 2026-11-12)

The launch edges are Hetzner hosts with native IPv6 (falkenstein-1 already listens on it); no AAAA record points at them yet. Later: AAAA for the edge,
API and preview wildcard names, then owner IP rules and `--host-header` checked with real IPv6 client addresses (the IPv6 part of row X-20).

### Edge and relay regions by where machines are — after launch (user 2026-11-20)

- Launch with one edge+relay region in the US and one in the EU; add the next region (likely Singapore for Asia/India) when machines there reach about 10–15% of active machines.
- Record a country per machine and per sign-up: GeoIP of the connecting public IP at enrollment/attach (store the country only, never the address); admin shows active machines and sign-ups by country. Today nothing stores a location (`users`, `user_machines` have no country; PostHog GeoIP is on for the marketing site only).
- Relays need no change: machines already measure every relay and pick the best (`cli/internal/peertransport/tailnet/regional_selection.go`).
- Edges do: an account's edge pair is chosen when its first machine attaches and kept while healthy (`server/internal/edgeset/reconcile.go` `EnsureTx`, `select.go` `choose`), so a new closer edge only gets new accounts. Move existing accounts make-before-break: the machine opens a carrier to the new edge, the new edge serves once its previews/tunnels are ready, the old one drains its open requests and is dropped. Prior art: cloudflared keeps several connections to different colos and replaces them one at a time. Test: a preview keeps serving during the move.

### Congestion control: BBRv3 in the quic-go fork — after launch, when funded

- Today we ship BBRv1 (Hysteria's port) in our quic-go fork (`github.com/pinksaucepasta/quic-go`, branch `paperboat`); it is the only sender for every connection.
- Later, when funded: port BBRv3 (Google, IETF draft-ietf-ccwg-bbr; Chrome quiche `bbr2` code) into the fork and re-run the benchmark, including the fairness and relay-limit tests.
- Reason: v3 fixes v1's loss blindness and unfairness.

## Terminal, console and CLI

### CLI UX review and fixes — before customizable shortcuts

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

### Browser terminal over WebTransport — planned direction

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

### Console talks to the local `pb` (browser ↔ local daemon bridge)

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

### Terminal recordings: asciicast v3 and player features — after launch (user 2026-11-17)

Today recordings are asciicast v2 segments, `pb recordings export` writes v2, and the console replays with the same
wterm + Ghostty renderer as live sessions (seek, 1–8× speed; the CLI also has `--idle-limit`). Decision: keep v2 and our
own player for launch (v2 is what most tools read; one renderer keeps replay identical to live). Later, in order:
idle limit in the console player; markers and keyboard shortcuts in our player; optional
`pb recordings export --format v3` (asciicast v3: relative intervals, marker/exit events, header term metadata) with v2
staying the default. Not planned: switching to asciinema-player (it brings a second terminal emulator).

- Socket-based agent that Git-for-Windows ssh can reach (after launch): the managed agent is a Windows named pipe, which MSYS OpenSSH cannot use; until then Git runs through `git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe` (pb ssh doctor prints it).

### Remote desktop beta — deferred former Task 38

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

## Install and platforms

### `pb pair` below 8 s on Linux

After the 2026-11-26 fixes pb pair takes ~11.5 s in a container (was ~19 s). What is left, measured
(journal, ms from pair start): enrollment and sudo ~2.9 s, carrier start ~1.3 s, hostd start ~1.2 s
(Type=notify after the worker boots), remaining systemd reloads ~0.4 s, bind restart to return ~2 s.
Also: nothing is printed between "Administrator approval is required..." and "ready" (now ~8 s);
progress text is a product call. Evidence: paperboat-private fix-batch-1122/pair-timing.
Next: (1) one shared keep-alive transport for the pre-sudo clients (api/http.go:34,
enrollment/client.go:331, machinecontrol): each opens a new TCP+TLS connection (~0.25 s from
linuxpool-4), ~1-1.5 s per pair; (2) trace a carrier and a hostd start to find their ~1.2 s each.

### Tunnel-only mode without a Paperboat machine (and an official container image)

`pb tunnel run --token <token>` for a Docker container, Kubernetes pod or CI job, like
`cloudflared tunnel run --token`: nothing joins the account and nothing installs into
the system; it only forwards a tunnel to the app next to it. Needs a tunnel token with
rotation and one more admission path. Decision (2026-10-20): not for launch; until then
every tunnel runs on an enrolled Paperboat machine.

### macOS Developer ID signing — after launch

Release builds are ad-hoc signed only (`.github/workflows/release.yml`), so macOS treats every `pb` update as a new app: the local-network permission prompt returns after each update and the identity macOS keys permissions on is not stable. Needs an Apple Developer Program membership (paid, user decision 2026-11-16: not now), a Developer ID Application certificate in CI secrets, `codesign --options runtime` with that identity and notarization (`notarytool`) in the release job, and a check that an update keeps the local-network grant.

## Servers and scale

### Self-hosted control server through pbh — after launch (user 2026-11-19)

- `pbh` already installs self-hosted relays and edges. Extend it to install the whole server (API, database, console), so a company can run Paperboat entirely on its own machines.
- Needs real work first: database setup and upgrades, sign-in without our WorkOS account, email, TLS and domains, backups, and updates through the same signed release path.

### Multi-region control servers — after launch (user 2026-11-19)

- Route the one API address to the nearest healthy region with a DNS load balancer (e.g. Cloudflare geo steering), like Tailscale's single control address. `pb` and the console stay unchanged.
- Each region's server already reads from its own replica once it has caught up with the client's commit position (`PAPERBOAT_DATABASE_REPLICA_DSN`); writes go to the primary.
- Server test speed: split the PostgreSQL `-race` suite across hosts and cache unchanged packages (about 25–30 min today).

### Before running a second server: per-server limits (2026-11-20)
Relay speed sharing (each account's relay budget split by per-relay demand), sign-up/sign-in rate limits and abuse-report rate limits live in one server's memory. Before a second server runs, move them to Postgres or a shared store, so two servers do not double the limits.

## SDK

### SDK, direct API and MCP — after launch (user 2026-11-16)

We need a Go SDK (sdk/go exists in part), a direct token-authenticated HTTP API that ships with it (scoped like a recipe,
attributed in Activity; API tokens were replaced by recipes), and an MCP server / agent skill so AI agents can use Paperboat.
Design each when it is picked up; earlier F24 design notes are in git history (WHITEBOARD before 2026-11-16).

## Engineering and held items

### Batched integration queue — after launch (user decision 2026-11-12)

Consider a bors-style queue that tests an actual combined batch once and bisects failed
batches, if native GitHub per-PR queue throughput proves insufficient. Before launch keep
GitHub's native queue with five concurrent speculative builds, rebase merges and the fast-PR /
heavy-merge-group split. Native merge limits do not amortize CI across PRs. Prior art:
[bors-ng batching](https://bors.tech/documentation/) and
[GitHub merge queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue).

### Remove the reliance on the Tailscale fork — after launch (user decision 2026-11-13)

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

`cli/upstream/PATCHES.md` names fork commit
ac1095ce while both go.mod files pin 2d32e5f0.

### Held for after launch — decided 2026-11-18

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
- **Status page: move to a hosted provider when revenue allows; swap the adapter.** Ours (`status/`) stays until then. The seams: probes post through
  one client (`tools/probe/status_report.py`), and the admin panel reaches incidents and maintenance through one server-side interface
  (`admin/lib/server/status-provider.ts`); a hosted provider (incident.io, Atlassian Statuspage) means one new adapter there plus publishing
  component states from `status/lib/tick.ts`.
- **Shared contracts not merged yet** (skipped on purpose by the dedup worktree; code health only, no user-visible gain):
  release index (move release policy to `shared/`, validate at the release gate and in the client), terminal attach
  descriptor (an optional field becomes required; client and server deploy together), tunnel config snapshot
  (hash-relevant field tags and two diverging validators must become one). Also still to check: inspector, tunnel view
  types, config assignment.
- **Errors back to Sentry on 2026-12-02** when the free quota resets (private ops runbook),
  then remove GlitchTip. Paid Sentry plan once Paperboat earns money. Until then GlitchTip takes errors only; Sentry logs,
  traces, release health and cron monitors are paused.
- **Release safety still post-launch by design:** upgrade matrix (old/new client × server × edge) and release channels
  (dev, beta, stable).
- **Admin panel data:** device/browser and country insights need the console and site to send `$browser` /
  `$device_type` with their events; retention needs new explicit events (privacy decision first).
- **TUI:** `huh` forms only if a real multi-field form appears (e.g. `pb env set` name + hidden value); a real Windows
  console check of the narrow-terminal hint (headless runs are fixed at 120 columns).

### From the final launch batch (26 Nov)

- Mac first open right after a daemon restart: handshake_lease takes 0.9–1.4 s and no other dial holds the token, so the cause is still unknown. Next step: trace inside acquireHandshake and the warm channel, and check that the step timer measures what it says. Other costs: application_authenticate ~0.7 s, 3 stream_authorize/admission calls in series ~0.5 s, resolve ~0.4 s. Evidence: fixwr/first-open-steps.md §7.
- **Machine-event fan-out (2026-11-26):** `private_routes` and `machine_services` are account-wide topics, so one
  machine's route or service change wakes every machine of the account. On the 72-machine smoke account that was ≈41
  moves/min (private_routes 16, machine_services 12, peer_network 8, inventory 5) and ≈35–38 wait answers/min per host
  process. Publish those two topics only to machines that can use the route or service (migrations 217/448, ~10× fewer
  answers); 800 ms coalescing alone saves ~1.4×, so it was not done.
- **Relay update failover (2026-11-27):** updating the only relay of a region drops relayed connections for 5–50 s
  (no relay handover: the old relay stops before the new one is ready). Not a launch blocker: launch has EU and US
  regions. Once a US node exists, check that relayed pairs fail over to the other region during a relay update.
- **Pre-launch live checks to turn into E2E rows (2026-11-27):** new Windows computer reaching peers outbound; GitHub
  disconnect and reconnect; drain and resume of one relay (staging only); `pb doctor` with an offline peer; parallel
  fleet install and pairing; web terminal keys typed in the first second; Windows service remove and reinstall.
