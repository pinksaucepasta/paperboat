# Paperboat launch plan

What to build and how. Progress is tracked **only** in [CHECKLIST.md](CHECKLIST.md): every
item below has an ID (e.g. `F3-07`) with a row there, and every `pb` command has a row in its
Part B. Read AGENTS.md first.

Work strictly in the order of section **3** (S0, S1, F1 … F15, S2, S3). For each feature:
make every Part A row and every Part B command row of that feature `[x]` on all platforms,
then move on. Regressions in earlier sections are fixed first.

There are no dates. Work continues whenever an agent session is available. The launch
target is **2026-10-20**; if something is clearly not going to make it, say so in the Log
(section 5) and tell the user — never discover it at launch.

Decided items (section 1) are not up for debate; items marked **USER DECIDES** must not be
worked on until the user records a decision here.

---

## 1. Decisions (fixed)

### 1.1 Platforms

| Platform | Binary | Verified how |
|---|---|---|
| Linux x86_64 | `pb-linux-amd64` | device **hp** |
| Linux ARM64 | `pb-linux-arm64` | build + unit tests only (no ARM Linux device) |
| macOS Apple Silicon | `pb-darwin-arm64` (+ `.pkg`) | device **apple** |
| Windows x86_64 | `pb-windows-amd64.exe` (+ installer) | device **victus** |
| Windows ARM64 | `pb-windows-arm64.exe` | build + unit tests only (no ARM Windows device) |

No Intel macOS. "Every platform" in checklists means **L** (hp), **M** (apple), **W** (victus);
ARM builds must compile, pass unit tests and be included in every release.

### 1.2 Domains

| Use | Domain | Example |
|---|---|---|
| Server, API, installer, relays, edges, all infrastructure | `*.pprbt.dev` | `api.pprbt.dev`, `get.pprbt.dev` |
| Previews and tunnels (public HTTP) | `<name>.pprbt.app` | `quiet-river-42.pprbt.app` |
| Local device access (private, from your own devices) | `<machine>.ppr.boats` | `homelab.ppr.boats` |
| Local device port access | `<port>.<machine>.ppr.boats` (lazy URL feature) and `<machine>.ppr.boats:<port>` | `3000.homelab.ppr.boats`, `homelab.ppr.boats:3000` |
| Marketing site | `paperboat.cloud` | |
| Console (web app) | `app.paperboat.cloud` | |

- Previews and tunnels both get names from the existing name generator. **Preview names
  cannot be changed. Tunnel names can be renamed** by the owner. Names are globally unique.
- `preview.pprbt.dev`, `tunnels.pprbt.dev` and `*.local.pprbt.dev` are **removed**
  (unreleased; no compatibility, no redirects).
- Public Suffix List submission for `pprbt.app`: the user handles it. Not in this plan.

### 1.3 Plans and billing (live Stripe at launch)

