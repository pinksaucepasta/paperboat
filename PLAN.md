# Paperboat launch plan

The single plan and status file. Read AGENTS.md first. Work strictly in the order of
section **3** (P0 → P13). Inside a section, work feature by feature: a feature is done
only when every one of its checkboxes is verified on real devices and recorded in the
**Log** (section 5) with the build commit and the smoke result file.

There are no dates. Work continues whenever an agent session is available. The launch
target is **2026-10-20**; if something is clearly not going to make it, write it in the
Log and tell the user — never discover it at launch.

Status: `[ ]` not started · `[~]` in progress · `[x]` verified on devices (commit in Log).
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
  → 402). Implementing Free is part of P8.
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

- This monorepo is pushed to one **private** GitHub repo (licence later, **USER DECIDES**).
  Push only when the user says. Release binaries need a public download host — see P13.
- One commit per fix; device builds only from a clean committed tree; device smoke suite
  after every device build (AGENTS.md).

### 1.6 Not in this launch

Generic UDP, SDK, WebTransport browser terminal, Codex/OpenCode integrations, remote desktop,
PostHog, agent skill (all in WHITEBOARD.md), desktop app, e2e harness. Operator admin
console frontend: **USER DECIDES**. Inspector capture/replay: ships only if it already works
(P5 checks it); no new inspector work.

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

### P0. Repository foundation

- [x] Phase B handoff merged and the first commit made (see Log). The old repos under
      `/home/ubuntu/pinksaucepasta` are read-only history; never sync from them again.
- [ ] `make build` passes. `go vet ./...` passes in `cli`, `relay`, `tunnel`, `server`.
      `console`: `bun install && bun run lint && bun run build` pass. `marketing` builds.
- [ ] Unit tests fully green per module (`go test ./...`). Known failures at handoff
      (cmd/pb: `TestMachineAvailabilityRequiresConfirmationAndReturnsAppliedJSON`,
      `TestMachineRevokeJSONOutputContract`, `TestSessionIdentificationListHumanAndJSON`,
      `TestTransferStatusJSONUsesNativeStreamAndClosesLease`,
      `TestLocalAccessTunnelClientStrictlyBoundsAndDecodesResponse`, plus any listed in the
      handoff): fix each, or delete it only if it tests removed code. Record each here.
- [ ] `go test ./cmd/pb/` finishes under 10 minutes (it currently exceeds 600 s).
- [ ] `console/public/terminal/browserterminal.wasm` (browser terminal, built from
      `cli/cmd/browserterminal-wasm`) is generated by the console build instead of committed.
- [ ] Cross-compile all five platform binaries (1.1) from a clean commit with the AGENTS.md
      build command extended to linux/arm64 and windows/arm64.
- [ ] CI workflows in `.github/workflows/` adjusted to this layout: `release.yml`,
      `tuf-refresh.yml` (from the CLI repo) and `server-ci.yml`, `server-container.yml`
      (from the server repo; `.github/server-generated/` holds their generated inputs).
      Fix paths, working directories, Docker contexts; add linux/arm64 and windows/arm64
      to the release matrix. Do not push or run them on GitHub until the user says.