| | Free | Pro | Team |
|---|---|---|---|
| Price | $0 | $8/month or $80/year | $15/seat/month or $150/seat/year |
| Scope | personal workspace | personal workspace | team workspace only |
| Relay + edge bandwidth / month | 5 GB | 100 GB | 100 GB per seat (for that member's use inside the team workspace) |
| Top-ups (100 GB, $5, one-time, expire at end of the calendar month) | no | yes, for the personal workspace | yes, into a team pool |
| Machines | unlimited | unlimited | unlimited |
| Features | everything | everything | everything + collaboration |

- Direct P2P traffic is always free and never metered.
- **Workspaces share nothing.** A personal workspace's plan never affects a team workspace and
  vice versa. A team seat gives no personal-workspace benefit.
- Team bandwidth: each member's usage inside the team workspace counts against that member's
  100 GB; when it is exhausted, usage draws from the team top-up pool; when that is empty,
  relay/edge traffic for that member in the team workspace stops.
- At any cap: relay/edge traffic stops until the next month or a top-up; **direct P2P keeps
  working**; the user sees a clear message with the upgrade / top-up link; team owners and
  admins are notified (email + console).
- **There is no Free tier in the code today**: every machine requires an active subscription
  or trial (`server/internal/usermachines/service.go` `ensureCurrentBandwidthPeriodAt`,
  `machine_plan_inactive`; `server/internal/httpapi/auth_handlers.go` `requireEntitlement`
  → 402). Implementing Free is part of F13.
- Stripe live: the agent sets everything up via the Stripe API with a restricted live key
  the user provides (products, prices incl. yearly, webhook endpoint, invoice/tax settings).
  Test mode first, then live. Business/legal details: ask the user only for values that do
  not exist anywhere yet.

### 1.4 Infrastructure

- One environment (no separate production yet). It runs on **coolify** (`ssh root@coolify`,
  Hetzner), which the user will **format before work starts**. Everything is rebuilt from
  this repo. Secrets and configuration from the old install are saved (owner-only) in
  `~/paperboat-archive/coolify-backup/` (`coolify-config-secrets.tgz`: deploy/secrets,
  compose file, Caddyfile, public key, manifest; `container-config.txt`: container env,
  binds, ports). Reuse WorkOS, GitHub app, Cloudflare and other external credentials from
  there; regenerate internal signing/encryption keys only if they cannot be reused.
- Relays currently run on other machines (oracle and one more); they are rebuilt from this
  repo and re-registered with the new server.
- Where the console and marketing site are hosted in production: **USER DECIDES** (marketing
  probably Vercel). Until then the console runs as today (dev server behind the Tailscale
  Funnel at https://oracle.newt-mermaid.ts.net).

### 1.5 Repository and process

- This monorepo lives at github.com/pinksaucepasta/paperboat (**private**; licence later,
  **USER DECIDES**). Push `main` after every commit. Release binaries need a public download
  host — see S3.
- One commit per fix; device builds only from a clean committed tree; device smoke suite
  after every device build (AGENTS.md).

### 1.6 Not in this launch

Generic UDP, SDK, WebTransport browser terminal, Codex/OpenCode integrations, remote desktop,
PostHog, agent skill (all in WHITEBOARD.md), desktop app, e2e harness. Operator admin
console frontend: **USER DECIDES**. Inspector capture/replay: ships only if it already works
(F7-07 checks it); no new inspector work.

---

## 2. How to verify a feature

- **Device matrix.** Client = the machine running `pb`; target = the machine reached.
  Unless a line says otherwise, verify these pairs: L→M, M→W, W→L, L→L (self), and the
  console (browser on apple) → each target. A line that says "all pairs" means all 9.
- **Each checkbox is a statement that must be true on the real devices.** Check it only with
  evidence: smoke-suite test id(s), or a recorded manual command and its output.
- **Extend the smoke suite** (`tools/device-smoke/smoke.sh`) with each feature's checks as
  you verify them, so later work cannot silently break them. The suite must stay runnable in
  one command. If a check is manual by nature (browser UI), record the exact steps in the Log.
- **Edge cases are part of the feature**, not follow-ups: offline target, network drop
  mid-operation, permission denied, wrong input, limits, Ctrl-C, restart, update mid-use.
- **Regressions first** (AGENTS.md): anything previously `[x]` that breaks is fixed before
  continuing.

---

## 3. Work, in order

Order: **S0 → S1 → F1 … F15 → S2 → S3.** S-sections are groundwork and release; F-sections
are product features. For each feature, everything a user can do with it is in scope: its
`pb` commands (CHECKLIST.md Part B lists them under the feature), its console screens, its
server side, its failure and recovery behaviour, on L, M and W. Finish a feature completely
(every row `[x]`), then start the next. A feature is never "done on Linux, rest later".

Each feature lists: **What** (the user-facing capability), **Surfaces** (CLI, console, server),
**Known** (open defects and where to start) and **Scenarios** (IDs used in CHECKLIST.md).

### S0. Repository foundation

- S0-01: Phase B handoff merged and the first commit made (see Log). The old repos under `/home/ubuntu/pinksaucepasta` are read-only history; never sync from them again.
- S0-02: `make build` passes; `go vet ./...` passes in `cli`, `relay`, `tunnel`, `server`; `console`: `bun install && bun run lint && bun run build` pass; `marketing` builds.
- S0-03: Unit tests fully green per module (`go test ./...`). Known failures at handoff (cmd/pb: `TestMachineAvailabilityRequiresConfirmationAndReturnsAppliedJSON`, `TestMachineRevokeJSONOutputContract`, `TestSessionIdentificationListHumanAndJSON`, `TestTransferStatusJSONUsesNativeStreamAndClosesLease`, `TestLocalAccessTunnelClientStrictlyBoundsAndDecodesResponse`): fix each, or delete it only if it tests removed code; record each in Evidence.
- S0-04: `go test ./cmd/pb/` finishes under 10 minutes (it currently exceeds 600 s).
- S0-05: `console/public/terminal/browserterminal.wasm` (built from `cli/cmd/browserterminal-wasm`) is generated by the console build instead of committed.
- S0-06: All five platform binaries (1.1) cross-compile from a clean commit (AGENTS.md build command extended to linux/arm64 and windows/arm64).
- S0-07: CI workflows adjusted to this layout: `release.yml`, `tuf-refresh.yml`, `server-ci.yml`, `server-container.yml` (+ `.github/server-generated/`): paths, working directories, Docker contexts, arm64 targets. `server-ci` and `tuf-refresh` are manual-only until this row is done; release/signing workflows run only when the user says.
- S0-08: Console dev service `paperboat-console.service` (systemd user unit; Funnel backend http://100.95.70.63:3000) runs from `~/paperboat/console`; sign-in at https://oracle.newt-mermaid.ts.net works.
- S0-09: Smoke suite `tools/device-smoke/smoke.sh` (the only suite) fixed and validated end to end on all three devices with one build (after S1 and F1). Known gaps: wrong Windows `pb.exe` path (take it from `pb install --json`), weak terminal/update continuity checks, weak exit-code/timeout checks, no cleanup of sessions/previews it creates, private previews not handled. Fix without weakening any check. From then on, every feature adds its checks to the suite.
- S0-10: Commands flagged `LEGACY?` in CHECKLIST.md (`pb approve`, `pb resolve`, `pb tag`, all "over gRPC IPC") decided: delete if leftovers (code, docs, tests, Part B rows) or move them into the feature they belong to.

### S1. Infrastructure rebuild and new domains

Prerequisite: the user has formatted coolify and said so.

- S1-01: **No domain is hardcoded anywhere** (AGENTS.md). Remove every domain literal from code in all modules. Find them with `grep -rnE "pprbt\.dev|pprbt\.app|ppr\.boats|paperboat\.cloud"` (today `local.pprbt.dev` ~43 files, `get.pprbt.dev` ~19, `api.pprbt.dev` ~12, `preview.pprbt.dev`/`tunnels.pprbt.dev` ~7 each). Sources: server/tunnel/relay = deployment configuration; CLI/daemon = API address from build config (`DefaultServerURL`, overridable with `--server`), all other domains from a server discovery response fetched at login/pair and cached; console = environment config + the same discovery response; marketing = site config; tests = `example.test`. Afterwards the grep returns nothing in code.
- S1-02: Local access names: `<machine>.ppr.boats`, `<port>.<machine>.ppr.boats` (lazy URL), `<machine>.ppr.boats:<port>`; local CA name constraint and leaves use the configured local domain; OpenSSH aliases `<machine>.<local domain>`; DNS `*.ppr.boats` and `*.*.ppr.boats` → `127.100.0.1` (Cloudflare).
- S1-03: Previews and tunnels at `<name>.pprbt.app`; names from the existing name generator; preview names immutable; tunnel rename in server, CLI and console; global uniqueness.
- S1-04: Console origin `app.paperboat.cloud` and marketing `paperboat.cloud` configured for production (links, CORS/allowed origins, WorkOS redirects) while the dev console keeps https://oracle.newt-mermaid.ts.net — configuration only.
- S1-05: Unit tests use the configured test domain; switching the deployment's domains in configuration works without a code change (verify once).
- S1-06: Server image from this repo; PostgreSQL 17; migrations from zero; diagnostics store; edge/proxy for `api.pprbt.dev` and `get.pprbt.dev`. Exact from-scratch deploy commands written in `server/README.md`.
- S1-07: Secrets restored from `~/paperboat-archive/coolify-backup/` into the deployment (never printed or committed).
- S1-08: DNS records for all of 1.2; TLS certificates issued and renewing (`*.pprbt.app` wildcard, `api`/`get.pprbt.dev`).
- S1-09: Tunnel edge serving `*.pprbt.app` (HTTP/1.1, HTTP/2, HTTP/3, WebSocket).
- S1-10: Relays rebuilt from this repo, registered, reported ready by the server.
- S1-11: WorkOS sign-in through the console dev origin; new account creation works.
- S1-12: Daily database backup runs; one restore into a scratch database tested.
- S1-13: Server `/healthz` and Sentry error reporting (if configured in the backup) work.
- S1-14: Console ↔ server contract fixes from console/NOTES.md that every feature depends on: unknown `/v1/` paths return 404 (not 401); OpenAPI response schemas for `/v1/me` and machine endpoints; sign-in return path.

### F1. Install and setup

**What:** get Paperboat onto a machine, enrolled, and off it again.
**Surfaces:** console "Add machine" one-liners from `get.pprbt.dev` (POSIX `curl … | bash`,
Windows `powershell -c "iex (irm …)"`); `pb install`, `pb pair`, `pb setup`, `pb reset`,
`pb uninstall`, `pb service …`; console install-command dialog for every platform.
**Start:** `cli/cmd/pb/install_command.go`, `cli/internal/hostruntimecmd/bootstrap_unix.go`,
`bootstrap_windows.go`, `one_shot_resume.go`, `purge_unix.go`, `cli/internal/hostruntime/hostinstall/`,
`cli/internal/machineguard/`, `cli/cmd/pb/uninstall_product_*.go`, `fresh_reset.go`.

- F1-01: The console one-liner installs **the build under test**, then pairs, on L, M, W as a normal user; administrator approval is asked once (sudo / UAC / macOS prompt) and nothing else.
- F1-02: After pairing the machine is online in `pb environments` and the console within 30 s; `pb doctor` healthy; a terminal and an exec to it work immediately.
- F1-03: Token expired / already used / revoked / wrong platform shape → specific message; nothing half-installed.
- F1-04: Ctrl-C or network loss at any point of install or pair → rerunning the same command completes or restarts cleanly; no manual cleanup.
- F1-05: Re-running install on an installed machine is a no-op or clean upgrade; enrollment kept.
- F1-06: macOS: browser HTTPS trust needing GUI approval → install completes; output says trust is pending and gives the exact command to finish it.
- F1-07: Strict umask (077); zsh, fish and PowerShell 7 as login shell; `~/.local/bin` not on PATH; non-ASCII user/home path (Windows); user without sudo rights → clear refusal.
- F1-08: Pair with no ENV assigned and with ENV pending: pair succeeds; pending is explained.
- F1-09: `pb uninstall` (prompts, and `--confirmation 'UNINSTALL PAPERBOAT' --hostname <h>`) removes **everything** (services, processes, files, accounts, CA trust, nft/PF/WFP rules, scheduled tasks, PATH entries, credentials); Inbox preserved; works on a broken or half-installed machine; the smoke suite's uninstall inventory check reports zero leftovers on L, M, W.
- F1-10: `pb reset` + pair again on the same machine works; the old machine record is replaced.
- F1-11: `pb service install/start/stop/restart/status/uninstall` behave as documented; the daemon restarts itself after a crash and after a reboot.
- F1-12: Console install dialog shows correct commands for every platform and live-updates when the machine connects.

### F2. Account and machines

**What:** sign in, switch workspaces, see and manage your machines.
**Surfaces:** `pb login/logout/switch`, `pb auth …`, `pb environments`, `pb machine …`,
`pb status`; console sign-in, home, machine list and machine details (capabilities,
availability, domains, hostname), workspace switcher.
**Known:** the console page the CLI login opens (`/cli/authorize`) 404s today; the daemon
looped on 401/400 refresh for hours with no backoff (see console/NOTES.md "Machines" and
the `shared` field note).

- F2-01: `pb login` with browser approval on L, M, W, and from a headless VPS via the printed link; `pb logout`; `pb auth status`.
- F2-02: The console CLI-approval page exists and works end to end.
- F2-03: `pb switch` / `pb auth switch` between personal and each team workspace; everything afterwards uses the selected workspace.
- F2-04: `pb environments`, `pb machine list/add/rename/revoke`, `pb status` correct on L, M, W.
- F2-05: A revoked machine loses access immediately and says so; credential refresh failures back off and stop at a clear "signed out" state (no tight retry loops).
- F2-06: Machine availability (keep awake / allow sleep) and capabilities set from CLI and console take effect on the device.
- F2-07: Console: sign-in/sign-out, workspace switching, machine list (paging beyond one page), details, rename, revoke, ownership label correct (the server's `shared` field fixed or replaced), machine hostname shown with the configured local domain.

### F3. Terminals

**What:** persistent remote shells with replay and reconnect, shareable with others.
**Surfaces:** `pb`, `pb new`, `pb connect`, `pb session …`; console terminal list, browser
terminal, sharing/participants panel, join-with-code page.
**Known:** console/NOTES.md "Terminals" lists the gaps (participant identity, presence,
input attribution, join-code read-back, guest listing, browser-only sessions, raw runtime
state).

- F3-01: Create, type, output, resize, colours, full-screen apps (vim, htop, less), Ctrl-C / Ctrl-D / Ctrl-Z, large paste, unicode — all pairs.
- F3-02: Detach and reattach the same session with output replay; reattach from another device.
- F3-03: Network drop, sleep/wake, Wi-Fi switch → reconnects to the same shell; no lost or duplicated input.
- F3-04: A session idle for 8 hours is still usable.
- F3-05: `pb session list/attach/close/rename/delete`; the 20-session limit gives a clear message (closed and unavailable sessions don't count).
- F3-06: Sharing: `pb session share` (view / interact), `shared`, `participants`, `join`, `unshare`, `remove`; revoking while connected ends access immediately.
- F3-07: Browser terminal from the console (browser on M → targets L, M, W): open, type, resize, reconnect, close.
- F3-08: Console sharing panel: participants show real identities, join codes and guests work (with and without an account), presence updates.
- F3-09: Windows target: PowerShell sessions behave like Unix shells for all of the above.
- F3-10: Status bar (`pb config status-bar …`) and `--status-bar` work in sessions.

### F4. Remote exec

**What:** run one command on a machine and get its output and exit code.
**Surfaces:** `pb exec`, `pb ping`, `pb wait`.
**Known:** a short HP→Apple exec once failed with "native peer-stream 503" (support
`1710eed9-7bc1-47d7-97b0-166248705b38`), cause not captured — start
`cli/internal/localdaemon/peer_stream.go:91`, `cli/internal/tunnel/native_runtime.go:588` and
first make the initiating dial failure visible; a long exec logged a recoverable EOF +
reconnect (find why the stream dropped).

- F4-01: Output, exit codes, stderr, stdin, very large output, unicode — all pairs.
- F4-02: Commands running 30+ minutes return their final output and exit code.
- F4-03: `--timeout` ends the remote command; Ctrl-C ends it; target reboot mid-command reports an unknown outcome clearly.
- F4-04: Target offline → clear message; ENV assigned but undeliverable → the specific ENV error.
- F4-05: `pb ping` reports real reachability and path (direct/relay); `pb wait` waits for each documented condition.
- F4-06: 50 short execs in a row and 10 in parallel all succeed (no intermittent stream failures).

### F5. Managed SSH

**What:** use standard SSH tools to reach machines without managing keys or networks.
**Surfaces:** `pb ssh`, `pb ssh doctor`, `pb ssh trust-host`, `pb scp`, `pb sftp`, `pb rsync`,
generated OpenSSH aliases; console machine "SSH" info.
**Known:** the **first** SSH attempt fails on both old and new builds (canceled before shell,
or EOF exit 255 after); later attempts work. Suspects (unproven): a failed new dial
invalidates the shared UDP client, or scope withdrawal closes it — start
`cli/internal/peertransport/native/owner.go:243`, `cli/internal/peertransport/tailnet/authorized_udp.go:63`.

- F5-01: Interactive `pb ssh <m>` and `pb ssh <m> -- cmd` work on the first attempt every time — all pairs.
- F5-02: SSH sessions held 1+ hour stay connected.
- F5-03: `pb scp`, `pb sftp`, `pb rsync` both directions, large trees, resume after interruption.
- F5-04: Plain `ssh <m>.<local domain>` and VS Code Remote-SSH connect through the generated aliases; host-key verification normal; `pb ssh trust-host` handles a changed key.
- F5-05: `pb ssh doctor` finds and explains broken integration.
- F5-06: Windows as target (Paperboat-managed OpenSSH) and as client.

### F6. File sending and Inbox

**What:** send files to a machine's Inbox; team members request and approve transfers.
**Surfaces:** `pb send …`, `pb inbox …`; console Files (Inboxes) and team inbox.
**Known:** console/NOTES.md "Files".

- F6-01: Single and multiple files and folders up to the limits (50 MiB per file, 10 files, 500 MiB per batch); received hash matches — all pairs.
- F6-02: Interrupted transfer resumes from the committed offset; no corruption or duplicates.
- F6-03: `pb send list/status/cancel/destination set/clear`.
- F6-04: `pb inbox` (path, set, reset, requests, approve, decline, policy) and team file requests expire correctly.
- F6-05: Over-limit, offline target, full disk → clear messages.
- F6-06: Console Files screens list, download and remove Inbox items; team requests can be approved there.

### F7. Previews

**What:** share a local port as a temporary public (or private/team) URL `<name>.pprbt.app`.
**Surfaces:** `pb preview …`; console Previews (create, stop, list).
**Known:** owner-preview discovery fix from Phase B (re-verify); console/NOTES.md "Previews
and tunnels" (no stop time, no ETag in preview leases, `team` access mode not exposed).

- F7-01: `pb preview <port>` foreground and `--background`; URL opens in a browser from another network — on L, M, W.
- F7-02: `--ttl` expiry, `stop`, `delete`, `list`, `status` behave as documented.
- F7-03: `--private` (only you) and `--team` (team grant) enforce access; others are refused.
- F7-04: Preview survives daemon restart (background) and ends cleanly when the origin stops.
- F7-05: HTTP/1.1, HTTP/2, HTTP/3, WebSocket and large bodies work through a preview.
- F7-06: Console: create a preview, see stop/expiry times, stop it.
- F7-07: `pb preview inspect/replay`: verify if it works; otherwise hide it from the product.

### F8. Tunnels

**What:** durable public endpoints with routes, renamable names and custom domains.
**Surfaces:** `pb tunnel …` (create, list, show, status, doctor, logs, pause, resume, stop,
delete, route, domain, connector, credentials, policy, inspect, replay); console tunnels,
routes, domains.
**Known:** console tunnel create and subdomain edit are UI-only today; `GET /v1/tunnels`
ignores `state` and returns deleted tombstones (console/NOTES.md).

- F8-01: Create a tunnel, add/update/remove routes, rename it, delete it — on L, M, W.
- F8-02: Custom domain add → DNS instructions → verify → serves with TLS → remove.
- F8-03: Tunnel survives daemon restart, machine reboot and update; pause/resume; stop.
- F8-04: Connectors add/list/drain/revoke and credential rotation without downtime.
- F8-05: On-demand port policy (`pb tunnel policy allow/get/revoke`).
- F8-06: HTTP/1.1, HTTP/2, HTTP/3, WebSocket, large uploads/downloads.
- F8-07: `pb tunnel doctor/logs/status` explain real failures.
- F8-08: Console: create, rename, route editing, domains, list filters (no deleted tombstones) all work against the server.

### F9. Private local access

**What:** reach your machines' ports privately from your own devices: `<machine>.ppr.boats`.
**Surfaces:** `pb access machine/tunnel`, `pb config local-access …`, `pb edge list`,
`pb relay list`; console Network (edges, relays, latency) and Domains.
**Known:** console/NOTES.md "Edges and relays", "Machine domains (dummy UI)", "Domains page".

- F9-01: `https://<port>.<m>.<local domain>` and `https://<m>.<local domain>:<port>` open with trusted HTTPS from every client platform — all pairs.
- F9-02: Raw TCP via `127.100.x.x` and `pb access machine <m> --port <p> --listen 127.0.0.1:<q>`.
- F9-03: `pb config local-access` alias, domain and proxy settings apply and reset correctly.
- F9-04: Access is denied without permission; revoking access ends active flows.
- F9-05: Works over direct P2P and over relay fallback (block UDP to force relay).
- F9-06: `pb edge list`, `pb relay list` correct; console Network shows edges, relays and real latency.

### F10. ENV

**What:** end-to-end encrypted environment variables injected into terminals and execs.
**Surfaces:** `pb env …` (list, set, unset, rotate, team, grants, host, vault); console Environment.
**Known:** empty-scope fix from Phase B (re-verify); headless vault password unlock is new
(key in daemon memory 15 min).

- F10-01: Set/unset/list for personal, team and per-machine overrides; values appear in new terminals and execs on L, M, W targets; never in logs or diagnostics.
- F10-02: Vault init/unlock/lock/password/recovery/recover/remove/reset/resume with the OS keychain (M, W, L desktop) and on headless Linux (password unlock, 15-minute memory key).
- F10-03: No ENV assigned → commands run normally; pending → clear message; undeliverable → the specific error.
- F10-04: Team scopes: create, grant, revoke, rotate, reset; `pb env grants sync`; access follows roles.
- F10-05: Rotation after a machine is revoked; `pb env host show` reports delivery truthfully.
- F10-06: Console Environment screens work end to end.

### F11. Config sync

**What:** keep machine configuration in sync with a Git repository.
**Surfaces:** `pb config …` (sync, status, approve, force, conflict, repository, unassign,
team-default-*); console Config sync.

- F11-01: Connect HTTPS, SSH and local repositories; store/remove credentials privately.
- F11-02: Assign, sync, apply and validate on L, M, W.
- F11-03: Conflicts listed, shown, compared and resolved both ways; consent and approve flow.
- F11-04: Force direction, unassign, team defaults set/adopt/unadopt.
- F11-05: Console Config sync screens work end to end.

### F12. Teams

**What:** shared workspaces with roles, members, machines and permissions.
**Surfaces:** `pb team …`; console Teams (members, invites, roles, machines, access, activity).
**Known:** console/NOTES.md "Teams (agreed direction, not built)".

- F12-01: Create, get, list, delete a team (payment handled in F13).
- F12-02: Invite by email (also people without an account), accept, decline, cancel, invitations listing.
- F12-03: Roles owner/admin/member; change role; remove member; leave; transfer ownership.
- F12-04: Team machines: enroll into team, share personal machine, grant, unshare, transfer-to-team, remove; default access shown while enrolling.
- F12-05: Explicit resource grants and `pb team attach`; `pb team activity`.
- F12-06: Suspension and recovery behave as documented.
- F12-07: Console Teams screens per console/NOTES.md "Teams".

### F13. Plans and billing

**What:** section 1.3 exactly (Free, Pro, Team, top-ups, bandwidth caps), live Stripe.
**Surfaces:** server billing, console billing and bandwidth/usage screens, CLI messages at caps.
**Known:** there is no Free tier in code today (see 1.3).

- F13-01: Free plan: new accounts pair machines with no subscription or trial; the old "active subscription required" gates are replaced by the allowance model.
- F13-02: Metering of relay and edge bytes per workspace (personal) and per member + pool (team); direct P2P never metered.
- F13-03: Caps enforced as in 1.3; CLI and console messages with upgrade/top-up links; team owners/admins notified.
- F13-04: Stripe test mode: Pro monthly/yearly, upgrade/downgrade/cancel, top-ups, Team seats monthly/yearly, seat changes mid-cycle (charged immediately, billing date fixed), failed payment → grace → suspension → recovery, invoices.
- F13-05: Stripe live: products/prices via API, webhook verified, one real $8 purchase and refund done by the user.
- F13-06: Console billing, bandwidth card (per-path series: direct/relay/edge), usage and plan screens.

### F14. Updates

**What:** keep every machine current without breaking work in progress.
**Surfaces:** `pb update`, `--force`, `--from`, `pb update check/download/status/settings/bundle`;
automatic updates (default on, 04:00 local, configurable); minimum supported version;
console update summary.
**Known:** Windows supplied update times out in native service installation (start
`cli/internal/hostruntime/hostinstall/install_windows.go:805`, `:2378`) and automatic rollback
fails with access denied (errno 5) leaving core services stopped — suspected deletion of the
still-mapped original executable (start `replacement_windows.go:328`, `install_windows.go:3298`);
reproduce with a Windows regression test first. `pb doctor --repair` restores a working
install. The update code has duplicate paths (`cli/internal/hostruntime/{updated,workerupdate,hostinstall,updateflow}`):
simplify while fixing. **Highest-risk feature.**

- F14-01: `pb update --from <build>` on L, M, W succeeds and reports the new version.
- F14-02: During an update, afterwards without user action: idle and foreground terminals reconnect to the same shell, an open `pb ssh` session stays connected, a running exec returns its result, a running transfer finishes or resumes, previews and tunnels come back on the same URL — on L, M, W (smoke `--update`).
- F14-03: Any failure (bad file, wrong platform, killed mid-update, failed restart, disk full) leaves the previous version working with a clear error — on L, M, W.
- F14-04: Old session-owner generations stop and are removed after their last session closes.
- F14-05: Real signed release → `pb update` and `--force` on L, M, W; tampered metadata or file rejected.
- F14-06: Automatic update at a configured time on L, M, W; `pb update settings` changes and disables it.
- F14-07: Minimum supported version blocks an old official build with a clear message; custom builds unaffected.
- F14-08: `pb update bundle` + `--from <file.pb-update>` installs offline on L, M, W.
- F14-09: `pb update check/download/status` accurate; console update summary accurate per workspace.

### F15. Diagnostics, support and CLI settings

**What:** understand and fix problems; personalise the CLI.
**Surfaces:** `pb doctor`, `pb status`, `pb bugreport`, support references; `pb config
customize …`, `pb config status-bar …`, `pb config set/unset/show/path`, `pb completion …`;
console Activity feed.

- F15-01: `pb doctor` is healthy only when terminal and exec actually work; every failing check says what to do; `--repair` fixes what it claims.
- F15-02: `pb bugreport` produces a bundle with no secrets; support references resolve in the server support lookup.
- F15-03: Diagnostics are written on L, M, W (confirm the Windows location) and never contain credentials, ENV values, terminal or file content.
- F15-04: `pb config customize` (shortcuts, defaults, TUI), `status-bar`, `set/unset/show/path` work and survive updates.
- F15-05: Shell completion for bash, zsh, fish, PowerShell installs and completes commands and machine names.
- F15-06: Console Activity feed shows real events.

### S2. Marketing site

- S2-01: Content matches the product (features, platforms 1.1, pricing 1.3, domains 1.2); install instructions use the configured installer host; builds; hosting **USER DECIDES**.

### S3. Release

- S3-01: CI release from a tag builds all five binaries, the macOS `.pkg` and the Windows installer, signs them, publishes TUF metadata, and publishes downloads to a **public** location (agree the host with the user).
- S3-02: The installer one-liners install the release.
- S3-03: Server, relay and tunnel images built from the release commit and deployed.
- S3-04: Full smoke suite (every feature F1–F15) green on the release build on L, M, W.
- S3-05: User and operator docs (`cli/docs`, `server/docs`, READMEs) match behaviour (each feature's docs fixed when the feature is verified).
- S3-06: Operator admin frontend: **USER DECIDES**.

## 4. Handoff notes

All open items from the Phase B handoff (2026-10-14) are filed in S0 (smoke suite), F4
(short-exec 503, long-exec reconnect), F5 (first-SSH failure) and F14 (Windows activation and
rollback, live-update matrix, signed and automatic update). Fixed during Phase B and to be
re-verified when their features are checked: empty-scope ENV launch (F10), private-preview
owner discovery (F7), Windows repair self-lock and session-owner generation cleanup (F14).
The coolify format (S1) also removes the old dev deployment and its test backups.

## 5. Log

One line per completed section or notable event: `date | section | commit | smoke results
file | note`. Row-level evidence goes in CHECKLIST.md.
2026-10-14 | S0 handoff merge + initial import | 75a0130 | — | old repos read-only from here