- [ ] Console dev service `paperboat-console.service` (systemd user unit; Funnel backend
      http://100.95.70.63:3000) runs from `~/paperboat/console`. Sign-in at
      https://oracle.newt-mermaid.ts.net still works.
- [ ] Smoke suite (`tools/device-smoke/smoke.sh`, the only suite) fixed and validated end to
      end on all three devices with one build (after P1–P2 make devices installable). Known
      gaps from the handoff: wrong Windows `pb.exe` path (take it from `pb install --json`),
      terminal/update continuity checks too weak, exit-code/timeout checks too weak, no cleanup
      of sessions/previews it creates, private previews not handled. Fix them without
      weakening any check. Record the first table.

### P1. Infrastructure rebuild and new domains

Prerequisite: the user has formatted coolify and said so.

Domain migration (code), done before deploying:
- [ ] **No domain is hardcoded anywhere** (AGENTS.md). Remove every domain literal from code
      in all modules (`cli`, `server`, `tunnel`, `relay`, `console`, `marketing`, `tools`).
      Find them with `grep -rnE "pprbt\.dev|pprbt\.app|ppr\.boats|paperboat\.cloud"`; today
      `local.pprbt.dev` appears in ~43 files, `get.pprbt.dev` in ~19, `api.pprbt.dev` in ~12,
      `preview.pprbt.dev`/`tunnels.pprbt.dev` in ~7 each. Where values come from:
      - server, tunnel edge, relay: deployment configuration (environment/config file);
      - CLI and daemon: the API address from build/deploy configuration (`DefaultServerURL`
        ldflags, overridable by `--server`); every other domain (preview/tunnel domain,
        local-access domain, installer host, console origin) from a server discovery
        response fetched at login/pair and cached;
      - console: environment configuration (`NEXT_PUBLIC_…`) plus the same discovery response;
      - marketing: site configuration;
      - tests: a configured test domain such as `example.test`.
      After the change, the grep above returns nothing in code (only deploy config files,
      PLAN.md and docs that describe the configured values).
- [ ] Local access: machine at `<machine>.ppr.boats`, ports at `<port>.<machine>.ppr.boats`
      (lazy URL) and `<machine>.ppr.boats:<port>`; the local trusted CA name constraint and
      leaf certificates use `.ppr.boats`; OpenSSH aliases become `<machine>.ppr.boats`; the
      fixed loopback range `127.100.0.0/16` and the public DNS records for `*.ppr.boats` and
      `*.*.ppr.boats` → `127.100.0.1` are set up (Cloudflare).
- [ ] Previews and tunnels: `<name>.pprbt.app`; preview names fixed; tunnel rename supported
      in server, CLI (`pb tunnel` rename command) and console; global name uniqueness enforced.
- [ ] Console origin `app.paperboat.cloud` and marketing `paperboat.cloud` used in links,
      CORS/allowed origins and WorkOS redirect configuration **for production**, while the
      dev console keeps https://oracle.newt-mermaid.ts.net (configuration, not hardcoded).
- [ ] Unit tests use the configured test domain; switching the deployment's domains in
      configuration (e.g. to a staging set) works without a code change — verify once.

Deploy on the formatted coolify:
- [ ] Server image built from this repo; PostgreSQL 17; migrations from zero; MinIO (or the
      chosen diagnostics store); the edge/proxy (previously `paperboat-control-proxy`
      tunnel image + Caddy) for `api.pprbt.dev` and `get.pprbt.dev`. Exact commands written in
      `server/README.md` so anyone can redeploy from scratch.
- [ ] Secrets restored from the backup into the new deployment (never printed or committed).
- [ ] DNS (Cloudflare) records for all domains in 1.2; TLS certificates issued and renewing
      (`*.pprbt.app` wildcard, `api`/`get.pprbt.dev`).
- [ ] Tunnel edge serving `*.pprbt.app` (HTTP/1.1, HTTP/2, HTTP/3, WebSocket).
- [ ] Relays rebuilt from this repo, registered, reported ready by the server.
- [ ] WorkOS sign-in works through the console dev origin; account creation works.
- [ ] Database backup job runs daily and a restore into a scratch database was tested once.
- [ ] Health: server `/healthz`, Sentry receives server errors (if configured in the backup).

### P2. Install, pair, uninstall, reset

User paths: console "Add machine" one-liner (POSIX `curl … | bash`, Windows
`powershell -c "iex (irm …)"`) served from `get.pprbt.dev`, `pb install` + `pb pair`,
`pb reset`, `pb uninstall`. Starting points: `cli/cmd/pb/install_command.go`,
`cli/internal/hostruntimecmd/bootstrap_unix.go`, `bootstrap_windows.go`, `one_shot_resume.go`,
`purge_unix.go`, `cli/internal/hostruntime/hostinstall/`, `cli/internal/machineguard/`,
`cli/cmd/pb/uninstall_product_*.go`, `fresh_reset.go`.

- [ ] The one-liner from the console installs **the build under test** (installer serves the
      current build), then pairs, on L, M, W as a normal user; administrator approval is asked
      once (sudo / UAC / macOS prompt) and nothing else is required.
- [ ] After pairing: machine online in `pb environments` and the console within 30 s;
      `pb doctor` healthy; a terminal and an exec to it work immediately.
- [ ] Token expired / already used / revoked / wrong platform shape → a specific message and
      no partial install left behind.
- [ ] Ctrl-C or network loss at any point of install or pair → rerunning the same command
      completes or cleanly restarts; no manual cleanup needed.
- [ ] Re-running install on an installed machine: no-op or clean upgrade; enrollment kept.
- [ ] macOS: browser HTTPS trust needing GUI approval → install completes; output says
      trust is pending and gives the exact command to finish it.
- [ ] Strict umask (077), zsh/fish/PowerShell 7 as login shell, `~/.local/bin` not on PATH,
      non-ASCII user/home path (Windows), user without sudo rights (clear refusal).
- [ ] Pair when ENV has nothing assigned and when ENV is pending: pair succeeds; pending is
      explained.
- [ ] `pb uninstall` (prompts, and `--confirmation 'UNINSTALL PAPERBOAT' --hostname <h>`)
      removes **everything** (services, processes, files, accounts, CA trust, nft/PF/WFP rules,
      scheduled tasks, PATH entries, credentials); the Inbox is preserved; works on a broken
      or half-installed machine; the smoke suite gains an `uninstall` mode with an inventory
      check per platform that must report zero leftovers.
- [ ] `pb reset` + pair again on the same machine works; the old machine record is replaced.
- [ ] Machine deleted in the console → the device stops using it and tells the user.

### P3. Core remote features

Known at handoff (direct comparison, Phase A build vs current, HP→Apple, public commands):
the 7-minute exec and the 50 MiB `pb send` **pass on both** (earlier failures came from a test
fixture, not the product). Open:
- A short `pb exec` HP→Apple failed once on the current build with "native peer-stream 503"
  (support `1710eed9-7bc1-47d7-97b0-166248705b38`); cause not captured. Start:
  `cli/internal/localdaemon/peer_stream.go:91`, `cli/internal/tunnel/native_runtime.go:588`;
  first make the initiating dial failure visible in diagnostics.
- The **first** managed SSH attempt fails on **both** builds (canceled before shell, or EOF
  exit 255 after); later attempts work. Suspects (unproven): a failed new dial invalidates the
  shared UDP client, or scope withdrawal closes it. Start:
  `cli/internal/peertransport/native/owner.go:243`,
  `cli/internal/peertransport/tailnet/authorized_udp.go:63`.
- A long exec logged a recoverable EOF + reconnect but still finished; find why the stream
  dropped.

**Terminal** (`pb`, `pb new`, `pb connect`, `pb session …`; browser terminal in the console)
- [ ] Create, type, output, resize, colours, full-screen apps (vim, htop, less), Ctrl-C /
      Ctrl-D / Ctrl-Z, paste of large text, unicode — all pairs.
- [ ] Detach and reattach the same session with output replay; reattach from another device.
- [ ] Network drop, laptop sleep/wake, Wi-Fi switch → reconnects to the same shell; no lost or
      duplicated input.
- [ ] A session idle for 8 hours is still usable.
- [ ] `pb session list/close/rename/delete`; limit of 20 open sessions per machine gives a clear
      message (closed and unavailable sessions must not count).
- [ ] Sharing: share view/interact with a teammate, join code for guests, unshare, revoke
      while connected (access ends immediately), participants list correct.
- [ ] Browser terminal from the console: open, type, resize, reconnect, close (browser on M
      to targets L, M, W).
- [ ] Windows target: PowerShell sessions behave the same as Unix shells for all of the above.

**Exec** (`pb exec <m> -- <cmd>`)
- [ ] Output, exit codes, stderr, stdin, very large output, unicode — all pairs.
- [ ] Commands running 30+ minutes return their final output and exit code.
- [ ] `--timeout` ends the remote command; Ctrl-C ends it; target reboot mid-command reports
      an unknown outcome clearly.
- [ ] Target offline → clear message; ENV assigned but undeliverable → the specific ENV error.

**SSH family** (`pb ssh`, `pb scp`, `pb sftp`, `pb rsync`, OpenSSH aliases)
- [ ] Interactive `pb ssh <m>` and `pb ssh <m> -- cmd`; sessions held 1+ hour — all pairs.
- [ ] `pb scp`, `pb sftp`, `pb rsync` both directions, large trees, resume after interruption.
- [ ] Plain `ssh <m>.ppr.boats` and VS Code Remote-SSH connect using the generated aliases;
      host-key verification behaves normally.
- [ ] Windows as target (Paperboat-managed OpenSSH) and as client.

**File transfer and Inbox** (`pb send`, `pb inbox`, team inbox)
- [ ] Single and multiple files and folders up to the limits (50 MiB per file, 10 files, 500 MiB
      per batch), received hash matches — all pairs.
- [ ] Interrupted transfer resumes from the committed offset; no corruption or duplicates.
- [ ] `pb inbox` list/open/remove; team inbox request accept/decline/expire.
- [ ] Over-limit, offline target, full disk → clear messages.

### P4. Updates

Requirements (from the redesign): automatic updates on by default at 04:00 local time
(configurable, can be turned off); `pb update`, `pb update --force`, `pb update --from
<path|URL>`, offline bundle (`pb update bundle` → `--from <file.pb-update>`); official
builds identified by signed TUF provenance, not version strings; minimum supported version
blocks old official builds, custom builds exempt. Known at handoff: Windows supplied update
times out in native Windows service installation (exact sub-step not captured; start
`cli/internal/hostruntime/hostinstall/install_windows.go:805` and `:2378`), and automatic
rollback then fails with access denied (errno 5), leaving core services stopped — suspected:
rollback deletes the still-mapped original executable (start `replacement_windows.go:328`,
`install_windows.go:3298`); reproduce with a Windows regression test first. `pb doctor
--repair` does restore a working install. **Highest-risk open defect**; the update code has duplicate paths (see
`cli/internal/hostruntime/{updated,workerupdate,hostinstall,updateflow}`), simplify while
fixing rather than adding branches.

- [ ] `pb update --from <build>` on L, M, W succeeds and reports the new version.
- [ ] During an update, all still work afterwards without user action: an attached idle
      terminal and a foreground terminal (same shell, reconnects), an open `pb ssh` session
      (stays connected), a running exec (returns its final result), a running transfer
      (finishes or resumes), a preview and a tunnel (back on the same URL) — on L, M, W
      (smoke `--update`).
- [ ] Any failure (bad file, wrong platform, killed mid-update, failed restart, disk full)
      leaves the previous version running and usable, with a clear error — on L, M, W.
- [ ] Old session-owner generations stop and are removed once their last session closes.
- [ ] Signed release path: a real signed release (P13 pipeline) → `pb update` and
      `pb update --force` from it on L, M, W; tampered metadata or file is rejected.
- [ ] Automatic update at a configured time on L, M, W (set the time a few minutes ahead).
- [ ] Minimum supported version set by the operator blocks an old official build with a
      clear message on its next run; custom builds unaffected.
- [ ] Offline bundle install works without network on one platform of each OS.

### P5. Previews, tunnels and local access

- [ ] `pb preview <port>`: foreground and `--background`, `--ttl`, `--private`, `--team`;
      URL `<name>.pprbt.app` opens in a browser; `pb preview list/status/stop` — on L, M, W.
- [ ] Durable tunnels: `pb tunnel create`, routes add/update/remove, custom domains, rename,
      delete; survive daemon restart and update; HTTP/1.1, HTTP/2, HTTP/3, WebSocket, large
      uploads/downloads — on L, M, W.
- [ ] Preview/tunnel create, stop, rename and tunnel route editing from the console work
      against the server (console/NOTES.md lists the ones that are UI-only today).
- [ ] Private access from each client platform: `https://<port>.<m>.ppr.boats` and
      `https://<m>.ppr.boats:<port>` with trusted HTTPS, raw TCP via `127.100.x.x`,
      `pb access machine <m> --port <p> --listen 127.0.0.1:<q>`, `pb resolve`, `pb ping`.
- [ ] Private access is denied to people without access; revoking access ends active flows.
- [ ] Inspector: if it already works, verify capture on one tunnel; otherwise hide it.

### P6. Account and machines

- [ ] `pb login` (browser approval; from a headless VPS via the printed link), `pb logout`,
      `pb switch` personal/team — on L, M, W.
- [ ] The console page the CLI login opens exists and works (`/cli/authorize` 404s today).
- [ ] `pb environments`, `pb machine list/add/rename/revoke`, `pb status`.
- [ ] Revoked machine loses access immediately. Credential refresh failures back off and stop
      at a clear "signed out" state (the daemon looped on 401/400 for hours before).
- [ ] Machine availability (keep awake / allow sleep) and capabilities.

### P7. ENV (end-to-end encrypted environment variables)

- [ ] Set/get/list/delete for personal, team and per-machine overrides; values appear in new
      terminals and execs on L, M, W targets; never appear in logs or diagnostics.
- [ ] Vault unlock/lock with the OS keychain (M, W, L desktop) and on headless Linux (password
      unlock, key held in daemon memory 15 minutes, `pb env vault lock` clears it).
- [ ] No ENV assigned → commands run normally; pending → clear message; undeliverable → the
      specific error.
- [ ] Rotation after a machine is revoked; team ENV access follows roles.
- [ ] Console ENV screens work (console/NOTES.md "Environment").

### P8. Plans, billing and teams

Implements section 1.3 exactly.

- [ ] Free plan exists: new accounts can pair machines without any subscription or trial;
      remove the "active subscription required" gates (`requireEntitlement`,
      `ensureCurrentBandwidthPeriodAt`) and replace them with the bandwidth allowance model.
- [ ] Bandwidth metering per workspace (personal) and per member + pool (team), relay and edge
      only; caps enforced as in 1.3; clear CLI/console messages; owners/admins notified.
- [ ] Stripe test mode: Pro monthly/yearly checkout, upgrade/downgrade/cancel, top-up purchase,
      Team creation with seats (monthly/yearly), add/remove seats mid-cycle (charged at once,
      billing date never moves), failed payment → grace → suspension → recovery, invoices.
- [ ] Stripe live: products and prices created via API, webhook verified, a real $8 Pro
      purchase and refund done once by the user.
- [ ] Teams: create (pay first), invite by email (also people without accounts), roles
      owner/admin/member, remove member, transfer ownership, team machines (enroll into team,
      default access shown, grants), suspension and recovery.
- [ ] Console team and billing screens per console/NOTES.md "Teams" and "Bandwidth card".
- [ ] Marketing pricing page matches 1.3.

### P9. Config sync

- [ ] Assign a repository, sync on L, M, W, conflicts shown and resolved, consent, disable;
      console config-sync screens.

### P10. Diagnostics and support

- [ ] `pb doctor` is healthy only when terminal and exec actually work; each failing check
      says what to do.
- [ ] `pb status`, `pb bugreport` (no secrets inside), support references (`support_…`) resolve
      in the server support lookup.
- [ ] Diagnostics are written on L, M, W (confirm the Windows location) and contain no
      credentials, ENV values, terminal or file content.

### P11. Console

Go through every open item in `console/NOTES.md` (read it fully). For each: implement the
server API the console needs, or confirm the console behaviour against the real server.

- [ ] Contract fixes: machine `shared` field meaning; unknown `/v1/` paths return 404;
      OpenAPI response schemas for `/v1/me` and machine endpoints; machine list paging;
      sign-in return path.
- [ ] Machines: details, capabilities, availability, domains (`*.ppr.boats`), limits.
- [ ] Network: edges and relays, latency reported by daemons.
- [ ] Bandwidth and usage per path (direct / relay / edge) with plan caps from 1.3.
- [ ] Activity feed.
- [ ] Terminals: participant identity, presence, join codes, guests, browser-only sessions.
- [ ] Previews and tunnels: create, edit, rename, stop times, ETags, filters, share a file.
- [ ] Files (Inboxes), Config sync, Domains, Environment, Teams, Billing.
- [ ] Install commands for every platform; docs links point to real pages (or hide).
- [ ] Operator admin frontend: **USER DECIDES**.

### P12. Marketing site

- [ ] Content matches the product (features, platforms 1.1, pricing 1.3, domains 1.2);
      install instructions use `get.pprbt.dev`; builds; hosting **USER DECIDES**.

### P13. Release

- [ ] CI release from a tag builds all five binaries, the macOS `.pkg` and the Windows
      installer, signs them, publishes TUF metadata, and publishes downloads to a **public**
      location (private repo → public release host or mirror; agree the host with the user).
- [ ] `get.pprbt.dev` one-liners install the release.
- [ ] Server, relay and tunnel images built from the release commit and deployed to coolify.
- [ ] Full smoke suite (every feature added in P2–P10) green on the release build on L, M, W.
- [ ] User and operator docs (`cli/docs`, `server/docs`, READMEs) match behaviour; fix each
      feature's docs when it is verified.

---

## 4. Handoff notes

All open items from the Phase B handoff (2026-10-14) are filed in P0 (smoke suite), P3
(short-exec 503, first-SSH failure, long-exec reconnect) and P4 (Windows activation and
rollback, live-update matrix, signed and automatic update). Fixed during Phase B and to be
re-verified when their features are checked: empty-scope ENV launch (P7), private-preview
owner discovery (P5), Windows repair self-lock and session-owner generation cleanup (P4).
The coolify format (P1) also removes the old dev deployment and its test backups.

## 5. Log

One line per completed step: `date | section/feature | commit | smoke results file | note`.
2026-10-14 | P0 handoff merge + initial import | 75a0130 | — | old repos read-only from here
