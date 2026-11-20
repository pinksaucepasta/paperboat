# Paperboat launch plan

What to build and how. Progress is tracked **only** in [CHECKLIST.md](CHECKLIST.md): every
item below has an ID (e.g. `F3-07`) with a row there, and every `pb` command has a row in its
Part B. Read AGENTS.md first.

Work strictly in the order of section **3** (S0, S4, S1, F1 … F15, S2, S3). For each feature:
make every Part A row and every Part B command row of that feature `[x]` on all platforms,
then move on. Regressions in earlier sections are fixed first.

Decided items (section 1) are not up for debate; items marked **USER DECIDES** must not be
worked on until the user records a decision here.

---

## 1. Decisions (fixed)

### 1.1 Platforms

| Platform | Binary | Verified how |
|---|---|---|
| Linux x86_64 | `pb-linux-amd64` | device **linux-test** |
| Linux ARM64 | `pb-linux-arm64` | build + unit tests only (no ARM Linux device) |
| macOS Apple Silicon | `pb-darwin-arm64` (+ `.pkg`) | device **macos-test** |
| Windows x86_64 | `pb-windows-amd64.exe` (+ installer) | device **windows-test** |
| Windows ARM64 | `pb-windows-arm64.exe` | build + unit tests only (no ARM Windows device) |

No Intel macOS. "Every platform" in checklists means **L** (linux-test), **M** (macos-test), **W** (windows-test);
ARM builds must compile, pass unit tests and be included in every release.

**Windows parity is active (user decision 2026-11-10, lifting the 2026-10-15 launch deferral).** Windows is built,
released and tested like Linux and macOS: the release publishes `pb-windows-amd64.exe` and `pb-windows-arm64.exe`
and the PowerShell installer, the Windows installs are unsigned (the Authenticode certificate stays a separate
decision), device smoke runs on windows-test, and the Windows job of Cross-platform is required. W cells count again.
Linux and macOS must not regress.

### 1.2 Domains

| Use | Domain | Example |
|---|---|---|
| Server, API, installer, relays, edges, all infrastructure | `*.pprbt.dev` | `api.pprbt.dev`, `get.pprbt.dev` |
| Previews and tunnels (public HTTP) | `<generated-address>.pprbt.app` | `calm-summer-beach-3476.pprbt.app` |
| Local device access (private, from your own devices) | `<machine>.ppr.boats` | `homelab.ppr.boats` |
| Local device port access | `<port>.<machine>.ppr.boats` (lazy URL feature) and `<machine>.ppr.boats:<port>` | `3000.homelab.ppr.boats`, `homelab.ppr.boats:3000` |
| Marketing site | `paperboat.cloud` | |
| Console (web app) | `app.paperboat.cloud` | |

- Previews and tunnels get a readable generated public address from the existing name
  generator (`naming.PublicSlug`: three words and four digits, reserved so no address is ever
  reused). **The address is stable and never derives from a name** (user decision 2026-10-20).
  A preview's name cannot be changed; a tunnel's name can be renamed by the owner, but it is only a
  label: renaming never changes the address, and names need not be unique. A custom address comes from
  a custom domain (F8-02). The CLI and console show the address next to the name.
- `preview.pprbt.dev`, `tunnels.pprbt.dev` and `*.local.pprbt.dev` are **removed**
  (unreleased; no compatibility, no redirects).
- Public Suffix List submission for `pprbt.app`: the user handles it. Not in this plan.
- DNS: `pprbt.dev` and `pprbt.app` are on Cloudflare (added by the user 2026-10-14).
  `ppr.boats` is not on Cloudflare and is already set up. The existing Cloudflare DNS token is saved
  owner-only in `~/paperboat-archive/deployment-host-backup/cloudflare-dns-token`; verified 2026-10-14 it
  can access zone `pprbt.dev` only. The user must add `pprbt.app` (Zone DNS Edit) to that token
  before S1 needs preview/tunnel certificates.

### 1.3 Plans and billing (live Stripe at launch)

| | Free | Pro | Team |
|---|---|---|---|
| Price | $0 | $8/month or $80/year | $15/seat/month or $150/seat/year |
| Scope | personal workspace | personal workspace | team workspace only |
| Relay + edge bandwidth / month | 5 GB | 100 GB | 100 GB per seat (for that member's use inside the team workspace) |
| Top-ups (100 GB units, $5/unit, one-time, end with the allowance month they are bought in) | no | yes, for the personal workspace | yes, into a team pool |
| Machines | unlimited | unlimited | unlimited |
| Features | everything | everything | everything + collaboration |

- Top-ups are bought as integer quantities of 100 GB units (unit_bytes=100000000000), 1–100 units by default; billing.topup_max_quantity is the single configurable purchase limit. A top-up ends with the allowance month it was bought in (decided 2026-11-09; a monthly plan: with the plan renewal), stays isolated to its workspace and is consumed only after the plan allowance.
- Failed renewal keeps paid benefits for **7 days**, then suspends them until payment recovers (user decision 2026-10-18).
- Paid bandwidth resets monthly on the workspace’s **original paid billing day** (UTC clock; clamp short months without shifting later dates). Switching monthly/yearly prices preserves the reset anchor and allowance already spent (user decision 2026-10-18).
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

- One environment (no separate production yet). It runs on **deployment-host** (`ssh tester@deployment-host`,
  Hetzner), which the user will **format before work starts**. Everything is rebuilt from
  this repo. Secrets and configuration from the old install are saved (owner-only) in
  `~/paperboat-archive/deployment-host-backup/` (`deployment-host-config-secrets.tgz`: deploy/secrets,
  compose file, Caddyfile, public key, manifest; `container-config.txt`: container env,
  binds, ports). Reuse WorkOS, GitHub app, Cloudflare and other external credentials from
  there; regenerate internal signing/encryption keys only if they cannot be reused.
  `deployment-host-full-state.tgz` (2026-10-14) adds every container's `docker inspect`, a `pg_dumpall`,
  deploy compose/env/Caddy/keys, tunnel and diagnostics volumes, and release TUF metadata.
  `github-variables.txt` holds the old repos' variables (Sentry DSNs, now also set on the new
  repo as `SENTRY_DSN`, `SENTRY_DSN_SERVER`, `SENTRY_DSN_TUNNEL`, `SENTRY_DSN_RELAY`).
- **TUF: start fresh with new keys** (user decision 2026-10-14); the old root chain is not
  continued. **Minimal TUF** (user decision 2026-10-14): one offline root key kept on macos-test
  (used only to set up and to replace a key), one online key in GitHub secrets signing
  targets, snapshot and timestamp; the scheduled refresh re-signs timestamp before expiry.
  No signing-state, automated rotation or separate metadata repo; delete that machinery.
  Client verification (hashes, rollback, expiry, root-signed key replacement) is unchanged.
  maintainer: F14 client side. maintainer: release/refresh workflows and key setup (S3), done by
  maintainer itself: generate the root key on macos-test over Tailscale SSH (key never leaves macos-test,
  never printed; backed up in macos-test's keychain/owner-only file), generate the online key and
  store it with `gh secret set`. No user step needed.
- **The CLI binary is independent of the server** (user decision 2026-10-14). `pb` is
  self-contained: anyone can compile it or download it and sign in and pair with it. Today pairing
  requires a server-published release record (`machineArtifact`; server
  `usermachines/service_04.go:430`, CLI `hostruntime/bootstrap/bootstrap.go:191`), so a fresh
  server with nothing published cannot pair anything. Changes:
  - maintainer: installation material carries only credentials, session and server address. Remove
    the release record from pairing, `ErrArtifactUnavailable`, `release_publication.go` and the
    dev-release publisher. No `pb` binary is selected, pinned or served by the server.
  - maintainer: setup accepts material without a release record. `pb pair` keeps installing the
    running binary (it already does). Official builds take their update/repair source from the
    build setting `DefaultReleaseURL` = our release host (TUF metadata on the download host, files
    on GitHub releases), verified with the embedded TUF root, whatever server they pair with.
    Custom builds have no update source and do not auto-update (as today). Windows repair
    (`hostinstall/install_windows.go:3436`) uses the same build-time source.
  - Kept: the console's update approval (server policy for which version machines may update
    to); it gates the version only.
- **Relay and edge hosts** (user decision 2026-10-14): both run on two machines, installed with
  `pbh`: **build-host** (this machine, Ubuntu 24.04 arm64) and **database-test-host**
  (`ssh tester@database-test-host`, Ubuntu 22.04 arm64). Not on deployment-host. `*.pprbt.app` has an
  A record for each IP. The records are Cloudflare-proxied today; the edge terminates TLS with
  its own wildcard certificate, so they must be DNS-only (needs the pprbt.app DNS Edit token,
  S1-08). maintainer owns these hosts' relay/edge services; maintainer still owns the test devices.
- Where the console and marketing site are hosted in production: **USER DECIDES** (marketing
  probably Vercel). Until then the console runs as today (dev server behind the Tailscale
  Funnel at https://console.example.test).

### 1.4b Release and installation model

- **`pb` CLI** is released as today: platform binaries (1.1), signed TUF metadata, installed by
  the `get.pprbt.dev` one-liners. **Launch has no Apple certificate (user decision 2026-10-15):**
  no macOS `.pkg` and no notarization; the one-liner downloads the ad-hoc-signed darwin binary
  with curl (no quarantine attribute), verifies it against TUF and runs `pb install`. Windows
  installer is after launch (1.1). Releases publish to this repo's GitHub Releases (1.5) with the
  workflow's own token; the user creates no tokens. `.pkg` + notarization come when there is an Apple Developer account.
- **Server, relay and tunnel** are released as Docker images for **linux/amd64 and
  linux/arm64** (`ghcr.io/pinksaucepasta/paperboat/server`, `…/relay`, `…/tunnel`).
- **The server has no curl installer and is not installed by `pbh`.** We run it ourselves on the
  deployment-host VPS from its image with a compose file in `deploy/` (server, PostgreSQL 17, its
  edge/proxy), using config/env for every domain.
- **`pbh` self-hosts relay and tunnel only**, its own top-level module `helper/`
  in `go.work` (the command stays `pbh`) (today misplaced in `relay/cmd/pbh`, `relay/deploy/install.sh`,
  `relay/deploy/package-selfhost.sh`, `relay/deploy/README.md`; S4-00 moves it). The one-liner, modelled
  on `curl -fsSL https://cdn.coollabs.io/deployment-host/install.sh | sudo bash` (read it first), is
  `curl -fsSL <get domain>/selfhost/install | sh -s -- --enable relay|tunnel|both`. It installs
  Docker if missing, installs `pbh`, runs the selected relay/tunnel images, health-checks them,
  prints the claim code, and on re-run upgrades in place keeping state. Idempotent, prints what
  it did, clear refusal on unsupported systems (Ubuntu 22.04/24.04, Debian 12; amd64, arm64).
  Rework the existing binary+systemd installer to this; do not keep both.
- **Our own relays and tunnel edge are installed with the same `pbh` installer** (dogfooding),
  then enrolled as Paperboat global nodes through the operator enrollment API.

### 1.4c Credential lifetimes (all features)

Short-lived credentials and descriptors gate **starting or joining** a connection only. Once a
connection, stream or session is admitted (terminal, exec, SSH, transfers, previews, tunnels,
private access, relay/edge carriers, browser terminal, config sync, everything), it never ends
because a credential expired. It ends only when the work completes, either side disconnects,
or access is **revoked** (machine removed, member removed, session signed out, policy change),
and revocation ends it immediately. Renewal and reattach exist for lost connections, never as
a workaround for expiry. Each worktree audits its own code for any timer, deadline or check that
ties an admitted connection to credential expiry, removes it, and adds a test per path: short
credential TTL, connection outlives it; revocation still cuts it.
One shared implementation per side, adopted path by path while fixing (no big-bang rewrite):
the CLI/runtime has one package for "admit with a short credential, keep until done,
disconnected or revoked, watch revocation" and one for reattach after a lost connection; the
server has one place that issues admission credentials, with the lifetime as one
configuration value. Every path above uses them; per-path timers, reattach loops and
5-minute literals are deleted as each path moves over.

### 1.4d One shared module, no copies

Code that more than one module needs lives once, in a new Go module `shared/` (added to
`go.work`), and every module imports it; the copies are deleted. Moved one package at a time,
each move its own commit with full `make gate` and device smoke; no big-bang move. Order:
1. Wire protocols with both sides in different modules: `connectorprotocol` (cli, server,
   tunnel) first, then `machineservices`, `managedssh`, `runtimeattachment`, `peeridentity`
   (cli + server).
2. Common packages: `auth`, `config` (shared parts only), `diagnostics`, `telemetry`,
   `contracttest`, `reporting` (relay, tunnel, helper).
3. Helpers: backoff/retry, identifier validation, atomic file writes, random IDs, error
   classification, HTTP client construction: one implementation each.
Module-specific behaviour stays in its module; only genuinely common code moves. maintainer owns
`shared/` (it produces the contracts); maintainer requests what it needs in the Log and moves its
consumers. 1.4c's shared admission/reattach packages also move into `shared/`.

### 1.5 Repository and process

- This monorepo lives at github.com/pinksaucepasta/paperboat and is **public since 2026-10-15** (user decision; full-history secret scan clean: only test
  fixtures and placeholders). Licence (user decision 2026-11-13): `server/`,
  `console/`, `admin/` AGPL-3.0; everything else MIT (root `LICENSE`, a `LICENSE` in each AGPL directory);
  name and logo reserved (`TRADEMARKS.md`).
  Releases are published on this repo's GitHub Releases (no separate download repo).
  Push `main` after every commit.
- One commit per fix; device builds only from a clean committed tree; device smoke suite
  after every device build (AGENTS.md).

### 1.6 Not in this launch

Generic UDP, SDK, WebTransport browser terminal, remote desktop,
PostHog, agent skill (all in WHITEBOARD.md), desktop app, e2e harness. Operator admin
console frontend: **USER DECIDES**. Inspector capture/replay: `pb preview inspect/replay` ships only if
it already works (F7-07 checks it); the console inspector is F17 (user decision 2026-10-22).

---

## 2. How to verify a feature

- **Device matrix.** Client = the machine running `pb`; target = the machine reached.
  Unless a line says otherwise, verify these pairs: L→M, M→W, W→L, L→L (self), and the
  console (browser on macos-test) → each target. A line that says "all pairs" means all 9.
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

**Parallel features (user decision 2026-10-18), overriding the strict F1 → F15 order below.**
Features do not wait for earlier features. With two Linux devices (linux-test, linux-fixture), verify the
main paths of every feature whose code exists (F10 ENV, F11 config sync, F14 updates, F15
diagnostics now) in parallel with fixes for blocked features (F7/F8). A row blocked for more
than a few hours is `[!]` with its cause and the worktree moves to the next independent row.

**Main paths before edge cases (user decision 2026-10-16), overriding "finish a feature
completely":** pass 1 goes F1 → F15 doing each feature's main-path rows (normal use working on L
and M: the commands and scenarios people use daily). Edge-case and recovery rows (interruption at
any point, network loss, offline, limits, reboot mid-command and similar) are pass 2, after
pass 1 covers every feature. A row that is taking more than a few hours in pass 1 is marked `[!]`
with what is known and the worktree moves on. F1-04 is pass 2.

**Feature rows first (user decision 2026-10-16).** Both Go worktrees work F1 → F15 rows now. An open
S-row (S0, S1, S3, S4) or a code reorganization (`shared/` moves 1.4d, Tailscale fork update) is
done only when a feature row needs it, and then only that part. All of them stay in scope and are
finished after the features, except the S1-02 local access names, S1-01 domain removal and S3
release, which feature work picks up as it reaches them. `pbh` / S4 waits until all core features
are done. Every worktree report starts with the CHECKLIST rows it ticked this turn; a turn with none
says which row it is on and what blocks it.

Order: **S0 → S4 → S1 → F1 … F15 → S2 → S3** (worktrees may run in parallel, see AGENTS.md). S-sections are groundwork and release; F-sections
are product features. For each feature, everything a user can do with it is in scope: its
`pb` commands (CHECKLIST.md Part B lists them under the feature), its console screens, its
server side, its failure and recovery behaviour, on L, M and W. Finish a feature completely
(every row `[x]`), then start the next. A feature is never "done on Linux, rest later".

Each feature lists: **What** (the user-facing capability), **Surfaces** (CLI, console, server),
**Known** (open defects and where to start) and **Scenarios** (IDs used in CHECKLIST.md).

### S0. Repository foundation

- S0-01: Phase B handoff merged and the first commit made (see Log). The old repos under `~/pinksaucepasta` are read-only history; never sync from them again.
- S0-02: `make build` passes; `go vet ./...` passes in `cli`, `relay`, `tunnel`, `server`; `console`: `bun install && bun run lint && bun run build` pass; `marketing` builds.
- S0-03: Unit tests fully green per module (`go test ./...`). Known failures at handoff (cmd/pb: `TestMachineAvailabilityRequiresConfirmationAndReturnsAppliedJSON`, `TestMachineRevokeJSONOutputContract`, `TestSessionIdentificationListHumanAndJSON`, `TestTransferStatusJSONUsesNativeStreamAndClosesLease`, `TestLocalAccessTunnelClientStrictlyBoundsAndDecodesResponse`): fix each, or delete it only if it tests removed code; record each in Evidence.
- S0-04: `go test ./cmd/pb/` finishes under 10 minutes (it currently exceeds 600 s).
- S0-05: `console/public/terminal/browserterminal.wasm` (built from `cli/cmd/browserterminal-wasm`) is generated by the console build instead of committed.
- S0-06: All five platform binaries (1.1) cross-compile from a clean commit (AGENTS.md build command extended to linux/arm64 and windows/arm64).
- S0-07: CI workflows adjusted to this layout: `release.yml`, `tuf-refresh.yml`, `server-ci.yml`, `server-container.yml` (+ `.github/server-generated/`): paths, working directories, Docker contexts, arm64 targets. `server-ci` and `tuf-refresh` are manual-only until this row is done; release/signing workflows run only when the user says.
- S0-08: Console dev service `paperboat-console.service` (systemd user unit; Funnel backend http://console-backend.example.test:3000) runs from `~/paperboat/console`; sign-in at https://console.example.test works.
- S0-09: Smoke suite `tools/device-smoke/smoke.sh` (the only suite) fixed and validated end to end on all three devices with one build (after S1 and F1). Known gaps: wrong Windows `pb.exe` path (take it from `pb install --json`), weak terminal/update continuity checks, weak exit-code/timeout checks, no cleanup of sessions/previews it creates, private previews not handled. Fix without weakening any check. From then on, every feature adds its checks to the suite.
- S0-10: Commands flagged `LEGACY?` in CHECKLIST.md (`pb approve`, `pb resolve`, `pb tag`, all "over gRPC IPC") decided: delete if leftovers (code, docs, tests, Part B rows) or move them into the feature they belong to.

### S1. Infrastructure rebuild and new domains

Prerequisite: the user has formatted deployment-host and said so.

- S1-01: **No domain is hardcoded anywhere** (AGENTS.md). Remove every domain literal from code in all modules. Find them with `grep -rnE "pprbt\.dev|pprbt\.app|ppr\.boats|paperboat\.cloud"` (today `local.pprbt.dev` ~43 files, `get.pprbt.dev` ~19, `api.pprbt.dev` ~12, `preview.pprbt.dev`/`tunnels.pprbt.dev` ~7 each). Sources: server/tunnel/relay = deployment configuration; CLI/daemon = API address from build config (`DefaultServerURL`, overridable with `--server`), all other domains from a server discovery response fetched at login/pair and cached; console = environment config + the same discovery response; marketing = site config; tests = `example.test`. Afterwards the grep returns nothing in code.
- S1-02: Local access names: `<machine>.ppr.boats`, `<port>.<machine>.ppr.boats` (lazy URL), `<machine>.ppr.boats:<port>`; local CA name constraint and leaves use the configured local domain; OpenSSH aliases `<machine>.<local domain>`. DNS is done (not on Cloudflare; already verified 2026-10-14: `x.ppr.boats` and `3000.x.ppr.boats` → `127.100.0.1`); never automate ppr.boats DNS.
- S1-03: Previews and tunnels at `<generated-address>.pprbt.app` (stable, from the existing name generator, never derived from a name); preview names immutable; tunnel rename (label only) in server, CLI and console with the address shown next to the name.
- S1-04: Console origin `app.paperboat.cloud` and marketing `paperboat.cloud` configured for production (links, CORS/allowed origins, WorkOS redirects) while the dev console keeps https://console.example.test — configuration only.
- S1-05: Unit tests use the configured test domain; switching the deployment's domains in configuration works without a code change (verify once).
- S1-06: Server stack (server, PostgreSQL 17 with migrations from zero, diagnostics store, edge/proxy for the API and get domains) deployed on the formatted deployment-host from the server image with the `deploy/` compose file; all domains from env.
- S1-07: Secrets restored from `~/paperboat-archive/deployment-host-backup/` into the deployment (never printed or committed).
- S1-08: DNS records for all of 1.2; TLS certificates issued and renewing (`*.pprbt.app` wildcard, `api`/`get.pprbt.dev`).
- S1-09: Tunnel edge serving `*.pprbt.app` (HTTP/1.1, HTTP/2, HTTP/3, WebSocket).
- S1-10: Our relays and tunnel edge installed with the `pbh` installer (S4), enrolled as global nodes, reported ready by the server.
- S1-11: WorkOS sign-in through the console dev origin; new account creation works.
- S1-12: Daily database backup runs; one restore into a scratch database tested.
- S1-13: Server `/healthz` and Sentry error reporting (if configured in the backup) work.
- S1-14: Console ↔ server contract fixes from console/NOTES.md that every feature depends on: unknown `/v1/` paths return 404 (not 401); OpenAPI response schemas for `/v1/me` and machine endpoints; sign-in return path.

### S4. Images and the pbh self-host installer (maintainer; needed by S1)

- S4-00: Move `pbh` out of `relay/` into a top-level `helper/` module (directory and module named helper; the built command stays `pbh`) (own `go.mod`, added to `go.work`; its own copy or a small shared package instead of importing `paperboat-relay/internal/reporting`), with its installer, packaging script and README; no `pbh` code left under `relay/`; gate and pbh tests pass.
- S4-01: Docker images for server, relay and tunnel build for linux/amd64 and linux/arm64 from this repo (multi-arch manifests), tagged with the release version.
- S4-02: The `pbh` installer implements 1.4b on a fresh Ubuntu 24.04 VPS for `relay`, `tunnel` and `both`: installs Docker, runs the images, health-checks, prints the claim code. The server image deploys from `deploy/` compose and is not part of this installer.
- S4-03: Re-running it upgrades in place to a newer image version, keeping data and secrets; a failed upgrade leaves the previous version running.
- S4-04: Works on amd64 and arm64 (arm64: build-only plus one run in an arm64 container/VM if available), and on Debian 12; unsupported systems get a clear refusal.
- S4-05: Uninstall/teardown option removes containers and (only with explicit confirmation) data.
- S4-06: Self-host documentation (`relay/deploy/README.md`): run your own relay/tunnel with `pbh`, claim it in the console, use it from `pb`; written from the actual script behaviour.

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
- F3-05: `pb session list/attach/close/rename/delete`; native terminals have no plan limit, closed sessions never count, and the machine's 256-session protection ceiling gives a clear message.
- F3-06: Sharing: `pb session share` (view / interact), `shared`, `participants`, `join`, `unshare`, `remove`; revoking while connected ends access immediately.
- F3-07: Browser terminal from the console (browser on M → targets L, M, W): open, type, resize, reconnect, close.
- F3-08: Console sharing panel: participants show real identities, join codes and guests work (with and without an account), presence updates.
- F3-09: Windows target: PowerShell sessions behave like Unix shells for all of the above.
- F3-10: Status bar (`pb config status-bar …`) and `--status-bar` work in sessions.

### F4. Remote exec

**What:** run one command on a machine and get its output and exit code.
**Surfaces:** `pb exec`, `pb ping`, `pb wait`.
**Known:** a short linux-test→macos-test exec once failed with "native peer-stream 503" (support
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
**Decision (2026-10-16):** the Inbox defaults to `~/Paperboat/Inbox`, not `~/Downloads`. macOS
protects Downloads, and ad-hoc-signed builds count as a new app on every update, so writing there
asks for permission again after each update (and stalls device smoke). Users may choose Downloads
or another folder explicitly; then macOS asks once for that choice.

- F6-01: Single and multiple files up to the limits (50 MiB per file, 10 files, 500 MiB per batch); received hash matches — all pairs.
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
delete, route, domain, policy, inspect, replay); console tunnels,
routes, domains.
**Known:** console tunnel create and subdomain edit are UI-only today; `GET /v1/tunnels`
ignores `state` and returns deleted tombstones (console/NOTES.md).

- F8-01: Create a tunnel, add/update/remove routes, rename it, delete it — on L, M, W.
- F8-02: Custom domain add → DNS instructions → verify → serves with TLS → remove.
- F8-03: Tunnel survives daemon restart, machine reboot and update; pause/resume; stop.
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

- F13-01: Free plan: new accounts pair machines with no subscription; the old "active subscription required" gates are replaced by the allowance model.
- F13-02: Metering of relay and edge bytes per workspace (personal) and per member + pool (team); direct P2P never metered.
- F13-03: Caps enforced as in 1.3; CLI and console messages with upgrade/top-up links; team owners/admins notified.
- F13-04: Stripe test mode: Pro monthly/yearly, upgrade/downgrade/cancel, top-ups, Team seats monthly/yearly, seat changes mid-cycle (charged immediately, billing date fixed), failed payment → grace → suspension → recovery, invoices.
- F13-04T: Server top-up producer: verified one-time catalog, paid-workspace Checkout journal/replay, provider-confirmed Personal/Team credits, permanent credits and isolated transactional consumption. Cap admission/debit integration remains F13-03; deployed UI/payment qualification remains F13-04/F13-06.
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
- F14-02: During an update, afterwards without user action: idle and foreground terminals reconnect to the same shell, an update never silently ends open `pb ssh`/forward connections (manual refuses unless --force, scheduled waits until none are open; user decision 2026-10-21), a running exec returns its result, a running transfer finishes or resumes, previews and tunnels come back on the same URL — on L, M, W (smoke `--update`).
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

### F16. ENV in plain SSH and VS Code (maintainer, user decision 2026-10-22)

**What:** ENV values reach shells that Paperboat does not launch: `pb ssh`, plain `ssh` and VS Code
Remote-SSH (today they get only sshd's environment). Opt-in shell hook, the direnv / 1Password
shell-plugin pattern.
**Surfaces:** `pb env hook <bash|zsh|fish>` (prints the snippet), `pb env hook --install`,
`pb env hook --uninstall`, `pb env hook --status`.
**Design:**
- `--install` adds one marked line to the user's rc file (`~/.bashrc` and `~/.bash_profile`
  sourcing check, `~/.zshrc`, `~/.config/fish/config.fish`); idempotent; `--uninstall` removes
  exactly that line.
- Whose values: only the Paperboat account logged in to pb for that same unix user on that
  machine, and only the workspace and scope it picked; `--install` records the choice in the pb
  config. Never guessed from the SSH login; no pb login for that unix user loads nothing, quietly.
- Values come from the local daemon over its local socket (same delivery as `pb exec`), are
  printed only as shell `export` statements on the hook's stdout, never written to disk or logs.
- Fails quietly and fast: locked vault, stopped daemon, no network or no login add nothing, with
  at most one short line on interactive shells; hard time budget (target <50 ms cached, bounded
  timeout otherwise); never blocks shell startup.
- VS Code Remote-SSH resolves the login shell environment for its server; documented reload step:
  "Remote-SSH: Kill VS Code Server on Host", then reconnect.

- F16-01: `pb env hook <shell>` prints a correct snippet for bash, zsh and fish; `--install` is idempotent and `--uninstall` removes only its line.
- F16-02: Plain `ssh host` interactive login and non-interactive `ssh host env` see the workspace values of the logged-in pb account; `pb ssh` too; values never in logs, files or diagnostics.
- F16-03: Locked vault, stopped daemon, no network and no pb login: the shell starts within the time budget, nothing is loaded, at most one short line on interactive shells.
- F16-04: VS Code Remote-SSH terminals and tasks see the values after the documented reload step.

### F17. Preview inspector in the console (maintainer, user decision 2026-10-22)

**What:** select a preview in the console and watch its captured requests live, with replay
(like ngrok's inspector: request list, headers/body view, replay, filters). Captures stay on
the machine (`cli/internal/inspector`); the console reads them over the same end-to-end
encrypted browser channel as the browser terminal, never through the server.
**Surfaces:** console Previews → preview → Inspect; daemon browser capability; server
browser-credential authorization. Same capture store, redaction and replay rules as
`pb preview inspect/replay` (F7-07). maintainer owns the feature across cli/ (A), server/ (B) and
console/ (C), keeping each change small and logging what it touched.

**Design (coordinator approved option B, 2026-10-22).** A machine-scoped browser channel with no
terminal session and no hidden shell; inspect/replay stay named actions (maintainer's policy rules
can cover them later).
- *Grants (exists):* the console gets per-action inspector grants with `POST /v1/inspector/credentials`
  (`inspect`, and `replay` when allowed): owner, or a team member with that exact grant
  (`inspectaccess.resolvePrincipal`).
- *Ticket (server, new):* `POST /v1/inspector/browser-ticket {credential_id, browser_public_key_sha256}`
  (browser session + CSRF). The credential must be the caller's, live, and for a preview; the
  ticket is single-use, 30 s, bound to user + preview + the preview's machine
  (`preview_leases.owner_machine_id`), stored in `browser_terminal_tickets` with
  `purpose='inspector'`. Same response shape as the terminal browser-ticket (edge stream URL,
  subprotocols, owner root key) and the same edge route as terminals.
- *Edge admission (server):* the existing edge control socket redeems it, re-resolves the
  credential, mints a JWT of class `browser_inspector_operation`, scope `inspector:use`, no
  terminal session, and re-checks the credential every 2 s (revoked/expired closes the channel).
  The edge itself is unchanged (the hub key is the attachment id).
- *Daemon (cli):* accepts that class only for the new `inspector.v1` capability. Each request
  carries `{method, path, query, body, grant}`; the daemon re-verifies the grant live against
  the server (existing `inspectorapi`) and requires its principal to equal the JWT user, then
  serves the existing inspector API. Captures and bodies never leave the E2E channel.
- *WASM + console:* `connect` gets an `inspector` mode (no attach) and `inspectorRequest`.
  Previews → Inspect page: live list (cursor poll every second while visible), detail
  (headers/body as captured), filters (method, status class, path), replay, capture
  on/off/bodies, clear, and clear offline/stopped/denied states.
- Order: server + migration (maintainer deploys) → daemon → WASM/console (server-first gate).

- F17-01: The daemon serves capture list, detail, live follow, enable/disable, purge and replay to an authorized console browser over the E2E browser channel; the server relays only ciphertext and never stores or sees captures.
- F17-02: Only people allowed to manage the preview (its owner, or the machine owner) get an inspector credential; a team member without that right and a stranger are refused by the server and by the daemon.
- F17-03: Console: open a preview's inspector, see requests arrive live (method, path, status, duration, time), open one to read request/response headers in full and body as captured (body redaction and size limit unchanged), and filter by method, status class and path text.
- F17-04: Console: replay a captured request and see the replay's result appear in the list, marked as a replay.
- F17-05: Console: turn capture on/off (with or without bodies) and clear captures; clear states for capture off, no requests yet, preview stopped and machine offline.
- F17-06: Losing the connection or the machine restarting reconnects and resumes the live list without duplicates; losing access mid-view ends it with a clear message.

### F18. Workspace policy: tags and access rules (maintainer, user decision 2026-10-22)

**What:** one access policy per workspace (personal or team, **every plan**, user decision
2026-10-22). Machines get tags (`tag:prod`), members get groups (`group:oncall`), and allow and
deny rules say who may use what on which machines. Edited in the console (a simple grid and a
file editor) and as a file with `pb policy`; validated before it is saved; tests inside the
file; a dry-run "who can do X on Y, and why"; every change audited and versioned. Today's
per-machine grants, inspector grants and the team setting "Allow private access" become the
simple view of the same policy, with no behaviour change on migration. The solo case is
first-class: one person with API tokens for their agents ("this token may only exec on tag:ci").
**Surfaces:** `pb policy get|set|test|explain|versions|restore|schema`, `pb api-token
create|list|revoke`; console workspace → Access (Simple, Policy file, Test, History) and
Settings → API tokens; server policy store, compiler and explain API; JSON Schema published by
the server. Reference for people and agents: `shared/policy/README.md`.
**Owner:** maintainer across server/ + shared/ (B), cli/ (A) and console/ (C), each change small
and logged in section 5; maintainer deploys server builds.

**What we took from whom.** Tailscale: one HuJSON file, tags, groups, autogroups (our
`role:`), `tests` that block a bad save; API access tokens that act as the user who made them,
with an expiry, and machine identity from tags. AWS IAM: explicit deny > allow > default deny,
and the policy simulator (explain against a draft before saving). Cloudflare Access: deny/
exclude on top of allows, and one decision view naming the rule that decided. Google
IAP/BeyondCorp: access is decided per request by the server from identity + resource, never
by network position. GitHub rulesets: "evaluate" mode = our review of access changes before
save. Teleport: labels on resources and named action sets; we keep that with fewer moving
parts (no per-role login lists, no conditions language).

#### Model

- **Subjects (who).** `user:<email>` (an active member; in a personal workspace only the
  owner), `group:<name>` (defined in the file; holds users), `role:owner`, `role:admin`
  (owners and admins), `role:member` (every active member), `*` (every member).
- **Targets (on what).** `machine:<alias>`, `machine:*` (every machine in the workspace),
  `tag:<name>` (machines carrying the tag), `preview:<name>` / `tunnel:<name>` (one preview or
  tunnel, for `inspect`/`replay` only), `env:team` (the team ENV; today ENV grants are
  team-wide, `team_resource_grants` kind `env`, resource_id = team; personal ENV scopes stay
  the owner's alone). One rule's targets are all machines/tags, all previews/tunnels, or ENV.
- **Actions.** Today's machine capabilities (`teams.MachineCapabilities`, one owner):
  `terminal`, `exec`, `managed_ssh` (pb ssh/scp/sftp/rsync), `files`, `preview_manage`,
  `tunnel_manage`, `private_access`; `inspect` and `replay` (F17 inspector and `pb preview
  inspect/replay`, also tunnels; on a machine they cover every preview and tunnel it
  publishes; exact-match as today: nothing implies them and `inspect` does not imply
  `replay`); `env_read`, `env_write` on `env:team`. `"*"` = every machine action.
  `private_access` takes optional `ports` (`[5432, "8000-8100"]`): an allow without ports
  covers 80, 443 and 1024-65535 (the automatic private access), a deny without ports every port.
- **Tags** are assigned in the file: `"tags": {"tag:prod": ["machine:db1", "machine:web1"]}`.
  A machine can carry many tags.
- **API tokens** (one concept, one name). `pb api-token create <name> [--expires 30d]
  [--scope '<json>']`: a credential for scripts and agents that **acts as the member who
  created it**, in one workspace. Optional scope narrows it with the policy's own names,
  e.g. `[{"dst": ["tag:ci"], "allow": ["exec"]}]`; a scope never widens access. Expiry
  required (default 90 days, max from config); shown once, stored hashed, revocable by its
  creator and by owners/admins; every use is attributed to the token in Activity. The creator
  losing access (removed, demoted, a rule change) takes it from the token at once. Same in
  personal and team workspaces. Tokens are not policy subjects; machines are identified by
  tags. OAuth-style clients for automation are not in v1.
- **Using a token with `pb`** (F18-06): `pb login --api-token-file <f>` (or the token on
  stdin) exchanges the token for a token-backed CLI session (`POST /v1/auth/api-token-sessions`):
  an ordinary CLI client session of the creator that carries the token's id, cannot outlive the
  token and ends when the token is revoked. Every machine authorization of such a session
  (terminal, exec, managed SSH, files: `usermachines.getMachineForCapability`) also checks the
  token's scope with one SQL function, `api_token_scope_allows`, over the scope's rows stored
  at token creation (machines by ID, tags and every-machine resolved per request, like policy
  rules). Outside its scope a request is refused with `outside_token_scope`.

#### Evaluation (one function, server side)

`decide(member, token?, workspace, target, action, port)`:
1. **Guardrails, outside the policy, unchanged:** account active; signed-in session or an
   unexpired, unrevoked API token; team not deleted or suspended; team paid entitlement where
   it applies today (`team_paid_entitled`; no new plan gate); membership active; machine
   enrolled and bound to this workspace; the machine's own service switches (`pb machine
   capabilities`). Any guardrail fails → denied, and explain names the guardrail.
2. **Owner guard:** the owner of a personal machine may do everything on it in the personal
   workspace, and the owner of a preview or tunnel (or of the personal machine it runs on) may
   always `inspect`/`replay` it. No rule can deny this, so nobody locks themselves out.
3. **Explicit deny:** a matching deny rule → denied (for `private_access`: when the port is
   in the deny's ports).
4. **Allow:** a matching allow rule → allowed.
5. **Default deny.** Rule order never matters; explain names the rule that decided.
6. **Team switch** `"allow_private_access": false` (today's setting, default false) gives no
   `private_access` from any rule.
7. **API token scope:** with a scoped token, steps 1–6 for its creator must allow **and** the
   scope must cover the request; else denied with `outside_token_scope`.

Administrative rights are not policy actions: owners/admins edit the policy and settings;
roles never imply use (today's rule, `machines.go` "Administrative roles govern mutations").

**Where it is enforced.** The policy is compiled at save into one table,
`workspace_policy_access(workspace, account, target_kind, target_id, action, ports)` (allows
minus denies), and token scopes into `api_token_scope_access(token, target_kind, target_id,
action, ports)`; both are recompiled in the same transaction whenever an input changes
(policy save, member join/leave/role change, machine bind/unbind/rename/removal, preview or
tunnel create/delete, token create/revoke). The two SQL checks the authorizations call today,
`team_machine_capability_allowed` and `machine_capability_allowed`, become one
`workspace_capability_allowed(actor, token, workspace, machine, capability, port)` reading
those tables (owner guard as today's personal branch); every caller (terminal and browser
terminal, exec, managed SSH, files/Inbox, preview and tunnel management, private access in
`machine_service_authorities`, `teams.AuthorizeMachineTx`, F17 inspector credentials) switches
in the same change, and the old functions and `team_machine_grants` are dropped (one owner of
state). ENV (`environment/vault_scopes.go` `authorizeVaultScopeTx`): the server refuses a vault
member grant the policy does not allow, and a change that removes `env_read` revokes the grant
exactly like `pb env team revoke` (rotation required); key delivery still needs a key holder's
client (`pb env grants sync`), and explain says "allowed, key not delivered yet". A save that
removes access advances the workspace generation, which fences active sessions the same way a
grant revoke does today.

**Migration, zero behaviour change.** One migration writes version 1 of each team's policy:
an `all_members` machine grant → `{"src": ["role:member"], "dst": ["machine:<alias>"],
"allow": [...]}`, a `selected_member` grant → `user:<email>`, an `inspect`/`replay` grant on a
preview or tunnel → `{"src": ["user:<email>"], "dst": ["preview:<name>"], "allow":
["inspect"]}`; inactive grants are dropped; `allow_private_access` is copied. Personal
workspaces start empty (owner only, as today). The migration test compiles every team's v1 and
asserts the table equals the old functions' answer for every member × machine × capability
before they are dropped.

#### File format

JSON with comments and trailing commas (HuJSON, as Tailscale; plain JSON is valid too, so
agents can write it with any JSON library). Stored verbatim (comments kept) with its canonical
JSON next to it. Unknown keys are errors. Full example and every field and error code:
`shared/policy/README.md`.

```jsonc
{
  "allow_private_access": true,               // team workspaces only
  "groups": {"group:oncall": ["user:tester@example.test"]},
  "tags":   {"tag:prod": ["machine:db1", "machine:web1"], "tag:ci": ["machine:runner"]},
  "rules": [
    {"src": ["role:member"], "dst": ["tag:ci"], "allow": ["terminal", "exec", "files"]},
    {"src": ["group:oncall"], "dst": ["tag:prod"], "allow": ["*"]},
    {"src": ["*"], "dst": ["tag:prod"], "deny": ["managed_ssh"], "note": "no SSH on prod"},
    {"src": ["group:oncall"], "dst": ["tag:prod"], "allow": ["private_access"], "ports": [5432]},
    {"src": ["group:oncall"], "dst": ["env:team"], "allow": ["env_read"]}
  ],
  "tests": [
    {"src": "user:tester@example.test", "dst": "machine:db1", "allow": ["exec", "private_access:5432"], "deny": ["managed_ssh"]}
  ]
}
```

- **Schema:** JSON Schema 2020-12, `shared/policy/schema.json`, served at `GET
  /v1/policy/schema`, printed by `pb policy schema`; a test keeps schema, parser and README
  in step.
- **Tests** name one `src` user and one `dst`; `allow`/`deny` list actions,
  `private_access:<port>` for one port. A failing test stops the save.
- **Validation** (one Go package, `shared/policy`, used by server and CLI): parse → shape →
  names (every user, machine, group, tag, preview, tunnel exists in this workspace) → limits
  → compile → tests. Errors are a list sorted by path, `{path: "/rules/3/dst/0", code:
  "unknown_machine", message}`, with stable documented codes; the same input always gives the
  same list.
- **References stay correct:** renaming a machine or removing a member/machine/preview
  rewrites the references in the stored policy in the same transaction (new version, audit
  "policy updated by rename of db2 → db3"), so a reused name never inherits access.

#### Explain and test API

- `POST /v1/workspaces/{ws}/policy/validate` `{policy}` → errors, test results, compiled
  counts. Dry run, nothing stored.
- `POST /v1/workspaces/{ws}/policy/explain` `{policy?, src?, dst?, action?, port?, token?}`:
  with src, dst and action → `{allowed, reason, rules: ["/rules/2"], guardrail?}` (reason:
  owner, rule, denied_by_rule, no_rule, port_not_allowed, private_access_off,
  outside_token_scope); with dst + action → who can; with src → what they can. `policy`
  evaluates a draft. Members may explain their own access; owners/admins anyone's.
- `PUT /v1/workspaces/{ws}/policy` `{policy, expected_version, message?, confirm?}` → validates
  and returns `access_changes` (gained/lost per member × target × action); it saves only when
  `confirm` is the digest of that list; a stale `expected_version` is 409.

#### Who may change what (user decision 2026-10-22)

Owners and admins edit everything. A member who owns a personal machine shared into the team
keeps today's right to set access for that machine: their save is accepted only if every rule
it adds, removes or changes targets only their own machines and nothing else changes (the
console's per-machine "Manage access" uses exactly this). Members read the policy and explain
their own access. In a personal workspace the owner edits everything.

#### Versions, audit, rollback

Every save is a new version (number, author and token if any, time, message, digest, access
change summary). `pb policy versions` / console History; restore = saving an old version as a
new one (same validation and review). The last N versions are kept (config). Audit/Activity:
`policy.updated`, `policy.restored`, `policy.rewritten` (rename/removal),
`api_token.created|revoked`, and access denials in the existing denied events with the
deciding rule path.

#### Console (maintainer paths, done by maintainer)

Workspace → **Access**:
- **Simple** (default): machines × people grid with today's capability checkboxes (the
  current "Manage access" dialog, unchanged), tag chips on machines, groups, the "Allow
  private access" switch. Rules the grid cannot show (tags, groups, ports, denies) appear as
  read-only rows "from the policy file" with an Edit link.
- **Policy file:** code editor with schema completion, inline errors at the exact path,
  Format, and **Review changes** (who gains/loses what) before **Save**.
- **Test:** pick who / machine / action / port (and optionally an API token) → Allowed or
  Denied with the deciding rule or guardrail, against the saved policy or the unsaved draft;
  runs the file's `tests`.
- **History:** versions with author, message, change summary, Diff and Restore.
Settings → **API tokens**: create (name, expiry, optional scope built from tags/machines and
actions), list (last used, expiry), revoke.

#### CLI (maintainer paths)

`pb policy get [--version N] [--json]` (file to stdout), `pb policy set <file|-> [--message]
[--yes] [--dry-run]` (validates, prints access changes, asks), `pb policy test [<file>]`
(validate + tests on the server, nothing saved), `pb policy explain --src <user> [--dst <m>]
[--action <a>] [--port <p>] [--file <draft>]`, `pb policy versions`, `pb policy restore <N>`,
`pb policy schema`; `pb api-token create <name> [--expires] [--scope]`, `pb api-token list`,
`pb api-token revoke <name>`. A token is used with `PAPERBOAT_API_TOKEN=<token> pb exec …` (any
command) on any computer with `pb`. All take `--workspace` and `--json`; errors print path
and code.

#### Limits (one config place, `server/internal/config`, same for every plan)

Policy file 256 KiB, 1000 rules, 500 tags, 200 groups, 500 entries per list, 500 tests, 100
versions kept, 100 API tokens per member per workspace, token lifetime max 365 days (default
90), explain "who can" answers bounded to 1000 rows.

**Not in v1** (WHITEBOARD if wanted): conditions on source device, network or time; tags
applied at enrolment (`pb pair --tag`); tag owners for non-admins; OAuth-style automation
clients; machines as `src`. Per-resource sharing stays as it is and shows read-only in the
Simple view (user decision 2026-10-22): preview/tunnel `use`/`manage` and lazy-policy grants
(`pb team grant`) and shared terminal sessions (`pb session share`).

- F18-01: Policy file format, JSON Schema served and printed, validation with stable path+code errors, limits; invalid files and failing tests are never saved.
- F18-02: Enforcement on the server: every machine capability (terminal, browser terminal, exec, managed SSH, files, preview/tunnel manage, private access with ports) and preview/tunnel inspect/replay (F17 inspector credentials) follows the compiled policy, deny over allow; tags and groups work; removing access ends active sessions as a grant revoke does today.
- F18-03: Migration: every team's existing machine grants, inspect/replay grants and private-access setting become policy v1 with identical access (compiled table equals the old answer for every member × machine × capability); personal workspaces unchanged.
- F18-04: Explain and test: allowed/denied with the deciding rule or guardrail, who-can and what-can, on the saved policy and on a draft; in-file tests run on validate and save; the owner guard holds against any deny.
- F18-05: Versions, access-change review before save, restore, audit events; stale saves refused; renames and removals rewrite references with an audit event.
- F18-06: API tokens: create/list/revoke with expiry, act as their creator, optional scope narrows (solo personal workspace "token may only exec on tag:ci" works), creator losing access takes it from the token, used by `pb` after `pb login --api-token-file`, every use attributed.
- F18-07: ENV in rules: env_read/env_write decide team ENV grants; removing access revokes as `pb env team revoke` does; explain reports "allowed, key not delivered yet".
- F18-08: Who may edit: owners/admins edit all; a member edits only rules for their own shared machines; members read and explain their own access.
- F18-09: Console Access page (Simple grid with today's grants, private-access switch, tags, groups; Policy file editor with inline errors and Review changes; Test; History with Diff/Restore) and Settings → API tokens (scripted browser check).
- F18-10: CLI `pb policy` and `pb api-token` commands work end to end against the live server (device check on the windows-test VMs).
- F18-11: Machine tags as a first-class action (user report: no way to tag machines): `PUT /v1/workspaces/{ws}/machines/{machine}/tags` and `GET /v1/workspaces/{ws}/tags` edit and list the policy file's tags map (the only store) as reviewed, audited versions with the editor's rights; tags on machine items; `pb machine tag <machine> add|remove|set`, TAGS and `--tag` in `pb machine list`, tags in `pb status`; console Edit tags, tag filter, Access links (maintainer).

### F19. Teams for business: SSO, directory sync, roles, audit log (maintainer, user decision 2026-10-23)

**What:** what companies pay Team seats for, required at launch. Free and Pro are everything for
one person; Team is collaboration plus these: single sign-on (SAML and OIDC) with verified domains
and enforcement, automatic member provisioning and deprovisioning (SCIM), more roles with one
permission matrix, and a searchable and exportable audit log streamed to the customer's SIEM;
invoices and billing details stay in the Stripe portal.
**Surfaces:** console team Settings → Security (SSO, domains, enforcement), Directory, Members
(roles), Activity (audit log: filters, export, streaming), Billing ("Manage billing" Stripe
portal); `pb team sso|scim|activity|role`; server identity, teams, activity and billing;
admin reference `server/docs/teams-business.md`.
**Owner:** maintainer across server/ (B), cli/ (A) and console/ (C, with maintainer's help), each change
small and logged in section 5; maintainer deploys. Billing parts in agreement with maintainer (F13);
roles as policy subjects in agreement with maintainer (F18).

**What we took from whom.** WorkOS (we already use AuthKit): SSO Connections, Directory Sync and
the Admin Portal, so a customer's IT admin sets up Okta, Entra ID, Google Workspace, OneLogin,
JumpCloud or any SAML/OIDC IdP themselves, with WorkOS's per-IdP guides; we never handle SAML
metadata or SCIM endpoints ourselves. Vercel and GitHub: "SSO required for this team" checked per
team (the account stays the person's own; workspaces share nothing), with a review of who loses
access before turning it on. Linear: JIT provisioning on SSO sign-in, directory group → role
mapping, directory-managed members locked against manual edits. Tailscale: roles are admin
permissions only (Owner, Admin, Billing admin, Auditor, Member); using machines is the policy's
job; log streaming to an HTTPS endpoint or S3-compatible storage. Stripe: its Customer Portal for
invoices and billing details; Stripe is the only owner of them.

#### Roles and permission matrix

Roles stay one per member: `owner` (the single `teams.owner_account`), `admin`, `billing`
(Billing admin), `auditor`, `member`. Roles are administrative permissions only; **using**
machines, ENV, previews and tunnels is decided by the workspace policy (F18), as today ("roles
never imply use"). Every role is a member and takes a seat.

| Permission | owner | admin | billing | auditor | member |
|---|---|---|---|---|---|
| See team, members, machines, settings, policy | ✓ | ✓ | ✓ | ✓ | ✓ |
| Invite, remove members, change roles (admins: only non-admins, never to admin) | ✓ | ✓ | | | |
| Team settings, machine approval, edit policy (F18-08) | ✓ | ✓ | | | own machines |
| SSO, domains, enforcement, exemptions, directory sync | ✓ | ✓ | | | |
| Read and export the audit log; streaming status | ✓ | ✓ | | ✓ | |
| Configure audit log streaming | ✓ | ✓ | | | |
| Billing: plan, seats, top-ups, Manage billing (Stripe portal) | ✓ | | ✓ | | |
| Delete team, transfer ownership | ✓ | | | | |
| Own API tokens, explain own access | ✓ | ✓ | ✓ | ✓ | ✓ |

One owner of the matrix: `teams.Allowed(member, permission)` in `server/internal/teams`; every
team endpoint and SQL authorization that checks a role today (`m.Role != "admin"` and the like)
switches to it in one change; the matrix is served at `GET /v1/teams/roles` for the console and
CLI, and a test asserts the README table equals the code. Policy subjects (maintainer): `role:billing`
and `role:auditor` are added next to `role:owner`, `role:admin`, `role:member`; `role:member` stays
"every active member", so a billing admin or auditor uses machines only when a rule allows it.

#### SSO (SAML and OIDC through WorkOS)

- **Organization.** Each team that starts SSO setup gets one WorkOS Organization (created by the
  server, id stored on the team; deleted when the team is deleted). Nothing exists for teams that
  never use SSO.
- **Self-serve setup.** Owners/admins click Set up SSO (or `pb team sso setup`): the server asks
  WorkOS for an Admin Portal link (`intent` = `domain_verification`, `sso` or `dsync`, return URL
  = the console team Security page) and the console opens it. The customer's IT admin verifies
  the domain with a DNS TXT record and connects the IdP there.
- **State** comes from the WorkOS Events API (`organization_domain.*`, `connection.*`,
  `dsync.*`), read by one server worker with a stored cursor every few seconds (config), events
  applied idempotently by event id; a full reconcile on activation and daily. No webhook to
  configure.
- **Sign-in.** AuthKit sends an email from a verified domain to that organization's IdP; IdP-
  initiated sign-in also works. The authentication response's `organization_id` and
  `authentication_method = SSO` are recorded; for every team of that organization the member row
  gets `sso_verified_at = now`. `pb login` stays the browser device flow, so it goes through SSO
  the same way.
- **JIT provisioning** (team setting, default off; directory sync makes it unnecessary): a person
  who signs in through the team's SSO and is not a member becomes a `member` (seat added as in
  provisioning below).

#### SSO enforcement and break-glass

- Team setting **Require SSO** (owners/admins), available once a connection is active and a domain
  is verified. Turning it on first shows who would lose access (members without a recent SSO
  sign-in or outside the verified domains), like F18's access review, and is confirmed by digest.
- While on, a member reaches the team (console, API, CLI, API tokens, new connections) only if
  `sso_verified_at` is within the team's **SSO session length** (setting, default 7 days, 1–30,
  limits in config). It is a guardrail in maintainer's `decide()` step 1 and in the team reads, with
  the typed error `team_sso_required` and the sign-in URL; the CLI prints "Sign in through your
  company's SSO: pb login". Like every credential gate (1.4c) it gates starting work only; it
  never cuts an admitted connection. Removal (directory or admin) cuts at once.
- Invitations go only to addresses in verified domains while enforced.
- **Break-glass:** the owner and up to 5 members marked **SSO exempt** (owners/admins set it;
  for contractors and emergency admins) keep access without SSO, so a broken IdP never locks the
  team out; the owner can turn enforcement off. Every exempt access without SSO is recorded
  (`sso.bypassed`, at most once per session per day) and shown on the Security page.

#### Directory sync (SCIM through WorkOS)

- Set up from the same Admin Portal (`dsync`). The console shows directory state, last sync, user
  and group counts, and the **group → role** mapping (owners/admins edit it; precedence admin >
  billing > auditor > member when someone is in several mapped groups; unmapped users get
  `member` unless the setting "Only provision mapped groups" is on).
- **Provisioning:** an active directory user whose email matches a Paperboat account becomes a
  member with the mapped role; without an account, a directory invitation is held (seat
  reserved, a welcome email sent) and turns into membership at their first sign-in with that
  email. Seats are added automatically when needed (charged as a normal seat change, F13).
- Members provisioned by the directory are marked **managed by directory**: their role and
  membership change only through the directory (console and CLI refuse with
  `directory_managed`). The owner is never removed or demoted by the directory (logged).
- **Deprovisioning:** a directory user deleted, suspended or (with "Only provision mapped
  groups") removed from every mapped group is removed from the team within one event poll,
  exactly like `pb team remove`: the membership generation advances so active sessions end
  immediately, their API tokens for the team are revoked, their shared personal machines leave
  the team, the seat frees. Their Paperboat account and personal workspace are untouched.

#### Audit log (Activity)

One audit log, today's `audit_events` table and `GET /v1/activity` extended; the separate team
activity endpoint is folded into it (one way to read). Personal workspaces have it too.
- **Recorded:** every access and admin event in the workspace: sign-in, SSO verification and
  bypass, `team_sso_required` denials; members, invitations, roles, exemptions, ownership;
  SSO/domain/directory changes and every directory provisioning action; machine enroll, share,
  approve, remove; policy, tokens (F18); ENV grants (never values); starting terminal, browser
  terminal, exec, SSH/SCP/SFTP/rsync, file transfer, preview/tunnel create/delete, private access,
  inspector use, and every authorization denial with its reason; billing changes (plan, seats,
  top-ups, portal sessions); export and streaming changes. Never contents, ENV values or
  secrets.
- **Event shape** (documented, stable, the same in API, export and stream):
  `{id, occurred_at, workspace, actor: {kind: user|api_token|directory|operator|system, id,
  email, token_id?}, action, target: {kind, id, name}, machine?, result: success|denied, reason?,
  ip?, user_agent?, request_id, details}` (`server/docs/teams-business.md`). Append-only (existing trigger); IP and user agent only on events from a
  signed-in request.
- **Retention per plan** in `server/internal/config` (Free 30, Pro 90, Team 365 days); the prune job deletes per workspace by its plan; the DB trigger's fixed 90 days becomes
  the workspace's configured value.
- **Search and filter** (API, console, `pb team activity`): actor, action (prefix), target,
  result, time range, free text over names and emails; cursor paging; owners/admins/auditors of a
  team, the owner of a personal workspace.
- **Export:** `GET /v1/activity/export?workspace=&format=csv|json&from=&to=&<filters>` streams a
  file (JSON = one JSON array); bounded by retention and a row limit in config; the export is
  itself recorded. Team plan.
- **Streaming to a SIEM** (Team plan; owners/admins; at most one destination of each kind):
  - *HTTPS:* POST of NDJSON batches (≤1 MiB or 5 s), `Content-Type: application/x-ndjson`, an
    optional custom header (e.g. `Authorization: Splunk <token>` or a Datadog key) stored
    encrypted, and `Pb-Signature: t=<unix>,v1=<HMAC-SHA256>` with a per-destination
    signing secret shown once. Works with Splunk HEC (raw), Datadog, Elastic, Axiom, Cribl, Panther
    and plain webhooks.
  - *S3-compatible:* endpoint, region, bucket, prefix, access key and secret (encrypted); objects
    `<prefix>/<workspace>/YYYY/MM/DD/<first>-<last>.ndjson.gz`, SigV4 PUT. AWS S3, Cloudflare R2,
    Backblaze B2, MinIO, GCS (interop).
  - At-least-once, in order, a cursor per destination; retries with backoff up to 1 h; events
    wait until delivered or their retention ends; "Send test event"; status (last delivery, lag,
    last error); owners/admins emailed once when a destination has failed for 1 hour.

#### Billing (with maintainer)

No invoices or billing details in our console or API (user decision 2026-10-23). "Manage billing"
opens the Stripe Customer Portal: invoices and receipts, payment methods, billing email, company
name, address and tax ID, printed on every invoice. Checkout collects the tax ID, name and address;
Stripe Tax computes tax behind one config switch. Pay by invoice is set per customer in the Stripe
dashboard (`send_invoice`); an overdue invoice follows the failed-renewal path (7 days of grace,
then suspension until paid, 1.3). Only the owner and billing admins open the portal.

#### CLI (maintainer paths)

`pb team sso status|setup|enforce on|off|exempt <email> [--remove]`, `pb team scim
status|setup|map <group> <role>`, `pb team activity [--actor] [--action] [--target] [--result]
[--since] [--until] [--search] [--json]`, `pb team activity export --format csv|json [--since]
[--until] -o <file>`; `pb team role` accepts the new roles. Setup commands print the Admin Portal
link (and open it where a browser is available). Streaming and billing stay in the console.

#### Decisions (user, 2026-10-23)

1. SSO and directory sync are available to Team workspaces with at least 10 seats (config
   value; WorkOS bills per connection). Smaller teams see "available from 10 seats".
2. Audit retention Free 30, Pro 90, Team 365 days (config); export and streaming Team only.
3. Pay by invoice through Stripe only, set per customer in the Stripe dashboard; no feature of
   ours (revised 2026-10-23, see Billing).
4. Tax ID, company name and address printed on invoices now; Stripe Tax computes tax when the
   single config switch `billing.stripe_tax` is on (the user turns it on once registered).
5. Deprovisioning removes the person from the team only; the account and personal workspace stay.
6. Every role takes a seat, billing admins and auditors included.

#### Not in v1

Directory groups as policy subjects (`group:` filled from the directory; WHITEBOARD), several
destinations of one kind, custom roles, SSO for personal workspaces, mandatory MFA settings (the
IdP's job), SAML-only Paperboat accounts without email.

- F19-01: Roles billing and auditor with the permission matrix in one place; every team endpoint and SQL role check uses it; `role:billing`/`role:auditor` policy subjects; change role in API, CLI and console.
- F19-02: SSO setup: team ↔ WorkOS organization, Admin Portal links for domain verification, SSO and directory sync (owners/admins only); domain, connection and directory state follow WorkOS events.
- F19-03: SSO sign-in: a verified-domain email signs in through the team's IdP (SP- and IdP-initiated, console and `pb login`); the SSO verification is recorded per team member; JIT provisioning when enabled.
- F19-04: Require SSO: review of who loses access before turning on; without a recent SSO sign-in the team is refused with `team_sso_required` everywhere (console, API, CLI, API tokens, new connections) while admitted connections continue; invitations limited to verified domains; owner and SSO-exempt members keep access, each bypass recorded; turning it off restores access.
- F19-05: Directory provisioning: directory users become members (or held invitations until first sign-in) with the role from the group mapping; seats added automatically; directory-managed members refuse manual edits; initial and daily reconcile.
- F19-06: Directory deprovisioning: a deleted or suspended directory user is removed within one event poll; active sessions end immediately, the member's team API tokens are revoked, shared machines leave the team; the owner is never removed; the personal account is untouched.
- F19-07: Audit log records every access and admin event of 'Recorded' with the documented shape (actor incl. API token and directory, target, result and denial reason, IP, user agent); never contents or secrets; append-only; retention per plan from config.
- F19-08: Audit log search and filter (actor, action, target, result, time, text) with cursor paging in API, console and `pb team activity`; owners, admins and auditors only for teams.
- F19-09: Audit log export as CSV and JSON for a time range and filters, streamed and bounded; the export is recorded.
- F19-10: Audit log streaming to HTTPS (NDJSON, signed, custom auth header) and S3-compatible storage: in order, at least once, retries, test event, status and failure email.
- F19-13: Console: Security (SSO, domains, Require SSO with review, exemptions, bypass log), Directory (state, group → role mapping), roles in Members, Activity filters/export/streaming (scripted browser check).
- F19-14: CLI `pb team sso`, `pb team scim`, `pb team activity` (filters, export) and new roles in `pb team role` work end to end against the live server.
- F19-15: Admin reference `server/docs/teams-business.md`: roles matrix, SSO and SCIM setup per IdP through the Admin Portal, enforcement and break-glass, audit event catalog and shape, export and stream formats with signature verification, billing; a test keeps the roles table and event catalog equal to the code.

### F26. Recipes: one command turns a computer into a configured machine (design, user decision 2026-11-16)

**What:** a recipe is a small file that says what happens on a machine when it joins: its tags,
what it may reach, which ENV vaults it can read, which repository it pulls config from, which
preview or tunnel it opens, and when it is removed. One command installs pb, pairs the machine
and applies the recipe, with nobody online and no prompt. Recipes **replace API tokens**
entirely (removed, no compatibility). Every section calls an existing pb command; the only new
things are the recipe itself, its token, and named ENV vaults.
**Surfaces:** `pb recipe create|list|show|update|rotate|revoke`, `pb pair [pbr_…]` (also reads
`PB_RECIPE`), the installer reading `PB_RECIPE`, `pb env vault create|list|delete` and
`pb env set --vault`; console Settings → Recipes (replaces Settings → API tokens); schema and
validation in `shared/recipe` (one owner for cli, server and console).
**Owner:** maintainer server + `shared/recipe`, maintainer cli, maintainer console; this section is the contract.

**What we took from whom.** Tailscale auth keys: reusable, ephemeral, pre-tagged, expiring;
key given by flag, file or env (`tailscale/cmd/tailscale/cli/up.go:102` `--auth-key` with
`file:`; `tailscale/cmd/containerboot/settings.go:96` `TS_AUTHKEY`). Headscale: key = public
prefix + stored hash, `Reusable/Ephemeral/Tags/Expiration` on the key
(`headscale/hscontrol/types/preauth_key.go:42-69`), ephemeral garbage collector that skips a
node with a live session (`headscale/hscontrol/app.go:160-192`). Teleport: a join token names
what the joiner becomes (`api/types/provisioning.go:42,299-314`); tbot's one file of
onboarding + outputs + services (`assets/loadtest/ansible-like/tbot.yaml`). cloud-init: one
user-data line runs at first boot and is safe to run again. devcontainer.json / fly.toml: one
small declarative file per thing, arrays of tables (`[[services]]`) for repeatable sections.
1Password service accounts: the token itself carries the key material, access is per named
vault, nobody has to be online (`OP_SERVICE_ACCOUNT_TOKEN`). GitHub tokens: fixed prefix and
shape so secret scanners find leaks.

#### 1. Three recipes

**CI runner: tests with ENV and a preview** (`ci.toml`)

```toml
# A GitHub Actions runner: canary secrets for the tests, a 2-hour preview of the app.
tags = ["ci"]
max_machines = 20
remove_after_offline = "30m"

[env]
vaults = ["canary-env"]

[[preview]]
port = 3000
ttl = "2h"
```

```yaml
# .github/workflows/test.yml (secret PB_RECIPE holds the token)
- run: curl -fsSL https://get.pprbt.dev/install | bash
  env: { PB_RECIPE: "${{ secrets.PB_RECIPE }}" }
- run: pb env run -- npm test
- run: npm start &        # the recipe's preview serves port 3000 for 2 hours
```

**Homelab VM fleet: config pulled from a repo, a tunnel per VM** (`homelab.toml`)

```toml
# Every VM created from this image pulls dotfiles and publishes its dashboard.
tags = ["homelab"]
expires = "365d"
remove_after_offline = "7d"

[config]
repository = "dotfiles"      # connected once with `pb config repository connect`

[[tunnel]]
port = 8123                  # name defaults to the machine name
private = true
```

```yaml
#cloud-config
runcmd:
  - curl -fsSL https://get.pprbt.dev/install | PB_RECIPE=pbr_k7f3q9xa2m_... bash
```

**AI-agent sandbox: may run commands only on `tag:ci`** (`agent.toml`)

```toml
# A disposable sandbox for a coding agent: it can exec and copy files on CI machines, nothing else.
max_machines = 1
remove_after_offline = "10m"

[[reach]]
dst = ["tag:ci"]
allow = ["exec", "managed_ssh", "files"]
```

```sh
docker run -e PB_RECIPE "$PAPERBOAT_IMAGE"   # the release image (cli/deploy/container) reads PB_RECIPE
# inside the sandbox the agent runs:  pb exec runner-7 -- make test
```

#### 2. CLI session (real output)

```text
$ pb recipe create -f ci.toml
Recipe ci created in workspace acme.
  Tags        ci
  Reach       nothing (machines from this recipe cannot use other machines)
  ENV         canary-env (key wrapped on this computer; Paperboat cannot read it)
  Preview     port 3000 for 2h
  Machines    up to 20; removed 30m after going offline
  Expires     2027-02-14 (90 days)

Token (shown once; save it as the secret PB_RECIPE):
  pbr_k7f3q9xa2m_g4zt6vw2nr6yh1sk9df3gj5lp7xc0bn2ma4eu6io8qrst2uvwxy

Install, with PB_RECIPE set:
  Linux, macOS  curl -fsSL https://get.pprbt.dev/install | bash
  Windows       irm https://get.pprbt.dev/install.ps1 | iex
  pb installed  pb pair

$ pb recipe list
NAME      TAGS      MACHINES   EXPIRES      LAST USED
agent     -         0 / 1      2027-02-14   never
ci        ci        3 / 20     2027-02-14   4 min ago
homelab   homelab   5          2027-11-16   2 days ago

$ pb recipe show ci
Recipe ci, revision 2, token pbr_k7f3q9xa2m_…, expires 2027-02-14, created by tester@acme.test
<the file as created, comments kept>
Machines (3 of 20)
  fv-az512-118   online    applied revision 2   3 min ago
  fv-az512-204   offline   removed in 24 min
  fv-az512-311   online    preview failed: preview limit reached (pb doctor on the machine)

$ pb recipe update -f ci.toml
Recipe ci updated to revision 3: preview ttl 2h -> 4h. 2 online machines apply it now.

$ pb recipe rotate ci
Rotating ci gives it a new token and a new key for vault canary-env.
  The old token stops working at once. Its 3 machines keep running and get the new key.
  Then replace PB_RECIPE everywhere the old token is stored.
Rotate with: pb recipe rotate ci --confirm K3M9QX
$ pb recipe rotate ci --confirm K3M9QX
Recipe ci rotated. New token (shown once):
  pbr_k7f3q9xa2m_w8bd2kq5mt3zr7fc1hx6yn4jp9ga2se5vl3ou7ic4rbn6dz2tqk

$ pb recipe revoke ci
Revoking ci:
  - its token stops working
  - its 3 machines are removed: fv-az512-118, fv-az512-204, fv-az512-311
  - vault canary-env gets a new key (1 other recipe, canary, keeps access)
Revoke with: pb recipe revoke ci --confirm 7RT2WD
$ pb recipe revoke ci --confirm 7RT2WD
Recipe ci revoked. 3 machines removed. canary-env has a new key.
```

On the machine (the installer is the same script for everyone; the token never goes in a URL):

```text
$ curl -fsSL https://get.pprbt.dev/install | bash
Paperboat 2026.11.16 for linux/amd64
Installed pb to /usr/local/bin/pb.
Paired fv-az512-118 to workspace acme with recipe ci.
  Tags      ci
  ENV       canary-env, 4 values, in new shells and pb env run
  Preview   https://calm-summer-beach-3476.pprbt.app -> port 3000, until 16:42 IST
Done in 6 s. Check it any time with pb doctor.
```

Running the installer again on the same machine with the same recipe prints `Already paired
from recipe ci; revision 3 applied.` and exits 0 (cloud-init and CI caches rerun it). Windows:
`$env:PB_RECIPE='pbr_…'; irm https://get.pprbt.dev/install.ps1 | iex`. Container:
`-e PB_RECIPE`, consumed on first start like today's enrollment credential.

#### 3. Every error in the first five minutes

Writing the file (`pb recipe create|update`, console editor; same text from `shared/recipe`):

```text
Error: ci.toml:7:9: expected a value after "=".
Error: ci.toml:3:1: unknown key "tag". Did you mean "tags"?
Error: ci.toml:9:1: unknown section [envs]. Did you mean [env]?
Error: ci.toml:4:16: max_machines must be a whole number, like max_machines = 20.
Error: ci.toml:5:24: remove_after_offline must be a duration like "30m", "12h" or "7d" (5m to 30d).
Error: ci.toml:2:1: expires must be at most 365d. Rotate the recipe to extend it.
Error: ci.toml:12:1: [[preview]] needs port, like port = 3000.
Error: ci.toml:16:1: [[tunnel]] needs port or from, not both.
Error: ci.toml:19:7: allow has unknown action "ssh". Did you mean "managed_ssh"? Actions: pb policy schema
Error: vault "canary" does not exist in workspace acme. Your vaults: canary-env, prod-env. Did you mean "canary-env"?
Error: repository "dotfile" is not connected in workspace acme. Connected: dotfiles. Did you mean "dotfiles"? Connect one with pb config repository connect <url>.
Error: a recipe named ci already exists. Change it with pb recipe update -f ci.toml.
Error: recipes in team acme are created by owners and admins (a recipe tags machines, which edits the access policy).
```

Vault locked (create, update, rotate or revoke of a recipe with vaults):

```text
Error: recipe ci reads vault canary-env, and your ENV vault is locked on this computer.
Unlock it with pb env vault unlock, then run the command again. The vault key is wrapped for
the recipe here, on your computer; Paperboat never sees it.
```

On the machine (the installer and `pb pair`):

```text
Paperboat: PB_RECIPE is not set. Copy the token from pb recipe create or the console
(Settings -> Recipes) and run: curl -fsSL https://get.pprbt.dev/install | PB_RECIPE=<token> bash

Paperboat: PB_RECIPE is not a recipe token. A token starts with pbr_ and is 67 characters;
this value is 41. Copy it again from where you saved it.

Paperboat: this recipe token expired on 2027-02-14 at 12:00 IST.
Get a new token with pb recipe rotate ci (its machines keep working), then update PB_RECIPE.

Paperboat: this recipe token was replaced on 2026-12-02 (pb recipe rotate ci).
Use the new token; update PB_RECIPE where this one is stored.

Paperboat: this recipe token is not valid: the recipe was revoked or the token was mistyped.

Paperboat: recipe ci already has 20 of 20 machines.
A machine leaves the recipe when it is removed (pb machine revoke <machine>, or by
remove_after_offline). To allow more, set max_machines and run pb recipe update -f ci.toml.

Paperboat: workspace acme has reached its plan's machine limit. Remove machines you no longer
use (pb machine revoke <machine>) or upgrade under Billing in the Paperboat console.

Paperboat: installing needs root or passwordless sudo, and this shell has neither.
Run the installer as root (in Docker the default user is root), or allow passwordless sudo for
this user. Recipe installs never ask for a password, so they never hang a CI job.

Paperboat: installing needs an administrator PowerShell. Start PowerShell with "Run as
administrator" and run the command again.

Paperboat: this machine is already paired to workspace other-team as build-3. To pair it from
this recipe instead, remove it first (pb uninstall), then run the installer again.

Paperboat: paired fv-az512-118, but the preview did not start: workspace acme has reached its
plan's preview limit. Stop a preview (pb preview list, pb preview stop <name>). Paperboat
retries every few minutes; pb doctor shows the state. (exit status 3)
```

Pairing is atomic (machine, tags, credentials in one server transaction); a later section that
fails leaves the machine paired, prints that section's own error, exits 3, and is retried by
the daemon with bounded backoff.

#### 4. The file

TOML or JSON, chosen by extension; the recipe's name is the file's base name (`ci.toml` → `ci`,
lowercase letters, digits, `-`, up to 63). Unknown keys are errors with a suggestion. The client
parses and validates, then sends canonical JSON plus the source text; the server validates the
JSON strictly again (`shared/recipe`) and stores both (the source keeps comments for `show`).

| Key | Type, default, rules | Calls (in-process, the same code as the command) |
|---|---|---|
| `tags` | tag names, default none; policy tag syntax | `pb machine tag <m> add <tags…>`, done server-side inside pairing |
| `[[reach]]` `dst`, `allow`, `ports` | policy rule fields; default none = this machine's pb reaches nothing | today's API-token scope check (`api_token_scope_allows`), renamed `recipe_reach_allows`; never more than the creator may do at that moment |
| `expires` | duration, default `90d`, max `365d` | token expiry (server) |
| `max_machines` | integer ≥ 1, default no limit | refusal at pairing (server) |
| `remove_after_offline` | duration `5m`–`30d`, default never | `pb machine revoke <m>` by the server, as Headscale's ephemeral collector |
| `[env]` `vaults` | vault names in the recipe's workspace, required in `[env]` | ENV delivery + `pb env load` / `pb env run`, decrypting with the recipe key |
| `[env]` `shell` | bool, default `true` | `pb env hook --install` for the installing user |
| `[config]` `repository` | a connected repository's name, required in `[config]` | `pb config repository connect` (already done by the creator) + `pb config sync apply --repository <name>` with machine mode `pull_only`; approval = the recipe's creation |
| `[[preview]]` `port` | 1–65535, required | `pb preview <port> --background` |
| `[[preview]]` `ttl`, `private`, `team`, `host_header`, `domain` | as the flags; `ttl` default `1h`, max `24h` | `--ttl`, `--private`, `--team`, `--host-header`, `--domain` |
| `[[tunnel]]` `name` | default the machine name (tunnel names need not be unique) | `pb tunnel create <name>` |
| `[[tunnel]]` `port` or `from`, `private`, `team`, `domain`, `duration` | exactly one of `port`/`from` | `--port`, `--from`, `--private`, `--team`, `--domain`, `--duration` |

Rules: config is pull only (many machines must not push to one repository); new repository
revisions apply without review only if the repository's shared file sets
`automatic_updates = true`, otherwise they wait for `pb config approve` like any machine. A
preview expires for good (re-applying never restarts an expired preview). The machine name is
the host name (`pb pair --name` on a clash, as today). Recipes live in one workspace (the
`--workspace` at creation). Not in v1: running arbitrary commands (the CI job or cloud-init
does that), machine availability, inbox and terminal settings; each can become a section
later, one existing command each.

#### 5. Console: Settings → Recipes (replaces API tokens)

- **List:** table Name, Tags, Machines (`3 / 20`), Expires, Last used; row menu Show, Rotate,
  Revoke. Empty state: one sentence ("A recipe pairs and sets up machines with one command")
  and the CI example as a "Start from" card, plus Homelab and Agent sandbox cards.
- **New recipe / edit:** left a form (Tags, Reach rows with target + action chips, Max
  machines, Remove after offline, ENV vaults picker, Config repository picker, Preview and
  Tunnel rows); right the same recipe as TOML, live, editable; both views edit one value and
  show the same `shared/recipe` errors inline. Picking a vault asks for the browser vault
  unlock (existing `console/lib/envvault`), which wraps the key in the browser.
- **Created:** the token once, Copy buttons for the token and for the Linux/macOS and Windows
  install lines, "I saved it" closes; a GitHub Actions snippet under a disclosure.
- **Show:** the file, revision, creator, its machines with apply state per section (links to
  each machine), Rotate and Revoke with the same impact text and confirmation as the CLI.

#### 6. Architecture

- **Token:** `pbr_<id>_<secret>`, `id` 10 and `secret` 52 lowercase base32 characters (32 random
  bytes); fixed shape for secret scanning. The device derives two values with HKDF-SHA256:
  `auth = HKDF(secret, "paperboat recipe auth")`, sent to the server, which stores only
  SHA-256(auth); and `key = HKDF(secret, "paperboat recipe key")`, an X25519 private key that
  never leaves the device. The server can admit a machine but can never decrypt a vault.
- **Tags and reach:** pairing creates the machine under the creator's account in the recipe's
  workspace and adds it to the recipe's tags in the policy (one policy version, author "recipe
  ci") in the same transaction, so the machine never exists untagged. Its pb gets an ordinary
  CLI session of the creator carrying `recipe_id` (today's `cli_client_sessions.api_token_id`
  path, renamed); every machine authorization also checks `recipe_reach_allows`. The creator
  leaving the workspace or losing a right takes it from the recipe at once; leaving revokes it.
- **Ephemeral removal:** when a recipe machine's last control connection ends, the server
  schedules removal at now + `remove_after_offline`; a new connection cancels it; removal is
  the `pb machine revoke` path (hidden at once, hard-deleted after revocations deliver).
- **Vaults:** a named vault has its own 32-byte key, kept in the owner's key ring (team vaults:
  through today's team key grants); values are encrypted once with it. Creating a recipe with
  vaults: the unlocked client derives the recipe public key from the new secret and uploads
  each vault key wrapped to it. The device unwraps with its derived private key, keeps that key
  in protected ENV key storage (`cli/internal/hostruntime/environmentkey`) and decrypts only
  those vaults, with nobody online. A value change is one encryption with the vault key,
  delivered by today's ENV delivery (new shells, `pb env run`). **Revoke:** delete the recipe's
  wraps, make a new vault key, re-encrypt the values, re-wrap for remaining recipes and members.
  **Rotate:** the same re-key with a new secret for this recipe; the new recipe private key is
  sent to each of its live machines encrypted to that machine's existing ENV host-recipient key
  (threat-model "ENV host-recipient custody"), so they keep working while the old token can
  neither pair nor read new values (values it already read stay read: rotate the secrets too).
- **Apply and re-apply:** `pb pair` exchanges `auth` for machine credentials and the recipe's
  canonical JSON + revision; the daemon stores the revision and a list of what each section
  created (preview id, tunnel id, managed machine config file). Apply runs tags (done
  server-side), env, config, preview, tunnel, each an "ensure": present and matching → nothing;
  missing → create; changed → change; removed from the recipe → delete what it created. The
  daemon re-applies on start and when the server pushes a new revision (machine events), so
  running it twice is a no-op. A failed section is retried with backoff (1 min doubling to 30
  min) and shown in `pb doctor` and `pb recipe show`.
- **Installs never prompt:** root runs directly; otherwise `sudo -n` only; neither → the error
  above (today `cli/internal/hostruntimecmd/bootstrap_unix.go:512` always calls `/usr/bin/sudo`
  and may ask `sudo -v`; a recipe install skips sudo as root and never runs `sudo -v`).
  Containers use `cli/deploy/container` with `PB_RECIPE`.

#### 7. API tokens today and after

| Use today | After |
|---|---|
| `pb login --api-token-file`: a pb session of the creator for scripts and agents (exec, terminal, SSH, files, previews, tunnels), narrowed by scope | a machine from a recipe: its pb has the same session, narrowed by `[[reach]]` (agent sandbox example) |
| Scope check on machine access (`usermachines/shared_access.go:424`, `nativeprivateaccess`, SQL `api_token_scope_allows`) | the same check keyed by recipe, `recipe_reach_allows` |
| `Authorization: Bearer pbat_…` on any HTTP route (`httpapi/router.go:740`, `api_token_handlers.go:26-120`) | removed; scripts drive `pb … --json` on a recipe machine (see open question 3) |
| `QUICK_UPDATE_API_TOKEN` mints enrollment tokens (`.github/workflows/quick-update.yml:96`, `tools/quick-update/run.sh:7`) | a reusable ephemeral recipe token stored as the secret |
| `pb policy explain --api-token`, `shared/policy` `DecideToken` | `--recipe <name>`, `DecideRecipe` |
| Audit/activity actor `api_token_id`, policy version author | `recipe_id`, author "recipe <name>" |
| Plan-limit session counting (`planlimits/sdk.go:64`), offline access exclusion (`peersessions/offline_access.go:63`) | same rule for recipe sessions |
| Smoke: `policy_token.py`, `mint_api_token.mjs`, `staging_api_token.py` | one recipe check (pair, tags, reach refusal, ENV, preview, rotate, revoke) |

Delete: `cli/cmd/pb/api_token_command{,_test}.go`, `auth_api_token_login{,_test}.go`,
`cli/docs/man/man1/pb-api-token*.1`, `cli/docs/cli/pb_api-token*.md`; `server/internal/apitokens/`,
`server/internal/auth/api_token_session.go`, `api_token_source.go`,
`server/internal/httpapi/api_token_handlers.go`, `api_token_postgres_test.go`,
`api_token_scope_route_test.go`, `token_scope_code_test.go`,
`server/internal/usermachines/api_token_scope_postgres_test.go`, `server/internal/teams/policy_tokens.go`,
`server/internal/db/queries/api_token_scope.sql`, `dbsqlc/api_token_scope.sql.go`;
`shared/client/session/source{,_test}.go` (token login only; confirm no caller);
`console/app/(console)/settings/api-tokens/`, `console/components/access/api-tokens.tsx`,
`new-token.tsx`; `tools/device-smoke/mint_api_token.mjs`, `staging_api_token.py`,
`policy_token.py`. Edit the token branches in `server/internal/httpapi/{router,router_02,auth_handlers,errors,policy_handlers,workspace_scope}.go`,
`server/internal/{app,config,audit,activity,accountdeletion,teams,usermachines,nativeprivateaccess,peersessions,planlimits}`,
`shared/policy`, `cli/internal/api/{policy,device,errors}.go`, `console/lib/{policy,activity,sections,query-keys}.ts`,
marketing pages that sell API tokens for agents, `server/docs/openapi.json`. Database: a
post-deploy migration drops `api_tokens` (127), `api_token_scope_rules` (133),
`api_token_create_requests` (153), `cli_client_sessions.api_token_id`,
`workspace_policies.updated_with_api_token`, `workspace_policy_versions.api_token` and
`api_token_scope_allows`, after the recipe tables (`recipes`, `recipe_reach_rules`,
`recipe_vault_wraps`, `cli_client_sessions.recipe_id`) ship.

**Acceptance (CHECKLIST rows to add when built):** F26-01 create/list/show/update/rotate/revoke
with the texts above; F26-02 install from a recipe on Linux, macOS, Windows and a container, no
prompt, rerun is a no-op; F26-03 tags at pairing and reach refusal outside `[[reach]]`; F26-04
vault values readable with nobody online, revoke and rotate re-key; F26-05 config pull,
preview, tunnel sections applied and re-applied; F26-06 max_machines and remove_after_offline;
F26-07 every error in section 3; F26-08 API tokens gone (CLI, server routes, console, DB).

**Open questions (USER DECIDES):**
1. `pb env vault` already names your password-unlocked key ring (`init|unlock|lock|…`). Rename
   those to `pb env unlock|lock|password|recover|…` so "vault" means a named vault, as in
   1Password? (Recommended.) Otherwise named vaults need another word.
2. Reuse `https://get.pprbt.dev/install` (one script, reads `PB_RECIPE`) instead of a separate
   `/recipe` path? (Recommended: one installer.)
3. No bearer credential for the HTTP API at launch: scripts use `pb … --json` on a recipe
   machine. OK?
4. Expiry default 90 days, max 365, no "never" (extend by rotating). OK?
5. Revoke also removes the recipe's machines; rotate keeps them. OK?

### S2. Marketing site

- S2-01: Content matches the product (features, platforms 1.1, pricing 1.3, domains 1.2); install instructions use the configured installer host; builds; hosting **USER DECIDES**.

### S3. Release

- S3-01: CI release from a tag builds all five binaries, the macOS `.pkg` and the Windows installer, signs them, publishes TUF metadata, and publishes downloads to a **public** location (agree the host with the user).
- S3-02: The installer one-liners install the release.
- S3-03: Server, relay and tunnel multi-arch images built from the release commit, published, and deployed to our infrastructure with the installer's upgrade path.
- S3-04: Full smoke suite (every feature F1–F15) green on the release build on L, M, W.
- S3-05: User and operator docs (`cli/docs`, `server/docs`, READMEs) match behaviour (each feature's docs fixed when the feature is verified).
- S3-06: Operator admin frontend: decided 2026-11-05, none; operator actions are API-only (/v1/admin/*).

### 3a. Shared endpoint engine: server and edge design (maintainer, approved 2026-10-22)

Approved (coordinator, user decision 2026-10-20; 2026-10-22): built now as written by maintainer end to end (server, edge, CLI
carriers, deletions), server-first in small deployable steps; maintainer deploys. Rows: F7-03, F7-04, F7-05, F8-06 on Linux.
Decision (user, 2026-10-20): previews and tunnels run on ONE engine after Linux, before macOS; the UX stays distinct
(tunnels like Cloudflare Tunnel: persistent, named, many routes and protocols; previews like ngrok: one command, ephemeral,
one URL). Both-edges serving is a property of the engine, not of either feature.

**Today (three attach flows over one carrier listener).** Previews: a lease + one fresh edge carrier per preview, an
attachment row with an outbox the edge drains, per-preview DNS, ~5.2k lines `previewattachment` + 2k `previewtunnelstore` +
edge `runtime/preview_carrier.go` + CLI `hostruntime/preview` AttachmentCarrier. Tunnels: tunnel + connector + session +
config generations + `tunnel_edge_route_assignments`, connector credentials, durable carrier admission (~8.4k `tunnelv1`,
~7.3k `controlplane`, ~6.3k `connectorprotocol`). Runtime hostnames: a per-machine carrier to BOTH edges (primary active,
standby ready), already active-active. Each flow has its own lifetime, readiness, DNS rule and cleanup, which is why a
preview flips edges under its URL and a tunnel's second route stalled.

**Model.** One resource, `endpoints(id, kind preview|tunnel, account, owner_team, agent_id, access_mode, lifetime
lease|persistent, lease_deadline, desired_state, generation)`, with `endpoint_routes` (hostname or generated name,
protocol http|ws|tcp|tls|private_tcp, match, origin on the agent, access policy) and `endpoint_hostnames` (generated under
the platform wildcard; custom domains for tunnels and previews). Kind is a policy preset, not a code path: preview = one http route,
lease with renewal, generated immutable name, deleted on expiry; tunnel = many routes, persistent, rename is a label,
custom domains, connector rotation. `/v1/previews/*` and `/v1/tunnels/*` stay as thin facades over `endpoints`.

**Carriers belong to the agent, not the endpoint.** An *agent* (an enrolled machine's daemon identity; no headless connectors, user decision) holds N carriers to EACH edge of its workspace's edge set. These
are today's runtime carriers, extended. An endpoint never opens a carrier: creating or changing it pushes a route
generation over carriers that already exist, so a preview is ready in one control round trip (no TLS/QUIC dial, no
admission outbox). Streams for any endpoint multiplex over any ready carrier of that edge.

**Placement.** The edge set (`workspace_edge_sets`, 2 edges, distinct failure domains, managed or own servers, never mixed,
dead member replaced after 10 min) is the only placement input. A reconciler creates
`endpoint_edge_assignments(endpoint_id, edge_node_id, route_set_generation, desired, observed, observed_at)` for every edge of
the set where the agent has a ready carrier; the edge pulls ONE complete snapshot of its assignments (complete:true, last
known good on failure; today's preview admission shape) and reports observed state. Serving = at least one assignment
observed ready; healthy = all edges of the set.

**DNS.** `edge_dns_publications` stays; one query lists the IPs of edges with an observed-ready assignment (TTL 60), so the
record can never name an edge that lacks the route, and a created endpoint's URL is returned only after the record is
written and at least one assignment is ready (the second edge may follow within seconds). Edge loss: the edge leaves the
record at once (ready flag), the set replaces it after 10 min, the agent opens carriers to the new member, assignments
follow, DNS follows. No edge-to-edge forwarding.

**Health, lifetime, cleanup (one owner each).** One lease reaper (preview deadline, user stop, owner machine revoked); tunnel
desired_state/delete; both end by setting `desired=removed` on assignments, the edge acks removal, then rows and DNS go.
Origin health (`tunnel_route_origin_health`) and usage/bandwidth keep their keys (endpoint_id replaces tunnel_id/preview_id).

**Removed.** Server: `preview_lease_carrier_attachments`, `preview_carrier_attachment_outbox`, per-preview carrier issuer and
edge-node selector, `previewattachment` (5.2k), `previewtunnelstore` (2k), preview DNS special cases (intent/attachment
branches), the whole connector identity stack: `tunnel_connectors`, `tunnel_connector_enrollments`,
`tunnel_connector_credential_generations`, `tunnel_connector_rotation_targets`, `tunnel_connector_activations`,
`tunnel_connector_sessions` (the agent's carrier session replaces them; tunnels use the machine identity), `serves_*` placement leftovers, duplicate readiness paths. Edge: preview admission worker,
durable-connector admission registry and connector credential rotation (one admission type remains: the agent carrier), preview alias reconciliation in its own
registry (routes come from assignments). CLI (maintainer, same series): `hostruntime/preview` AttachmentCarrier, the
tunnel connector's own carrier dial; both become "register routes with the agent daemon". Estimate: about -9k/+4k server,
-3k/+1k edge.

**Migration.** Existing rows move in the server migration series: tunnels, routes, domains, config generations map 1:1 to
`endpoints`/`endpoint_routes`/`endpoint_hostnames` (ids kept, so URLs and DNS do not change); connectors collapse into their machine's agent identity (a tunnel is owned by the machine that created it); assignments are recomputed by the reconciler; live preview leases are ended at cut-over (minutes-long, pre-release);
certificate and DNS tables are untouched. Forward-only migrations numbered by whoever lands them, each rehearsed on a
restored production backup.

**Order (each step deployable server-first, every historical smoke check stays green).**
1. Agent carrier model: N carriers per edge; carriers advertise `endpoints` capability; PG + edge tests with two edges.
2. Tables + reconciler + edge snapshot/observation for `kind=preview` only; new `/v1/endpoints`; preview facade switched, old
   attachment path deleted in the same series (no dual path kept).
3. CLI: preview via the agent daemon (maintainer); smoke preview, preview-protocols, preview-private.
4. Tunnels onto endpoints (routes, domains, rotation, private_tcp, catch-all); CLI tunnel manager; smoke tunnel*, tcp_private.
5. Deletion commits for everything in "Removed"; DNS/health/failover runbook rewritten; chaos test: kill one edge mid-traffic.

**Step 4 slices (maintainer, 2026-10-23).** Today a tunnel's carrier identity, stream preface, ingress decision and control
session (Hello/Welcome/snapshot/ack/rotation) are all bound to one tunnel's connector; an agent carrier serves many tunnels, so
route authority moves from the carrier identity to the route (as preview endpoint routes already do: RouteID-authorized
admissions on the runtime carrier). Each slice is gated on an agent capability `tunnel_endpoints` plus every edge serving
endpoints, so non-capable machines keep the connector path until the slice that deletes it.
4.1 Server: a capable tunnel becomes `endpoints(kind=tunnel, id=tunnel id)` + `endpoint_routes(id=tunnel route id,
revision=route generation)`; runtime admissions carry the route's match (host, match type, path prefix, priority, protocol,
domain bindings); tunnel ingress, health/readiness and DNS read endpoint assignments the way preview_routes does.
4.2 Edge: tunnel endpoint admissions become matcher rules whose replica is the agent's runtime carrier (HTTP/WS first).
4.3 CLI: the agent serves tunnel routes on its hubs (OriginStreamForwarder per route) from a machine-authenticated route
list; the tunnel manager stops dialing its own carrier when served.
4.4 TCP, TLS passthrough, private_tcp and custom domains on the same path. 5: deletions (connector stack, attachments), the unused single-connection connector.Manager/Supervisor: audit T9).
Gate per slice: smoke tunnel-manage, tunnel-protocols, tunnel-transfer, tunnel-survival, tunnel-lifecycle, tcp-private green.
4.3 on prod, 2026-10-23/26: two rollouts failed on prod edges and were rolled back to tunnel .9. (1) Engine route rows carried the
runtime carrier's thumbprint format; the edge's durable registry wants the connector JWK thumbprint (fixed c6b4b834, golden contract
test across server and edge). (2) Prod edges check readiness with a connector_ready probe (their route registry has an ingress
authority); the agent's engine forwarder did not answer it (fixed b12530ed; end-to-end contract: server golden row -> real edge
carrier and route registry -> recorded probe -> real agent hub). Staging (linux-test-vm-4, self-hosted edge): the engine route goes ready
(22:13:45Z 2026-10-25). Staging cannot serve HTTPS for a tunnel (self-hosted edges need a verified customer domain and staging cannot
issue a certificate: Cloudflare DNS-01 only), so tunnel-manage cannot run there; F20-02 [!] waits for a coordinator decision
(single-edge prod canary with rollback ready, or a staging certificate path).
4.3 as built (branch test-43): only public HTTP tunnels move (a private/team route needs an agent-side ingress authority for
engine routes first, which 4.4 adds); the agent refetches its engine tunnels on the `tunnels` machine event with a 1-min safety
poll, verifies each snapshot's content hash, and serves every active route on every carrier hub; the tunnel manager keeps no
connector for a moved tunnel (reconcile within 5 s) and brings it back if the tunnel leaves the engine.
*Prior art (4.3, failover, drain; ~/ref, read 2026-10-23).* One process holding all routes over a few long-lived edge connections:
cloudflared keeps `HAConnections` (default 4) spread over two edge regions (`supervisor/supervisor.go:105-186`,
`edgediscovery/allregions/regions.go`), and serves every ingress rule over each of them, first match on an immutable rule list
(`ingress/ingress.go:42-68`): our 2-edge pair with every route on both carriers is the same shape. Config by version push, not
polling: cloudflared applies remote config only on a version change (orchestration), Tailscale sends one full map then deltas
(tailcfg PeersChanged/Removed): our machine-event topic + content-hash check follows that. Server-initiated moves: DERP sends
`ServerRestartingMessage{ReconnectIn, TryFor}` before a restart (`tailscale/derp/derp_client.go:500-517`); Teleport's proxy
`DrainConnections` closes its listener and advises every agent to reconnect elsewhere while open connections finish
(`teleport/lib/reversetunnel/srv.go:673-689`, agent side `agent.go:552`); cloudflared's connector sends `UnregisterConnection`
and keeps a grace period on its own shutdown (`connection/control.go:120-140`). Ours (agreed with maintainer): the edge marks itself
draining and sends GOAWAY {reason, drain_deadline} as an edge-opened stream, admits no new streams on that node's carriers and
lets in-flight ones finish (<=20 s); the server's own requests already dial an edge with a valid carrier on a non-draining
node (d7b68a39), and the agent's backup carrier on the other edge serves at once. Host-initiated drain (audit T10, cloudflared
UnregisterConnection) is the mirror: the agent tells the edge before closing a carrier. Private shares: zrok keeps private
shares reachable only through an authenticated `access` frontend (`zrok/controller/access.go`, share.go private token); our
restricted routes keep the edge's signed browser decision plus the agent's own authority check, so they move only in 4.4.

4.4 design (2026-10-26, maintainer). One rule for every engine stream: a public stream (HTTP, TCP, TLS passthrough) carries no
ingress decision preface, the edge alone authorizes it and applies ingress limits (as public previews do, 0fe5072e/f60f7a53);
a restricted stream (team/private HTTP) is opened with kind `http_browser` plus the edge's signed browser decision, and the
agent re-checks it with the server's machine-authenticated browser authority (`/v1/browser-access/ingress/authorize`, the
lookup restricted engine previews already use) before the origin is reached. Edge: engine replicas skip the preface in
ForwardPublicTCP and open restricted HTTP as `http_browser`. Agent: the engine forwarder reads a preface only on
`http_browser`, accepts `tcp_public` without one. Server (maintainer): the engine predicate takes tcp/tls and team/private
routes; public TCP listener publication and the browser authorize endpoint accept the runtime carrier identity; private_tcp
(accessor) moves last. Prior art: cloudflared authorizes public ingress at the edge and the connector serves it
(no per-request re-check); zrok private shares are reachable only through an authenticated access frontend and the share
checks the frontend's grant (`zrok/controller/access.go`, `share.go`): our restricted path keeps both checks.

**Decided.** No headless connectors (tunnels run on enrolled machines only); 2 carriers per edge; the URL is returned when one
edge is ready; custom domains work for previews as for tunnels. Timing: engine work starts after Linux is done, before macOS.

**CLI and host half (maintainer draft, docs only; code waits for Linux done and for maintainer's step 1 on the server).**

*Today on a machine.* The worker holds the runtime-hostname carriers: `runtimeattachment.Service` (~0.7k lines) asks
`/v1/runtime/carrier-attachment` for edges, `edgeplacement.Selector` picks them, and a `connector.DataCarrierPool`
keeps them (primary active, standby ready). Previews and tunnels do NOT use these: `hostruntime/preview` (11.4k lines; leases,
owner sessions, dispatch/proxy, private access, origin health) dials one carrier per lease through `AttachmentCarrier`
(`attachment_*`, `carrier_provider`, `lease_observer_carrier`, `data_carrier`, `machine_runtime`, `lazy`: ~4.3k), and `tunnelmanager` (6.2k) plus
`tunnelenrollment` (4.3k) and `connector` admission/supervisor/manager (~1.1k of 4.2k) run a connector identity with its
own carrier and credential rotation. Three attach flows, three lifecycles, which is why a preview flips edges and a second
tunnel route stalls.

*Target: one `agent` package in the worker.* It is the renamed `runtimeattachment` + pool and is the ONLY thing that
dials edges: 2 carriers per edge of the workspace's edge set (set and replacement come from the server's edge-set answer),
reconnect with the existing bounded backoff, per-carrier readiness reported to the server (`PUT` of observed carriers, so the
reconciler only assigns to edges where this agent is ready). It exposes a small in-process API used by everything that
serves a URL, nothing else dials:

    Register(ctx, EndpointSpec{ID, Kind, AccessMode, Routes[]{Protocol, Match, Origin, Policy}}) (Handle, error)
    Handle.Update(routes) / Handle.Release() / Handle.Ready() <-chan struct{} (>=1 assignment observed)

`Register`/`Update` call the server's `/v1/endpoints` with the machine identity; the server fans assignments out to the
edges (maintainer above). The agent never pushes routes to an edge. Inbound streams carry `(endpoint_id, route_id)` in their
first frame; the agent looks the route up in its registry and hands the stream to the route's origin handler, failing
closed (stream reset, one rate-limited diagnostic) for an unknown or expired route. State needed to re-register after a
worker or daemon restart (endpoint ids, routes, lease deadlines) is persisted in the existing worker state root, so
previews/tunnels return on the same URL after restart or update (F14-02).

*Preview (step 3).* `pb preview <port>` -> local API -> worker: `Register(kind=preview, one http route, lease)`; the URL is
printed when `Handle.Ready()` fires (one control round trip, no dial); the lease renews through the same call
(`owner_lease*`, `owner_sessions`, `session_renew` become the endpoint lease renewer); stop/expiry = `Release`. Private/team
previews keep today's access checks (`private_access`, binding validation) inside the route policy; origin health and
`dispatch.go` proxying move unchanged into `agent/origin` (shared with tunnels).

*Tunnels (step 4).* The manager reduces to: read desired routes for this agent from the server, `Register`/`Update` them,
report origin health. Connector enrollment, activation, credential generations and rotation, admission registry and the
connector's own carrier dial are deleted; a tunnel is owned by the machine that created it, so `pb tunnel` commands and
their JSON do not change except that `connector` fields disappear (CLI contract change made together with the server
facades, no compatibility shim).

*Deleted from the CLI (estimate -13k / +3k lines, tests -8k / +2.5k):* `preview/attachment_*`, `carrier_provider`,
`lease_observer_carrier`, per-lease `data_carrier`, `machine_runtime`, `lazy` (~4.3k); all of `tunnelenrollment` (4.3k);
`tunnelmanager/{production*,data_carrier,factory,update_gate}` and connector identity state (~3.6k); `connector/{admission_source,
supervisor,manager}` (~1.1k). Kept: the data-carrier protocol (`connector/data_carrier*`), `edgeplacement`, dispatch/proxy,
private access, origin health, inspector streams, usage accounting.

*Order and compatibility.* CLI steps follow the server steps (server-first, each deployable). Software is pre-release: when the
server drops `/v1/previews` attachment and connector routes, older CLIs lose previews/tunnels until `pb update` (the
minimum-version floor makes that an actionable message, F14-07); no dual path is kept. Every historical smoke check
(preview, preview-protocols, preview-private/access, tunnel*, tcp_private, update_continuity) stays the gate for each step.

*Tests per step.* Agent: register/update/release lifecycle; ready only after an observed assignment; reconnect to a restarted
edge; second edge joining later; unknown route fails closed; restart re-registers the same ids. Fault test: drop all carriers
of one edge mid-stream, traffic continues on the other and the dropped streams error cleanly. Preview: the existing
`preview_*` Python checks plus a two-edge URL check from maintainer. Windows/macOS: no platform code in the agent (pure network
and in-process); platform risk is only service lifecycle, already covered by the daemon/launchd/systemd tests.

*Open for maintainer (needed before code):* the exact edge-set answer (is it `carrier-attachment`'s response or a new
`/v1/agent/edges`), the observed-carriers report shape, whether `endpoint_id` goes in the stream header or the first frame,
and the capability flag name the agent advertises so the server can refuse a CLI that cannot serve endpoints.

### 3b. Performance

One section, two owners: **data plane** (maintainer: `pb` CLI, daemon/hostd/carriers, P2P and relay, edge
data path) and **API/DB** (maintainer: server handlers, queries, pagination, retention). Rows: CHECKLIST
section P. A client loop that costs the server is fixed in the client by maintainer; a slow endpoint by maintainer.

#### Go SDK v1 (coordinator 2026-10-26; move order approved by maintainer)

**Contract.** `sdk/go/` is its own module, added to `go.work`; reusable code lives in the
existing top-level `shared/` module. V1: API-token login, machine discovery by name/ID, native
exec, private TCP/HTTP access and file send/receive, with streaming, deadlines, typed errors
and `Close`. Exec is the differentiator; networking makes the client useful inside ordinary
applications (database drivers, HTTP clients and build tools) as well as agents. No subprocess
`pb`, installed daemon, machine enrollment, OS network changes or edge-decrypted fallback.
Relay fallback carries the same encrypted native connection. Server/service addresses come
from explicit server configuration and discovery. SDK imports no `cli/internal` package.
This section is design only; implementation and acceptance rows go in CHECKLIST before code.

**Product phases (each has its own CHECKLIST acceptance rows before implementation).**
0. `pb mcp`: stdio MCP server inside the existing CLI (maintainer approved 2026-10-26); SDK worktree
   implements and A reviews CLI changes. Expose `list_machines`, `exec` (exact argv, bounded
   output, timeout) and `send_file`. Inbox listing/fetching is included only if an existing
   authorized offered-artifact API supports it; request approvals alone are not artifact fetch.
   `read_file`/`write_file` and `create_preview` come later. Acceptance: CHECKLIST F24-00–03.
   Claude Code, Codex and Cursor can use these tools through their MCP configuration.
   Reuse Go command services and the user's pb login or an explicitly supplied API token,
   never shell out to pb. Read API tokens from an explicit secret file/configuration, not MCP
   stdin or command-line token arguments. Calls retain scopes and token-attributed audit. Stdio
   stdout is protocol only; diagnostics go to stderr. Bound tool arguments, output bytes,
   concurrent calls and execution time; report truncation and uncertain execution explicitly,
   honor MCP cancellation and close on EOF. Local files used by send are explicitly selected
   local inputs, constrained by caller-configured roots and snapshotted safely before approval.
   Later read/write tools name a remote machine and remote path, never an implicit local path;
   remote filesystem access follows remote permissions and explicit file capability. No exec fallback
   to implement file-only tools. Until phase 1, MCP has the CLI's existing enrolled-source
   requirements; MCP does not make standalone API-token exec magically work. Qualification,
   not a promised number of days, determines which tools ship first.
1. maintainer deploys one session-bound source model for exec, native private-access grants and
   file descriptors, with destination admission enforced by maintainer. Each capability is a
   separate explicit scope granted by the token creator; no widening scopes to make an SDK
   call succeed. Creator access and workspace policy remain upper bounds.
2. Extract the shared owners in the agreed A/R move order below, preserving CLI behavior.
3. Ship one Go Client with Machines/Machine, Exec, Dial/HTTPClient and Send/Receive. Runnable
   examples cover an agent build, a private database connection, an HTTP request and artifacts.
4. V2 `Expose(ctx, localAddr or http.Handler, ExposeOptions)` returns an owned endpoint with
   public preview/tunnel URL and Close. Closing removes its lease and local serving workers.
   Public HTTP terminates TLS at the edge; describe that trust boundary explicitly. This is
   not the native private-access E2EE promise. Server/edge lifecycle and quotas remain owners.
5. Remote filesystem phase: read_file/write_file and Receive(arbitrary remote path) require
   their own explicit scope, protocol, path/overwrite rules and A/B/R tests before shipping.
   Offered Inbox artifacts remain distinct; no implementation through an exec-only shortcut.
6. Python and TypeScript bindings reuse the Go core through a C shared-library ABI (the
   libtailscale approach) or a native binding distribution like ngrok-python/javascript.
   Choose packaging/ABI during that phase; do not build bindings or reimplement transport now.

**Network SDK prior art (local source paths; references, not code to copy).** Under `~/ref/`:
`tailscale/tsnet/tsnet.go`: Dial, Listen, ListenTLS, ListenFunnel, HTTPClient, Up, Close;
`netbird/client/embed/embed.go`: New/Start/Stop, Dial/DialContext, ListenTCP/UDP,
NewHTTPClient, Expose, Status; `ngrok-go/agent.go`: Listen/Forward, with listener lifecycle in
`ngrok-go/listener.go` and forwarding in `ngrok-go/forwarder.go`;
`zrok/sdk/golang/sdk/{share,listener,dialer}.go`: CreateShare/NewListener/NewDialer, alongside
`zrok/sdk/python/` and `zrok/sdk/nodejs/`. These establish private dial/listen and public
exposure as useful primitives. Paperboat v1 offers outbound private Dial, not an unrestricted
Listen that turns an SDK process into a managed host. Teleport's local
`teleport/lib/auth/bot.go` records bot-instance renewal generations and mismatch locking;
the sparse reference lacks api/client and tbot implementations, so the linked tbot architecture
below is the source for its client credential lifecycle, not an uninspected local path.

**Extraction boundary (exact destination packages).** Moving whole `api`, `config`, `resolver`
or `tunnel` packages is rejected: their transitive imports include host runtime, local API,
CLI reporting and user-profile discovery. Split the following owners in place first, preserving
CLI behavior and tests; then move each resulting package and update all consumers together.
Paths below are relative to `cli/internal/` unless prefixed `shared/`:

- Whole leaf packages → same names in `shared/`: `wirejson`, `httptransport`, `serverclock`,
  `supportref`, `backoff`, `diagnosticlog`, `machinename`, `remotepath`.
- Whole peer packages → `shared/peertransport/{endpointidentity,fixedpacket,mesh,peerquic,
  streamauth,native}`. Move production files and self-contained tests; cross-package acceptance
  tests that import host runtime stay in CLI as external consumer tests. maintainer owns these moves.
- `config` → `shared/client/state`: credential/account/profile value types, issuer normalization,
  endpoint certificate/key custody, peer-network key/high-water transactions and refresh-attempt
  state from `auth*.go`, `peer_identity.go`, `peer_network.go`, `refresh_attempt.go`,
  `network_secret.go`. Move storage-independent transactions; inject storage and locking.
  OS keyrings, profile paths, file ACL/atomic-file adapters and default profile selection stay CLI.
- `api` → `shared/client/api`: HTTP request/response owner and safe structured API errors from
  `client.go`, `http.go`, `errors.go`, `api_errors.go`, `issuance_retry.go`, `response_lost.go`;
  session exchange/refresh/revoke from `device.go`; discovery from `client_configuration.go`;
  inventory/pagination from `api_machines.go`, `inventory_pages.go`; identity/network calls from
  `api_e2ee.go`, `peer_network.go`; exec/file descriptor/types from `api_terminal.go`; private
  grants from `native_private.go` and announced-port discovery from `machine_services.go`; workspace
  selection from `workspaces.go`. Split mixed files by declaration, retaining other endpoints
  in CLI. Inject credentials, product/version headers, clock and observation callbacks: no
  `buildinfo`, global `errorreport`, host config or self-update dependency in shared code.
- `auth` and peer helpers → `shared/client/session` (login completion and serialized refresh),
  `shared/peertransport/{identitybootstrap,trustedkeys,clientauthority}` (existing package code,
  using the extracted API/state contracts). Browser/device approval and logout UX stay CLI.
- `hostruntime/auth/jwks.go` → `shared/peertransport/jwks`: network JWKS fetch/cache only,
  splitting file-cache helpers and reporting hooks first. Host credential verification/replay
  policy remains host-owned. `peertransport/tailnet` → `shared/peertransport/tailnet`, replacing
  its `api`/`config` imports with the extracted contracts. maintainer first splits relay client
  contracts out of `relay/derpquic`: `shared/peertransport/relaygrant` owns Grant/Peer/Scope,
  Verifier, key/disco parse/format, membership and grant bounds; `relayclient` owns NewClient,
  NewWSSClient, NewFallbackCarrier and their client configs; `relay/nodetls` moves whole to
  `shared/peertransport/nodetls`. Mesh/tailnet then import shared, never relay; relay remains a
  consumer of shared. No copied relay or cryptographic implementation.
- `resolver` → `shared/client/discovery`: inventory selection and connection descriptor types;
  split terminal queues and CLI telemetry away. `tunnel` → `shared/client/runtime` (native
  authority/runtime ownership from `peer_terminal.go`, `native_runtime.go`) and
  `shared/client/exec` (exec protocol from `helper_exec.go`, native stream adaptation from
  `tailnet_terminal.go`, shared framing from `helper_websocket.go`, and execution recovery from
  `cmd/pb/exec_command.go`, `exec_run.go`). Move required framing declarations, not WebSocket
  dialing, terminal/SSH APIs, local-daemon transport or host config. Runtime receives state,
  credentials, JWKS and connection authority explicitly. Exec receives a narrow dial/descriptor
  interface; it does not import runtime. Existing `shared/reattach` remains the retry owner.
- Private access → `shared/nativeprivate` (whole `nativeprivate/binding.go` and its tests),
  `shared/client/access` (TCP private-stream opening and target validation split from
  `tunnel/native_private.go`, `hostruntime/preview` native private session interfaces and
  machine-service adapters; API call owner above). OS proxy/PAC, DNS, listeners and host
  announcement services stay CLI. A net.Conn adapter carries the existing authenticated
  private stream; HTTPClient uses that same adapter, not a separate networking stack.
- File transfer → `shared/client/files`: prepare/manifest, native transfer, committed-offset
  recovery and receipts from `filetransfer/{source,client,native_client,native_transport,
  native_grant}.go` plus platform source helpers. Split protocol types used from
  `hostruntime/protocol` into `shared/client/files/protocol`, consumed by both endpoints;
  this protocol package does not import files. Inject diagnostics, lease renewal and retry
  classification instead of importing local-daemon restart detection. CLI owns its transfer
  queue, Inbox UX and preferences; SDK owns its instance's transfers. `cmd/pb/send_command.go`
  and receive/Inbox orchestration split in place before moving reusable orchestration.
  Bind mode enforcement to maintainer's path-kind API; no copied policy constants or native stack.

Import graph (arrows mean imports; external libraries omitted):

```text
sdk/go → shared/client/{api,session,discovery,runtime,exec,access,files,state}
cli/{cmd,internal adapters} → those same shared packages
client/session → client/{api,state}, peertransport/identitybootstrap
client/discovery → client/api
client/runtime → client/{api,state,discovery}, peertransport/{clientauthority,native,tailnet,jwks}
client/exec → client/discovery, peertransport/streamauth, shared/reattach
client/access → client/{api,discovery}, shared/nativeprivate, peertransport/streamauth
client/files → client/{api,discovery,files/protocol}, shared/{reattach,httptransport,serverclock}
client/{exec,access,files} → injected stream/lease interfaces implemented by client/runtime
peertransport/{identitybootstrap,clientauthority,trustedkeys,tailnet} → client/{api,state}, peer leaves
peertransport/native → peertransport/{mesh,peerquic,streamauth,tailnet}
peertransport/{mesh,tailnet} → peertransport/{relaygrant,relayclient,nodetls}
relay → shared/peertransport/{relaygrant,relayclient,nodetls}; shared NEVER imports relay
client/api → client/state, shared/{wirejson,httptransport,serverclock,supportref,machinename}
peer leaves / client/state / JWKS → shared leaf helpers and existing shared wire packages
shared/* → NEVER cli/* (including tests); CLI acceptance tests remain downstream
```

**Move sequence and coordination.** Coordinator obtains A/R sign-off on each source slice and a
short edit freeze for that slice, not a blanket CLI freeze. One commit per package boundary:
(1) SDK worktree takes A's eight leaf moves with A review, diagnosticlog first (A approved
2026-10-26), then the other leaves, each with darwin/windows cross-builds;
(2) R1 endpointidentity/fixedpacket/streamauth (coordinator go), R2 peerquic after diagnosticlog,
R3 relay contract/client/nodetls preparation then mesh; (3) A coordinates API after maintainer's
shared/apiroutes change and its current error/descriptor edits, then config/state, session and
discovery; dependency-preparation declarations land before either consumer moves. Keep the
optional machine-source CredentialRejected hook from dcb4d920. (4) A extracts JWKS; R4 moves
identitybootstrap/trustedkeys/clientauthority after API/state, R5 tailnet then native after the
relay split. R freezes mesh/tailnet only after N5 verification/follow-ups; each slice has an
announced approximately one-hour window. A/R sign-off received 2026-10-26; active source slices
still require the owner's individual freeze;
(5) A/R split runtime and exec framing, then mechanically move each; (6) A extracts exec
recovery out of cmd/pb and switches CLI to that single owner; (7) A/R move nativeprivate,
prepare/split private-stream interfaces and access adapter, then file protocol/transfer core
and reusable send/receive orchestration, one owner per commit; (8) add SDK facade and examples.
Boundary preparation is a separate no-behavior-change commit; each mechanical commit is only
`git mv`, package/import rewrites and module metadata, never a fix or protocol change. Move
existing owning tests with code, preserve downstream integration tests, and verify the full
import closure on Linux/macOS build variants before each move. No compatibility aliases or
second implementations. Existing CLI terminal, exec, SSH, files, previews/private access and
host native-session checks cover shared transport consumers; auth refresh/revoke, discovery,
peer registration/direct/relay and exec uncertain-start/reattach/cancel checks are mandatory.
Run affected checks and workspace gate per commit; maintainer alone owns device verification.

**Instance state (tsnet prior art).** Local reference `~/ref/tailscale/tsnet/tsnet.go`,
`Server.Dir`, `Store`, `Ephemeral`, `Close` and initialization: one embedded engine and state
owner per instance, no global daemon. [tsnet](https://tailscale.com/docs/features/tsnet) enrolls
an actual node; [ephemeral nodes](https://tailscale.com/docs/features/ephemeral-nodes) still have
node identity and lifecycle. Reusing that model for Paperboat would avoid a new source shape
but create a managed-machine record for each fresh SDK process, plus cleanup and inventory
churn. [Teleport tbot](https://goteleport.com/docs/reference/architecture/machine-id-architecture/)
uses a bot identity and renewable short-lived certificates; certificates are not necessarily
one per exec session. Borrow per-instance ownership and short-lived credentials, recommend
Paperboat's existing token-backed client session as source, not a new machine or bot principal.

`Config{ServerURL, APIToken, Workspace, StateDir, ClientLabel}`: memory-only by default,
including refresh credentials and peer keys; no fallback to CLI config, global keyring or
implicit user directory. Optional explicit StateDir stores only this SDK instance's session,
keys and replay/rotation state with owner-only permissions, atomic updates and exclusive
ownership lock (second opener fails). Never persist the supplied API token. Explicit reuse of
that dir resumes the session; separate dirs/memory clients have separate sessions and keys.
`Close` is bounded/idempotent, rejects new work, cancels active operations, joins workers and
closes transports. Memory mode best-effort revokes its session; failure is returned and expiry
bounds server leftovers. Persistent mode retains its session for explicit reopen; provide
`Logout(ctx)` to revoke it and erase its state. No telemetry or command contents sent implicitly.

**Public API (proposed signatures; one Client).** `New(ctx, Config) (*Client, error)`,
`Machines(ctx, ListOptions) (MachinePage, error)`, `Machine(ctx, nameOrID) (Machine, error)`,
`Exec(ctx, machine, ExecOptions) (ExecResult, error)`,
`Dial(ctx, machine, network, address) (net.Conn, error)`, `HTTPClient(machine) *http.Client`,
`Send(ctx, machine, Source, SendOptions) (TransferResult, error)`,
`Receive(ctx, machine, RemoteSource, Destination, ReceiveOptions) (TransferResult, error)`,
`Close() error`, `Logout(ctx) error`. Machine resolves a name to exactly one authorized ID,
otherwise `*AmbiguousMachineError` or a non-disclosing not-found/denied error. All operations
accept name or ID, resolve once and pin the resulting ID across retries (never re-resolve an
alias after an operation has started). Machines returns inventory IDs and pagination.

`Dial(ctx, "database", "tcp", ":5432")` opens a private port on that machine, using the same
workspace access switch, target/port grants and token scope as `pb access machine`. V1 accepts
TCP and a numeric port only; it cannot dial an arbitrary host through the target. Every
connection obtains an exact target/port grant and binds the authenticated endpoint. Return a
net.Conn with deadlines; reconnecting transport does not replay arbitrary application bytes.
HTTPClient takes a resolved Machine handle from Machine(ctx, nameOrID), binding its transport
to that exact ID: request scheme/port selects the authorized
port, normal HTTPS certificate verification remains enabled, and redirects cannot escape to
another machine or the public Internet. The caller owns response bodies; Client.Close also
closes idle HTTP transports and active connections. No system DNS/PAC or proxy changes.

Source/Destination are explicit path or reader/writer forms; remote paths never mean local
paths. Send and Receive retain transfer IDs, committed offsets and verifiable receipts; no
silent overwrite, path traversal or symlink escape. Transfer mode is explicit: Automatic
(default, currently ≤2 GB, P2P then any relay) or Unlimited (direct/self-hosted relays only).
Use server FileTransfer.AutomaticMaxBatchBytes from discovery/descriptor, not an SDK constant;
reuse the transfer-mode section's path-switch pause/resume rules on both endpoints. A reader
must supply length/hash and a replayable source, or opt into bounded caller-configured spooling
before issuance; do not hide unlimited buffering or invent a manifest after streaming starts.
Receive names its remote source explicitly: distinguish an offered Inbox artifact from a
request to read an arbitrary remote path. The latter and MCP read/write need an authorized
filesystem protocol; existing send/Inbox operations alone do not establish that capability.
Add that product contract and its owning acceptance rows before implementing these forms.

ExecOptions contains Argv, Dir, Env, Stdin (io.Reader), Stdout/Stderr (io.Writer) and Timeout. Argv is exact, no implicit shell; PTY is
outside v1. ExecResult contains OperationID, ExitCode, Signal and ExitedAt; nonzero remote
exit is a result, not a transport error. Errors support errors.Is/As: `*APIError` exposes
safe Code/Status/RetryAfter, `*AuthError`, `*AccessDeniedError`, `*MachineUnavailableError`,
`*StartUncertainError{OperationID}`, `*ResultUnavailableError{OperationID}` and
`*TransportError`; transfer errors expose typed size/path-policy/paused-state details.
Context cancellation/deadline causes remain unwrap-able. Do not reveal
whether an unauthorized machine exists. Client generates one operation ID before issuance;
all retries attach that ID with replay cursors. Lost start response never authorizes a new
start. Output has bounded queues/backpressure; writer failures cancel the operation and keep
the primary error. Deadline/context cancellation sends remote cancel with a bounded cleanup
budget; missing acknowledgement reports an uncertain remote outcome, never claims success.

**Server contract audit / maintainer prerequisite.** `httpapi/router.go` already allows session
exchange (`POST /v1/auth/api-token-sessions`), account/workspace and machine inventory reads,
client discovery, session refresh/revoke, peer keys/certificates, `POST /v1/e2ee/bootstrap`,
and `POST /v1/peer-network/{register,config}` for client-session bearers. The peer handlers
bind role=cli and endpoint ID to the authenticated session, not a caller-selected machine.
`scopedTokenRoute` explicitly admits bootstrap/network/exec-descriptor plumbing; machine
authorization also enforces the token scope. Certificate renewal's exact write route must
remain within that allowlist: use existing bootstrap enrollment, not a blanket endpoint PUT
exception. SDK identifies itself with ClientLabel and a versioned SDK User-Agent; existing
supported device_type (server/container/desktop) describes its host, not a new SDK identity.

Scoped-token private access needs a maintainer change: `scopedTokenRoute` currently omits
`POST /v1/native-private-access/grants`; admit only that exact action after enforcing its
explicit access capability and port restrictions. Do not permit generic mutations or borrow
exec authority. `native_private_handlers.go` already derives the source client session;
reuse it and align descriptor/claim/admission source semantics with exec and files.

Exec and file transfers are NOT currently standalone:
`usermachines/service_02.go:operationDescriptor` requires nonempty sourceMachineID and sourceMachineAllowed; the handler alone accepting a session is
insufficient. `usermachines/service_03.go:fileTransferDescriptor` likewise requires an enrolled
source and checks sourceMachineAllowed. Phase 1 covers both plus native private grants.
maintainer must first implement and deploy one session-bound source model: descriptor and
signed operation claim name source kind=client_session and the authenticated token-backed
session ID, with no fake machine ID. Derive source server-side; validate account/workspace,
active session/token/expiry, a role/grant permitting the requested capability from a session
source, creator's current target access AND the token's separately granted exec/access/files
scope (and port/direction restriction where applicable), target and operation. Never turn an
exec-only token into a file/private-network token. Machine-sourced requests retain their actual
machine-source checks. Destination admission binds the signed session source to the authenticated peer endpoint, target runtime
generation, workspace and operation; no client-supplied source substitution. Update producer,
shared exact signed-claim schema, persistence/audit and destination consumer together in
server-first order; check every validator expecting source_machine_id (including host native
admission and shared stream claims). Reuse role=cli peer identity; no new enrollment endpoint.

**Threat model and acceptance.** A stolen API token can mint sessions within its creator's
current permissions until expiry/revocation; short-lived transport credentials do not remove
that risk. Encourage narrow exec-only token scopes/expiry, never log tokens, refresh material,
private keys, Env or output. Prove token revocation ends refresh and new descriptors AND reaches
live destination operations through the existing revocation/config feed, terminating admitted
streams/processes within a configured bounded interval; fail closed at authorization lease
expiry if the feed is unavailable. Do not infer live revocation from issuance checks alone.
maintainer owns any missing propagation; maintainer owns destination enforcement. Audit names creator,
API token ID, client-session ID, workspace, target, operation ID and outcome, including denials
and uncertain outcomes, without command/file contents. Require configured per-token/account
session-creation, descriptor and concurrent-exec limits, bounded refresh/backoff, pagination,
output queues and connection counts; existing invalid-token/IP throttling alone is insufficient.
maintainer owns missing limits and 429 Retry-After; SDK honors it without retrying command starts.
Before v1 is accepted, named workspace tests cover isolation between two clients and pb login,
refresh/revocation, scope denial, spoofed source/peer mismatch, direct/relay exec, exact argv,
stream separation/backpressure, deadlines, cancellation acknowledgement loss, lost-start and
reattach without rerun, replay gaps, shutdown leaks and directory-lock conflicts; maintainer then
runs SDK exec/private-TCP/HTTP/send/receive examples against L/M. Add separate MCP framing,
byte limits, cancellation, remote/local path separation and file-only-token checks; TCP port
scope denial, redirect containment, connection deadlines, file mode/path-switch/size/receipt
and offered-artifact versus remote-path checks. No SDK capability is marked complete by this
design.

**Strongest objection.** MCP-first is useful, but a safe read_file/write_file/Receive(remote
path) is new filesystem authority, not a wrapper around existing send/Inbox. Implementing
these tools as shell commands would require exec authority and defeat file-only scopes;
names on MCP tools cannot enforce least privilege. An exec grant already allows the remote
OS user's filesystem access; file-only grants must not silently acquire that power. Ship only
tools backed by the corresponding real capability, and treat remote filesystem operations as
explicit product work with maintainer contracts and tests. Do not promise the full MCP surface in days or hold useful
list/exec/send tools hostage to it. Dial belongs in v1, provided phase 1's port-scoped grants
and transport admission are complete; it is not just exporting a Dial method.

#### Data plane (maintainer, measured 2026-10-22/23)

| What | Before | After / now | Reference | Check |
|---|---|---|---|---|
| Relay DERP-over-QUIC forwarding, per account | ~10 Mbit/s (budget counted datagrams, 4096/s; a dropped fragment wasted its pair) | 20–31 MB/s lossless in-process | Tailscale peer relay ≈ line rate | PR-01 |
| Peer-relay (UDP) forwarding cap, per account | 4096 pkt/s ≈ 42 Mbit/s | 48 Ki pkt/s ≈ 500 Mbit/s | upstream udprelay: no cap | PR-01 |
| Edge carrier on UDP-blocked networks | no carrier (immediate EPERM/ENETUNREACH treated as terminal) → exec/ssh 409 | h3 → h2 fallback on any non-authorization failure; h2 preferred 5 min after a fallback | Cloudflare Tunnel: QUIC → HTTP/2 | PR-02 |
| Idle usage reports per machine | 360 POST + 360 state writes /h | 120 /h | — | PR-03 |
| Inventory refresh, account with N machines | 1 + 2N (ssh-target, ssh-host-keys) + N (terminal sessions) requests per daemon | 1 + 1 | Tailscale: one streamed netmap | PR-04 |
| `pb send` round trips per 1 MiB chunk | 2 (HEAD + PATCH) | 1 | scp/rsync: streaming | PR-05 |
| Edge data carrier, one stream at 50 ms RTT | 38 Mbit/s (window pinned 256 KiB) | pending (4 MiB max window measured 248 Mbit/s; waits for maintainer) | Cloudflare Tunnel / ngrok 100+ Mbit/s | PR-06 |
| `pb --version` / `pb help` cold start | 33 / 38 ms, 47 MB RSS | — | tailscale CLI ~20 ms | PR-07 |
| `pb status` / `pb machine list` | 1.16 / 1.18 s (TLS + ~150 KB list at 170 ms RTT) | — | `tailscale status` local ~20 ms | PR-07 |
| Idle API traffic, one machine | 88–162 MB/h (user daemon 54, worker 24, carrier 10 MB/h) | 147–172 MB/h with the ssh fix (test-run-b, dev-86b79b40, prod degraded); responses were uncompressed (250 KB machine list → 16 KB gzip): maintainer adds path-scoped compression; peer-network config stays uncompressed (bearer relay grants) | Tailscale idle: KB/min | PR-03 |
| Idle CPU / wakeups, all 8 `pb` processes of one machine | 17–20% of a core, ~1,680 wakeups/s (updater re-hashed both ~94 MB binaries every 5 s: 13.7%, 1,056/s) | 2.8–3.2%, 340–410/s, ~190 MB RSS (test-run-b, dev-86b79b40) | tailscaled idle ~0%, 30–50 MB | PR-03 |
| Plain ssh on the same link (container to container, linux-test-vm-2) | — | `ssh true` 0.32 s, scp 50 MiB 564 Mbit/s, rsync 500 × 4 KiB 1,127 files/s | baseline for PR-05 | PR-05 |
| Server load from the fleet | 135 req/s (~3–4 per machine) | re-measure after PR-04 ships | — | PR-03 |

Budgets, one place each: relay byte budgets `relay/derpquic/wire.go` (64 MiB/s per carrier, 128 MiB/s
per account, 4 MiB burst) and `relay/peerrelay/server.go` (48 Ki pkt/s, 4 Ki burst); carrier fallback
`connector.preferFallbackFor` (5 min); usage reporting `bandwidth.idleReportInterval` (30 s); device
budgets (exec latency, send/scp/rsync throughput, small files/s per path type) in
`tools/device-smoke/perf_budgets.py`, set from the first L/M run of `perf_dataplane.py` and enforced by
it after that. Open: daemon's 1 s carrier-attachment poll (maintainer), 8 s machine-services poll (needs a
push topic, server first), live upgrade of an h2 carrier back to QUIC (maintainer's agent carriers).

#### Design (awaiting user decision): long-lived network scopes, per-operation authority in the stream token

Not built. Coordinator takes it to the user; maintainer (host/CLI) and maintainer (server) review here first.

*Problem (measured 2026-10-23, test-run-a → test-run-b-2, same docker bridge, server .12):* after the client
fixes 9e74c29d/b9943159, `pb exec true` takes 8.7–10.6 s; plain `ssh true` on the same link takes 0.32 s.
Every operation descriptor creates a NEW access session (`server/internal/usermachines/service_02.go`
`operationDescriptor`: `accessSessionID := newID("session")` → `admitRuntimeIngress` in `runtime_quota.go`),
and the signed peer-network configuration turns each live row of `user_machine_access_sessions` into a
`machine_access` scope keyed by that id (`server/internal/peersessions/network.go`, the access-session query
and `g.kind = "machine_access"`). So for each operation both ends must fetch a new configuration before a
stream can open: the CLI forces `POST /v1/peer-network/config` before every dial
(`cli/internal/tunnel/native_runtime.go` `acquireNativeRuntime`: "the operation descriptor may have just
created its peer grant"; 3–5 calls per exec in the trace), and the host refreshes on the first stream whose
scope it lacks (`cli/internal/peertransport/native/session.go` `authorizeIncoming`). At 0.5–6 s per call that
is most of the exec time, and the direct handshake looks dead until the host has refreshed.

*Design (the industry split; Tailscale: network reachability from ACLs, application auth per request):*
1. **Network scope = a grant, not an operation.** One grant per (CLI session, machine, capability), with an
   id that stays stable while the grant is live, a generation, and an expiry renewed while it is used (e.g.
   1 h, renewed when a descriptor is issued with under half left). `peersessions/network.go` emits one scope
   per live grant (`resource_id` = grant id), so a configuration changes only when a grant starts, ends or
   changes generation, not per command.
2. **Per-operation authority stays in the stream token.** The descriptor still mints a short-lived,
   operation-scoped token (`exec:operate`/`ssh:operate`, operation id, about 2 min) whose resource is the
   grant id. The host verifies it on every stream before any scope check (`authorizeIncoming` → `authorize`
   → `streamauth` header; signature via JWKS), as today.
3. **The CLI dials without a refresh when the scope is already live.** `acquireNativeRuntime` refreshes only
   if `Authority.Allows(machine, grant, capability, "dial")` is false; the host's check-then-refresh in
   `authorizeIncoming` needs no change.

*Revocation stays immediate:* (a) every new stream needs a fresh descriptor, and the server issues one only
for a live grant (membership, capability policy, machine and session state checked at issuance as today), so
a revoked user, member or machine cannot open new streams; (b) revoking (member removed, team or machine
revoked, capability turned off, CLI session ended) ends the grant or bumps its generation, publishes the
peer topic to both endpoints, and the next configuration generation drops the scope; open streams end at
their admitted-lifetime check when the scope disappears, the mechanism that ends them today; (c) a token
minted just before revocation is bounded by its own expiry and by (b), the same window as today, where an
open stream also lives until the next refresh.

*Migration (server first, no dual path):*
1. Server (maintainer): give access sessions a grant key (user, CLI session, machine, capability); reuse a live
   grant in `operationDescriptor` instead of `newID("session")` and renew its expiry; emit scopes per grant in
   `peersessions/network.go`; publish the peer topic on grant end or generation change. Old CLIs and hosts
   keep working unchanged: they still refresh before or at the first stream and then find the scope. Usage
   accounting (`resourceusage`, keyed by access session) becomes per grant + stream id; check bandwidth
   reports keep their per-operation split through the stream id.
2. CLI (maintainer): skip the forced refresh in `acquireNativeRuntime` when the scope is live; a test counts
   `POST /v1/peer-network/config` per dial (0 on a warm grant, 1 on a new one).
3. Device checks: perf_dataplane.py exec median budget (target < 1 s warm on L/M) and the revoke smoke checks
   (machine revoke, member removal, capability off): a revoked grant cannot open a new stream, and open
   streams end within one refresh.

*Review:* maintainer (host admission, CLI dial): **approve, with conditions** (2026-10-23): host admission keeps revocation immediate only if a revoked grant's scope disappears from the host's configuration: every stream re-checks `authorize` (streamauth token: signature, expiry, operation) and then `Allows(peer, resource, capability, "accept")` (native/session.go authorizeIncoming), and open streams are bound to a lease that `Watch`es ScopeChanges and closes on `AllowsAdmitted` false (native/scopes.go admitScope). Conditions: (1) the scope key is (resource, capability, direction) with no generation, so a revocation modelled as a generation bump of the SAME grant id keeps open streams alive; revocation must end the grant (a re-grant gets a new id) or the scope must carry the grant generation, with a host test that a narrowed/re-generationed grant closes open streams; (2) immediacy depends on the host receiving the peer topic for that grant (machine events, then its periodic refresh as fallback), so the server must publish to BOTH endpoints on grant end, as the note says. CLI dial: skipping the forced refresh when `Allows(machine, grant, capability, "dial")` is true is sound; the host still refreshes at its first stream if its config is older (one call, only when stale). Also keep: relay grant scopes use the same resource id (relay/derpquic reciprocity), so relay grants change only on grant start/end (fewer refreshes, good); per-stream byte metering is keyed by the resource id today (MeterIncoming), so per-operation usage needs the stream id as the note says. · maintainer (server grants, revocation topics): **approve, with
conditions** (2026-10-23): (1) the grant key includes the acting team (user, CLI session, machine, capability,
team), and a unique partial index on live grants with an upsert, so concurrent descriptors cannot create two;
(2) every path that ends authority ends or re-generations the grant in one place (a trigger or one service
function), each with a PG test: member removal, team suspension, machine revoke/disconnect, capability or
policy turned off, CLI session ended, plan/bandwidth refusal. A grant now lives up to 1 h instead of ~2 min, so
a missed path is a real hole; (3) renewal happens only through descriptor issuance, which re-checks authority;
nothing else extends expiry; (4) usage: the runtime accounting authority (maintainer's 88e486d7, authority_id =
access session id) moves to the grant id, edge usage reports keep a per-stream id for the per-operation split,
and a bandwidth refusal at issuance still stops new streams; (5) migration: grants reuse
user_machine_access_sessions rows (no second table), old rows expire on their own. It also cuts peer-config
churn, which helps the serialization load measured tonight.

#### Data plane at 1k/10k peers per account (maintainer audit, 2026-10-23)

Ranked by cost at 10k peers × how often it runs. "Break" = a fixed cap refuses work above it. Code
reading plus the measurements named; fixes go top-down, each with a test that proves the bound.

| # | What (file) | At 10k × frequency | Bound today | Fix |
|---|---|---|---|---|
| 1 | Relay admits 16 connections per account, 256 per process (`relay/derpquic/server.go:20-21`) | Break: 16 of 10k admitted; the rest retry every 4–12 s (~1.2k handshakes/s per relay). Hit live: first contact on linux-test-vm-2 failed "relay capacity exhausted" (10:02Z, ~40-machine test account) | hard cap | caps from config sized to the fleet; per-account fairness instead of a 16 ceiling |
| 2 | Signed peer network carries every peer, cap 2048 (`shared/peernetwork/bounds.go:9`) | Break above 2048; below it every machine downloads all N peers per refresh (9–36 s), O(N²) bytes server-wide | 2048 | deltas (user decision; maintainer server, maintainer daemon) and a cap sized to the target |
| 3 | Edge carrier registry cap 4096, per-request scan + sort of all carriers (`edge/internal/edgehttp/durable_carrier_transport.go:26, 366-399`) | Break above 4096 machines per edge; O(carriers) per public request | 4096 | index carriers by route; cap from config |
| 4 | Outage polling without backoff: inventory 15 s (`cli/internal/localdaemon/inventory.go`), revocations 15 s, heartbeat 10 s | During a server outage ~4–5k req/s from 10k machines, unjittered, exactly when the server recovers. Live: 22 machines → ~355 `/v1/machines`/min since the 09:31Z restart | none | back off 15 s → 5 min with jitter while the change stream is unhealthy |
| 5 | Reconnect herd: peer-network retry jitter only +0–25 %, regional stale retry fixed 1 s, startup refresh immediate (`tailnet/refresh.go`, `regional_recovery.go`) | After a deploy all machines retry within ~250 ms of each other | backoff, little jitter | full jitter on every control-plane retry |
| 6 | Per-packet peer lookup scans all clients under one lock (`mesh/engine_backend.go peerByIP`) | 3.86 ms per outbound packet at 10k admitted clients (benchmark) | none | address index: 45.6 ns, 0 allocs (this batch) |
| 7 | Relay per-datagram scope scan (`relay/derpquic/authority.go:267-293`) and one global mutex taken 2–3× per datagram (`server.go:417-489`) | Relay-wide throughput ceiling, grows with grant size (≤2048 merged peers) | none | precomputed reciprocal pair set; sharded locks |
| 8 | Netmap rebuilt, sorted and re-published per new peer and per regional pass even when unchanged; endpoint advertise O(N²) (`mesh/engine_backend.go updateServerMapLocked, advertiseEndpoints`; `mesh/regional.go setRelayRegions`) | O(N² log N) to bring up N peers; full rebuild every ~3 s while recovering | none | incremental netmap updates; skip unchanged regions (goes with deltas) |
| 9 | Each config apply re-marshals every peer, rebuilds the packet filter, re-checks scopes (`tailnet/authorized_udp.go replaceLocked`, `mesh/engine_authority.go`) | O(N) CPU per refresh on every machine | ≤2048 | deltas apply only what changed |
| 10 | Edge: stream authorization scans all admissions; up to 64 streams × 4 MiB window per carrier (`edge/internal/datacarrier/admission.go:604`, `carrier.go:959`) | O(admissions) per stream; memory bound per carrier only | per stream | index admissions; aggregate per-carrier memory cap |
| 11 | Files without bound: bug-report bundles (25 MiB each, no count cap), release targets never pruned, macOS launchd log not rotated, inbox without total cap (`cli/internal/diagnostics/bundle_unix.go`, `hostruntime/bootstrap/artifact.go:285`, `hostruntime/service/service.go:452`) | Disk grows per report/update for the life of the install | none | keep the newest few; prune old targets; rotate |
| 12 | Relay pre-auth: unauthenticated QUIC/WSS handshakes hold the same 256 slots for 5 s; no accept-rate or source limit (`relay/derpquic/server.go:128, 179, 203`) | A few clients can keep every slot busy; reconnects refused | 5 s per slot | separate pre-auth budget, address validation |

#### Reuse the Tailscale fork and headscale for deltas, reconnects and relay failover (maintainer, 2026-10-23, read-only)

*Daemon: apply deltas through the fork's delta path, not a netmap rebuild (audit #8/#9).* The fork already has
per-peer netmap mutations: `netmap.NodeMutationUpsert` / `NodeMutationRemove` (`types/netmap/nodemut.go`), and
`ipnlocal.LocalBackend.UpdateNetmapDelta` (`ipn/ipnlocal/local.go`) applies them with `magicsock.Conn.UpsertPeer` /
`RemovePeer` (O(1) map updates, `wgengine/magicsock/magicsock.go:3300-3370`), `UpdateNetmapDelta` for endpoint and
DERP-home patches, and `wgengine.Engine.SyncDevicePeer(key)` for exactly the changed WireGuard peers. Our mesh does
not run LocalBackend or controlclient (it drives the engine directly), so it cannot take a `tailcfg.MapResponse`
as is; it calls the same three primitives: `mesh` gains `UpsertAllowedPeer` / `RemoveAllowedPeer` that update
`allowedPeers`, the client index (94ce6a62) and the filter entry for one peer, then `UpsertPeer` / `RemovePeer` +
`SyncDevicePeer`. `updateServerMapLocked` and `ReplaceAllowedPeers` (full rebuild + sort + `SetNetworkMap`) remain
only for snapshots. `UpdateNetstackIPs` reads only the self node, so peer changes skip it. The signed delta
(`paperboat-network-delta+jwt`, contract above) is verified and its digest checked first; the mesh only sees
already-authorized changes, and the packet filter stays the authority boundary.

*Server: headscale's mapper (BSD-3, ~/ref/headscale/hscontrol/mapper) maps onto maintainer's change log.* `types/change`
(typed changes: node added/removed/changed, policy) ≈ our change rows; `mapper.Batcher` coalesces changes for
`tuning.batch_change_delay` (default 800 ms) and fans them out with a worker pool to per-node connections, building
per-node `PeersChanged` / `PeersRemoved` / `PeersChangedPatch` (endpoint/DERP only) responses; a node that
reconnects gets a full map; offline node state is dropped after 15 min. Useful for maintainer: coalescing window,
per-recipient diff against policy, patch type for presence/endpoint churn (keeps our deltas small), worker-bounded
fan-out. Differences: headscale streams `MapResponse` over a held Noise connection; we answer a long-poll per
machine and sign every answer.

*Reconnect jitter (audit #5).* tailscaled uses `util/backoff` everywhere: delay = min(n² × 10 ms, max) × U(0.5, 1.5),
max 30 s for the control map/auth/update routines (`control/controlclient/auto.go:63,317,588`) and 5 s per DERP
region (`wgengine/magicsock/derp.go:600`). Ours: peer-network retry jitter is only +0-25 %, regional stale retry a
fixed 1 s, the start-up refresh immediate. Fix: one shared backoff in the daemon (the same quadratic shape with
U(0.5, 1.5) jitter, capped 30 s for control, 5 s for relay regions) for every control-plane retry, plus a random
0-5 s start delay for the first refresh after a fleet-wide daemon restart.

*Relay failover and home stability (audit #1, the 6065/9831 flapping).* netcheck keeps the current home DERP unless
the new best is clearly better: kept if the latency gain is under an absolute threshold or the best is more than 2/3
of the current one, and only switched when the old region is not heard from (`net/netcheck/netcheck.go:1479-1510`).
Ours sets home to the first region of every snapshot (`mesh/regional.go setRelayRegions`) and selection can flip it
each pass. Fix: keep the current home while it is reachable unless a candidate wins by that margin; a full node
answers "relay full" and the client moves to the next candidate (with the registry capacities from config, maintainer).

#### API/DB (maintainer, measured 2026-10-22/23)

**Scale** (`server/internal/perf/seed.sql`, isolated test PostgreSQL only): 1200 accounts, 100 paid teams (team 1:
200 members, 1000 team machines), 5000 machines, 50k browser sessions, 20k CLI sessions with tokens, 200k audit
events, 50k terminal sessions, 50k access sessions, 20k preview leases, 5k tunnels, 200k bandwidth events; three
actors (team owner, solo user with 100 machines, team member).

**Budgets**, one place: `server/internal/perf/budgets.json`. Every listed read, through the real router, has a
median budget (default 50 ms) and a maximum number of database queries per request (the N+1 guard). The gate host
is shared with every worktree's builds, so its p95 measures their load: the check enforces the median with headroom and
the query count and reports p95 (target p95 < 100 ms on an idle host). An endpoint above the default names its
reason; each fix lowers its line. Check: `TestPerformanceBudgetsOnSeededPostgres` (internal/app), run by
`tools/gate.sh` when `PAPERBOAT_PERF_DATABASE_DSN` is set (`tools/perfdb.sh` on build-host).

**Rules.** (1) A list starts from an indexed candidate set that is a superset of its access rule (own; owned by or
bound to an active team of the account; hosts the account may manage) and runs the exact access functions only on
candidates, until the page is full; the account's own personal machines take the functions' first branch without
calling them. (2) Polled reads take no row locks and no serializable predicate locks (`db.InReadOnlySnapshot`).
(3) Serializable transactions use index scans (`SET LOCAL enable_seqscan TO off` in `db.InTx`): on our small
tables a sequential scan takes a table-wide predicate lock and every concurrent write then conflicts. (4) Every
lookup a trigger or access function makes has an index.

| What | Before | After | Commit |
|---|---|---|---|
| GET /v1/machines, team owner, all workspaces (seeded) | 57 s (count 28.5 s, run twice) | 82 ms count, one pass | 44f18cec |
| GET /v1/machines?workspace=team (seeded) | >5 s | 103 ms count | 44f18cec |
| GET /v1/machines (prod) | 1,954 ms avg, 500 at the daemons' 2 s timeout ~21/min | own machines skip the access functions; 0 5xx after deploy | 683a7410 |
| GET /v1/previews, every actor (seeded) | >5 s timeout (33 s query) | 1.3–70 ms p50 | a30a9c4e |
| GET /v1/tunnels (seeded) | >5 s timeout (9.4 s query) | 6.6–66 ms p50 | a30a9c4e |
| GET /v1/terminal-sessions, solo / team (seeded) | 0.74 s, 555 queries / >5 s | 57–100 ms p50, 105 queries | 92825315 |
| GET /v1/machines/update-summary?workspace=team (seeded) | >5 s | indexed team candidates | fc97a9e2 |
| GET /v1/machine-services (8 s daemon poll; prod 1,261 ms) | every online machine evaluated | candidates first | 045ca58a |
| POST /v1/machines/{id}/disconnect (prod) | 503 after 7.7 s, serialization pivot | index-page predicate locks only (migration 123) | 90ca7e63 |
| POST /v1/machines/pairings (prod) | 500 after 9.3 s, serialization pivot vs daemons' peer config | 1–5/20 → 0/20 under concurrent load (migration 131) | f3ec3bc2 |
| GET ssh-target / ssh-host-keys (prod, ~48/s from old daemons) | serializable + FOR UPDATE, access functions on every call | read-only snapshot, row first, own-machine branch | 1d81dcf5, 28f943d4 |
| Serializable transactions outside InTx (peer config etc.) | seq scans: table-wide predicate locks | db.BeginSerializable: index scans | a32f4476 |
| GET /v1/machine-services (prod 1,261 ms → 2 s under load) | authority function twice per port, for every candidate | once per port; own machines' automatic ports skip it | bed2d87d, 759160bc |
| Machine topic fan-out (prod ~5.7 → ~107 updates/s) | presence flips and service snapshots woke every daemon's inventory | presence ignored; snapshots move `machine_services`; hosts told only topics they wait on; daemon refresh ≤1/30 s, `pb wait` asks its own daemon | 7db75a9d, e50f8372, 960aa6a9, cfab5697 |
| Production, whole (.12/.15 → .16) | load ~21, postgres 340–356% of 4 cores, peer config 4.3 s with 245 errors / 3 min | load 0.8, postgres 63%, peer config 153 ms with 0 errors (02:53Z; part may be a quieter device wave) | — |

**Scaling audit** (maintainer 2026-10-23; one account at 1k / 10k machines and 100 members, platform 100k). Ranked by
cost at 10k x how often it happens. W = per-machine work, N = machines in the account. Fix top-down, one commit
each with a PG/load test proving the bound at 10k; maintainer audits daemon/CLI/relay/tunnel the same way.

| # | Path | Cost at 10k per event / steady | Happens | Fix (state) |
|---|---|---|---|---|
| 1 | Peer network: a generation bump makes every machine refetch its whole peer list | N x N entries (10^8) per join/leave/key change, 128 KiB cap | every pairing, revoke, renewal | signed deltas per scope, below (building) |
| 2 | Event waits never healthy (MaxWait 30 s = client timeout) → every daemon polls `/v1/machines` every 15 s x (1 + teams) | 355 lists/min from 22 machines; ~2.7k lists/s at 10k | always | MaxWait 25 s (maintainer); list cheap (#5) |
| 3 | Revocation document unfiltered, oldest-first LIMIT 10000, no expiry filter: past 10k rows new revocations never reach edges; hosts poll 15 s, edges 5 s | security bug; 4 queries x 10k rows x ~667 req/s | always | per-account, newest/unexpired, delta by seq (next) |
| 4 | Reconnect storm after deploy/outage: every machine fetches peer config, inventory, services at once | N snapshot builds at once → 503s | every deploy | jittered reconnect (maintainer), snapshot cache per policy scope, per-account build rate bound; load test 5k-machine team |
| 5 | Machine list: `count(*) OVER ()` runs access functions on every candidate; N daemons per inventory event | ~7N function calls per list; N^2 per event | every inventory event | candidates by policy (3cdce5f8), rule index (2958c232), identical requests coalesced (built), set-based access description (next) |
| 6 | Wait hub rechecks generations every 5 s per held waiter (2 per machine), ListenFor reconnect wakes all | 4k q/s at 10k, 40k q/s at 100k | always | one sweep per tick (`= ANY`), recheck 60 s jittered |
| 7 | machine_services: refresh every 2 s past ~10 service machines, all N candidates; ErrDenied above 512 | broken at 1k | always | delta/topic-driven, candidate bound by policy |
| 8 | Relay grants: one signature per relay node per 64-peer chunk, peers loaded N+1, silently cut to 16 | ~5k signatures per fetch | every peer config | one signature per fetch, set-based, no silent cut (maintainer) |
| 9 | SQL topic triggers uncapped (Go caps 1024): a policy save bumps machine_access on every team machine → each refetches #3 | N refetches of #3 | every policy save | cap + coalesce; `team_machine_grants_publish` gets a WHEN |
| 10 | edgeset.ReconcileAll every 1 s: one tx per workspace, GROUP BY over all user_machines per page | O(platform) per second | always | dirty-workspace queue (maintainer) |
| 11 | Edge desired state / ingress: full snapshot, no LIMIT, polled; ErrIngressDenied above 4096 | O(platform) per poll | every control interval | deltas by seq (maintainer) |
| 12 | previews topic wakes every carrier in the audience, each POSTs attachments.Allocate | N POSTs per preview change | every preview change | topic per carrier |
| 13 | Team view / actions return the full team incl. inactive rows; List x 128 teams; policy save row by row | 6.4 MB per read at 1k | every team page | paginated team members/machines/grants (contract with A/C), batch insert |
| 14 | Heartbeats: user_machines 339 updates/min from 22 machines | ~2.5k writes/s | always | maintainer interval; server writes only on change |
| 15 | Dashboard counted from the first 200-row page | wrong totals | every console load | GET /v1/machines/summary (built) |
| 16 | No retention: access sessions, CLI/browser sessions, usage receipts (edge every 250 ms), config sync history, operations, stripe events, device grants, tunnel logs, connector sessions, machine requests; audit prune 1,500 rows/min | unbounded tables | always | retention jobs, batched deletes |
| 17 | tunnels / config topics: written and NOTIFYed with no subscriber | wasted writes | every change | delete the triggers |
| 18 | Console loads every page (`allPages()` previews/tunnels) | O(N) per page view | every view | keyset cursors, server search/sort (PQ-04) |

#### Peer network deltas (maintainer server, maintainer daemon; user decision 2026-10-23)

*Today* a peer-network change bumps the topic and every machine refetches its whole signed configuration
(`POST /v1/machine-peer-network/config`, `paperboat-network-config+jwt`): N x N per change.
*Contract* (server first; old daemons keep the full fetch until updated; v2 after maintainer's review):
1. **State.** A machine's signed state is `self` + `peers` (as today). `generation` stays the endpoint's monotonic
   config generation: every answer (snapshot page or delta) allocates the next one, and the relay grants and
   regional candidates in that answer are issued against it, as today. `self` (keys, key/endpoint generation)
   changes only through a snapshot; every delta carries `self_key_generation` and the daemon refuses another one.
2. **Where changes come from.** The N x N part is the personal mesh (every endpoint of the account): it is rebuilt
   once per change, set-based, into a shared per-account log `(account_id, mesh_seq, endpoint_id, op, entry)`; a
   join or leave writes O(1) rows however large the account. The per-endpoint part (machine_access, inspector,
   config_compare scopes, at most 128 grants; admitted certificates) is computed per request and diffed against
   what that endpoint was last sent (stored per endpoint with its generation and `mesh_seq`).
3. **Request.** `POST /v1/machine-peer-network/changes {base_generation, base_digest}` (machine proof as config).
4. **Delta**, signed with the config's issuer, kid and audience, `typ` `paperboat-network-delta+jwt`: claims
   `generation`, `base_generation`, `base_digest`, `digest` (after applying), `self_key_generation`, `iat`,
   `exp` (the state's new expiry), `changes[]` = `{op: add|update|remove, endpoint_id, identity?, scopes?}` (an
   update replaces identity and every scope; remove carries endpoint_id only), at most 256 per answer, `more: true`
   = ask again with the new generation. No change: only a new `generation`/`exp`.
5. **Digest** (v3: updatable in O(changes); a hash of the whole list costs O(N) per request, N^2 per event) =
   lowercase hex of the 32-byte big-endian sum mod 2^256 of sha256(JCS(entry)) over every `peers` entry, where an
   entry is `{identity, scopes}` with `scopes` sorted by (resource_kind, resource_id, capability, direction, port)
   and JCS is RFC 8785; `self` excluded; order of entries does not matter. Changing an entry subtracts its old hash
   and adds the new one. It proves both sides hold the same state; authenticity is the signature. Vectors: no peers
   `0000000000000000000000000000000000000000000000000000000000000000`; peer a =
   `{"identity":{"account_id":"user_1","disco_public_key":"","endpoint_generation":1,"endpoint_id":"machine_b",`
   `"key_generation":1,"machine_generation":1,"machine_id":"machine_b","quic_certificate_fingerprint":"ab",`
   `"quic_public_key":"BBBB","role":"machine","virtual_address":"100.64.0.2","wireguard_public_key":"AAAA"},`
   `"scopes":[{"capability":"connect","direction":"dial","port":443,"resource_generation":1,"resource_id":`
   `"network_x","resource_kind":"machine_network"}]}` -> `74f978ea9e3cfdd4f6380e7cc4cd2fd01f4a51b2d0e6c1edab66b0d8749399ec`;
   a + b (b = a with endpoint_id and machine_id `machine_c`, virtual_address `100.64.0.3`) ->
   `7ade17e24a93a6c29cfded5053bb68f76eea349eea510158271d759f7672ac0e`.
   The daemon applies a delta only if `base_generation` and `base_digest` match its state, recomputes `digest`
   after applying and asks for a snapshot on any mismatch, so gaps, reordering and replay are safe.
6. **Expiry.** Mesh scopes carry no `expires_at` (they last until the state `exp`); only per-operation scopes carry
   their own and come and go as add/remove. Keeping 1k peers alive is one tiny delta, not 1k updates.
7. **Snapshot** `{snapshot: [pages]}` when the base is unknown or older than the stored state, the digest differs,
   after key rotation, or after a policy change that rewrites the set. Pages are signed configs of at most
   128 KiB each with `page`, `pages`, the same `generation` and the `digest` of the whole state; the daemon assembles
   them and verifies the digest. Builds are cached per (account, mesh_seq) so machines of one account share the mesh
   build, and limited per account (token bucket) so a reconnect storm queues instead of failing; clients reconnect
   and refresh with full jitter (maintainer). The daemon's MaxEndpoints (2048) becomes the visible-peer bound (point 9).
8. **Regional candidates and relay grants** stay full in every answer (about 2 nodes).
9. **Bounds (coordinator 2026-10-23).** An account holds up to 10,000 machines (plan limits: Pro 1,000; Teams
   unlimited with a hidden 10,000 ceiling), so the per-account cap the mesh build uses (`peernetwork.MaxEndpoints`,
   2,048 today) becomes 10,000. What one machine receives is bounded per machine instead: its visible peers, the
   endpoints policy lets it reach or be reached by (personal mesh: every peer of the account; team machines: scoped
   by the team policy), capped by one configured MaxVisiblePeers in `shared/peernetwork`; a snapshot pages through
   them (point 7). The mesh build stays key-probed, O(account endpoints) per change (about 50 ms at 1,000 machines,
   maintainer 2026-10-23); no change capture in the trigger transactions. Test for the delta step (maintainer, PG): a
   10,000-machine account, one machine joins: one log row, the refresh under 1 s, and another machine's delta
   carries exactly that one entry.
*Prior art* (read 2026-10-23; headscale 494e2a6, BSD-3; NetBird 8397bed, AGPL: ideas only, no code):
1. *Push vs poll.* Both hold one long-lived stream per node and send the full map only on connect: headscale the
   Tailscale map long-poll with keepalives every 50 s + up to 9 s jitter (`hscontrol/poll.go:26,58,334`), NetBird a
   gRPC stream fed by a bounded per-peer channel whose send never blocks (full channel: dropped, the peer resyncs;
   `management/internals/controllers/network_map/update_channel/updatechannel.go` SendUpdate). Ours: keep the HTTP
   long-poll, but the wait answers with the signed delta itself (no wake-then-fetch round trip), held below the
   client timeout (the MaxWait = client timeout bug made every stream unhealthy) with jittered keepalives.
2. *Log vs batcher.* Neither stores a per-node change log. headscale keeps all nodes in a copy-on-write in-memory
   snapshot (`hscontrol/state/node_store.go` NodeStore) and a per-node "last sent peers" to diff against
   (`mapper/batcher.go` computePeerDiff); NetBird a per-account network serial the client compares and drops stale
   maps by (`client/internal/engine.go:1614`). We run rolling deploys and several request paths, so we keep the
   state in PostgreSQL: one shared per-account mesh log (not per node) plus each endpoint's last delivered state,
   written by that endpoint's own request (O(1) per machine, no fan-out).
3. *No per-row fan-out writes.* headscale writes only the changed node; fan-out is in memory (`addToBatch` appends
   to each connected node's pending list, `processBatchedChanges` sends one bundle per node per 800 ms tick, at
   most one in flight, policy repeats de-duplicated). NetBird debounces account-wide changes on a per-account timer
   (`controller/controller.go` BufferUpdateAccountPeers, BufferUpdateAffectedPeers). Ours: a topic generation per
   account, not per machine (one row per change, not N; the 4,097-row insert writes one), statement-level triggers,
   and a per-account debounce of mesh rebuilds.
4. *Reconnect storms.* headscale #3346 (`mapper/initialmap_storm_test.go:108-132`): after a restart every node
   redialed; per-node filtering serialized on one exclusive policy lock, time-to-initial-map went from 4 s to 76 s,
   clients aborted and redialed and the storm never converged; fixed with shared read locks, NodeStore write
   batching and jittered keepalives. NetBird coalesces concurrent account loads into one per 100 ms window
   (`management/server/account_request_buffer.go` GetAccountWithBackpressure). Ours: one snapshot build per
   (account, mesh_seq) shared by every waiter, no exclusive lock across builds, a per-account build token bucket,
   client full jitter, and a server answer always faster than the client timeout.
5. *Per-node policy cost.* headscale computes the peer map O(n^2) once per write batch, not per request
   (`policy/v2/policy.go:698` BuildPeerMap), caches each node's matchers until the policy changes
   (`policy.go:79,947` matchersForNodeMap) and has a global-filter fast path. Ours: per-row SQL access functions
   (~2 ms each) are the cost; compile "who may reach whom" once per (account, policy version), set-based, and make
   each node's view a lookup; the personal mesh (everyone) needs no filter at all.
Test (PG): one join on a 1,000-machine account writes O(1) change rows per affected scope and each machine's next
answer carries one change; a 5k-machine team survives a server restart (snapshot storm) without 503s.

#### Multi-account hosts: per-account addresses, one host gateway routed by the caller's uid (maintainer design, maintainer server, maintainer + GPT review; user direction 2026-10-26; replaces the dropped global 127.64.0.0/10 pool) — rev 2

**Fact that shapes it.** Local names are not in a hosts file or local DNS: the local access domain is a public wildcard,
so every `<machine>.<domain>` and `<port>.<machine>.<domain>` resolves to 127.100.0.1, the browser gateway, which routes
by Host/SNI (checked with dig; resolver files are only cleaned up, `server_darwin.go:166`).
So names never collide in DNS; the one host-wide bottleneck is the gateway's 127.100.0.1:80/443 (+ its 1024-65535 ports).
**Goal.** Any number of OS users, each with any account, simultaneously, with no platform-wide cap; one user's
conflict degrades only that item (done: 5080a29b, `listener_refused`). Per-host ceiling: 127.100/16 = 65,533 machine
addresses shared by all users of one host (stated, not a platform cap).
**1. Addresses (server, maintainer, agreed).** Lease per (viewer account, machine): unique (account_id, machine_id) and
(account_id, slot), lowest free slot in 127.100/16; existing rows keep their slot under the owner account; other viewers
of a team machine get their own slot on first sight; no quarantine (the guard keys by machine, see 2); 54000 per account
past 65,023. Team machines: one slot per viewer (stable for the viewer; a raw IP is not shareable between teammates).
**2. Guard state per (uid, account, machine), with lifetimes.** The guard derives the uid from the kernel (SO_PEERCRED on
its unix socket; never from the request) and reserves atomically under its registry lock. `claims[(uid, account_id,
machine_id)] = {ip, port set}`; account_id is the immutable server id, never a handle. The first claimant of an address keeps
it; another (uid, account) whose preferred address is taken gets the lowest free 127.100.x.y, persisted (stable for it on
this host). Claims persist independently of live listeners.
- *Release (maintainer 1):* each reconcile sends the daemon's complete (account, machine) set; claims absent from it are
  released; a released address stays held for its previous (uid, account) for 7 days, then is free. Logout and account
  switch release that account's claims (into the 7-day hold); `pb uninstall` releases the uid's claims immediately; a
  deleted OS user (uid without a passwd entry) is released at guard start. UID reuse: a new user with an old uid inherits
  nothing older than the 7-day hold, since uninstall/logout released it; account rename changes nothing (ids are immutable).
- *Legacy adoption (maintainer 2):* today's `reserved.IPs/Names[...] = uid` load as (uid, "", "") entries; one is adopted only
  by an Acquire of the same uid whose preferred address equals it; entries not adopted within 7 days are released.
  Single-user hosts keep every address.
- *Bounds (maintainer 5):* per uid 512 claims and 4 gateway-routed accounts; host-wide 65,533 addresses and 8,192 live
  listeners. The refused item gets `listener_refused` with the reason; `pb status`/`pb doctor` show
  "local access limit reached on this computer (<which>)" for the refused user only. Others are unaffected.
**3. Isolation (GPT 1-2, maintainer 3b).** Linux (today, kept): nft output allows a lease ip:port only for `meta skuid <owner>`;
input accepts a SYN to the range only for the guard's marked sockets (`socket mark`), so B binding A's absent address
receives nothing; everything else in the range is rejected. On release the guard closes the lease's listener and every
connection it accepted (conntrack entries die with them), so a reassigned address carries no old connection.
macOS: pf `pass in ... user <guard>` (receiving socket owned by the guard) and `pass out ... user <owner>` per lease,
block the range otherwise (`server_darwin.go` darwinRules), same release rule. Tests on Linux and macOS: B cannot connect
to A's address, B cannot receive on A's address while A's listener is absent, both across a guard restart and a guard crash
(rules are installed before any listener and persist across restart; on crash the default-deny table stays).
**4. Gateway: one host gateway, routed by the caller's uid (recommendation).** The guard (root) owns 127.100.0.1 on
80/443/1024-65535 once per host. For each accepted connection it finds the connecting socket's owner uid from the kernel
(Linux: sock_diag INET_DIAG lookup by the 4-tuple, `idiag_uid`; macOS: the `net.inet.tcp.pcblist_n` entry for the
4-tuple, `xso_uid`), and hands the connection to that uid's daemon gateway (the daemon's existing proxy, over the guard
socket, as listeners are handed today). The daemon routes by Host/SNI among only its own routes; the guard authorizes
nothing by hostname (GPT 2). Result: Bob's and Alice's `linux-test.<domain>` and `3000.laptop.<domain>` both work, short, at the same
URL, each reaching their own machine; a uid without a daemon gets a refusal page.
- Two accounts under ONE uid with the same machine name: the daemon routes both; the second account's machine gets
  `<machine>.<workspace-slug>` as its name label (the slug the user picked, never an email; visible only to that uid,
  maintainer 4). Persisted per (uid, account, machine); never changes for the first.
- *Certificates (maintainer 3a/3c):* the guard issues a certificate only for a name in the requesting uid's own route set,
  under that uid's own local CA (browser trust is per user already); `certificate.go:142` stops matching
  `lease.ip == BrowserGatewayIP` and checks the uid's gateway registration instead. Test: B gets no certificate for A's
  name and B's connection to the gateway with A's name reaches B's routes only (refused if B has none).
- *TLS/cookies (GPT 5):* names are exact per route; the CA issues `*.<machine>.<domain>` per machine for port and alias
  names, so deeper names are covered. Cookies are scoped by browser profile, and each OS user has their own profile and
  own CA, so users never share cookies; within one user, cookies cross ports and sibling subdomains as on any site (stated,
  not isolated).
**Rev 3 conditions (maintainer re-review, GPT round 2; coordinator GO 2026-10-26). These are part of the design and tested.**
- *Attribution is the security gate, fail closed.* The guard keeps the accepted FD open, takes the CLIENT side's reversed
  full tuple (family, addresses, ports, network namespace) and needs exactly one live match with an owner uid, retrying at
  most 3 times within 50 ms; missing, ambiguous, inconsistent, or a uid with no registered daemon closes the connection
  (or answers the refusal page on 80/443). Never the accepted socket's uid, never "the active/first user", a hostname or a
  cached tuple. Linux: INET_DIAG exact 4-tuple (a hash lookup). macOS: `net.inet.tcp.pcblist_n`, field `so_uid` of the
  xsocket record; it copies every pcb, so it ships behind a benchmark (10,000 open sockets, one lookup < 2 ms) and a
  device churn proof before macOS multi-user is enabled; single-user macOS keeps today's path. Other namespaces, NAT or
  container paths into 127.100.0.1 are refused. Tests: unknown uid, peer closed before lookup, close/RST/reconnect with
  tuple reuse under churn, all refused, none misattributed.
- *Identity contract.* The caller is the credential owning the client socket: `sudo curl` is root (no daemon: refusal
  page), a setuid helper is its effective uid, a socket passed to another process stays its creator's, a local proxy is
  the proxy's uid. SUDO_UID is never read. System users have no daemon and get the refusal page.
- *TLS (maintainer 1, GPT 4).* The connection is handed off as raw TCP after attribution; TLS ends in the user's daemon.
  Isolation guarantee: (a) CA signing keys stay in the guard's root-only state (`rootCA-key.pem`, 0600 root) and daemons
  receive only leaves for their own routes; (b) a connection from uid A always reaches A's daemon. Per uid: own CA, own leaf
  keys (owner-only files), certificate cache keyed by uid + name + route generation, every SAN authorized at issuance
  against that uid's routes (`*.<machine>.<domain>` does not cover `<machine>.<domain>`; both are listed), separate TLS
  session-resumption state per daemon, Host/:authority checked against the daemon's routes on every request. Trust: each
  CA goes into its own user's stores (macOS login keychain user domain; Linux user NSS db and the user's browser profiles);
  the system-wide install stays only for the CA of a host that has a single user (today's hosts), and when a second uid
  joins, its CA is user-scoped. Since (a) and (b) hold, a CA trusted host-wide grants nothing to other users. Test: a leaf
  issued to B for a name A also uses is never presented on a connection from A.
- *Releasing never acts on a partial set (maintainer 3).* The complete (account, machine) set is sent only from a complete,
  verified configuration (every page read, signature checked); never from a failed, degraded or partial read, and never as
  an empty set while claims exist for that account (offline until revoked: the last good set stays until an explicit
  revocation). Test: server unreachable for 10 min, then a truncated or empty list: no claim released, no listener closed.
- *UID reuse (GPT 5).* A claim belongs to a user incarnation: uid + home directory inode + passwd entry creation (the
  home inode where no creation time exists); a deleted-then-recreated uid starts empty.
- *Cookies (GPT 6).* Each OS user has their own browser profile, so users never share cookies. Inside one user, an origin
  (`<name>.<domain>`) that served an account is never reassigned to another account of that user while its claim or
  7-day hold lasts; cookies cross ports and sibling subdomains as on any site (stated, not isolated).
- *Proofs before enabling:* Linux now (nft deny, anti-spoof bind, attribution tests on a two-user linux-test-vm-2 container); macOS
  after the user's macOS go.
- *Not in scope here (maintainer robustness row):* public wildcard names need working DNS (offline, DNS-rebinding filters).
CHECKLIST: F23-01 two OS users on one Linux host (addresses, gateway, names, isolation), F23-02 attribution fail-closed,
F23-03 release/hold/partial-set, F23-04 macOS multi-user (after macOS go).

**5. Options weighed for names.**
(a) per-uid DNS answers (NSS module, resolved stub peer uid, macOS resolver): rejected, the asking uid is lost on macOS
(mDNSResponder resolves as root) and Linux coverage is partial; public wildcard DNS makes it unnecessary anyway.
(b) collision-only suffix (rev 1): rejected, first-claim order becomes identity on a shared host.
(GPT 6) always account-qualified canonical names plus short aliases bound to their first owner: unambiguous, but makes
every URL longer or order-dependent again for the short form. With uid-routed gateway, short names are unambiguous per
user without qualification, so neither is needed; the slug label is used only within one uid (section 4).
**6. Windows (after launch).** Kernel uid of a loopback TCP peer: `GetExtendedTcpTable` (owning PID -> user SID); filtering
per user with WFP (already used for the guard range). Same design.
**7. Tests.** Two uids on one fake guard: same slot, different addresses; stable re-acquire after restart; release and 7-day
hold (600 machines come and go, a new acquire still succeeds; a held address is not given to another uid); per-entry legacy
adoption; gateway routing by uid (Bob and Alice `linux-test.<domain>` reach their own routes); certificate refusal for another uid's
name; one refused item leaves the other user intact. Device: two OS users on one linux-test-vm-2 container, Linux; macOS VM.
*Prior art:* Tailscale serves one user at a time per host (`~/ref/tailscale/ipn/ipnserver/server.go:255`
checkConnIdentityLocked, "already in use by"; profiles switch the node, `ipn/ipnlocal/profiles.go:126`); NetBird one
active profile per host (`~/ref/netbird/client/internal/profilemanager/service.go`); Headscale bumps colliding names to
base-1, base-2 with the holder keeping its label (`hscontrol/state/node_store.go:669`). Routing loopback connections by the
peer's kernel uid is what Tailscale's LocalAPI does for its unix socket (peercred) and safesocket on macOS
(`~/ref/tailscale/safesocket/safesocket.go`); we apply it to the TCP gateway.

#### File transfer modes: Automatic and Unlimited (maintainer design; maintainer server, maintainer paths, maintainer console; user decision 2026-10-26)

**Modes.** Exactly two; self-hosted relays add no third mode and do not lift Automatic's bound.
- **Automatic** (default): total of one `pb send` (every file) <= 2,000,000,000 bytes. Path:
  P2P, else any relay including Paperboat-hosted. Hosted-relay bytes count toward plan bandwidth (relay quota, as today).
- **Unlimited**: no Paperboat size bound. Path: P2P, else only the account's/team's self-hosted relays. File bytes never
  cross a Paperboat-hosted relay. With no eligible path the transfer pauses and resumes by itself when one returns.
**Byte definition.** total_bytes = sum of file sizes in the prepared batch (`filetransfer.Prepare`), known before any
server call. The bound is one config value (server `FileTransfer.AutomaticMaxBatchBytes`, mirrored in the descriptor),
not copied in the client. Today's 50 MB/file and 500 MB/batch client limits are replaced by it (Automatic) or removed (Unlimited).
**Authority (maintainer).** The inbox request carries files[{basename,size,sha256}]; POST
`/v1/machines/{id}/file-transfer-descriptor` adds `transfer_mode` and `declared_bytes` (= the manifest total). The signed
file_transfer claims gain `transfer_mode` ("automatic"|"unlimited") and, for automatic only, `max_bytes` = declared total.
Automatic over the bound: 409 `transfer_too_large` details {total_bytes, max_bytes}. Destination verifiers accept the two
claims before the server emits them (claims are exact). The destination cuts a stream at max_bytes (forged-size test).
**Enforcement, at the endpoints (relays stay content-blind; coordinator option a).** A hosted relay forwards encrypted
peer packets and cannot see streams. (1) Sender: for unlimited, a file_transfer stream is opened only when the peer path is
direct or a self-hosted relay (`peertransport` path kind; maintainer exposes PathKind = direct|self_hosted_relay|hosted_relay);
if the path moves to a hosted relay mid-stream, the sender stops at the next chunk and pauses. (2) Destination: refuses an
unlimited stream whose peer path is hosted_relay (`file_transfer_path_not_allowed`). Both are tested with a fake path that
switches to hosted_relay: no file byte is written to that path (test fails if one is).
**Pause and resume.** Uploads already commit offsets (tus-style `committed_offset`). New: the batch state (batch_id,
descriptor request id, per-file sha256/size/committed offset, mode) is persisted under the user state dir and owned by
the local daemon's transfer queue, so `pb send` can exit and a daemon restart resumes. Paused reasons (one enum):
`no_eligible_path` (unlimited: only a hosted relay is available), `destination_offline`, `relay_quota_exhausted`
(automatic). Resume triggers: path change event, destination online, backoff 5 s -> 5 min.
**UX.** `pb send` = Automatic unless `--unlimited` or the preference. Over the bound it refuses before any network call:
"This transfer is 3.4 GB. Automatic sends up to 2 GB. Use --unlimited to send directly or through your own relay."
Paused: "Waiting for a direct connection or your relay (<reason>)" in `pb send`, `pb transfers`, inbox.
Preference: `pb config set transfers.mode automatic|unlimited` writes the account preference on the server (one owner;
console Settings edits the same field): GET/PATCH `/v1/me/preferences` field `file_transfer_mode`. Per account, not per
workspace (the mode is the sender's choice of path, not a team policy).
**Console/inbox fields (maintainer).** Transfer and inbox items: `transfer_mode`, `transfer_state` (sending|paused|done|failed, beside the approval `status`), `pause_reason`
(enum above), `total_bytes`, `committed_bytes`. Errors: `transfer_too_large` {total_bytes,max_bytes},
`file_transfer_path_not_allowed`.
**Tests/rows.** CHECKLIST feature row F22-01 (two modes) and command rows for `pb send --unlimited`, `pb config set
transfers.mode`: size boundary (2,000,000,000 ok, +1 refused, before any call), batch accounting (files summed),
unlimited never on hosted relay (failing-if-touched fake path), pause/resume on path loss, resume after daemon restart.
**Rollout, in order (coordinator 2026-10-26; old clients fail only the new feature, never lose anything else).**
Released pb validates the published transfer policy and the signed regional candidate set strictly, so a field or
bound it does not know makes it refuse the whole connector admission or candidate set (f5628bae/8385e540 were
reverted for this, c8163091).
1. *Descriptor only (now, maintainer).* Policy and candidate sets keep today's shape. A send that declares
   `transfer_mode`/`declared_bytes` is checked (409 `transfer_too_large` over 2,000,000,000) and its credential carries
   `transfer_mode` + `max_bytes` only when the destination signals it understands them; to an older destination a
   declared send within today's caps gets today's credential, and Unlimited or a larger one gets 409
   `destination_update_required` naming the machine. An undeclared send (released pb) is exactly today's.
   The destination signal is observed capability `file_transfer_modes` (server accepts it since 85afe804): maintainer's
   pb release N accepts both host store schemas, N+1 lifts the 50 MiB per-file store CHECK, takes the claims as its
   receive bound and reports the capability; config `access.file_transfer.modes_minimum_version` = first official
   from N+1. The Team Inbox keeps today's 50 MiB / 500 MiB request caps until N+1 is that minimum.
2. *Published bound and `hosting` (maintainer, measured).* `automatic_max_batch_bytes` in the descriptor and connector
   admission policy (replacing max_file_bytes/max_batch_bytes, spool 2,000,000,000) and regional `hosting:
   "self_hosted"` ship only when no machine on official <= 2026.10.25.0 was active in the last 7 days:
   `SELECT reporter_version, count(*) FROM paperboat.user_machines WHERE deleted_at IS NULL AND revoked_at IS NULL
   AND last_seen_at > now()-interval '7 days' GROUP BY 1` (heartbeat-reported client version since server .84, migration
   180; maintainer reads it on prod; dev builds older than 77d8a1bc / 07866f9f count as old).
3. *Declaration required (maintainer, after maintainer's sender ships in an official release and step 2's query shows no older
   sender).* The descriptor refuses an undeclared send (400 `invalid_transfer_intent`, "update pb to send files"); the
   forged-size cut is then always at the declared total.
Prevention: removed 2026-10-26 (no released users before launch): no compat suite against old official releases.
*Prior art:* Taildrop (`~/ref/tailscale/feature/taildrop/resume.go`, `send.go`): no size cap, resumable `.partial` files
with 64 KiB block checksums, sent over whatever path the tailnet has, DERP included; Tailscale peer relays
(`feature/relayserver`) are user-run and chosen like any path. croc and magic-wormhole use a public relay by default
and accept a self-hosted relay flag; neither bounds size. Our split (bounded on hosted relays, unbounded only on
direct/own relays) bounds hosted relay cost without a bandwidth bill surprise.

#### Relay pair authorization by membership, and relay health without 15 s renewals (maintainer design; maintainer server signing; coordinator decision 2026-10-26)

**Why.** (1) A relay grant lists every peer the endpoint may reach, each with its scopes. One grant naming
16,384 peers is ~4.3 MiB, so a 10k-machine Teams account on one relay needs tens of GB, and the relay caps a
grant at MaxMergedPeers = 2,048. That contradicts unlimited Teams (10k ceiling). (2) Signed relay health
observations last 15 s (regionalObservationMaxAge), so every engine renews its whole signed configuration about
every 9 s just to keep its relays usable: ~13 config POSTs a minute per machine, measured on linux-test-vm-2.

**Prior art.** Tailscale's derpserver authorizes a client once at connect by node key and keeps no per-peer
lists (derp/derpserver/derpserver.go:1732 verifyClient); end-to-end authorization stays with the peers'
WireGuard keys and netmaps. Tailscale nodes do not re-fetch the netmap for DERP health: health is their own
netcheck and home-DERP connection (net/netcheck, wgengine/magicsock/derp.go).

**Design 1: memberships instead of per-peer scopes.** Today the relay forwards a pair when both grants name
the same scope (resource kind, id, generation, capability, port) in opposite directions, under each other's
peer entry (derpquic reciprocalScopes). Those resource ids are already pair- or group-specific: the account's
network for the mesh, one access grant for a shared machine. So the per-peer nesting adds size but no
authority. A grant instead carries `memberships`: the same scope tuples, attached to the endpoint itself
(bounded, e.g. 256 per grant: one for the account mesh, plus one per access grant, share, private route or
team network the endpoint takes part in). The relay forwards a pair only when one grant holds (R, dial) and
the other holds (R, accept) for the same resource kind, id, generation, capability and port, both unexpired
(maintainer review: shares and private routes are one id held by their host and every viewer, so holding the
same membership is not enough; two viewers of one share stay unpaired). Mesh and team networks hold both
directions, so they keep a full mesh. One index lookup per pair. Relay-control peers become "same account and
both hold the account's network membership". Disco keys come from each grant's own signed field. Memory per
grant becomes O(memberships) (a mesh endpoint: one entry), independent of the account's size.
Rollout (agreed with maintainer 2026-10-26): a grant has one shape, so the server chooses per endpoint and the relay
pairs a per-peer grant with a membership grant. Order: (1) every relay node on ≥ 37b5b38f (accepts memberships);
(2) the server accepts the request field `membership_grants`; (3) its configuration answer offers
`membership_grants: true`; (4) clients ≥ b1e33786 then ask with `"membership_grants": true`, verify every
membership against their signed configuration and take relay-control peers from the mesh membership; (5) the
server issues membership-only grants to identities that asked, per-peer grants to the rest; (6) once no
per-peer client remains, per-peer lists, MaxMergedPeers and the client's relay-control cap are removed. Test: 10,000 endpoints of
one account registered on one relay, heap per grant and pair-check time (budget: < 1 KiB per mesh grant,
< 1 µs for a pair's first check with grants spread over memory, < 100 ns per datagram after it, as the
connection remembers its pair until a grant changes), plus the existing revocation, quota and cross-account refusal tests.

**Design 2: relay health that does not expire every 15 s.** The signed candidate set keeps a node's identity,
endpoints and hosting for its own lifetime (the node's `expires_at`, e.g. 5 min). The engine's own relay
connection is the health signal (a failed connection or a refused admission fails over at once, as today).
The control plane announces a node's state change (drain, stop, capacity crossing, revocation) on the
`peer_network` topic, so engines renew on the event instead of every 9 s. The 15 s `observed_at` freshness
check goes; the authority boundary moves to the node's signed `expires_at`, the relay's own fencing (node
generation, process epoch, revocations) and the pushed state change.
Expected: config POSTs per machine fall from ~13/min to the topic events, plus one renewal per signed
lifetime.

Owners: maintainer (relay derpquic, CLI tailnet), maintainer (server signing, topic publish). Keep MaxMergedPeers
2,048 until design 1 lands.

#### Direct paths without a relay: peer endpoints in the signed peer configuration (N5; maintainer design, maintainer server first; coordinator 2026-10-26)

**Why.** Two machines on one LAN cannot connect natively while no relay is usable. The first contact resolves
a relay region and exchanges endpoints only through the relay (meow + disco CallMeMaybe over DERP,
`native/owner.go:249` PrepareRegional before Dial, `mesh/engine_backend.go` advertiseEndpointsTo), so with no
relay the dial fails before any packet (staging, maintainer 2026-10-26). That breaks the P2P promise during a relay
outage, and it makes every first contact pay a relay round trip.

**Prior art.** Tailscale's client reports its endpoints in each MapRequest (`tailcfg.MapRequest.Endpoints`;
control/controlclient/direct.go). Control hands every peer's endpoints to the others in the netmap
(`tailcfg.Node.Endpoints`, changes as `PeersChangedPatch`). magicsock disco-pings them directly
(wgengine/magicsock/endpoint.go updateFromNode), and DERP is only one of the paths.

**Contract.** Order (maintainer review): the server accepts `endpoints` in the request bodies and deploys (they
decode strictly, so an early client would get 400); then a CLI round sends it and accepts the delivered field
(the client accepts it since 3e401cc3/06f77b82); then the server delivers it. maintainer owns the two config paths,
maintainer the `/changes` request. The server stores the set normalized (sorted, deduplicated, valid unicast
ip:port) and publishes only when the normalized set differs from the stored one, coalesced per identity to one
per 60 s with the latest set winning.
1. *Report.* The peer-network configuration request (`POST /v1/machine-peer-network/config` and
   `/v1/peer-network/config`, and the delta `/changes` request) carries `endpoints`: the engine's own candidate
   addresses as `ip:port` strings. These are local interface addresses, the STUN-mapped public address and a
   port-mapped address, at most 8, sorted, deduplicated. The server stores them per peer-network identity and
   writes only when the set changed.
2. *Publish, bounded.* A changed set moves `peer_network` for the endpoint's audience. The publish is coalesced
   per endpoint to at most one every 60 s; a set that flaps waits. With deltas, a change costs each peer one
   small `update` entry.
3. *Deliver.* Each peer entry of the signed configuration gains `endpoints` (omitempty) beside `identity` and
   `scopes`. An entry without endpoints serializes as today, so the delta digest vectors stay valid. Only peers
   the policy already lets the endpoint reach or be reached by receive them, as for every other field. LAN
   addresses are visible to those peers, as in Tailscale.
4. *Client.* An engine reports when its endpoint set changes (debounced 5 s, at most one report per 30 s) and
   with every renewal. It puts each peer's endpoints in that peer's netmap node (`tailcfg.Node.Endpoints`), so
   magicsock pings them at once. The first contact then no longer requires a relay: a failed PrepareRegional is
   fatal only when the peer has no endpoints. Relay and direct are tried at the same time, and the first to
   answer wins.

**Tests.** (a) Workspace: two engines on one host, no relay candidates, each peer's endpoints in the other's
signed configuration; a native dial succeeds over a direct path within 2 s. Without the endpoints it fails, as
today. (b) Device: two containers on one linux-test-vm-2 bridge with every relay withheld (no regional candidates, or the
relay UDP/TCP dropped); `pb exec` and `pb ping` work both ways and `pb ping` reports direct. (c) PG (maintainer): an
unchanged endpoint report makes no write, and a flapping set publishes at most once per 60 s.

Owners: maintainer (report storage, publish coalescing, signed field), maintainer (client report, netmap endpoints,
first contact without a relay).

#### Scale matrix, end to end per feature (maintainer, from 2026-10-23)

Targets: one machine with 100 concurrent terminals; one shared terminal with 50 viewers; an account with 1,000
concurrent previews/tunnels and 100 browser terminals; the platform at 100k machines. Verdict per cell: **G**
degrades gracefully up to the target (a clear limit error, backpressure or a queue), **A** degrades before the
target, **R** crashes, leaks, storms or lets one user hurt others, **?** not measured yet. A cell is G/A/R only from
a load test or prod measurement (named in Proof). Load runs never touch production; data-plane runs go on windows-test
VMs (linux-fixture host for in-process binaries), server runs on the staging VM (linux-test-vm-4) or pb-test-pg.

**Platform, idle machine cost** (prod 10:10Z, 19 online machines, 10 min): 28,210 requests = 2.5 req/s per idle
machine, so 100k machines ≈ 250k req/s: **R**. Target ≤ 0.05 req/s per idle machine, event-driven.
Re-measured 12:07–12:17Z after maintainer's MaxWait 25 s (waits now 204 at 25.0 s, client-gone logged 499), 17 online
machines, mostly pre-fix builds: 22,281 requests = 37 req/s ≈ 2.2 req/s per machine including the edge's own pulls.
GET /v1/machines 2.3/min per machine (was 15.7), GET /v1/previews 2.0 (was 9.9). Still per machine per minute:
ssh-target 8.9, JWKS 10.7, machine-services 6.8, runtime-observations 6.1, peer config 8.8, certificates 5.2
(maintainer's list, items 5–7). /v1/machines/{id}/terminal-sessions 5.8 comes from `pb` commands (cmd/pb/targets.go),
i.e. device test runs, not a daemon loop: **R → A**, re-measure after the next shared build.

| Finding | Measured | Verdict | Owner, status |
|---|---|---|---|
| Event long-poll never healthy: client header timeout 30 s = server hold 30 s; the client gives up first, so every daemon polls inventory every 15 s | user-daemon waits end at 29,995–29,999 ms; inventory topic moved 0 times in 3 min while 139 refreshes ran | R | maintainer: fixed in e9b79df2 (held waits skip the header timeout, `TestHubStaysHealthyAcrossEmptyWaitsLongerThanTheHeaderTimeout`) + 9e83beed (outage backoff 15 s → 5 min); maintainer: MaxWait 25 s for released clients; maintainer: re-measure prod mix after the next shared build |
| One inventory refresh = own list + managed-SSH re-list + 1 list per team + workspaces + client-configuration + machine-services + preview pages + terminal catalog | ~6.7 GET /v1/machines per refresh, 355/min from 22 machines; avg 791 ms | R | maintainer, in order with counting tests: managed-SSH reuse, team lists on event, completions off the refresh |
| Private-access dial lists every machine of the account | code: tunnel/native_private.go:27 | R (O(dials × machines)) | maintainer |
| Server re-reads topic generations every 5 s per held waiter | staging (linux-test-vm-4, `TestLoadHeldEventWaiters`): 100 / 500 / 1,000 / 2,000 held waits all answered 204, 0 client timeouts, 42 / 183 / 356 / 687 DB tx/s = 0.34–0.42 per wait per s, ~5 goroutines per wait → ≈ 70k tx/s at 100k machines | R at 100k (G to 2,000 on one box) | maintainer |
| Other per-machine polls: ssh-target 8.9/min, machine-services 8.4, JWKS 11.7 (3 fetchers), certificates 6.9, runtime-observations 6.0, peer config 10.2 | prod mix | A | maintainer (peer config: delta rewrite) |
| Idle machine, whole request mix (staging linux-test-vm-4, pb main bc362feb, 2 idle machines, 10 min, no relay node) | ≤ 0.1 req/s per idle machine (100k machines ≈ 10k req/s) | **≈ 0.97 req/s per idle machine** (1,169 machine-side requests / 2 machines / 600 s; 100k machines ≈ 97k req/s). Per machine per minute: machine-services 8.6, runtime-observations 8.2, helper-runtime-policies/resolve 5.4, usage/native 4.0, events waits 3.8 + 3.1, runtime/carrier-attachment 2.9, ssh-host-keys 2.8, ssh-authorized-keys 2.8, peer-network/config 2.1, machine-peer-network/config 1.9, client inventory lists (machines, previews, terminal-sessions, workspaces, e2ee) 1.1 each, JWKS 0.25. F21-18 config/JWKS fixed (was 55 + 56 per minute) | R | maintainer: machine-services, runtime-observations, helper policies, usage, ssh keys onto the event wait or ≥ 5 min; maintainer: carrier-attachment cadence |
| Storage and writes per idle machine (staging linux-test-vm-4, pb main bc362feb, server main afaac96d, 1 h 23:14–00:14Z, ~1.25 machine-hours: stg-m2 + one machine part of the hour; per-table pg_stat deltas, `~/lanew/snap.sh` + `analyze-storage.py`) | bounded storage; ≤ 0.1 row writes/s per idle machine | **storage: bounded** — steady inserts are pruned as fast as written (proof_nonces 1,316 in / 1,332 out, managed_ssh_operations 143 / 117 — one row per ssh-host-keys/authorized-keys poll); net kept growth of an idle machine ≈ a few MB per machine-month; the window's 91.6 MB/machine-month total is one-time enrollment/tunnel rows and audit events of actions (none from idling). **writes: ~2.2 row updates/s per idle machine** (≈ 8,100/h; 100k machines ≈ 220k updates/s): user_machines 2,596/h, machine_service_discovery_policies 1,726, endpoint_edge_assignments 1,245, runtime_carrier_attachments 969, machine_service_snapshots 862, lazy_runtime_owners 862, user_machine_update_observations 431, native_usage_reporting_status 428, peer_network_identities 413, machine_edge_placements 296, machine_node_latency 274. Settled idle request rate ≈ 0.58 req/s per machine After maintainer's fixes 7a425fab, 8d9da5fa, 821f3c61, 0bc918ff (staging server 0bc918ff, 1 idle machine, 30 min 01:06–01:36Z): machine updates **~8,100 → ~2,400 /h (0.67/s)**: user_machines 2,080→220, discovery policies 1,380→0, service snapshots and lazy owners 690→108 each, update observations 345→54, usage status 345→92, latency 220→8, placements 237→120; unchanged: carrier attachments 696 and endpoint assignments 572 (edge observations refresh every 10 s because readers require observed_at within 15–30 s), peer_network_identities 316 (config_generation per config build). Found and fixed after: tunnel health rewrote every tunnel every ~5 s (1,440/h, a2ebc52a), diagnostics refresh (9bc83192); re-measure pending. Also per machine: proof_nonces ~1,100 ins + 1,100 del /h (one per signed request, F21-13). Each fix has a PG test that unchanged input keeps the row's xmin | R (0.67/s, target 0.02/s) | maintainer: re-measure; maintainer: readiness windows (observation refresh 10 s → 60 s needs readers at 2 min); maintainer: config_generation time-derived (base·2^41 + unix-ms, monotonic across instances); maintainer: proof nonces (UNLOGGED or in-memory) |
| Edge pulls endpoint/carrier admissions every ~300 ms per edge node, full snapshot | 3.4 + 2.8 req/s; size grows with previews | A | maintainer: true delta (changed_at cursor, 10 s overlap, removals), full snapshot every 2 min; reports snapshot bytes at 1k/10k |
| Server has no request admission cap, no WriteTimeout/IdleTimeout; pgxpool 20 conns (1 pinned by LISTEN), no acquire timeout | code | ? (storm test on staging) | maintainer |
| Machines per vCPU of one server box (server + PostgreSQL), daemon refresh mix | staging (linux-test-vm-4, 4 vCPU, `TestLoadIdlePollMix`, main with a1382712, warm DB, 300 s refresh as after maintainer's event-stream fix): 100 / 250 / 500 / 1,000 / 2,000 daemons → **0 failed** at every step; host busy 0.17 / 0.30 / 0.56 / 1.13 / 1.93 vCPU = 589 / 846 / 898 / 885 / 1,036 machines per busy vCPU; p99 flat (machines 45–56 ms, previews 325–401 ms, others ≤ 57 ms); 2,822 DB tx/s at 2,000. At the 15 s refresh (pre-fix) 50 daemons of one account already hit the list limits. Scope: the refresh mix of one 100-machine account; held event waits (≈0.35 tx/s each, row above), peer-network config and edge pulls not included | G to 2,000 (≈1,000 machines per busy vCPU for this mix) | maintainer: whole-machine mix incl. waits and peer config, then 3 instances |
| First machine list of a fresh 100-machine account | < 1 s | address leases allocated inside the list query under a table lock (`ensure_control_sync_lease`) | 29–33 s on staging (warm-up list, three runs); every other list of the account gets the 10 s busy answer meanwhile; ~0.3 s per machine → ~5 min at 1,000 | R | maintainer: allocate at enrollment, no table lock |
| Horizontal scaling: 3 server instances behind the proxy on one PostgreSQL; kill the worker leader mid-test | requests and held waits continue on the others within the clients' backoff; workers run once; no daemon degraded | staging (linux-test-vm-4, main with F21-04 + 1a137fb0, `TestLoadFailover` via `tools/loadtest/run-multi-instance.sh`, 200 daemons with held waits + refresh mix): leader killed at t=30 s → 65 failed requests inside a **1 s** window, then 5,907 successes, none failing; lease row moved from the killed instance (last heartbeat 22:55:29) to a standby at 22:55:32 (**~3 s** takeover) and only ever named one leader. 1 vs 3 instances (2026-10-24, main 8a86ac5d, `TestLoadIdlePollMix` 300 s refresh via `run-multi-instance.sh`, same box with staging running = 0.30 vCPU idle, subtracted): 250 / 500 / 1,000 daemons → 1 instance 1.15 / 1.12 / 1.50 busy vCPU (217 / 446 / 667 machines per vCPU), 3 instances 1.28 / 1.18 / 1.45 (195 / 424 / 690); 0 failed in both; PostgreSQL CPU the same at both (~45 / 49 / 65 %); 411 leader samples, never 2 leaders. Three instances add no measurable CPU on one box; the per-box capacity gain of more boxes needs a second VM (Postgres at 65 % of one core at 1,000 daemons is the next limit) | G (failover, 1 vs 3 overhead) | maintainer: second-box run when a VM is free |

**Terminals (native, replay/reconnect, shared, browser)**

| Dimension | Target | Bound in code | Measured | Verdict | Owner, status |
|---|---|---|---|---|---|
| Sessions per machine | 100 | one host ceiling 256 (b9ebbc8d, `TestRouterAttachesToEverySessionAtTheCeiling`); server count: maintainer | F3-05 round 12 before the fix; device recheck in maintainer round 16 | A | maintainer: client closes a refused server row, error names the ceiling; maintainer: drop the server count |
| 100 PTY sessions + output, echo latency | 100 | — | on main (3f068585): created in 0.45 s, echo p50 3.4 ms p95 6.2 ms max 18 ms (`TestLoadHundredSessionsFiftyViewers`, linux-fixture) | G | — |
| Close after an output burst | every close works | host store: one connection, IMMEDIATE (2d8f3fc8); group-committed output every 50 ms, history ring as the only backlog with replay gaps, close waits ≤ 2 s (3f068585); closed sessions release output buffers (ba37e014) | 20 / 50 / 100 sessions: close all + shutdown 0.15 / 0.44 / 1.0 s, echo p95 8.2 / 5.1 / 8.6 ms, heap after flood 20 / 45 / 97 MiB, heap after close 3.1 / 4.2 / 5.3 MiB (baseline 2.1–2.3, two GCs), no goroutine/fd leak (`TestLoadHundredSessionsFiftyViewers` on main, linux-fixture) | G | — |
| Viewers on one shared session | 50 | host 128 attachments/session; edge 100 participants, 512 KiB/viewer queue; guests 64 | host: 32 MiB to 50 viewers in 1.6 s (19.8 MiB/s each); edge hub at 20 MiB/s: p99 0.7 ms, 0 of 50 readers cut off (`TestLoadBrowserTerminalHub`, linux-test-vm-3) | G | — |
| Slow viewer | evicted, others unaffected | host: evict at 1 MiB pending; edge: subscriber dropped | stalled viewer evicted on host and edge, other viewers' rate unchanged | G | — |
| Replay ack map per attachment ID never pruned | bounded | history/history.go acks | code | A (grows with reconnects for the session's life) | maintainer |
| Browser terminals per account | 100 | edge 4,096 hub sessions per node, 128 E2EE connections per host | edge hub, 100 terminals at 2 MiB/s + 50 viewers on one: p99 2 ms, no reader cut off (in-process; network path still ?) | G (hub) | maintainer: network path |
| Reconnect storm after server deploy | all sessions back < 30 s, no 5xx | — | ? | ? | maintainer (staging) |

**Exec, transfers, SSH**

| Dimension | Target | Bound in code | Verdict | Owner |
|---|---|---|---|---|
| Concurrent exec per host | 32 then clear error | execprocess 32 → resource_limit | 32 × 4 MiB output in 2.2 s; 33rd refused in 27 µs (ErrCapacity); 2,000 short execs at 288/s, p99 146 ms, 0 failed; no leak (`TestLoadExecConcurrencyAndChurn`, linux-fixture) | G |
| Concurrent exec, one client → one peer, over the network | 32 then clear error | client peer stream; server team write lock (08be2089) | before 08be2089: 25/40 and 35/40 ok, `transport_unavailable` "outcome may be unknown". After (server 2026.10.23.63): **40/40 ok twice**, but `sleep 20` takes 23–53 s (p50 35 s); server 2026.10.23.74 (S2, no team lock): **39/40 ok**, 1 local-service error at 3.8 s (support_510fa5e9), setup p50 16.6 s, p90 26.9 s, max 29.9 s (was p50 ~15 s / max 33 s) | A (no failures; setup up to 33 s under concurrency) | maintainer: setup latency |
| Concurrent transfers per host | 2 then queue | MaxConcurrentTransfers 2 | 6 parallel 10 MiB `pb send` one client → one peer: 6/6 ok in 11 s | G | — |
| Managed SSH streams per host | none (removed, user decision 2026-10-26) | no Paperboat cap: the machine's sshd MaxStartups/MaxSessions decides, as Tailscale and NetBird; a connection sshd ends before its banner prints pb's ErrSSHServerClosed text | before removal: 40 parallel `pb ssh` → 32 ok / 8 refused with only OpenSSH's "Connection closed"; setup since fixed (F5-07, warm p50 0.5 s) | removed | maintainer |
| Relay connections per account | 1,000 machines | derpquic 16 per account, 256 per relay node; peer relay 16 allocations per account | relay nodes take 1,000 machines of one account (825c0730, 4cf26e35); grants cover every peer in chunks of 16 (1ee03b1e, server 2026.10.23.66): prod's `kept=16` warning went from ~212/min to 0 | G for current CLIs (released CLIs merge at most 16 until they update) | maintainer: caps from fd/memory, fairness from byte budgets, "relay full" → next node; tests: 1,000 registrations of one account, one account's flood doesn't slow another |

**Previews, tunnels**

| Dimension | Target | Bound in code | Verdict | Owner |
|---|---|---|---|---|
| Previews per machine | — | PLAN 3a endpoint engine (old preview.Registry has no production constructor: dead code, not a leak) | ? | maintainer / maintainer |
| Previews/tunnels per account | 1,000 | server lists paged (200); completion lists page through all previews on every refresh | R (with the refresh storm) | maintainer |
| Streams per tunnel carrier | — | datacarrier 64 streams default, 1,024 max | ? | maintainer |

**Console** (code read, lib/api.ts, lib/tunnels.ts, lib/terminal/queries.ts)

| Dimension | Target | Bound in code | Verdict | Owner |
|---|---|---|---|---|
| Previews/tunnels tables, running counts | 1,000 per account | `allPages()` re-pulls up to 10 × 200 every 10–30 s per tab and silently stops at 2,000; terminals list one page of 200 | A (wrong counts above the cap; ~2 req/s per idle tab at 1,000) | maintainer: PQ-04 cursors + running counts; maintainer: show "first N of total" meanwhile |
| Participants of a shared terminal | 50 viewers | every viewer's tab polls `/sharing` every 4 s → 12.5 req/s per shared terminal | A | maintainer: participants event; maintainer: poll ≥ 60 s after |

**ENV, config sync, teams/audit/usage**: bounds listed (ENV 128 records/batch, 256 KiB live; config sync
5 MiB/file, 1 min remote poll; team members 128; audit export 1M rows); load tests after staging is up: **?**.

#### Robustness gaps vs reference implementations (maintainer, 2026-10-23)

Compares the structure of Tailscale (tailscaled, controlclient, derp, magicsock, health), Headscale (hscontrol) and
NetBird (engine, management) with ours. Ranked by blast radius × likelihood. Code reading, each claim re-checked
in our source; "verify" = not yet proven by a test. Already in Worktrees Q/R/W rows above (deltas, polling and outage
backoff, jitter, relay caps, retention, admission cap) is not repeated. TS = ~/ref/tailscale, HS = ~/ref/headscale.

| # | Gap | Ours | How the reference does it | Impact | Owner |
|---|---|---|---|---|---|
| 1 | Data plane dies with the control plane: the signed peer network expires 2 min after issue (cap 5 min) and the daemon then refuses every peer | `server/internal/admission/issuer.go:11`, `config/config.go:678`; `cli/internal/peertransport/tailnet/authority.go:500` | TS keeps using the last netmap when control is unreachable and only warns "Out of sync" (`health/warnings.go:122-130`); node keys last months; revocation is pushed | server outage, bad deploy or DB stall > 2 min stops all native exec/ssh/terminals/private access, even between peers that already trust each other | User decision 2026-10-23: **until revoked** (keep last verified state with no time limit; only a received revocation or key expiry ends access; "out of sync with Paperboat" warning). B (contract) + R (daemon). CHECKLIST F21-01 |
| 2 | Clock skew turns a machine dark: proofs need the client clock within ±60 s, the client checks server `exp` with no leeway or offset, and the 401 trips the machine-wide breaker | `server/internal/controlplane/machine_proof.go:135`; `httpapi/machine_events_handler.go:54` (401); `cli/internal/hostruntime/controlbackoff/controlbackoff.go:75-77,121-129`; `cli/internal/resolver/api.go:375` | TS keeps `clockDelta = ControlTime − now` and uses it for expiry (`ipn/ipnlocal/expiry.go:38-41`); its Noise transport has no per-request timestamp | a laptop or VM whose clock drifts > 1 min loses every control call until the clock is fixed; no message says why | B (server time in every response; a distinct `clock_skew` error, not 401) + A (offset from server time, used for proofs and expiry checks) |
| 3 | No database time bounds: no `statement_timeout`, `lock_timeout` or `idle_in_transaction_session_timeout`; pool 20 hardcoded, no acquire timeout | `server/internal/db/db.go:62-71` | HS pool size and timeouts in config (`hscontrol/db/db.go:489-493`) | one stuck query or lock queue holds connections until the 20-slot pool is gone and every request waits (a test INSERT spun 5.5 h on 2026-10-23) | Q (adds to maintainer's pool row) |
| 4 | A retired server keeps running every background worker with its old code next to the new server for as long as any long-lived connection stays open; no single-writer lock; migrations "assumed additive" with no check (46 non-baseline files use DROP COLUMN; 0 CREATE INDEX CONCURRENTLY) | `deploy/rollout-server.py:81-97,171`; `server/internal/app/app.go:897-1014`; `server/internal/db/migrations/` | HS/NetBird run one instance that migrates at start; common pattern: drained instance stops writers, expand-then-contract migrations | two versions reconcile billing, certificates, DNS, edge sets at once; a contract migration breaks the old server mid-drain | B |
| 5 | Request deadline bypass by header: any request with `Accept: text/event-stream` or `Upgrade: websocket` skips the 15 s timeout on any path; the server sets no Read/Write/Idle timeout or MaxHeaderBytes | `server/internal/httpapi/router_02.go:50,102-118`; `server/internal/app/app.go:1045-1049` | HS sets server Read/WriteTimeout and extends the deadline only on its long-poll route (`hscontrol/app.go:737-741`, `poll.go:103`) | a few clients can pin handlers and connections without limit | B |
| 6 | Health is computed and dropped: transport observation publisher, regional failover status and the health tracker have no reader; `pb status`/doctor show path, relay and MTU as "unknown" | `cli/internal/localapi/client_unix.go:611` (0 callers); `cli/internal/peertransport/tailnet/regional_recovery.go:296` (0 callers); `cli/internal/localdaemon/inventory.go:404-412` | TS `health.Tracker` Warnables feed `tailscale status`, the GUI and bug reports (`health/health.go`, `health/warnings.go`) | users and support cannot see relay-only, UDP-blocked or failover states; bug reports lack them | A (status/doctor/bugreport) + R (producers in peertransport) |
| 7 | A second, unused control channel: gRPC Sync (full snapshot every 5 s, forced close every 5 min that withdraws all routes on the client, no panic interceptor so a handler panic kills the server); not enabled in prod (`PAPERBOAT_SYNC_ADDRESS` unset) | `server/internal/sync/server.go:27`, `sync/service.go:23`; `cli/internal/controlsync/client.go:139-153,238-242` | TS: one map stream, server keepalive, client watchdog (`control/controlclient/direct.go`); HS: per-node channel + batcher | dead code with latent route-flap bug; "one way to do each thing" | A + B: delete both ends (or make it the one stream, by design decision) |
| 8 | Signing-key revocation is not enforced by the issuer itself: revoked kids stay valid in-process; keys come only from env, rotation = restart | `server/internal/mint/mint.go:871-900` (no revocation check), `mint.go:573,606`; `controlplane/edge.go:392` | TS key authority makes removal a signed state change (`tka/aum.go`) | after a key compromise, tokens verify on the server until restart; edges: verify they honor `key_ids` | B |
| 9 | Shutdown: workers stop before HTTP; `/readyz` never reports draining; 10 s shutdown < 25 s event waits, so waiters get a TCP reset, not a reconnect hint; compose has no `stop_grace_period` | `server/internal/app/lifecycle.go:174-198`; `config/config.go:404`; `machineevents/events.go:41` | HS stops HTTP first, then the batcher (`hscontrol/app.go:905-969`); DERP sends `FrameRestarting` with a retry window (`derp/derp.go:128-133`) | restarts outside rollout-server.py (crash, `compose restart`) drop every waiter at once → herd | B |
| 10 | Bulk UPDATEs still fire row-level topic triggers (migration 141 fixed INSERT only): team deactivation, access-session revoke, service revoke upsert the same generation row N times in one tx | `server/internal/teams/service.go:289`; `operatoractions/service.go:228`; `machineservices/service.go:174`; `db/migrations/110_machine_topic_generations.sql:18`, `141_…:1-20` | TS/HS compute changes in process and batch them (`hscontrol/mapper/batcher.go`) | O(N × audience) writes and lock contention per bulk change | Q (extends maintainer #9) |
| 11 | `BeginSerializable` callers bypass the bounded 40001 retry of `InTx`, incl. native private access on every request | `server/internal/nativeprivateaccess/machine_service.go:23`; `machineservices/service.go:153`; `db/db.go:133-174` | — (pattern: every serializable tx retried) | serialization failures under load surface to users as errors | Q |
| 12 | Client state corner cases: `hostd.token` written in place (crash leaves a 0-byte token that re-install keeps); older `pb` re-saves config without keys it does not know; a newer prefs or inbox-journal version blocks every command/delivery with no reset; config-sync `.lock` left by SIGKILL is never cleared; auth lock trusts a reused PID; inbox journal has no cross-process lock | `cli/internal/hostruntime/hostinstall/install_state_unix.go:115`; `cli/internal/config/config.go:580`; `cli/internal/preferences/preferences.go:166,264`; `cli/internal/inbox/inbox.go:345,635`; `cli/internal/configsync/repository_local.go:208`; `cli/internal/config/auth_lock.go:124` | TS state store keeps opaque per-key blobs (unknown keys survive, `ipn/store/stores.go:192-226`), empty file = missing, atomic writes everywhere | rare but sticky failures that need a manual file delete | A |
| 13 | Path liveness and MTU: when no relay works, a dead direct path is noticed only by the 2 min QUIC idle timeout; MTU fixed at 1232; the PMTU prober has no production caller | `cli/internal/peertransport/peerquic/session.go:44-45`; `mesh/packet.go:25`; `networkadaptation/pmtu.go:69` | TS magicsock heartbeat 3 s, trusts a UDP path 6.5 s (`wgengine/magicsock/magicsock.go:4084-4088`), peer MTU probing | streams hang up to 2 min after a network change; dead code | R |
| 14 | Advertised home relay region is the alphabetically first eligible region, not the selected relay (verify wire effect) | `cli/internal/peertransport/tailnet/regional_recovery.go:525-529`, `tailnet/regional.go:193-198`, `mesh/regional.go:45-73` | TS home DERP from netcheck latency (`net/netcheck/netcheck.go`) | peers may try the wrong region first | R |
| 15 | Rate limits exist only on device auth; operator login lockout is per email only (lockout DoS), password hash under `FOR UPDATE` | `server/internal/auth/device.go:711-721`; `operatorauth/service.go:205-210` | NetBird per-peer escalating login filter and API rate limiter; DERP per-client token bucket (`derp/derpserver/derpserver.go`) | one client or account can consume shared capacity; operator account can be locked out | B (with maintainer's admission row) |
| 16 | Mutations without idempotency: `POST api-tokens` retried after a lost response returns 409 and the once-shown secret is lost; machine-proof requests have no nonce, replayable for 60 s (connector proofs do reserve nonces) | `server/internal/httpapi/api_token_handlers.go:116-145`; `controlplane/machine_proof.go:110-137` vs `connectorprotocol/sql_store.go:236-273` | TS Noise sessions (fresh ephemeral keys per handshake, `control/controlbase/handshake.go`) | lost tokens on flaky networks; replay needs TLS capture + bearer (low) | B |
| 17 | Client-version gate reads the DB on every gated request with no cache; a DB blip returns 503 for all of them | `server/internal/httpapi/client_update_policy.go:114-140`; `clientupdate/service.go:175-177` | — (pattern: cached policy, last good value) | DB hiccup amplifies to a full API outage for official CLIs | B |

Checked and fine: atomic writes with dir fsync (`cli/internal/atomicfile`, stronger than TS), host state backup/repair,
self-update rollback, body limits on every route, HTTP and worker panic recovery, bounded `InTx` retry, migration
advisory lock, blue/green rollout with connection-count drain, gRPC/event reconnect re-reads generations (no lost
change after a deploy).

#### Offline-capable authority: "until revoked" and new sessions without the control plane (maintainer server, maintainer daemon, maintainer review; user decisions 2026-10-23)

Agreed by maintainer and maintainer, reviewed by maintainer (2026-10-23, conditions folded in). Server first, after
the peer-network delta work (contract v3) it extends.
1. **Peer state lives until revoked (F21-01).** `machine_network` mesh scopes carry no `expires_at`. The document's `exp` becomes
   `refresh_by`, a hint: past it the daemon keeps using the last verified state while control is unreachable and warns
   "out of sync". Access ends only by (a) a newer state without the peer or a `remove` delta, (b) the host revocation
   document, (c) the peer identity's own `not_after` (≤ 90 days), which every peer entry carries and the daemon enforces.
   Per-operation scopes (machine_access, inspector, config_compare) keep their short `expires_at`.
2. **Session rights are scopes on the peer entry (F21-11).** A peer endpoint that may start sessions on this host gets scopes
   `{resource_kind: "machine_access", capability: terminal|exec|managed_ssh|files, direction: "accept"}` with no
   `expires_at` and `resource_generation` = the deciding policy version. No parallel list: the v3 delta, digest, paging, visible-peer bound and removal-first rule apply unchanged. The
   server compiles them with the same functions that issue descriptors (`machine_capability_allowed`, team policy) and
   signs them in the same document. No secrets, no ENV.
3. **Offline admission on the host.** Online nothing changes: descriptor plus stream token, and an online refusal is final.
   "Unreachable" is only a connect failure, a timeout, or a 503 from the API origin itself; a 5xx from one endpoint while
   others answer counts as online. The server never answers a denial as a 5xx (test: every descriptor denial is a 4xx).
   Offline, the client sends a new stream-header kind on the existing native session, `offline_admission {capability,
   operation_id, nonce}`; that session already proves the peer (mutual QUIC TLS with the endpoint certificate inside
   WireGuard keyed by the signed peer state), so the host trusts nothing the client presents. The host decides with the
   same scope check as online admission (`Allows(peer, self, capability, "accept")`): one verifier, no second signature
   format next to the server-signed stream token.
4. **What offline does not skip.** The client chooses when it is offline (it can block the API), so each server-side limit is
   decided: session count, bounded by the host's own ceiling (256); bandwidth and plan gates apply only to hosted relay and
   edge bytes, so direct sessions are unaffected and relay grants follow point 7; per-operation approvals and step-up
   (policy requiring them) mean the capability is not granted offline at all, scope withheld; ENV, pairing, browser
   terminals, previews and tunnels, and policy edits are not offline and say "needs Paperboat online". Offline usage is
   in the audit backlog so billing and audit can count it.
5. **Audit fails closed.** The host records each offline session start and refusal, signed with its machine key, at most
   10 000 events or 7 days, and uploads on reconnect to `POST /v1/machines/{id}/audit-backlog`. The server dedups by event id
   and records `origin=offline` in the deciding workspace's log (F19-07). When the backlog is full, new offline sessions
   are refused with "needs Paperboat online"; nothing is dropped.
6. **Every way access ends changes the document, in one place.** A single service function (also used by triggers) bumps the
   state of both endpoints, each change with a PG test: machine revoke; member removal; team suspension; policy change that
   removes a scope or adds an approval/step-up requirement; CLI session ended or logout (peer endpoints include native CLI
   sessions, peersessions/network.go:369-397); account disabled or deleted; SSO session revoked or SCIM deprovision (F19);
   seat or plan change that drops a capability. On reconnect the first answer carries removals first. The host revocation
   document stays the second path; its unfiltered LIMIT 10000 (maintainer #3) is fixed first.
7. **Monotonic across restores.** Versions are `(issuer_epoch, generation)`: the daemon refuses a lower pair, so old state can't
   be replayed, and accepts a snapshot under a higher epoch. Restoring a database backup bumps the epoch
   (deploy/verify-backup.sh restore path); test: restore, refresh, daemon recovers. Relay grants follow the same rule: the
   relay keeps its last grant while unreachable and drops it on revocation.
8. **Tests.** PG: a revoke issued while a host is offline is in its first answer after reconnect; session scopes equal the
   descriptor decision for every (account, capability) on a seeded team; each item in point 6; every descriptor denial is
   a 4xx; restore-then-refresh. Contract vectors for v3. Daemon (maintainer): fake clock 1 h past `refresh_by`; removed peer
   refused; replay refused; offline session accepted by a scope, refused for a revoked account and when the audit backlog
   is full; unreachable only on connect failure, timeout or origin 503. Device (F21-11, staging only, never prod): stop
   the server, then a new exec, ssh and terminal between two paired machines work and a revoked user is refused.

#### Account deletion (GDPR erasure; maintainer server, maintainer console, maintainer CLI link; user request 2026-10-23)

Console only. `DELETE /v1/account` accepts only a browser session that signed in again within the last 5 minutes,
with body `{"confirm_email": "<the account email>"}`. CLI sessions and API tokens get 403 `account_deletion_console_only`
with the console URL; a stale sign-in gets 401 `reauthentication_required`; a wrong email 400 `confirmation_mismatch`.
`GET /v1/account/deletion-preview` lists what will go: machines (count), personal ENV vaults, previews, tunnels,
config sync, inbox items, API tokens, an active subscription, and teams: the ones deleted with the account (it is
the only member) and the ones that block it (it owns the team and others are members).
Refusal: 409 `account_owns_team` with `{teams: [{slug, members}]}` while the account owns a team with other members
(transfer ownership or delete the team first). Otherwise, in one transaction and irreversibly: every machine is
revoked (hosts get the revocation, generations bump for every peer, F21 rules); all browser and CLI sessions, refresh
tokens and API tokens end; ENV values and personal vaults are deleted; personal workspace, previews, tunnels, config
sync, inbox, lazy access and peer identities are removed; single-member teams the account owns are deleted. After
commit, best effort with retries (an outbox, not the request): delete the WorkOS user, cancel the Stripe subscription
(invoices and payment records stay, legally required), send a confirmation email (Resend). Team audit entries keep the
event with actor "deleted user" (no name, email or id); billing and usage rows keep only what tax law needs (amounts,
dates, workspace id), with the account's email and name removed. The email may sign up again later as a new account.
`pb account delete` prints the console link only; there is no CLI deletion path and no --yes.
Tests (PG): each removal; the owner refusal; console-only refusals (CLI session, API token, stale sign-in, wrong email);
audit anonymization; the outbox jobs (WorkOS, Stripe, mail) retry and never block the deletion.

#### PlanetScale Postgres readiness (maintainer, 2026-10-23; user decision: production DB moves to PlanetScale at launch)

Source: planetscale.com/docs/postgres (compatibility, connecting, pgbouncer, connection-resilience, parameters,
imports, plans/regions), read 2026-10-23; our code at main. Today: Hetzner Helsinki, PostgreSQL 17.11, 373 MB,
26 connections, one pgxpool (`server/internal/db/db.go:62-71`: MaxConns 20, lifetime 30 min); tunnel and relay
have no database. **No blocker**: every feature we use is supported. Six changes are required before cutover.

| What we use | Where | PlanetScale | Connection |
|---|---|---|---|
| LISTEN/NOTIFY (machine-events hub; 2 `pg_notify` in triggers) | `machineevents/events.go:145-155` | supported; not through PgBouncer | **direct** (NOTIFY in triggers works pooled) |
| Session advisory locks: migrations, team identity sync, tunnel cert address slots/hosts, planned F21-04 leader | `db/migrations.go:26`, `teamidentity/sync.go:31`, `tunnelcert/address_reconciliation.go:102,153,308` | supported; not through PgBouncer | **direct** |
| `pg_advisory_xact_lock` (32 uses), SERIALIZABLE + 40001 retry, `SET LOCAL` (seqscan, search_path, collapse limits) | `db/db.go:133-192,301`, `connectorprotocol/ingress_projection.go:220` | supported, transaction-scoped | pooled |
| Row and statement triggers incl. transition tables (121 CREATE TRIGGER), plpgsql/sql functions, jsonb, inet, deferrable constraints, views, LOCK TABLE / DISABLE TRIGGER on our own tables in migrations | `server/internal/db/migrations/` (135 files, 001-152) | "all trigger types", no superuser needed (owner only) | pooled; migrations direct |
| Extensions | none (gen_random_uuid is core) | curated list; none needed | — |
| Sequences | 204 uses | pg_dump carries setval; logical replication does not | — |
| Roles/grants/superuser | none in migrations or code | no SUPERUSER; default role near-superuser; app should use its own role | — |
| Startup params `search_path=paperboat`, `jit=off` | `db/db.go:66,69` | PgBouncer accepts only tracked params (`ignore_startup_parameters` default `extra_float_digits`) | **change** |
| Named prepared statements (pgx default cache mode; 683 sqlc queries + raw SQL) | `db/db.go` (default `QueryExecModeCacheStatement`) | PgBouncer tracks them, `max_prepared_statements` default 200 | **change** |

**Required changes** (owner maintainer unless noted; verify each with a test connection to a PlanetScale branch):
1. **Two pools.** Pooled DSN (port 6432, dedicated primary PgBouncer for failover resilience) for queries. A small
   direct pool (5432, about 4 connections per instance) for LISTEN, every session advisory lock and the migrate
   command. Through PgBouncer these fail silently: the unlock runs on another server connection, so the lock leaks
   on a pooled connection and other instances never get it; LISTEN never wakes the hub (falls back to its 5 s recheck).
   Test: a CI check that every `pg_advisory_lock`/`pg_try_advisory_lock`/`LISTEN` runs on the direct pool.
2. **Role defaults instead of startup params.** `ALTER ROLE <app> SET search_path = paperboat, jit = off` plus the
   F21-03 timeouts (`statement_timeout`, `lock_timeout`, `idle_in_transaction_session_timeout`; PlanetScale's guide
   recommends exactly this); drop `RuntimeParams` from the pooled DSN.
3. **Prepared statements.** Raise the bouncer's `max_prepared_statements` to ≥ 1024, or switch pgx to
   `QueryExecModeCacheDescribe`; measure p50 on a branch (to verify which is cheaper).
4. **Pool sizing.** PgBouncer `default_pool_size` (20) is per user/database for the whole bouncer and `max_client_conn`
   is 100: size them for instances × MaxConns during blue/green (3 × 20). Set `query_wait_timeout` (default 120 s)
   to about 5 s so overload fails fast within our 15 s request timeout (maintainer: fold into the F21-03 pool row).
5. **App role owns the schema.** Restore with `--no-owner` as the app role (not the default `postgres` role), so
   migrations (ALTER/DDL need ownership) and `DISABLE TRIGGER` keep working.
6. **Failover behavior.** Planned failovers cut direct connections (a few seconds; PgBouncer PAUSEs pooled ones).
   The hub reconnects already (24 s LISTEN sessions). A holder of a session advisory lock must treat a dropped
   connection as a lost lock (teamidentity, tunnelcert, F21-04 leader stops its workers). Keep transactions < 3 s and
   connections < 24 h (ours: 30 min).

**Region (latency per query).** No Nordic region; nearest are AWS eu-central-1 Frankfurt and eu-west-1 Dublin,
GCP europe-west4/west1, Azure westeurope. Measured TCP connect from the current server (Helsinki): Frankfurt
30 ms, Dublin 44 ms, us-east-1 106 ms. At maintainer's query counts (e.g. 105 queries for terminal sessions), 30 ms is
about 3 s per request: **the server must move with the DB.** Options: Hetzner Nuremberg/Falkenstein next to AWS
eu-central-1 (a few ms, to verify by measuring from a Hetzner DE host), or the server on AWS eu-central-1 itself
(sub-ms in-region). maintainer's per-request query budget becomes a latency budget (queries × RTT).

**Migration path.** 373 MB, so pg_dump/pg_restore (PlanetScale: < 20 GB, downtime up to an hour; ours is
estimated at 10-15 min, to measure). It copies sequences, and triggers are created after the data, so COPY fires
none. Logical replication is not worth it: it needs `wal_level=logical` (a restart on Hetzner), does not carry
sequences or DDL, and needs a frozen schema during the sync. Steps: (1) rehearse on a PlanetScale branch: restore,
run the PG suites and `TestPerformanceBudgetsOnSeededPostgres` against it from the new server region; (2) **F21-01
first**: without it, a DB window longer than 2 min stops native access fleet-wide; (3) maintenance window: stop
the server, final dump, restore, ALTER ROLE defaults, switch both DSNs, start, then the smoke checks; (4) keep the
Hetzner DB read-only for rollback for 7 days. Deploy changes (maintainer): compose drops the postgres service;
`backup-database.sh`/`verify-backup.sh` move to PlanetScale backups/PITR (or run pg_dump over the direct port);
the migrate service uses the direct DSN.

#### Full codebase audit (maintainer, from 2026-10-23; replaces the narrow DB review, which is folded in as D1–D10)

Every product package checked against one fixed list and against the matching subsystem of Tailscale, Headscale, NetBird, Teleport, and for tunnels, previews and
edges cloudflared (supervisor, connection, ingress, QUIC/HTTP2, metrics), zrok (OpenZiti shares, frontends, limits,
metrics) and ngrok-go (agent session, reconnect, heartbeat) (references shallow-cloned in ~/ref). **Checklist:** (a) growth and retention, (b) fan-out and N², (c) polling,
timers, retries, (d) queries per request, indexes, transaction scope, (e) memory/goroutine/fd bounds, (f) timeouts and
cancellation, (g) failure, idempotency, restart, (h) locks and races, (i) wire size, full vs delta, (j) input limits, rate
limits, fairness, (k) duplicate paths and dead code, (l) data model. A finding is listed only after maintainer has re-read it
in our code; "verify" marks the rest. Ranked by impact at 100k users × frequency; each goes to its owner worktree; fixes worth
doing before launch become F21 rows after the coordinator agrees. **Coverage: 308 / 433 packages done (71 %)** (inventory
2026-10-23 at 6a290a9d: 311 Go packages, 490k non-test lines; 122 console areas, 59k lines; 6 test-only packages).

**Findings so far** (DB review, measured on prod catalog statistics since 2026-10-14 plus counts by expiry; plans on the
seeded pb-test-pg database). Proposed policies are deletes in bounded batches (`LIMIT 500`, `FOR UPDATE SKIP LOCKED`) by
an existing worker.

| # | Finding | Evidence | Fix (owner) |
|---|---|---|---|
| D1 | Per-operation tables nothing prunes: `control_config_credentials` (TTL 2 min; 0 live, 10,678 expired, ~2.6k/day), `control_config_repository_access_operations` (0 live, 8,346 expired), `managed_ssh_operations` (94k idempotency rows, ~10k/day), `operations`, `control_usage_receipts`, `control_config_sync_status_history`, `helper_identity_renewals`, `cli_access_tokens`/`cli_refresh_tokens` | no `DELETE FROM` for any of them outside migrations (grep of server/internal) | credentials and repo access: delete 1 h after expiry once provider revocation is done; idempotency rows (managed SSH, operations): 24 h after a terminal state; tokens: 1 day after expiry or revocation has propagated; usage receipts: after acknowledgement + 2 days; status history: newest 100 per environment. Extends maintainer #16 (Q) |
| D2 | Encrypted credentials kept long after use: 1.24 KB GitHub credential ciphertext per row (most of the ~2.7 KB/row, plus a dead copy from the revoke update), 533 B repo-access ciphertext; `jti` and `jti_hash` both unique-indexed, the `jti_hash` primary key never scanned | pg_stats avg_width; `pg_stat_user_indexes` | null the ciphertext at expiry, then D1's delete; keep one key column (B) |
| D3 | The revocation document full-scans `control_config_credentials` (543k scans, avg 3,225 rows each) and `user_machine_access_sessions` (427k scans) on every host/edge poll: its predicate `revoked_at IS NOT NULL AND expires_at > now` has no credential index | `queries/control_plane.sql` ListRevokedControlCredentialJTIs; prod stats | D1 removes the rows; partial index `(expires_at) WHERE revoked_at IS NOT NULL`; goes with maintainer #3 (Q) |
| D4 | Unconditional upserts rewrite unchanged rows: `runtime_carrier_attachments` 7.3M updates on 615 rows (~9/s), each rewriting the `admission` JSON with the edge certificate chain PEM (6 MB TOAST); `tunnel_platform_certificate_targets` 3.2M on 2 rows (~4/s); `machine_edge_placements` 2.9M on 313; `control_tunnel_nodes` 790k on 4 rows with 0 % HOT (12.8k autovacuums) | `runtimeattachment/service.go:258`, `edgeplacement/placement.go:87`, `queries/tunnel_certificates_v1.sql:720`, `queries/control_plane.sql:1039` | `DO UPDATE … WHERE (…) IS DISTINCT FROM (EXCLUDED…)`; liveness timestamps in a narrow row or memory; build admission at read instead of copying the edge certificate per row (B). Every write is WAL to two replicas on PlanetScale |
| D5 | Access functions run per candidate row: `teams` seq-scanned 564M times, `billing_workspaces` 349M, `team_members` 212M, `team_resource_bindings` 203M since 2026-10-14, for 36 teams | `pg_stat_user_tables` | set-based access description (maintainer #5, "next"): one join per request, not one function call per row (Q) |
| D6 | One `support_reports` row per error occurrence: ~6.4k/day (45k inside the 7-day TTL) from ~25 machines, ~2.6M/day at 10k machines | 7,312 deletes by the TTL job; avg 146 B error chain | aggregate by fingerprint (component, code, cause, source line) per hour with count and first/last seen, as Sentry does (B) |
| D7 | Indexes: 169 of 604 never scanned (46 non-unique, 7.9 MB); 4 exact duplicates (`account_e2ee_keys`, `github_oauth_tokens`, `release_authority_bundles`, `tunnel_connector_activations`); `audit_events` has 10 indexes, 37 MB of index over 35 MB of heap, 2 never scanned (`account_cursor` 4 MB, `correlation`) | `pg_stat_user_indexes`, `pg_index` | drop duplicates and unused non-unique indexes on write-hot tables after one more week of stats (Q) |
| D8 | `pending_provider_revocations` in GetConfigSyncMetrics and the revoke-by-environment/repository updates can't use the repo-access partial indexes: 762k full scans | index predicates vs queries in `queries/control_plane.sql` | match the partial index predicate, or index `(revoked_at) WHERE provider_revoked_at IS NULL` (Q) |
| D9 | 125 of 423 `*_id` columns have no foreign key (e.g. credentials' environment/assignment, usage receipts, bandwidth events) | catalog | decide per column: per-operation rows get `ON DELETE CASCADE` so a removed machine takes them along; history rows stay keyless on purpose, written in a comment (B) |
| D10 | Text ids with prefixes are 42–48 B in every key and foreign key (uuid: 16 B); indexes are 133 MB vs 207 MB heap | pg_stats avg_width | not worth changing before launch; note for post-launch (—) |

Hot read plans on the seeded database (5k machines, 200k audit events, 50k access sessions): warm planning 0.3–1 ms,
execution ≤ 0.2 ms for the revocation document, the workspace activity page and the bandwidth month sum; the right indexes are used.

**Ranked (impact at 100k users × frequency), top 15:** 1 P1 config + JWKS refetch every 1 s per active client ·
2 S1 one worker's DB error stops the server · 3 T1 edge fails closed on a control outage · 4 S5 presence: 4–6 write tx per
10 s per machine · 5 P4 machine-services poll pinned at 2 s · 6 P3 private TCP routes POST every 1 s per session · 7 C1/C2
console full machine list every 15 s per tab, per-row inbox/domain polls · 8 S2/S7 one global lock, Stripe call inside it ·
9 E2 4 MiB config re-read per connector heartbeat · 10 D1/E4 tables nothing prunes · 11 E1 ACME re-order every second ·
12 P2 full peer status built twice per metered read/write · 13 A1 local snapshot 1 MiB cap vs 10k machines · 14 H2 input
journal lifetime cap · 15 S3/E3 N+1 terminal and preview lists. Tunnel stack vs cloudflared/zrok/ngrok-go (done): T5 descriptor refetch every
4 s per healthy connector ranks with #5; T6 QUIC carriers unmonitored (2 min dead-link detection) with #11; T7–T9 below.
Ours is equal or better on reconnect backoff (full jitter; ngrok-go has none), protocol fallback and origin timeouts.

**Status** (open = briefed, not landed; fixed = commit + test on main, re-read by maintainer; verified = its CHECKLIST row ticked from that test). D1–D10: open, briefed (D2 is maintainer security work). maintainer re-checks main and updates this column as fixes land.

**Findings by package group** (verified in our code by maintainer; ranked list follows when coverage is complete)

| # | Finding | Ours | Reference does | Owner | Status |
|---|---|---|---|---|---|
| C1 | Console refetches the whole machine list every 15 s per open tab: up to 10 pages × 200 + revoked page, cut at 2,000 for a 10k account | `console/lib/api.ts:245-251, lib/machine-pages.ts:6-17` | Teleport UI: server keyset cursor + search; TS: one streamed netmap | C (server: Q for cursor/ETag) | open, briefed |
| C2 | Per-row polling fan-out: one relayed inbox request per online machine every 30 s (1,000 machines ≈ 2,000 req/min per tab); one paged domains query per tunnel every 15 s | `console/components/files/file-list.tsx:58, lib/files.ts:61; components/domains/domain-list.tsx:113` | one list endpoint across machines / per workspace | C + B (endpoint) | open, briefed |
| C3 | Polls that never stop: new preview polls 1 s until "ready", also after failed/expired/stopped | `console/components/tunnels/new-preview.tsx:218-221` | stop on terminal state | C | fixed 50052425 (new-preview stops polling a dead preview); no automated check yet |
| C4 | Silent first-page reads: update summary limit=200 with no paging (machines past 200 get no update state); installs and config-sync status the same | `console/lib/api.ts:343-345` | cursor paging | C | open, briefed |
| C5 | Client fetches take no AbortSignal and no timeout; SSR prefetch loads machine pages serially at 3 s each | `console/lib/api.ts:29-41; lib/server/prefetch.ts` | forward signal, fail fast | C | partly fixed 50052425 (stuck read times out); SSR serial prefetch open |
| R1 | Byte-identical duplicate packages: relay/admission = shared/admission, relay/reattach = shared/reattach, relay/internal/reporting = helper/internal/reporting (1.6k lines, security-sensitive) | `diff -rq: 0 differing files` | one owner in shared/ (AGENTS.md "one way") | R + B | partly fixed fc5dea98 (relay/admission, relay/reattach deleted; build is the check); relay/helper reporting copy open (maintainer moves it to shared/) |
| R2 | Relay re-posts every authority subject to the server every 3 s, no delta: ~1.6 MB per node per 3 s at 16k connections; server processes the full list each time | `relay/nodelifecycle/client.go:25,262` | TS derper mesh: changes only; NB relay: no per-tick subject list | R + B (endpoint) | fixed and live (no CHECKLIST row): 231fcf4e + 96b2682f/2d82c641/1bcdd76f, tests TestRelaySubjectsSteadyMembershipKeepsStoredSetPostgres, TestRelaySubjectsIneligibleAccountIsCutAboveItsGrantsPostgres; relay 2026.10.23.5 built from 96b2682f (maintainer), server .70; prod 15 min: digest_only 4983, full 101 (after restarts only) |
| R3 | Every admitted relay lease polls revocation every 1 s, each tick taking the relay's global lock: 16k lock round trips/s at capacity | `relay/admission/lifetime.go:136; relay/derpquic/server.go:241-251` | TS derpserver: revocation pushed (closes the client), no per-client poll | R | open, briefed |
| R4 | Connector frames allocate up to 4 MiB before type/auth check, and a snapshot is canonical-JSON validated ~3 times | `shared/connectorprotocol/protocol.go:689-692, authority.go:816-838` | TS controlbase: small fixed frame limit pre-auth | B (shared) + M | open, briefed |
| T1 | Edge ingress fails closed on any control fetch error: decisions are dropped and every TLS accept / public request refetches under one mutex (2 s timeout, no backoff), so a control-plane outage takes down every public preview and tunnel and serializes all accepts | `edge/internal/control/ingress_authority.go:44-80` | Teleport proxy serves from lib/cache while auth is unreachable; TS DERP keeps serving known clients | M + B (same "until revoked" rule as F21-01 for the edge) | fixed (not deployed): 9af1e1eb, test TestIngressAuthorityKeepsPublicDecisionsThroughAControlOutage; edges rolled back to tunnel 2026.10.23.9 after the 4.3 gate failures, F21-17 L back to [ ] (waits for maintainer re-ship with the merger fix) |
| T2 | Per-carrier 100 ms loops scan and sort every admission (C × 10 × A per second per edge node); preview reconcile at the same rate | `edge/internal/runtime/runtime_carrier.go:264-283, preview_carrier.go:27, datacarrier/admission.go:487-503` | event-driven: wake on registry change (Teleport lib/services watchers) | M | fixed (not deployed): e4a4554c (index by carrier + Changed wake) [maintainer: edge still tunnel 2026.10.23.9 on 2026-10-27, so not deployed] |
| T3 | Public TCP and helper streams have no idle or lifetime deadline (io.Copy both ways) and share the accept-slot pool with TLS dispatch: idle streams starve HTTPS accepts | `edge/internal/edgehttp/public_tcp.go:108-124, shared_tls_listener.go:68-69` | Teleport app/connections_handler idle timeout; TS serve IdleConnTimeout 90 s | M | fixed (not deployed): passthrough streams in their own bounded pool (`TestTLSPassthroughStreamReleasesItsAcceptSlot`); 1 h idle timeout, clean FIN close, edge config `public_tcp_idle_timeout`, TCP keepalive 30 s/10 s×3 (`TestPublicTCPIdleTimeoutClosesQuietStreamsCleanly`) [maintainer: edge still tunnel 2026.10.23.9 on 2026-10-27, so not deployed] |
| T4 | Edge pulls full lists every 500 ms (runtime endpoints, preview snapshot + full ack echo) and the full ingress decision set every 5 s, no version/delta | `edge/internal/runtime/runtime_carrier.go:170-183, control/preview_carrier.go:401-420, shared/connectorprotocol/ingress.go:59` | Teleport services watcher (event stream from backend) | M (known in maintainer row "edge pulls ~300 ms"; adds ingress + ack echo) | partly fixed 20688cf9 (server answers endpoint deltas, TestEndpointAdmissionDeltasOnPostgres); edge does not ask for endpoint_deltas yet, ingress set and ack echo unchanged |
| K1 | Six fault/error-reporting implementations of the same model (fault.go in cli/errorreport, cli/diagnostics, server/supportreport, relay+helper reporting (identical), tunnel/reporting), each with its own Sentry/telemetry glue | `md5 of each fault.go; relay/helper identical` | one shared/ package (TS logtail/logpolicy used by every binary) | B (shared) + A/R/M consumers | open, briefed |
| A1 | Local API snapshot capped at 1 MiB both ways while it may hold 10,000 machines (29 fields each, roughly 0.6–1 KB JSON): above ~1–1.5k machines pb status, watch and warm exec/connect fail with no fallback (verify exact bytes/machine) | `cli/internal/localapi/server_unix.go:27,322; types.go:19` | TS LocalAPI status?peers=false and watch masks | A | verified: F21-24 L [x] (44856ec1) from TestASnapshotOfTheMostMachinesFits (fix ace47988, bound = machines x 1 KiB + 64 KiB) |
| A2 | Commands fetch the whole account list to find one machine: team `pb status` (alias map), cold exec/connect/ssh/wait (resolveUserMachine), degraded machines always cold; 50 serial pages at 10k machines per command; server already filters on q | `cli/cmd/pb/status_command.go:36, targets.go:307-309,404` | TS: CLI asks tailscaled only | A | verified: F21-24 L [x] from TestResolvingAMachineAsksOnlyForItsName (fix bee90ab3, q=<name>); live on device builds from round 23 dev-b4ad5c98 (round 22 dev-e71c7180 withdrawn) (the round 21 device sanity in the row predates bee90ab3) |
| A3 | GET retries stack: transport retries ×attempts inside send()'s 3 read retries, 30 s client timeout: up to ~9 attempts / ~90 s against a blackholed server | `cli/internal/api/http.go:45-75, response_lost.go:37-45` | one retry layer with backoff+jitter (TS util/backoff) | A | open, briefed |
| S1 | A transient DB error in preview-lease, tunnel-expiry or team-inbox receipt workers returns from its worker, the supervisor cancels every worker and the server shuts down (other workers log and continue) | `server/internal/app/preview_tunnel_workers.go:98-109,161-170; teaminbox/notification.go:48-58; workers/supervisor.go:54-71` | Teleport/HS: per-loop retry with backoff, never process exit | B | fixed b14620bb (+ teaminbox), test TestPeriodicPreviewTunnelWorkerKeepsRunningThroughFailures; verified in code |
| S2 | Global advisory lock teams.Lock (key 160235) taken in 62 write/authorize paths (inspector authorize, managed SSH, ~20 user writes): every team-touching write on the platform serializes on one lock | `server/internal/teams/store.go:35-37; 62 call sites` | NB: per-account lock (GetAccountWithLock); HS: per-node state | B (Q for measurement) | partly fixed, live server .75: c4e54dec + 84f3eff9 (no teams.Lock in access sessions or workspace.ValidateTx), test TestPostgresPersonalAccessSessionsDoNotQueueOnTheTeamLock (team sessions too); team-state writers still take the global lock (per-team locks and S7 open, F21-20) |
| S3 | Terminal session inventory is N+1 in one RepeatableRead tx: getTerminalSession per id, then sharing/grants per id (~3 queries per row, ~600 per 200-row page) | `server/internal/usermachines/inventory.go:162-175, terminal_sharing.go:226-332` | set-based batch per page | Q (extends the 105-query brief) | fixed, live server .75: e787a914 (one page context, /v1/terminal-sessions 104 -> 12 queries), test TestPerformanceBudgetsOnSeededPostgres; no CHECKLIST row |
| S4 | peer_mesh_log (new, deltas) is never pruned: pruned_seq is declared but nothing writes it or deletes; latestMeshEntries reads the account's whole log history on every rebuild | `server/internal/db/migrations/151_peer_mesh_log.sql:9; peersessions/mesh.go:101-102` | TS sends PeersChanged/Removed, keeps no history | Q | partly fixed (not deployed): af7a271d prunes superseded and removed peer_mesh_log entries (1 h window, TestRetentionBoundsEveryPerOperationTableOnPostgres), not in server .96; latestMeshEntries still reads the whole log and pruned_seq is still unwritten |
| S5 | Every 10 s runtime observation per machine runs a metering tx with FOR UPDATE + 3 unconditional UPDATEs plus up to 4 more service writes: ~1k POST/s and 4–6k write tx/s at 10k machines, mostly no change | `server/internal/httpapi/runtime_observation_handlers.go:209-275; metering/runtime.go:42-74; cli hostruntime/runtime/production_unix.go:317` | TS: Hostinfo sent only on change | Q + R (client interval) | open, briefed |
| S6 | Activity export streams up to 1M rows inside one InTx (pool slot + snapshot held for the whole download), contradicting its own comment | `server/internal/activity/export.go:49-65` | Teleport: export via paged search, no long tx | B | open, briefed |
| H1 | Terminal history leaks: events slice reclaims its dead prefix only when fully drained, so continuous output grows it ~56 B per chunk for the session's life | `cli/internal/hostruntime/history/history.go:173,290-303` | ring buffer | A | verified: F3-21 L [x] (01ec8d2f) from TestContinuousOutputKeepsHistoryStorageBounded (fix 6907a006) |
| H2 | Per-session input journal has a 10,000-decision lifetime cap with no pruning (not even on new generation), each write also persisted to SQLite: after 10k sequenced writes the session refuses input (verify which clients use sequenced Write vs WriteStream) | `cli/internal/hostruntime/session/input.go:83-133, manager_input.go:20-60, config/config.go:47` | drop decisions of old generations / window | A | verified: F3-21 L [x] from TestInputJournalKeepsARecentWindowForLongSessions, TestInputDecisionsArePrunedToTheWindow (fix 0f28ffc3) |
| H3 | Connector rotation journal keeps every record forever with a 64-record cap: after 64 rotations on a host every rotation fails | `cli/internal/hostruntime/connectorrotation/rotation_journal.go:160-166` | prune completed records | A/R | verified: TestTheRotationJournalPrunesFinishedRotations green on the round 29 tree (dev-c7467151; cli and shared unchanged to 8385e540, GitHub run 40247518028, -race) (fix 2a030e40); no CHECKLIST row |
| H4 | An unknown JWT kid triggers a synchronous JWKS refresh before signature check, no cooldown/single-flight (attacker-chosen kid = 1 upstream fetch per request) | `cli/internal/hostruntime/auth/verifier.go:198-212, jwks.go:146-173` | Teleport cache with miss TTL and coalescing | A | verified: TestJWKSRefreshCooldownCollapsesUnknownKeyMisses green on the round 29 tree (dev-c7467151; cli and shared unchanged to 8385e540, GitHub run 40247518028, -race) (fix a04222d9); no CHECKLIST row |
| S7 | Stripe seat change (45 s timeout) runs inside a transaction holding the global teams.Lock, reconciled up to 128 ops/minute: every team write on the platform waits on Stripe latency | `server/internal/billing/workspace_seats.go:172-218` | NB: row locks, no provider call inside a lock | B | open, briefed |
| S8 | Teams are hard-capped at 128 members against the 1k-member target; team list reads ≥6 statements per team with unpaged member/grant/machine arrays | `server/internal/teams/types.go:25, store.go:379-420` | NB: paginated peers/users | B (product limit: coordinator) | open, briefed |
| S9 | Bandwidth gate cache is wiped whole at 4,096 accounts: beyond that each hot-path request does a DB lookup and a denied account fails open while billing is slow | `server/internal/bandwidthgate/gate.go:92-99` | Teleport ratelimit: per-entry TTL sweep | B | open, briefed |
| S10 | SSO identity sync re-marks every stale team dirty on every 5 s tick (no `AND NOT dirty`) and does one OR-joined UPDATE per WorkOS event | `server/internal/teamidentity/sync.go:36-40, 89-99` | HS: per-node timers, no rescans | B | open, briefed |
| E1 | Tunnel-domain certificates have no retry backoff: failed/rate-limited domains are picked every 1 s tick (CAA + lock + ACME order each time) and the ACME RetryAt is discarded; the preview variant has a 1-minute backoff | `server/internal/db/queries/tunnel_certificate_domains_v1.sql:8-40 vs :93; tunnelv1/certificate_worker.go:79-119` | TS ipn/ipnlocal/cert.go: renew near expiry, honor Retry-After; cloudflared: no per-tick reissue | B | open, briefed |
| E2 | Every connector heartbeat (5 s) re-reads the full tunnel config snapshot (≤4 MiB) from the DB and re-parses it; the server never offers deltas (OfferDelta has no caller); ~2 row writes per heartbeat | `server/internal/connectorprotocol/transport.go:438-446, sql_store.go:423-460,809-870, persistence.go:424-432` | cloudflared: remote config pushed on version change only; TS: delta map after the first | B (M for the connector side) | fixed (live in server .96): 181a3ebf (connector heartbeat on an unchanged config reads no snapshot payload; TestControlTransportPersistsReadyHeartbeatAndGenerationSafeDisconnect); legacy connectors still go with step 5 |
| E3 | Preview list is N+1: previewView runs CurrentPreviewRoute + ListProjection per row (~401 round trips per 200-row page); the edge carrier pull does 2 statements per admission (≤8192/pull) and the ack 1 tx per admission with FOR UPDATE on the edge node row | `server/internal/previewv1/service.go:660-668,990,1002; previewattachment/outbox.go:163-185,273-290; edge_http.go:317-322` | zrok: share list is one query; batch by ids | Q | open, briefed |
| E4 | Tables nothing prunes (beyond D1): preview_carrier_attachment_outbox, tunnel_log_entries (has a retention index but no delete), tunnel_connector_enrollments, peer_endpoint_certificates/enrollment requests, managed_ssh_client_keys, transactional_mail_outbox (payload blanked only) | `grep: no DELETE outside migrations` | Teleport: TTL on every backend item | Q | partly fixed (not deployed): af7a271d retention for credentials, control operations, preview attachment outbox, connector enrollments, mail outbox, retired certificates, revoked SSH keys, peer certificates (TestRetentionBoundsEveryPerOperationTableOnPostgres, migration 194), not in server .96; tunnel_log_entries is still deleted only by account deletion |
| P1 | While any native stream is open, every client re-fetches its signed peer-network config and the JWKS every 1 s (the revocation lease poll runs a full Refresh), ~2 requests/s per active client | `cli/internal/peertransport/tailnet/refresh.go:119-132, 60-82; shared/admission/lifetime.go:136` | TS: map updates arrive on the open long poll (controlclient direct.go); revocation pushed | R + B (push revocation) | verified: TestAdmittedScopeDoesNotPollTheConfigurationEverySecond and TestJWKSFollowsMaxAgeAndDropsWithdrawnKeys green on the round 29 tree (dev-c7467151; cli and shared unchanged to 8385e540, GitHub run 40247518028, -race); measured on linux-test-vm-2 dev-9d9704d4 (maintainer). per-second polls gone: 0e560ffd (no live-scope poll; load test 21 -> 1 req/20 s), f27e2a99 (JWKS hourly from max-age, fetched only on an unknown kid; NetBird prior art is shared/auth/jwt/validator.go forcedRefreshCooldown), fd443db2 (no-relay 1 s poll -> one renewal per episode + 8 s..60 s backoff; load test 20 -> 3/20 s). Measured on linux-test-vm-2, dev-9d9704d4, 10 min, one live native stream: 0 JWKS; config 65 (daemon engine) + 63 (host engine) per machine = one renewal per ~9 s per engine, because signed relay observations last 15 s (regionalObservationMaxAge) and recovery renews before they end. F21-18's "≤ 1 config per 10 min absent changes" needs longer-lived relay observations or relay health pushed (server design with maintainer) (maintainer) |
| P2 | Every metered Read/Write builds a full magicsock status of all peers twice (path observer before and after each op), plus json+sha256 per add() under the recorder lock, which also fsyncs state up to 128× per flush | `cli/internal/bandwidth/meter.go:40-66, recorder.go:136-160,248-258; peertransport/mesh/regional_status.go:30-43` | TS wgengine/netlog: typed counters under lock, serialize at a 5 s flush; per-peer lookup O(1) (peermap) | R | partly fixed ce276005 (peer path cached 1 s, TestUsagePathIsComputedOncePerTTLPerPeer); recorder lock/fsync part open |
| P3 | Each active private TCP session POSTs /v1/private-access/routes every 1 s (lease poll → validate → uncached discovery snapshot); the server returns up to 4,096 rows per call | `cli/internal/hostruntime/preview/private_tcp_access_manager.go:345-350, accessor_discovery.go:150-172; server privateaccess/accessor_routes.go:162-170` | Teleport lib/srv/monitor: subscribes to a lock watcher (push), no per-session poll | R + B | partly fixed: client part 9d9704d4: one shared snapshot per machine per second, not one per session (TestRoutesSnapshotIsSharedByEverySessionForASecond, 16 sessions -> 1 request). Still 1/s per machine while a private TCP session is open: route changes publish no topic yet (maintainer, snapshot + deltas with maintainer) |
| P4 | Machine-services refresh waits min(lease expiry) − 10 s over every listed machine's 30 s lease, floored at 2 s: with many announcing machines the floor is hit constantly, so each daemon GETs /v1/machine-services every ~2 s (server does per-port ResolveTx per candidate) | `cli/internal/daemoncmd/machine_services_refresh.go:18-37; server machineservices/service.go:29` | TS: services/peers arrive in the netmap stream; Teleport heartbeat scaled to fleet size (lib/inventory/controller.go:298-302) | R + Q | partly fixed (not in a device build yet): cd9737c9 (F21-19) the daemon fetches machine services at start, on its topic, on a nudge and every 5 min, no longer every ~7 s (TestMachineServicesAreFetchedOnlyOnTopicOrNudge); the per-lease refresh floor for many announcing machines is not measured |
| P5 | CallMeMaybe is sent to every peer on every status change and meow ping, each send scanning all peers (O(N²) per event); admit rebuilds and sorts the whole netmap under the backend lock | `cli/internal/peertransport/mesh/engine_backend.go:262-274,98-109,362-393` | TS magicsock sends CallMeMaybe only to the peer starting discovery (endpoint.go:1428-1450); updateNodes diffs | R (extends maintainer #8) | open, briefed |
| P6 | Inbox pending poll retries a failed request after a fixed 1 s, no backoff or jitter, for the life of each `pb connect` session: 10k sessions = 10k req/s exactly when the server recovers | `cli/internal/inbox/inbox.go:270-284` | TS controlclient: exponential backoff with jitter, honors Retry-After (auto.go, direct.go) | A | verified: TestPendingRetryWaitBacksOffWithJitterToAMinute green on the round 29 tree (dev-c7467151; cli and shared unchanged to 8385e540, GitHub run 40247518028, -race) (fix 2798485c); no CHECKLIST row |
| P7 | Client metrics sampler sends up to ~68 metric items to Sentry every 30 s per daemon, bypassing the 32/min detail budget it documents (~1.4M items/min at 10k daemons; verify series count) | `cli/internal/errorreport/metrics.go:95-137` | TS logtail: buffered, rate-limited uploads | A (B owns Sentry quota) | verified: TestSnapshotSendsOnlyChangedValuesBetweenRefreshes green on the round 29 tree (dev-c7467151; cli and shared unchanged to 8385e540, GitHub run 40247518028, -race) (fix 5ab25e22); no CHECKLIST row |
| T5 | Every tunnel connector pool re-fetches its carrier descriptor from the server every 4 s, even when healthy: the repair loop calls Connect every 1 s with no "pool full" early return, and Connect refreshes targets after 4 s (~0.25 req/s per connector, ~2.5k req/s at 10k machines) | `cli/internal/hostruntime/connector/data_carrier_pool.go:88-112,342-358; tunnelenrollment/bootstrap_descriptor.go:131-134` | cloudflared: edge discovery once, re-resolved only on connection failure (supervisor) | M (A for cli paths) | open, briefed |
| T6 | QUIC carriers (the default preferred transport) get no liveness monitor; only HTTP3/HTTP2 carriers are pinged every 5 s. A blackholed QUIC carrier is detected only by the 2 min idle timeout (keepalive 30 s), and its streams hang meanwhile | `cli/internal/hostruntime/connector/data_carrier_pool.go:236-238, data_carrier_pool_types.go:76, data_carrier_endpoint.go:293` | cloudflared QUIC: idle 5 s, keepalive 1 s (quic/constants.go) | M | verified (agent-only fix): TestBlackholedQUICCarrierClosesAfterTheIdleTimeout green on the round 29 tree (dev-c7467151; cli and shared unchanged to 8385e540, GitHub run 40247518028, -race) (fix 6c9d2e9a, keepalive 5 s, idle close 15 s); F21-28 stays [ ] until T7 ships on the edge |
| T7 | Every edge stream open takes the route registry's exclusive lock and scans every active rule (≤4,096) with no early exit | `edge/internal/route/matcher.go:630-652, matching_rules.go:76-103` | cloudflared ingress: first-match over an immutable slice, no lock (ingress/ingress.go:42-68); copy-on-write snapshot | M | fixed (not deployed): e43cc669 (host index, no scan under the lock; benchmark flat 10→4,096) [maintainer: edge still tunnel 2026.10.23.9 on 2026-10-27, so not deployed] |
| T8 | A pending preview attachment POSTs Allocate every 250 ms until ready, each a server write transaction (placement, latency, attachment upsert): 1k pending = 4k write tx/s | `cli/internal/hostruntime/preview/attachment_types.go:26, attachment_client.go:176-202; server runtimeattachment/service.go` | cloudflared: versioned config pushed on the open stream (orchestration/orchestrator.go) | M + B | open, briefed |
| T9 | Each machine keeps at most 2 carriers on 2 edges (failure domains), so one dead edge leaves a single path; unused single-connection connector.Manager/Supervisor remain | `cli/internal/hostruntime/connector/data_carrier_types.go:28; server edgeset/select.go:16; connector/manager.go, supervisor.go:87` | cloudflared: 4 HA connections by default (cmd/cloudflared/tunnel/cmd.go:792) | M (product choice: edge pair, see memory edge-model-account-pair) | open, briefed |
| T10 | Host-initiated drain tells nobody: the connector stops admitting streams locally (BeginDrain) and waits up to DrainTimeout 15 s, but the only drain message in the protocol is server→connector, so the edge keeps routing new requests to the draining carrier until it closes (verify: whether the edge retries those on the second carrier) | `cli/internal/hostruntime/connector/data_carrier_pool.go:660-705; tunnelmanager/manager_shutdown.go:124-136; server/internal/connectorprotocol/drain_dispatcher.go:296` | cloudflared: UnregisterConnection on the control stream, then a grace period (connection/control.go:126-137) | M | fixed (edge half not deployed) for engine carriers: the daemon sends a `goaway` (host_draining, shared/connectorprotocol/goaway.go) on each runtime carrier before closing; the edge refuses new streams and drops that carrier's routes at once (`TestHostGoawayDrainsTheCarrierAndKeepsOpenStreams`, `TestRuntimeCarrierRoutesLeaveOnHostGoaway`, `TestClosingRuntimeCarrierSendsHostGoaway`); legacy connectors go in step 5 [maintainer: agent half 0f0c6ee2 in round 29 dev-c7467151; edge still tunnel 2026.10.23.9] |
| T11 | Edge ingress limits are in-memory token buckets per edge process with hardcoded defaults (account 128 conns, 50 opens/s, 32 MB/s): with the 2-edge pair an account gets 2×, a self-hosted edge applies its own, and they are not in the one config place | `edge/internal/edgehttp/ingress_limits.go:39-64; durable_carrier_transport.go:106` | zrok: per-account limits computed centrally from usage (controller/limits/agent.go); Teleport limiter from config | M (B for config/plan values) | partly fixed: edge done: one set (shared/edgelimits, same for all plans, per edge, coordinator 2026-10-26) arrives in the heartbeat answer `ingress_limits` and applies to current keys; limit hit = 429 `edge_rate_limited` + Retry-After; publication byte rate raised 8→24 MiB/s for PR-06. Waits for maintainer: server config field answering the heartbeat (edges first, they accept 204) |
| N1 | Pooled HTTP/2 control-plane connections had no health check: after a network outage a connection that was mid-flight stays open on backed-off TCP retransmits (≤120 s), requests that time out don't close it, so every daemon request queues on a dead connection (`pb status` "Control plane is unavailable" for minutes; seen windows-test 17:52Z, linux-peer 09:05Z; repro: linux-test-vm-2 300 s drop → degraded 108 s after restore) | `cli/internal/httptransport/transport.go (HealthCheckInterval/Timeout → HTTP2 SendPingTimeout/PingTimeout)` | TS: KeepAlive map responses + 120 s watchdog drops the stream (controlclient direct.go:1023,1094,1174-1191), netmon link-change reaction (ipnlocal local.go:720,1205); per-connection HTTP/2 PING is our equivalent | P (A) | verified on device: linux-test-vm-2 300 s traffic drop, ready 25 s after restore on dev-cb92d5e4 vs degraded 108 s on dev-a2a37bc0 (linux-test-vm-2 ~/lanep/blip-test-run-p1a.log, -r18b.log, re-read by maintainer); test TestTransportReplacesAConnectionThatStalledDuringAnOutage; ships in round 20 dev-7b9bef89; no CHECKLIST row |
| N2 | Native terminal transport (peer_stream) also stayed broken after the same network blip on linux-peer while doctor reported healthy: second client with the N1 symptom, outside httptransport (being checked by maintainer) | `cli/internal/peertransport (to locate)` | TS: netmon link change rebinds magicsock and resets endpoints (wgengine/magicsock rebind) | P + A | partly fixed: first contact fixed on device, round 27 dev-ef53e4cd (b57e7735, f11d7823, 616c8dd8), 3 fresh linux-test-vm-2 pairs, first pb ping 1.3-1.4 s (round 25: 10 s timeout), first exec 1.6-2.7 s both ways (maintainer); the 17 min linux-peer outage with doctor healthy is still unreproduced, and doctor does not yet probe the native path (maintainer) |
| N3 | After a NAT rebinding (new source ports during an outage), connections INTO the rebound machine fail for ~50–65 s after restore while its outbound works at once: peers keep dialing the old endpoint until the new one is learned (linux-test-vm-2 round 20: RESTORE 18:57:37, inbound rc=1 ×4 until 18:58:25, first success 18:58:42; control ready in 33 s) | `cli/internal/peertransport/mesh (endpoint advertise / CallMeMaybe on change), tailnet regional recovery` | TS magicsock: on netmon change ReSTUN + send CallMeMaybe to active peers within seconds; heartbeat 3 s, trustUDPAddrDuration 6.5 s so a peer falls back to DERP and re-discovers quickly (magicsock.go:4084-4088, endpoint.go) | R | fixed, measured on device (no CHECKLIST row; automated rebind check still to add): 18d12f74 + 8d25702f (presence retried within 2 s), ae56f19b (native-only descriptors), 616c8dd8 (relays kept); round 28 dev-07866f9f linux-test-vm-2 2026-10-26: REBIND udp 41000-41999, RESTORE 23:21:17, first inbound exec n2->n1 rc 0 at 23:21:24 (7 s), 43/43 rc 0 (~/lanep/rebind-test-run-n1.log, re-read by maintainer); was 50-65 s failing |
| N4 | Local names depend on public DNS: every <machine>.<domain> is a public wildcard answering 127.100.0.1 (dig @1.1.1.1, 2026-10-26), so local access names need working upstream DNS and are expected to fail offline and behind DNS-rebinding filters that drop private/loopback answers (dnsmasq --stop-dns-rebind, many routers, Pi-hole); to verify on linux-test. Prior art: Tailscale MagicDNS answers its names locally (100.100.100.100), never from public DNS | `machineguard + hostruntime (no hosts entries since the wildcard)` | tailscale net/dns, MagicDNS resolver | A (B for DNS) | open, to verify on linux-test (maintainer); found in the multi-account hosts review |
| N5 | Two machines cannot reach each other natively without a relay, even on one LAN: first contact resolves a relay region and exchanges endpoints through it (meow + CallMeMaybe) before any direct path forms, so with no usable relay node the dial fails before any packet (staging, maintainer 2026-10-26: native_private_failed/internal) | `cli/internal/peertransport/native/owner.go (PrepareRegional before Dial), mesh/engine.go (meow over DERP), engine_backend.go advertiseEndpointsTo` | TS: control puts each node's endpoints in the netmap (tailcfg.Node.Endpoints) and magicsock pings them directly with disco, DERP optional (wgengine/magicsock/endpoint.go) | R (+ B: endpoints in the signed peer config) | open, maintainer (+ maintainer: endpoints in the signed peer config); reported by maintainer on staging 2026-10-26; code path confirmed (native/owner.go:249 PrepareRegional before Dial). Field agreed with maintainer (server accepts the request field → CLI sends and accepts → server delivers; maintainer adds it to /changes). Client accepts delivered endpoints (3e401cc3, 06f77b82) and uses them (e169f105: nodes from signed contacts, disco ping, dial without a relay; TestPeersWithSignedEndpointsConnectDirectlyWithoutARelay, ~5 s with no relay at all, one WireGuard retry as in Tailscale without DERP). Server side moved to maintainer (coordinator). Client report done (aa8b4fa7, faa80d68 server's normalization, e41390e5 explicit empty set): sent only after the server answers `endpoint_reports: true`, then on change (5 s debounce, ≤ 1 per 30 s). Step 1 (maintainer 917095b6) live in server .86, verified 2026-10-26 on linux-test-vm-2 test-run-n1/n2 (dev-b1e33786): both identities stored 2 endpoints, renewals 200, exec both ways. Round 30 dev-b1e33786 out. Step 2 (maintainer 93387e9b, migration 186): peers' endpoints on the signed peer entry only for engines that sent an endpoints field (capability, never unset); a changed set publishes peer_network ≤ 1 per 60 s per identity, latest wins, the held-back set published by the leader's worker_peer_endpoints; TestPeerEndpointsReachOnlyCapableRecipientsCoalescedOnPostgres; tools/compat/run.sh also decodes the non-capable configuration with 2026.10.25.0. Device check done 2026-10-26 (linux-test-vm-2, prod, dev-36f3cb82 = main 80f9760f): two fresh containers, every relay host dropped, ping and exec both ways rc=0, path=direct (exec 8.2 s and 12.2 s cold, ping connect 13-18 ms; first contact still 5-12 s, not yet explained). The same-NAT relay fallback is proven by the workspace test only: the docker bridge ignored the direct-path drop (ping stayed direct), so maintainer's two-NAT rerun is the device proof. /changes delivery comes with deltas 2b |

**Coverage**

| Package | Lines | Owner | Reference subsystem | Status | Findings |
|---|---|---|---|---|---|
| `cli/cmd/browserterminal-wasm` | 2379 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/cmd/environmentvault-wasm` | 1257 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/cmd/pb` | 37014 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | A2 |
| `cli/docs` | 126 | A | — | done | checked, nothing |
| `cli/internal/api` | 9399 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | A3 |
| `cli/internal/atomicfile` | 484 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/atomicfile/atomicfiletest` | 63 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/auth` | 271 | A | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `cli/internal/backoff` | 30 | A | TS util/backoff | done | checked, nothing |
| `cli/internal/bandwidth` | 387 | A | Teleport lib/usagereporter; — | done | P2 |
| `cli/internal/bugreport` | 629 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/buildinfo` | 51 | A | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | checked, nothing |
| `cli/internal/causetext` | 59 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/command` | 65 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | checked, nothing |
| `cli/internal/config` | 7065 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/connectorprotocol` | 4206 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/controlsync/proto` | 897 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/daemoncmd` | 3229 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | P4 |
| `cli/internal/daemonrpc` | 1073 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/daemonrpc/proto` | 907 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/diagnosticlog` | 130 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/diagnostics` | 1597 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/diskspace` | 55 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/doctor` | 203 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/endpointbinary` | 110 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/environmente2ee` | 3849 | A | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `cli/internal/environmentmanager` | 2445 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/internal/errorreport` | 1871 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | P7 |
| `cli/internal/fileindex` | 231 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/internal/filetransfer` | 1935 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/internal/hostruntime/auth` | 924 | A | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | H4 |
| `cli/internal/hostruntime/autoupdate` | 808 | A | TS clientupdate; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/availability` | 544 | A | TS health (Tracker/Warnable) | done | checked, nothing |
| `cli/internal/hostruntime/binarytarget` | 96 | A | TS clientupdate; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/bootstrap` | 2819 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/browserbroadcast` | 216 | A | Teleport lib/web/terminal | done | checked, nothing |
| `cli/internal/hostruntime/browserbroadcastserver` | 428 | A | Teleport lib/web/terminal | done | checked, nothing |
| `cli/internal/hostruntime/carriercontrol` | 391 | R | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/config` | 101 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/hostruntime/configapply` | 76 | A | — | done | checked, nothing |
| `cli/internal/hostruntime/configsync` | 9069 | A | — (Git-based; compare: Teleport lib/services watcher) | done | checked, nothing |
| `cli/internal/hostruntime/connector` | 4186 | A | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing; T5, T6, T9; T10, T5, T6, T9 |
| `cli/internal/hostruntime/connectorrotation` | 2943 | A | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | H3 |
| `cli/internal/hostruntime/controlbackoff` | 147 | A | TS util/backoff | done | checked, nothing |
| `cli/internal/hostruntime/edgeplacement` | 178 | A | TS tailcfg DERPMap / derp mesh; Teleport lib/reversetunnel (agentpool) | done | checked, nothing |
| `cli/internal/hostruntime/enrollment` | 1375 | A | TS control/controlclient (register), types/key | done | checked, nothing |
| `cli/internal/hostruntime/envinject` | 1005 | A | — | done | checked, nothing |
| `cli/internal/hostruntime/environmentkey` | 1538 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/internal/hostruntime/errors` | 25 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/execprocess` | 2279 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/filetransfer` | 1477 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/internal/hostruntime/health` | 673 | A | TS health (Tracker/Warnable) | done | checked, nothing |
| `cli/internal/hostruntime/history` | 310 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | H1 |
| `cli/internal/hostruntime/hostd` | 459 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/hostdproto` | 2898 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/hostinstall` | 13011 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/hostservice` | 1876 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/hoststate` | 1944 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/hostruntime/identity` | 1118 | A | TS control/controlclient (register), types/key | done | checked, nothing |
| `cli/internal/hostruntime/inspectorapi` | 999 | A | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/inspectorauth` | 234 | A | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/installsource` | 823 | A | TS clientupdate; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/machinecontrol` | 432 | A | TS tailcfg DERPMap / derp mesh; Teleport lib/reversetunnel (agentpool) | done | checked, nothing |
| `cli/internal/hostruntime/machineevents` | 401 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/hostruntime/machineevents/eventstest` | 153 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/hostruntime/nativeproto` | 147 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/nativesession` | 188 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/internal/hostruntime/nativesignature` | 76 | A | TS control/controlclient (register), types/key | done | checked, nothing |
| `cli/internal/hostruntime/networkrecovery` | 1092 | A | TS net/netmon | done | checked, nothing |
| `cli/internal/hostruntime/operation` | 272 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/originhealth` | 200 | A | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/peeridentity` | 255 | A | TS control/controlclient (register), types/key | done | checked, nothing |
| `cli/internal/hostruntime/preview` | 10984 | R | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | P3; P3, T8 |
| `cli/internal/hostruntime/privateproxyconfig` | 1271 | A | — | done | checked, nothing |
| `cli/internal/hostruntime/process` | 289 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/protocol` | 902 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/pty` | 1196 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/releaseeligibility` | 792 | A | TS clientupdate; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/releaseindex` | 295 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/releasepolicy` | 414 | A | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `cli/internal/hostruntime/runtime` | 9870 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/hostruntime/runtimeattachment` | 823 | A | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/runtimeport` | 8 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/server` | 7883 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/hostruntime/service` | 6758 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/session` | 4464 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | H2 |
| `cli/internal/hostruntime/sessionowner` | 2542 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/internal/hostruntime/store` | 1706 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/hostruntime/store/storesqlc` | 620 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/hostruntime/supervisorupdate` | 952 | A | TS clientupdate; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/supportbundle` | 1309 | A | — | done | checked, nothing |
| `cli/internal/hostruntime/telemetry` | 1442 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/hostruntime/testleak` | 49 | A | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | checked, nothing |
| `cli/internal/hostruntime/tunnelcreatejournal` | 823 | R | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/tunnelenrollment` | 4285 | R | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/tunnelmanager` | 6227 | R | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntime/updated` | 7189 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/updateflow` | 351 | A | TS clientupdate; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/workerupdate` | 2241 | A | TS clientupdate; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/hostruntime/workloadbridge` | 763 | R | cloudflared supervisor, connection (QUIC→HTTP/2, HA connections, reconnect), ingress rules; ngrok-go session (reconnect, heartbeat); zrok shares (public/private/reserved) | done | checked, nothing |
| `cli/internal/hostruntimecmd` | 9695 | A | — | done | checked, nothing |
| `cli/internal/hostruntimeentry` | 49 | A | — | done | checked, nothing |
| `cli/internal/httptransport` | 592 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | checked, nothing; N1 |
| `cli/internal/inbox` | 1199 | A | TS taildrop (feature/taildrop); — | done | P6 |
| `cli/internal/inspector` | 2164 | A | Teleport lib/reversetunnel, lib/proxy, lib/srv/app; TS ipn/ipnlocal serve (funnel) | done | checked, nothing |
| `cli/internal/liveconnections` | 37 | A | TS wgengine/magicsock, net/netcheck; NB client/internal/peer | done | checked, nothing |
| `cli/internal/localapi` | 4110 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | A1 |
| `cli/internal/localdaemon` | 5362 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/localwait` | 303 | A | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | checked, nothing |
| `cli/internal/machineguard` | 9397 | A | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `cli/internal/machineguard/wfp` | 1428 | A | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `cli/internal/machineloopback` | 65 | A | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing |
| `cli/internal/machinename` | 25 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | checked, nothing |
| `cli/internal/machineservices` | 565 | A | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | checked, nothing |
| `cli/internal/managedssh` | 6294 | A | TS ssh/tailssh; Teleport lib/srv (node, regular) | done | checked, nothing |
| `cli/internal/nativeprivate` | 115 | A | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing |
| `cli/internal/neterr` | 46 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/netlatency` | 184 | A | TS wgengine/magicsock, net/netcheck; NB client/internal/peer | done | checked, nothing |
| `cli/internal/ospeer` | 49 | A | TS wgengine/magicsock, net/netcheck; NB client/internal/peer | done | checked, nothing |
| `cli/internal/paste` | 1122 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/internal/peertransport/clientauthority` | 407 | R | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `cli/internal/peertransport/endpointidentity` | 494 | R | TS control/controlclient (register), types/key | done | checked, nothing |
| `cli/internal/peertransport/fixedpacket` | 49 | R | TS control/controlclient (register), types/key | done | checked, nothing |
| `cli/internal/peertransport/identitybootstrap` | 692 | R | TS control/controlclient (register), types/key | done | checked, nothing |
| `cli/internal/peertransport/mesh` | 2091 | R | TS wgengine/magicsock, net/netcheck; NB client/internal/peer | done | P5; N3, P5 |
| `cli/internal/peertransport/native` | 1142 | R | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing; N5 |
| `cli/internal/peertransport/networkadaptation` | 2269 | R | TS wgengine/magicsock, net/netcheck; NB client/internal/peer | done | checked, nothing |
| `cli/internal/peertransport/networkmonitor` | 1164 | R | TS net/netmon | done | checked, nothing |
| `cli/internal/peertransport/peerquic` | 1148 | R | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing |
| `cli/internal/peertransport/portmapping` | 591 | R | — | done | checked, nothing |
| `cli/internal/peertransport/privatepreview` | 162 | R | Teleport lib/reversetunnel, lib/proxy, lib/srv/app; TS ipn/ipnlocal serve (funnel) | done | checked, nothing |
| `cli/internal/peertransport/recoverykey` | 73 | R | — | done | checked, nothing |
| `cli/internal/peertransport/streamauth` | 102 | R | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `cli/internal/peertransport/tailnet` | 3890 | R | TS wgengine/magicsock, net/netcheck; NB client/internal/peer | done | P1 |
| `cli/internal/peertransport/trustedkeys` | 109 | R | — | done | checked, nothing |
| `cli/internal/peertransport/udpsocket` | 442 | R | — | done | checked, nothing |
| `cli/internal/preferences` | 534 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/privatepreviewproxy` | 1127 | A | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing |
| `cli/internal/processlaunch` | 37 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/processlifetime` | 138 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/prompt` | 513 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | checked, nothing |
| `cli/internal/remotepath` | 58 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/internal/resolver` | 854 | A | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing |
| `cli/internal/selector` | 1216 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | checked, nothing |
| `cli/internal/selfupdate` | 42 | A | TS clientupdate, cmd/tailscaled install; Teleport lib/automaticupgrades | done | checked, nothing |
| `cli/internal/serve` | 1351 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/internal/serverclock` | 49 | A | — | done | checked, nothing |
| `cli/internal/session` | 388 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/internal/shorttemp` | 30 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/splitdns` | 1158 | A | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing |
| `cli/internal/statusbar` | 1291 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/internal/supportref` | 45 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/telemetry` | 1367 | A | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `cli/internal/testcert` | 72 | A | — | done | checked, nothing |
| `cli/internal/tunnel` | 4999 | R | — | done | checked, nothing |
| `cli/internal/userpaths` | 41 | A | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `cli/internal/windows/elevation` | 1828 | A | — | done | checked, nothing |
| `cli/internal/windows/pipeauth` | 99 | A | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `cli/internal/windowsenvironment` | 192 | A | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `cli/internal/windowsopenssh` | 5332 | A | TS ssh/tailssh; Teleport lib/srv (node, regular) | done | checked, nothing |
| `cli/internal/windowssecurity` | 370 | A | — | done | checked, nothing |
| `cli/internal/wirejson` | 34 | A | TS cmd/tailscale/cli, client/local (LocalAPI) | done | checked, nothing |
| `cli/tools/http3-probe` | 138 | A | — | done | checked, nothing |
| `cli/tools/metric-schema` | 182 | A | — | done | checked, nothing |
| `cli/tools/preview-e2e-client` | 74 | A | Teleport lib/reversetunnel, lib/proxy, lib/srv/app; TS ipn/ipnlocal serve (funnel) | done | checked, nothing |
| `cli/tools/preview-e2e-target` | 40 | A | Teleport lib/reversetunnel, lib/proxy, lib/srv/app; TS ipn/ipnlocal serve (funnel) | done | checked, nothing |
| `cli/tools/release-plan` | 375 | A | — | done | checked, nothing |
| `cli/tools/releaseplan` | 1066 | A | — | done | checked, nothing |
| `cli/tools/terminal-benchmark` | 701 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/tools/terminal-experience-benchmark` | 543 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/tools/terminal-input-benchmark` | 613 | A | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `cli/tools/tuf-repository` | 1270 | A | — | done | checked, nothing |
| `server/cmd/paperboat-backup` | 203 | B/Q | — | done | checked, nothing |
| `server/cmd/paperboat-server` | 223 | B/Q | — | done | checked, nothing |
| `server/cmd/paperboat-watchdog` | 143 | B/Q | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `server/internal/access` | 48 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/accessdescriptor` | 97 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/accountingauthority` | 121 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/activity` | 511 | B/Q | Teleport lib/events (audit log, retention, pgevents) | done | S6 |
| `server/internal/admission` | 92 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | Robustness #1 (F21-01) |
| `server/internal/apitokens` | 684 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/app` | 2389 | B/Q | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | Robustness #4, #9; S1 |
| `server/internal/audit` | 175 | B/Q | Teleport lib/events (audit log, retention, pgevents) | done | checked, nothing |
| `server/internal/auditstream` | 770 | B/Q | Teleport lib/events (audit log, retention, pgevents) | done | checked, nothing |
| `server/internal/auth` | 2137 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/backupreport` | 1203 | B/Q | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `server/internal/bandwidthgate` | 224 | B/Q | Teleport lib/usagereporter; — | done | S9 |
| `server/internal/billing` | 3885 | B/Q | Teleport lib/usagereporter; — | done | S7 |
| `server/internal/browseraccess` | 1743 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/browseringress` | 180 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/browserterminal` | 1028 | B/Q | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `server/internal/buildinfo` | 9 | B/Q | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | checked, nothing |
| `server/internal/canonicaljson` | 77 | B/Q | — | done | checked, nothing |
| `server/internal/clientupdate` | 251 | B/Q | — | done | checked, nothing |
| `server/internal/config` | 1818 | B/Q | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `server/internal/connectorprotocol` | 6446 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | E2; E2, T10 |
| `server/internal/controlplane` | 7440 | B/Q | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | Robustness #2 (fixed aa987d5d); hot paths read, rest grep-screened |
| `server/internal/db` | 743 | B/Q | HS hscontrol/db; Teleport lib/backend (pgbk), lib/cache | done | DB review D1–D10 below |
| `server/internal/db/dbsqlc` | 34306 | B/Q | HS hscontrol/db; Teleport lib/backend (pgbk), lib/cache | done | checked, nothing |
| `server/internal/diagnostics` | 284 | B/Q | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `server/internal/diagnosticuploads` | 645 | B/Q | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `server/internal/edgeplacement` | 91 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | D4 |
| `server/internal/edgeset` | 275 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/emailpolicy` | 35 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/endpoints` | 365 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/environment` | 5705 | B/Q | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `server/internal/favorites` | 193 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/github` | 1557 | B/Q | — (Git-based; compare: Teleport lib/services watcher) | done | checked, nothing |
| `server/internal/helperruntime` | 428 | B/Q | — | done | checked, nothing |
| `server/internal/httpapi` | 17884 | B/Q | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | Robustness #5 (timeout bypass), #17; S5 |
| `server/internal/inboxdispatch` | 413 | B/Q | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `server/internal/inspectaccess` | 721 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/jobmonitor` | 426 | B/Q | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `server/internal/lazyaccess` | 1010 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | hot paths read, rest grep-screened |
| `server/internal/machinealias` | 74 | B/Q | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | checked, nothing |
| `server/internal/machineevents` | 232 | B/Q | TS control/controlclient (auto, direct, map); HS poll; NB client/internal/engine | done | Robustness #9; recheck 5 s per waiter (maintainer #6) |
| `server/internal/machinelatency` | 144 | B/Q | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | checked, nothing |
| `server/internal/machineservices` | 551 | B/Q | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | hot paths read, rest grep-screened |
| `server/internal/managedssh` | 1110 | B/Q | TS ssh/tailssh; Teleport lib/srv (node, regular) | done | hot paths read, rest grep-screened |
| `server/internal/metering` | 73 | B/Q | Teleport lib/usagereporter; — | done | checked, nothing |
| `server/internal/mint` | 1459 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | Robustness #8 |
| `server/internal/naming` | 77 | B/Q | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | checked, nothing |
| `server/internal/nativeprivateaccess` | 373 | B/Q | TS ipn/ipnlocal (serve, dns), net/dns; NB client/internal/dns | done | checked, nothing |
| `server/internal/nodeauth` | 168 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/operator` | 770 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/operatoractions` | 340 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/operatorauth` | 316 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/operatorinfra` | 660 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/operatorreporting` | 1930 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/peeridentity` | 1090 | B/Q | TS control/controlclient (register), types/key | done | hot paths read, rest grep-screened |
| `server/internal/peersessions` | 1433 | B/Q | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | hot paths read, rest grep-screened |
| `server/internal/perf` | 112 | B/Q | HS hscontrol/db; Teleport lib/backend (pgbk), lib/cache | done | checked, nothing |
| `server/internal/planlimits` | 221 | B/Q | — | done | checked, nothing |
| `server/internal/previewattachment` | 5230 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/previewdispatch` | 461 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/previewdomain` | 2431 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/previewtunnelapi` | 636 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/previewtunnelstore` | 2073 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/previewv1` | 1845 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | E3 |
| `server/internal/privateaccess` | 2633 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | hot paths read, rest grep-screened |
| `server/internal/publicationdns` | 142 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/releaseauthority` | 390 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/releases` | 502 | B/Q | — | done | checked, nothing |
| `server/internal/resourceusage` | 580 | B/Q | Teleport lib/usagereporter; — | done | checked, nothing |
| `server/internal/runtimeattachment` | 865 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | D4 (unconditional upsert, 9 writes/s) |
| `server/internal/secrets` | 85 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/selfhost` | 1617 | B/Q | — | done | checked, nothing |
| `server/internal/supportreport` | 2340 | B/Q | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | D6 (one row per error) |
| `server/internal/sync` | 671 | B/Q | — (Git-based; compare: Teleport lib/services watcher) | done | Robustness #7 (unused gRPC channel) |
| `server/internal/sync/proto` | 896 | B/Q | — | done | checked, nothing |
| `server/internal/teamidentity` | 1240 | B/Q | TS control/controlclient (register), types/key | done | S10 |
| `server/internal/teaminbox` | 1107 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/teams` | 4303 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | S2, S8 |
| `server/internal/teams/teamstest` | 64 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/internal/telemetry` | 4999 | B/Q | TS logtail, logpolicy, health, cmd/tailscale bugreport; Teleport lib/events (debug) | done | checked, nothing |
| `server/internal/testauth` | 31 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `server/internal/testtopology` | 913 | B/Q | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | checked, nothing |
| `server/internal/testutil` | 23 | B/Q | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | checked, nothing |
| `server/internal/transactionalmail` | 184 | B/Q | — | done | checked, nothing |
| `server/internal/tunnelcert` | 8495 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | D4 (platform targets 4 writes/s on 2 rows) |
| `server/internal/tunnelendpoint` | 18 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/tunnelv1` | 8478 | B/Q | zrok controller (shares, frontends, reserved names, limits, metrics); cloudflared ingress rules; Teleport lib/proxy, lib/reversetunnel | done | checked, nothing |
| `server/internal/usermachines` | 9348 | B/Q | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | S3; hot paths read, rest grep-screened |
| `server/internal/workers` | 410 | B/Q | HS hscontrol/app.go; Teleport lib/web, lib/limiter | done | checked, nothing |
| `server/internal/workspace` | 234 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `server/tools/metric-schema` | 110 | B/Q | — | done | checked, nothing |
| `server/tools/scenario-matrix` | 177 | B/Q | — | done | checked, nothing |
| `relay/admission` | 291 | R | TS derp/derpserver, net/udprelay; NB relay/server | done | R3 |
| `relay/cmd/paperboat-relay` | 467 | R | TS derp/derpserver, net/udprelay; NB relay/server | done | checked, nothing |
| `relay/derpquic` | 2724 | R | TS derp/derpserver, net/udprelay; NB relay/server | done | R3 |
| `relay/internal/reporting` | 1604 | R | TS derp/derpserver, net/udprelay; NB relay/server | done | checked, nothing |
| `relay/nodelifecycle` | 386 | R | TS cmd/derper (cert, mesh); NB relay | done | R2 |
| `relay/nodetls` | 72 | R | TS cmd/derper (cert, mesh); NB relay | done | checked, nothing |
| `relay/peerrelay` | 302 | R | TS derp/derpserver, net/udprelay; NB relay/server | done | checked, nothing |
| `relay/reattach` | 117 | R | TS derp/derpserver, net/udprelay; NB relay/server | done | checked, nothing |
| `relay/selfhost` | 1429 | R | TS cmd/derper (cert, mesh); NB relay | done | checked, nothing |
| `edge/cmd/paperboat-edge` | 1890 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/admission` | 271 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/auth` | 570 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/config` | 400 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/control` | 3021 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | T1 |
| `edge/internal/datacarrier` | 4575 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/diagnostics` | 336 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/edgeerrors` | 104 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/edgehttp` | 7794 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | T3; T11, T3 |
| `edge/internal/node` | 348 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/operation` | 149 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/reporting` | 1722 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/route` | 1897 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing; T7 |
| `edge/internal/runtime` | 4767 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | T2, T4 |
| `edge/internal/store` | 151 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/strictjson` | 102 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/telemetry` | 2096 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/testedge` | 329 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/tlscert` | 1092 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/internal/usage` | 684 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/tools/control-conformance` | 221 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `edge/tools/metric-schema` | 290 | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | checked, nothing |
| `shared/admission` | 291 | B/Q | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing; P1 |
| `shared/connectorprotocol` | 6080 | B/Q | Teleport lib/srv (exec, reexec); TS ssh/tailssh incubator | done | R4, T4 |
| `shared/machineservices` | 105 | B/Q | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | checked, nothing |
| `shared/managedssh` | 132 | B/Q | TS ssh/tailssh; Teleport lib/srv (node, regular) | done | checked, nothing |
| `shared/peeridentity` | 265 | B/Q | TS control/controlclient (register), types/key | done | checked, nothing |
| `shared/peernetwork` | 18 | B/Q | HS hscontrol/state, mapper; NB management/server (network map, peers) | done | checked, nothing |
| `shared/policy` | 2181 | B/Q | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `shared/reattach` | 117 | B/Q | — | done | checked, nothing |
| `shared/relaylimits` | 10 | B/Q | — | done | checked, nothing |
| `shared/runtimeattachment` | 183 | B/Q | Teleport lib/reversetunnel, lib/proxy, lib/srv/app; TS ipn/ipnlocal serve (funnel) | done | checked, nothing |
| `helper/cmd/pbh` | 693 | B/Q | TS cmd/derper deploy; NB infrastructure_files | done | checked, nothing |
| `helper/internal/containers` | 945 | B/Q | TS cmd/derper deploy; NB infrastructure_files | done | checked, nothing |
| `helper/internal/reporting` | 1604 | B/Q | TS cmd/derper deploy; NB infrastructure_files | done | checked, nothing |
| `console/lib/upload.ts` | 45 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/teams.ts` | 252 | C | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | C1, C4, C5 |
| `console/lib/api-schema.d.ts` | 28869 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/clipboard.ts` | 37 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/use-now.ts` | 55 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/account-deletion.ts` | 74 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/activity.ts` | 283 | C | Teleport lib/events (audit log, retention, pgevents) | done | C1, C4, C5 |
| `console/lib/workspace.ts` | 36 | C | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | C1, C4, C5 |
| `console/lib/machine-filter.ts` | 92 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/enrollment.ts` | 97 | C | TS control/controlclient (register), types/key | done | C1, C4, C5 |
| `console/lib/tunnels.ts` | 268 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/network.ts` | 120 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/sections.ts` | 70 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/use-time-format.ts` | 30 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/api.ts` | 509 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/machine-pages.ts` | 41 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/inspector` | 498 | C | Teleport lib/reversetunnel, lib/proxy, lib/srv/app; TS ipn/ipnlocal serve (funnel) | done | C1, C4, C5 |
| `console/lib/domains.ts` | 18 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/usage-period.ts` | 18 | C | Teleport lib/usagereporter; — | done | C1, C4, C5 |
| `console/lib/identity.ts` | 106 | C | TS control/controlclient (register), types/key | done | C1, C4, C5 |
| `console/lib/billing.ts` | 181 | C | Teleport lib/usagereporter; — | done | C1, C4, C5 |
| `console/lib/policy.ts` | 441 | C | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | C1, C4, C5 |
| `console/lib/recent-machines.ts` | 23 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/format.ts` | 135 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/machine-domains.ts` | 156 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/audit-streams.ts` | 33 | C | Teleport lib/events (audit log, retention, pgevents) | done | C1, C4, C5 |
| `console/lib/config-sync.ts` | 105 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/envvault` | 407 | C | TS taildrop (feature/taildrop); — | done | C1, C4, C5 |
| `console/lib/query-keys.ts` | 10 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/utils.ts` | 1 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/github.ts` | 101 | C | — (Git-based; compare: Teleport lib/services watcher) | done | C1, C4, C5 |
| `console/lib/privacy.ts` | 42 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/server` | 162 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/files.ts` | 111 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C1, C4, C5 |
| `console/lib/terminal` | 2626 | C | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | C1, C4, C5 |
| `console/components/machine-picker.tsx` | 153 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-remove.tsx` | 93 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/page-header.tsx` | 48 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/section.tsx` | 60 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/section-page.tsx` | 35 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-actions.tsx` | 115 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/cli` | 238 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/github` | 205 | C | — (Git-based; compare: Teleport lib/services watcher) | done | checked, nothing |
| `console/components/machine-status.tsx` | 68 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/workspace-switcher.tsx` | 91 | C | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `console/components/machine-table.tsx` | 357 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/usage-limits.tsx` | 502 | C | Teleport lib/usagereporter; — | done | checked, nothing |
| `console/components/providers.tsx` | 34 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/account.tsx` | 38 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-address.tsx` | 66 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/file-drop.tsx` | 66 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-tags.tsx` | 156 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/config` | 348 | C | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `console/components/brand-screen.tsx` | 50 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/logo.tsx` | 10 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-overview.tsx` | 316 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/share-machine-dialog.tsx` | 118 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/usage` | 328 | C | Teleport lib/usagereporter; — | done | checked, nothing |
| `console/components/crumb-picker.tsx` | 20 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-identity.tsx` | 145 | C | TS control/controlclient (register), types/key | done | checked, nothing |
| `console/components/app-sidebar.tsx` | 67 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/workspace-picker.tsx` | 61 | C | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `console/components/billing` | 510 | C | Teleport lib/usagereporter; — | done | checked, nothing |
| `console/components/server-data.tsx` | 36 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/files` | 782 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C2 |
| `console/components/environment` | 299 | C | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `console/components/delete-account.tsx` | 150 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/email.tsx` | 54 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/team` | 2035 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/not-found-game.tsx` | 413 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/domains` | 583 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/quick-terminal.tsx` | 254 | C | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `console/components/user-menu.tsx` | 102 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/access` | 1330 | C | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `console/components/tunnels` | 2725 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | C3 |
| `console/components/access-login` | 90 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/command-palette.tsx` | 123 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/ui` | 3513 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-shell.tsx` | 240 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/brand-bar.tsx` | 247 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/copy-button.tsx` | 112 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-update.tsx` | 107 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-filters.tsx` | 126 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/quota-banner.tsx` | 82 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-query.tsx` | 48 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/machine-edit.tsx` | 95 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/limit-reached.tsx` | 33 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/activity` | 556 | C | Teleport lib/events (audit log, retention, pgevents) | done | checked, nothing |
| `console/components/list-toolbar.tsx` | 195 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/row-menus.tsx` | 126 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/network` | 494 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/add-machine-dialog.tsx` | 284 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/components/terminal` | 2412 | C | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `console/components/time.tsx` | 23 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/hooks/use-mobile.ts` | 17 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/hooks/use-debounced.ts` | 11 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/settings` | 16 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/config` | 18 | C | TS ipn/store, atomicfile; NB profilemanager | done | checked, nothing |
| `console/app/(console)/terminals` | 18 | C | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `console/app/(console)/usage` | 18 | C | Teleport lib/usagereporter; — | done | checked, nothing |
| `console/app/(console)/previews` | 36 | C | Teleport lib/reversetunnel, lib/proxy, lib/srv/app; TS ipn/ipnlocal serve (funnel) | done | checked, nothing |
| `console/app/(console)/billing` | 16 | C | Teleport lib/usagereporter; — | done | checked, nothing |
| `console/app/(console)/files` | 18 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/environment` | 18 | C | TS taildrop (feature/taildrop); — | done | checked, nothing |
| `console/app/(console)/page.tsx` | 44 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/team` | 21 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/domains` | 14 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/layout.tsx` | 23 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/access` | 16 | C | HS hscontrol/policy; Teleport lib/services (RBAC, roles) | done | checked, nothing |
| `console/app/(console)/tunnels` | 36 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/machines` | 77 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/activity` | 18 | C | Teleport lib/events (audit log, retention, pgevents) | done | checked, nothing |
| `console/app/(console)/network` | 14 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(console)/account` | 37 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/(terminal)` | 60 | C | Teleport lib/srv/sess, lib/events (session recording); TS ssh/tailssh | done | checked, nothing |
| `console/app/auth` | 190 | C | Teleport lib/auth (CA rotation, tokens); TS controlbase (Noise), tka | done | checked, nothing |
| `console/app/cli` | 14 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/github` | 42 | C | — (Git-based; compare: Teleport lib/services watcher) | done | checked, nothing |
| `console/app/access-login` | 14 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/api` | 172 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/not-found.tsx` | 9 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `console/app/layout.tsx` | 27 | C | Teleport web/packages (teleport UI); Headscale-UI n/a | done | checked, nothing |
| `cli/internal/contracttest` | test only | A | — | done | test-only package, not product code |
| `cli/internal/hostruntime/contracttest` | test only | A | — | done | test-only package, not product code |
| `cli/tools` | test only | A | — | done | test-only package, not product code |
| `server/internal/contractchaos` | test only | B/Q | — | done | test-only package, not product code |
| `server/internal/contracttest` | test only | B/Q | — | done | test-only package, not product code |
| `edge/internal/contracttest` | test only | M | cloudflared connection, supervisor, ingress, metrics (edge side); zrok controller (shares, limits, metrics); Teleport lib/reversetunnel, lib/proxy | done | test-only package, not product code |
#### Horizontal scaling: per-instance server state (maintainer, audit 2026-10-23; maintainer proves it with 3 instances, one killed mid-test)

Goal: N identical server instances behind the proxy. F21-04 (one worker leader through a Postgres advisory lock with lease
and handover) comes first, right after the red server tests. Per-instance state, fix per item:
- **Breaks with N>1 (fix before scaling out):**
  - tunnelcert DistributionHub keeps pending certificate actions and results in memory
    (distribution_http.go:137-151, 613-648): edge pulls/acks on another instance see nothing. Move pending and results
    to Postgres (SQLDistributor already holds the DB).
  - The distribution-identity replay nonce cache is in memory (distribution_identity.go:59-70, 125-142): a replay
    passes on another instance. Reserve nonces with a unique insert, as connector proofs do.
  - The private-access idempotency cache is per instance (privateaccess/service.go:63-70, 382-400): same key with a
    different body slips through on another instance. Move idempotency to Postgres; the decision cache may stay at a short TTL.
  - The backup report producer uses a local directory plus flock (backupreport/producer.go): run as a single leader
    job.
  - mint ephemeral keys outside production are per process (mint.go:597-602): every environment with >1 instance
    must configure shared signing keys (startup refuses ephemeral keys when an instance count >1 is configured).
- **Wasteful, put behind the F21-04 leader lock:** team billing reconcile (N Stripe reads/min), edge DNS
  mutations, edge-set ReconcileAll, operational health probes, support snapshot loop, self-host ingress probe,
  diagnostic upload cleanup, tunnelcert coordinator alert dedupe (N duplicate alerts).
- **Fine as is (documented):** row-claiming workers (SKIP LOCKED / leases / advisory locks: mail, DNS, outboxes,
  audit streams, team identity, edge assignments, stale nodes, certificates); Postgres-backed rate limits and lockouts
  (device auth, operator login); the machine-events hub (LISTEN/NOTIFY on every instance); browser-terminal and
  connector-control WebSockets pinned to one instance for their lifetime (state in Postgres; the proxy must allow
  upgrades and long holds); bounded per-instance caches (bandwidth gate 10 s, inventory single-flight); per-instance
  support sampling quotas and stream caps (they scale with N; documented).
- **To verify:** worker_tunnel_expiry, terminal-code and operator-reporting rate limits, inspector hubs,
  diagnostic upload storage location, any server path that must push to a connector stream synchronously (it must
  reach the holding instance); release artifacts served from each instance's local directory (deploys must be identical).

#### Regional control plane: clients pick a nearby region, no global load balancer (maintainer; user decision 2026-10-26; record now, build later)

Decided:
- No global load balancer and no anycast front for the API. The server's discovery response carries a **control-plane
  region map**, built the same way as the relay and tunnel discovery: per region an id, an API origin and a health
  state, signed with the rest of discovery.
- Clients (CLI, daemon, console) measure latency to each region's API origin, pick the healthiest nearby one, and keep
  it while it stays healthy. They fail over to the next region by measured order. The choice is cached with the
  discovery generation and is never a hardcoded host (AGENTS.md: domains come from discovery).
- A region may run its own load balancer in front of its instances (F21-04's single worker leader still holds per
  database). Regions are added or drained by changing the map, not DNS.
Prior art: Tailscale's DERP map (tailcfg/derpmap.go:19,117), where control sends regions and the client ranks them
by measured latency (net/netcheck/netcheck.go:115 PreferredDERP), while the control URL itself is one origin
(ipn/prefs.go:42 DefaultControlURL); NetBird management hands clients their signal and relay endpoints
(management/internals/shared/grpc/conversion.go:85-100), and clients connect to those, not through one front.
OPEN, not final: one central writable database with regional read replicas. To design before building:
- freshness bounds per read: which reads may be served from a replica;
- authorization and revocation consistency: a revoke must never be readable as granted from a lagging replica, so
  authz reads either go to the primary or carry a generation floor;
- write routing and latency from far regions;
- primary failover, and what clients see during it.
Rows: none yet; becomes an F21 row when the build is scheduled.

Design (approved by the user 2026-11-04, with graceful drain and LSN read-your-writes):
- **One primary, regional replicas.** Every instance has two pools: `primary` (writes, LISTEN, advisory locks,
  migrations; the F21-15 direct pool) and `replica` (its region's read replica, or the primary where none exists).
  Teleport runs many auth servers on one shared Postgres backend (lib/backend/pgbk/pgbk.go) and fans changes out
  through one change feed (pgbk/background.go, wal2json); we keep that shape: one database of record, no
  multi-writer. Headscale and NetBird management run a single control instance on one database and have no answer here.
- **Read-your-writes on replicas** (Aurora/PlanetScale/CockroachDB follower-read model; Postgres
  `pg_current_wal_lsn()` / `pg_last_wal_replay_lsn()`). Every write response carries the primary commit LSN in
  `Paperboat-Commit-Position`. Clients (CLI, daemon, console) keep the highest position they have seen and send it on
  each read. A replica serves the read only once `pg_last_wal_replay_lsn()` is at or past that position; otherwise the
  read goes to the primary. Reads without a position may use any in-rotation replica.
- **Never from a replica, whatever the position:** authentication (sessions, API tokens, machine credentials),
  authorization decisions, grant and descriptor issuance, revocation and billing checks, idempotency and nonce
  reservations, and any read followed by a write in the same request. These use the primary, or a replica with a
  generation floor proven past the generation the decision depends on. So a revoke is never read as granted.
- **Lag.** Each instance samples its replica's replay lag every 5 s. Above 2 s it drops the replica out of rotation
  and reads the primary; it rejoins after 30 s under the bound.
- **Pushes.** NOTIFY does not reach hot standbys, so every instance's machine-events hub LISTENs on the primary pool
  (one connection per instance).
- **Write routing.** No forwarding layer: a far-region instance writes to the primary over the WAN. Keep a request
  to one transaction (one round-trip set); budget about RTT x statements per write path and measure enroll, pair,
  grant and revoke from the farthest region before adding regions.
- **Built 2026-11-05 (reliability worktree, part 5a):** `database.replica_dsn` opens the region's replica pool; every
  instance samples its lag every 5 s (out at once above 2 s or on a failed sample, back after 30 s under);
  `db.ReadPool(ctx, position)` picks the replica only when in rotation and replayed past the client's position.
  With `regional_domain` set, successful writes answer `Paperboat-Commit-Position` (exposed and allowed in CORS) and
  reads pass the client's header to ReadPool. First replica readers: the discovery region map and the operator
  server-node list. Clients sending the header back waits for maintainer (CLI) and maintainer (console).
- **Primary failover.** The database provider promotes a replica (PlanetScale, F21-15). During it, writes return a
  typed 503 `control_plane_read_only` with Retry-After; replica reads keep serving; machines keep their last verified
  state (F21-01 offline-until-revoked). Instances reconnect the primary pool with backoff; no app-level election.
- **Drain never cuts** (Envoy drain_listeners and HTTP/2 GOAWAY; Cloudflare graceful restarts). A draining node:
  - leaves the region map and fails its load-balancer health check at once, so it gets no new clients;
  - sends HTTP/2 GOAWAY on its connections, and WebSocket close 1012 (service restart) with a reconnect hint on its
    long streams (browser terminal, connector control, event waits);
  - lets clients reconnect transparently to the next node and resume their state: a terminal resumes at its replay
    cursor, an event wait at its generation. Users see no error;
  - exits only when its connections are gone, or after a long cap of 30 min.
- **Server nodes are equal peers** (user rule 2026-11-04). There is no primary, main or central node. Any node's
  operator panel can enroll, drain or remove another. Enrollment state lives only in the shared database; background
  jobs stay on the F21-04 database lease, never a fixed node. Losing any one node breaks neither enrollment, discovery
  nor the panel: every other node reads the same rows.
- **Enrollment mirrors global edges** (selfhost/enrollment.go Enroll and reserve; POST /v1/admin/selfhost/enrollments).
  1. The node starts from the server's own image in `unenrolled` mode, with no DB credentials. It generates a TLS key and
     an enrollment key and prints a one-time claim code: its address, its SPKI pin, a secret and an expiry of 15 min.
     This is the edge code format (parseCode, claimTarget).
  2. An operator pastes the code into any node's panel (`POST /v1/admin/server-nodes/enrollments`, operatorRoute).
     That node dials the new one pinned to the SPKI (pinnedTransport) and checks the signed inspection: version,
     region, public address. It then writes `control_server_nodes(id, region, endpoint_host, tls_spki_sha256,
     state=registered, ...)` in one transaction, as reserve does for control_tunnel_nodes.
  3. The same request delivers the node's runtime bundle over the pinned channel: primary DSN, the region's replica
     DSN, the shared signing and secret keys (F21-56 requires shared keys once N>1), and the edge and relay config. The
     bundle is sealed to the node's enrollment public key (NaCl box), so the code alone reveals nothing. The node
     stores it root-only and then restarts as a full node.
  Prior art: Teleport nodes join with a one-time provision token, then receive their identity from auth
  (~/ref/teleport/lib/auth/join/join.go:350 Register, lib/auth/register.go); auth servers are equal peers on one
  shared backend (lib/backend/pgbk/pgbk.go). Headscale and NetBird management have no multi-node enrollment.
- **Regional name, DNS and TLS.** The name is `<region>.<control domain>`, for example mumbai.<domain>; the control
  domain comes from server configuration, never code. DNS goes through the existing edge DNS reconciler
  (app/edge_dns.go, tunnelcert/cloudflare_dns.go), which publishes A/AAAA records only from verified address metadata.
  TLS goes through tunnelcert: ACME DNS-01 for the regional name, delivered through the DB-backed DistributionHub
  (F21-52). A node serves its regional name only after its certificate arrives; until then it is `registered` but
  not in the map.
- **Regional TLS and DNS, as built** (2026-11-05, reliability worktree, proven live on an database-test-host staging stack): one
  certificate for `*.<regional_domain>` covers every region. The worker leader issues and renews it with DNS-01 through
  the existing ACME issuer (regional zone only; renew 30 days ahead; 10 min backoff after a failure) and stores it
  encrypted in the shared database; every node serves it on `regional_https_address`, reloading each minute. No port
  80, one issuance for all regions and nodes (the cert-manager and Caddy pattern for one name on many nodes). A node
  turns `active`, and so enters the map, only once it serves the certificate. DNS: per region, A/AAAA records for the
  public addresses the region's active, fresh nodes declared at enrollment, written by the leader only when the set
  changes (or every 5 min). Propagation of both is checked at the zone's authoritative nameservers (lego's precheck),
  not a recursive resolver. A node with `regional_domain` set refuses to start without its DNS token and ACME key.
  Prior art: headscale terminates its own TLS (hscontrol/app.go:1166 getTLSSettings); Teleport auth servers share one
  backend (lib/backend/pgbk/pgbk.go).
- **Health and the map.** A node heartbeats its `control_server_nodes` row every 10 s with its state, version and
  replica lag. Discovery lists the regions that have at least one healthy node. Clients pick the nearest healthy
  region and fall back to the next (see the region map above).
- **Drain and removal** from any node's panel: `draining` runs the graceful drain above. `remove` deletes the DNS record and revokes its certificate and
  keys, then deletes the row once it has stopped heartbeating. Rotating the shared keys after a removal is a separate
  operator action.
- **Failover.** A dead node drops out of the map after 30 s without a heartbeat; clients move to the next region.
  Database primary failover is as described above. No node holds a role that needs an election.
- **Operator surface:** API only (user decided 2026-11-05): GET /v1/admin/server-nodes, POST /v1/admin/server-nodes/enrollments, .../{id}/drain, .../{id}/remove; no console page.

#### Inspector: a fresh machine's first preview can miss its first requests (maintainer, open, 2026-10-24)

Seen 2 times in about 40 runs of check-preview-inspector.js (aaf3f121 on 2026-10-23, round 21 dev-97238006): on the
first preview of a just-paired machine, the first two requests sent after capture-on never appeared in the list for
20 s; requests sent about 40 s later were captured. Not reproduced since in 15 deliberate first-preview runs on
freshly paired linux-test-vm-2 containers (round 21) or in 23 earlier fresh-preview runs.

Ruled out, with evidence:
- Capture-policy timing in the daemon: SetPolicy is synchronous in the one shared store, and
  TestInspectorPublicCaptureAndReplay sends a request the moment the policy PUT is answered on a carrier that
  already served traffic. It is captured.
- Grant generation drift: all inspector credentials for the failing and the passing previews carry generation 1,
  one grant each (read-only query of inspector_credentials).
- Edge stream reuse: the edge opens one stream per request (DataCarrierPreviewTransport.RoundTrip), and the
  capture decision is made per stream.
- List cursor: a real bug was found and fixed in 1fbfd9f2 (a request that finished after a later one was skipped),
  but that skips one record, not all of them.

Edge side fixed (maintainer, 2026-10-26): a request to a preview admitted on the edge but not yet attached got a 404 (RouteState hides pending admissions); it now waits up to 3 s for the route, else 503 `preview_starting` with Retry-After 1 (TestAdmittedPreviewWaitsForItsRouteInsteadOfNotFound). Still open: the edge answering the first requests before the new preview's route serves them (a no-cors fetch
resolves on an edge error too), or the carrier's run restarting (its exit Revokes the policy and captures). The
check now reports unsent requests, stops when nothing arrives, and leaves capture on. ~/lanei/loop.sh logs the
origin's hits and the daemon's own list (pb preview inspect), so the next occurrence names the side.


#### Managed SSH: review and fixes (maintainer, review 2026-10-26, waits for coordinator review)

Baseline tag `managed-ssh-before-review` (a3dd2ffa). One implementation at the end: a rewritten component is built next to the old one, passes the full check list below, and the switch-over commit deletes the old path. No compatibility layer, no versioned names. Sources: our path (cli/cmd/pb/ssh_*.go, cli/internal/managedssh, cli/internal/hostruntime/runtime/managed_ssh_*, server/internal/managedssh, httpapi/managed_ssh_handlers.go); prior art ~/ref/tailscale/ssh/tailssh + cmd/tailscale/cli/ssh.go, ~/ref/netbird/client/ssh, ~/ref/teleport/lib/srv (the tree has no tool/tsh, so Teleport's client side is not compared).

| Aspect | Us (HEAD 63d06489) | Tailscale | NetBird | Teleport | Verdict | Fix |
|---|---|---|---|---|---|---|
| Setup round trips | Per `pb ssh`: GET client-configuration (ssh_command.go:44). Per OpenSSH KnownHostsCommand run: client-configuration + ssh-target (cache) + GET ssh-host-keys (ssh_command.go:213-228). Per ProxyCommand run: client-configuration + **live** GET ssh-target (ssh_resolve.go:101, cache never used; on error a second machines + ssh-target) + POST ssh-descriptor with up to 5 issuance retries, an access-session row per call (source.go:73, issuance_retry.go). Warm: 4 + 2k control calls. Measured ~13 ssh-target/attempt, setup p50 34.7 s (PLAN row "Managed SSH streams per host") | 0: policy from the in-memory netmap, WhoIs local (tailssh.go:597-686) | 0 to management: ProxyCommand asks the local daemon (proxy.go:56-95) | Cert check local (authhandlers.go:375); one auth-preference read, semaphore only with max_connections (session_control.go:250,352) | **Wrong design** | **Rewrite** the client setup path: proxy and known-hosts ask the local daemon only; target, host keys and authorization come from what the daemon already holds (topics), no control call per connection (slice 3, 4) |
| Where the 35–47 s goes | Not measured per phase; code timeouts (2 min admission, 3 s probe, 10 s dial) do not explain it | — | — | — | Unknown | Measure first (slice 1); the rewrite must not be justified by a guess |
| Auth model | Per-CLI-session ed25519 key, private key in the credential store, served by pb's agent; public key in the registered user's authorized_keys via the host reconciler (managed_ssh.go:27-78, authorized_keys_unix.go) | Network identity, no keys (NoClientAuth, tailssh.go:421-460) | JWT in the password field (server.go:678-727) | User certificates only (authhandlers.go:381) | Right for us: works with the machine's stock sshd, VS Code, scp, rsync | **Fix in place**: key line gets no options today (authorized_keys_unix.go:126-129); keep, decided in D2 |
| Host key trust, no TOFU | KnownHostsCommand returns the server's active set, StrictHostKeyChecking yes, known_hosts none (openssh_config_unix.go:342-346); `pb ssh trust-host` promotes a pending set | Host keys in Hostinfo → netmap → generated known_hosts (cli/ssh.go:159-196) | Daemon verifies against the peer's stored key (common.go:43-70) | Host certs (authhandlers.go:853) | Parity | **Fix in place**: serve host keys from the daemon's peer state, not a live GET (slice 3), so it works offline |
| OS user mapping | One registered user per machine (pb setup = user.Current()); the automatic per-session key is written only into that user's authorized_keys. A different user went through the same forced-key client options, so they could never sign in and got sshd's "Permission denied (publickey)" | SSHUsers map, "=" never root (tailssh.go:1235-1256) | Login name = OS user, root blocked by default (engine_ssh.go:317) | Role logins (ssh_access_checker.go:41) | Decided (user, 2026-10-26): **the managed key is for the setup user only; any other user is never refused by us.** They reach the machine over Paperboat's transport (same access rules) and authenticate with their own key, agent or password via the machine's sshd | **Fix in place** (done): generated ssh config scopes the managed key (agent, public key file, BatchMode, no password) to `Match host <alias> user "<setup user>"`; `pb ssh` adds those options only for the setup user; ErrSSHUsernameNotAllowed deleted. Failures for other users read as sshd's normal auth failure |
| Live-session revocation | Key removal rewrites authorized_keys; live sessions keep running (reconcile never closes streams; only turning managed_ssh off does, dispatcher_ssh.go:109-121) | Policy change re-checks every conn, "Access revoked." (tailssh.go:807-821) | Not killed on user removal | Locks kill at once (monitor.go:496-548) | Gap | **Fix in place**: the host maps each SSH stream to its actor; on a machine_access change it closes streams whose actor lost managed_ssh; pb prints "access revoked" (slice 6) |
| Concurrency limit and error | None of ours (removed 2026-10-26, user decision): the machine's sshd limits apply. A connection sshd ends before its banner reads as pb's "the machine's SSH server closed the connection before answering" (copySSHProxy) | No cap | No cap | "too many concurrent ssh connections for user %q (max=%d)" | Done | Cap, ErrSSHHostBusy and the busy refusal deleted |
| scp/sftp/rsync, IDEs | Machine's sshd serves all; pb wrappers (ssh_tool_wrappers.go); VS Code Remote-SSH checked on the native fixture, reconnect not; JetBrains not | Embedded SFTP, scp/rsync via exec | SFTP default off | SFTP/SCP gated by role | Parity (better than Tailscale/NetBird: real sshd) | Every reconnect pays the full setup today; fixed by slice 3. Add checks (slice 8) |
| Port/agent/X11 forwarding | Linux/macOS: whatever the machine's sshd allows; Windows: our sshd_config disables all (windowsopenssh/config.go:45-58) | Per-rule allow flags; no X11 | Local/remote flags; no agent/X11 | Role flags with clear errors | Inconsistent across platforms | D2: decide one policy; no new feature until decided |
| Env | Nothing sent or accepted by us; sshd's AcceptEnv decides | AcceptEnv rules, LD_*/DYLD_* refused | Whitelist | Accepts all | Parity (sshd decides) | Keep |
| Audit / recording | Server audit for key/target/host-key writes; per connection only the descriptor's access-session row; nothing from the host; bridge byte counts discarded (dispatcher_ssh.go:55) | auditd login, optional recorder | Logs only | Full events + recording | Gap (and the descriptor row goes with slice 4) | **Fix in place**: host reports SSH session start/end (actor, bytes, outcome) through the F21-11 audit queue (659f54c3), online and offline. Session recording is a new feature (D4) |
| Error messages | Refusals reach the user only as OpenSSH's text; pb adds a hint only for a pending host key (ssh_command.go:61-75) and ssh_target_not_ready via daemon health | "tailscale: tailnet policy does not permit you to SSH as user %q" etc., in the banner | Login-stage messages | Specific messages per refusal | Gap | **Fix in place**: one typed reason per refusal (busy, no access, revoked, machine offline, SSH not set up, Paperboat unreachable), printed by pb proxy to stderr with the next step (slice 2) |
| Server down | Client: nothing works (live ssh-target, ssh-host-keys, descriptor). Host: any failed refresh **removes every managed key** (managed_ssh_reconcile_unix.go:122-124) | Cached netmap keeps policy; existing and new sessions work | Last auth stays | Works if CA/roles cached | **Contradicts the user decision "offline until revoked" (F21-11)** | **Rewrite** this behaviour: the host keeps its last verified key set until a revocation it received (bounded by key expiry); the client side runs from daemon state (slices 4, 5) |
| First-attempt failure | "canceled before shell, or EOF exit 255" on a fresh pair (PLAN, maintainer) | — | — | — | Bug | Reproduce in slice 1; fixed by slice 3/4 or its own fix |

Per component:
- Client setup path (`__ssh-proxy`, `__ssh-known-hosts`, ssh_resolve.go, ssh_target_cache.go, per-connection client-configuration): **rewrite**. It is built around live control calls per OpenSSH helper run. The daemon already holds peer state (machine-events topics, F21-01 peer documents), so the helpers become local reads.
- Per-connection authorization (POST ssh-descriptor + access-session row): **rewrite onto F21-11**. The host already gets signed access rules for offline admission (14e4ca79); using them for every SSH stream, online or offline, is one path instead of two. Needs maintainer agreement (D1).
- Host bridge (managedssh.Host, BridgeSSH, MaxStreams): **fix in place** (typed refusals, stream→actor map, close on revoke, session events).
- authorized_keys writer: **fix in place** (keep last set on a failed refresh; it is already atomic, locked and bounded).
- Server managedssh service (keys, targets, host keys, operations): **fix in place**; delete the descriptor endpoint for SSH after the switch (D1).
- OpenSSH config writer (paperboat_config, Include): **fix in place**; points the helpers at the daemon.

Delete (at the switch-over): live GET ssh-target and GET ssh-host-keys from the OpenSSH helpers; `resolveSSHCommandTargetLive`; ssh-targets.json cache and its unlocked read-modify-write (ssh_target_cache.go); GET client-configuration per helper run; POST ssh-descriptor for SSH and its access-session row (D1); "remove every managed key on a failed refresh"; ErrSSHUsernameNotAllowed if D3 keeps one user.

Add: daemon-served SSH target + host keys (local API) kept current by the existing topics; typed SSH refusal codes on the peer stream and pb texts; host stream→actor map and close-on-revoke; host SSH session events into the F21-11 audit queue; checks below.

Slices (each with its check; maintainer reviews cli/):
1. **Measure** one `pb ssh` and one plain `ssh <alias>` on a warm and a fresh pair: phase timings (-vvv timestamps), count of ProxyCommand/KnownHostsCommand runs and of control calls. Check: smoke `ssh-setup-phases` records them; reproduce the first-attempt EOF. No product change.
2. **Typed refusals**: no access, offline, not set up, unreachable (and sshd closing first) → pb text on stderr, exit 255 kept for OpenSSH callers. Check: workspace test per reason.
3. **Daemon-served helpers**: proxy and known-hosts read target/host keys from the daemon. Check: test asserts 0 control requests from the helpers with a warm daemon; smoke setup p50 < 2 s (plain ssh on the link is 0.32 s).
4. **Host-held authorization (D1)**: SSH stream admitted by the host's signed access rules; no descriptor POST. Check: daemon test admits/refuses as the server's rules would (shared with F21-11 "access_rules equal descriptor decisions"); smoke ssh with the descriptor endpoint blocked.
5. **Offline**: host keeps the last key set; client works from daemon state. Check: staging smoke with the control plane stopped: existing user connects, a user revoked before the outage is refused (F21-11 smoke control_outage_sessions).
6. **Live revoke**: revoke the CLI session or the grant → the open session closes within seconds, pb prints "access revoked". Check: smoke `ssh-live-revoke` with a timing bound.
7. **Session audit**: start/end per session in the workspace log, online and offline. Check: PG test on the audit backlog; smoke reads the event.
8. **Tools and IDEs**: scp, sftp, rsync, VS Code Remote-SSH connect + reconnect, JetBrains Gateway connect. Check: smoke ids per tool; IDEs as a recorded manual step re-run at the switch.
Switch-over: when slices 2–8 pass on L and M, one commit deletes the old path; the full SSH smoke set (existing ssh checks + the new ones) must pass on that build.

Decisions (coordinator, 2026-10-26):
- D1 yes: every SSH stream is admitted from the host-held F21-11 access rules, online or offline; the per-connection descriptor for SSH is deleted; the OpenSSH helpers read only from the daemon (zero control calls per connection). Coordinated with maintainer (F21-11 server) and maintainer (offline admission).
- D2: one forwarding policy on every platform: sshd's defaults (port and agent forwarding as OpenSSH allows, X11 off). The Windows sshd_config follows in the Windows phase.
- User decision 2026-10-26: `pb ssh bob@m` and plain `ssh bob@m.<name>` are NOT refused when bob is not the setup user; the managed key applies to the setup user only (F5-11).
- D3, D4: not now; parked in WHITEBOARD.md ("Managed SSH: more OS users, session recording").
- A failed refresh never empties authorized_keys: the host keeps its last verified keys until an explicit revocation it received.
- Live sessions are closed on revocation (slice 6, required).
Order: slice 1 first, then 2–8. Nothing is ticked until the full SSH check set passes on L.

Slice 1 result (2026-10-26, dev-8910ed61, two fresh linux-test-vm-2 containers, target OpenSSH 9.6, ~/lanei/sshphase.sh):
plain `ssh -vvv <alias> echo ok` 4.5–6.9 s (5 runs, p50 ~5 s), `pb ssh <alias> -- echo ok` 4.6–5.6 s; plain
OpenSSH on the same link is 0.32 s. Per connection: ProxyCommand runs once and takes 2.4–4.9 s to the remote
banner (client-configuration + live ssh-target + ssh-descriptor POST + peer dial); KnownHostsCommand runs
**twice** (OpenSSH asks once to order host key algorithms, once to verify), 0.85–1.2 s each, each with its own
client-configuration + ssh-target + ssh-host-keys calls; kex, auth and the command take < 0.2 s. So ~95% of
setup is our helpers' control calls, the target of slices 3 and 4. 40 in parallel from one container hit the
container's process limit ("fork failed"), so the 35–47 s at 40 parallel (maintainer, laptop) is not reproduced
here; re-measure it on the rewrite with the same load from a host without that limit.


#### Terminal: review and fixes (maintainer lead; maintainer web, maintainer server + edge; user priority 2026-10-26)

**Ask (user).** Terminals have been flaky since the beginning. Go through every line and every decision. Make it durable
and efficient, delete what is not needed, add what is missing. No new features. Pre-launch, so each piece is either
rewritten or fixed in place. A rewrite may sit next to the old code while it is built, but the switch-over commit deletes
the old code and repoints every consumer: one implementation, no compatibility path, no version suffix. A rewritten piece
must pass the full failure matrix before the old one is deleted. Tag `terminal-before-rewrite` when slice 1 starts.

**How it was reviewed.** Nine reviews, each covering flows, state owners, limits, failure modes, deletes, gaps and prior
art, with `path:line` on every claim. K1 host session + pty, K2 session owner + bridge, K3 host server + protocol,
K4 browser broadcast + carrier, K5 native transport, K6 native commands, C web (console, WASM), V server + edge, plus
maintainer's failure matrix on devices (`tools/device-smoke/terminal_matrix.py`, `web_terminal_matrix.mjs`). Prior art read
in `~/ref`: teleport, tmux, mosh, EternalTerminal, wezterm (mux, client), vscode (platform/terminal), ttyd, wetty,
xterm.js.

**What fails today (reproduced on round 31, linux-test-vm-2 containers, 2026-10-26).**
- Native passes: two windows, rejoin (tail replay), 90 s sleep with keys typed asleep, 45 s network drop, 2 min server
  outage, resize storm, 256 KiB and bracketed paste, 50 MB output, 20 sessions.
- **N6 Ctrl-C under output.** Run `yes` and press Ctrl-C: nothing happens and `yes` keeps running for minutes.
  - The capture loop takes `session.opMu` per 32 KiB chunk (`session/manager_capture.go:16-36`).
  - Input only `TryLock`s that mutex every 5 ms (`pty/input_context.go:14-35`), so under a flood input never runs.
  - On the stream, input is applied in the main loop and its reply queues behind output on one writer with no
    deadline (`server/server.go:162`, `server_io.go:89-103`, `websocket.go:188`).
  - The 1 MiB per-attachment queue evicts the client, which reattaches every few seconds.
- **N11 (new) a native terminal reconnects about every 256 keystrokes,** with a ~2.3 s freeze.
  - Every keyed input gets a `terminal_input_result` event.
  - The client publishes each one into a 256-slot channel that nothing reads, and closes the connection when it is
    full (`tunnel/helper_websocket.go:669-686`).
  - Measured: 200 keys, 0 reopens; 300 keys, 1 reopen.
- **Browser terminals on prod barely attach.**
  - 189 of 197 tickets in 2 h were never redeemed.
  - A drained runtime carrier still gets 200 tickets. The edge then refuses the WS with 502/503
    (`edge/internal/edgehttp/browser_terminal_bridge.go:24`), which JS cannot read, so the console loops Connecting ↔
    Reconnecting.
  - The 9th unconsumed ticket is refused (`browserterminal/tickets.go:97-102`) as 404 `terminal_session_not_found`, which
    the console shows as "No access" (`use-terminal-connection.ts:115-116`).
  - The edge logs nothing. maintainer own the live outage.
- **Server op queue.** 133 terminal operations on revoked or deleted machines stay pending forever (attempts up to 2606),
  and the worker reports a failure every few seconds (V F3).
- Native input around a drop: the earlier sticky-uncertain bug, now local commit 03990d13, under review.

**The design: one model for native and browser.**
1. **Session (host).** One owner process holds the PTY and child, and survives runtime updates (K2, keep).
   - Input and output never share a lock: output capture owns the PTY read side and the history, input owns the write
     side and the journal. Input admission blocks with its context; there is no try-lock polling.
   - One session lock is taken only for state changes (attach, detach, resize, lifecycle), never per chunk or per key.
   - `Manager.Create` no longer holds the global lock across spawn and store writes (K1).
   - Prior art: tmux reads the pane and writes input on separate events (`~/ref/tmux/server-client.c`); VS Code's pty host
     serialises only state (`~/ref/vscode/src/vs/platform/terminal/node/ptyService.ts`).
2. **Output: sequence, display acks, checkpoint resync on lag.**
   - Every output byte has a session sequence. The client acks what it has displayed, not what it has read ahead
     (K5: today's cursor advances at read-ahead).
   - Replay from the history ring only serves a gap that is still inside the ring; a byte ring cannot rebuild screen state.
   - A viewer that falls behind, or whose gap left the ring, gets an atomic checkpoint at sequence S, cut under the
     same lock that appends output, then only bytes after S on the same stream. Stale queued output for that viewer is
     fenced (dropped by sequence), so nothing before S arrives after the checkpoint.
     - The checkpoint holds the screen, cursor, modes, alternate buffer, parser state and size.
     - Scrollback above the checkpoint's 200 lines is lost and is not redrawn.
   - A permanently slow viewer gets at most one checkpoint per 5 s. In between its output is dropped, never queued
     without bound, and it is never disconnected for lag.
   - The browser checkpoint and publication start are the same atomic step (K4 M1).
   - Prior art: mosh state sync, the newest screen state to a lagging client (`~/ref/mosh/src/statesync/complete.cc`,
     `~/ref/mosh/src/network/transportsender-impl.h`); VS Code flow control at 100 000/5 000 chars
     (`~/ref/vscode/src/vs/platform/terminal/common/terminal.ts`).
3. **Input: stable ids, fenced connections, one cumulative cursor.** Settled before slice 3.
   - Every input chunk has a stable id: the attachment id plus a sequence. The attachment id is stable for the life of
     the client terminal, across transport loss.
   - Ids are scoped by the session generation and by a connection epoch that the host bumps on each attach. Frames
     carrying an older epoch are refused, so a half-dead connection can never write after its replacement attached.
   - The host keeps one cumulative applied cursor per attachment id and generation, and acks it cumulatively. The client
     resends everything above the acked cursor in order, before new input; the host drops what it has already applied.
     The cursor and its dedupe state survive transport loss and detach. The acked prefix is reclaimed; only the
     cursor stays until the session generation ends.
   - A PTY write and a journal update cannot be atomic. After an owner crash with a chunk in flight, that chunk is
     reported as uncertain ("the last keys may not have reached the shell") and never replayed blindly.
   - The queue lives in the `pb` process or the WASM terminal, not per daemon stream (K5). Input is chunked at 4 KiB
     with a bounded queue (1 MiB), and a full queue is visible backpressure, not a transport loss.
   - No per-keystroke result events: the cumulative ack replaces them (the unread result channel is deleted in slice 2).
   - Prior art: Eternal Terminal sequence reconnect (`~/ref/EternalTerminal/src/base/BackedWriter.cpp`,
     `BackedReader.cpp`).
4. **Size: latest active controller (decided 2026-10-26).**
   - The PTY follows the controlling attachment (owner or interactive) with the most recent activity, ordered by the
     host's clock. Typing counts, and so does focus, debounced 500 ms.
   - Activity is tracked per stable attachment id; a reconnect alone never steals control. Viewers never resize.
   - Every attachment is told the size and which attachment set it.
   - Prior art: tmux `window-size latest` (`~/ref/tmux/options-table.c:1929`, `resize.c`).
   - Replaces today's mix: native last-resize-wins, browser first-owner-only, and an interactive joiner's first resize
     changing the owner's PTY (K6).
5. **Liveness that never fires because a tab was hidden.** Browsers throttle background page timers (Chrome to about
   one per minute after 5 min hidden), so no liveness may depend on page or worker JS timers.
   - Browser: the edge sends WebSocket protocol pings, which the browser answers itself, and treats a missing pong as
     dead. The edge and server tolerate application silence for at least 10 min. Nothing in the console or WASM
     declares a socket stale from elapsed time.
   - On visibility, focus or `online`, the client sends one probe. Its deadline (5 s) starts at that moment, so time
     spent hidden never counts, and an open socket is never torn down because a timer fired late.
   - Native (`pb` and every hop under it): application heartbeat every 10 s in both directions; a peer silent for 25 s
     is dead and the client reconnects at once.
   - Writes have deadlines on every hop, including host output writes, which have none today (K3).
   - Prior art: Teleport pings with a write deadline (`~/ref/teleport/lib/web/ws_io.go:74-95`); VS Code 5 s heartbeat
     with a two-stage warning (`~/ref/vscode/src/vs/platform/terminal/common/terminal.ts:476-493`).
   - maintainer's proposed rolling read deadline in the WASM (C top-1) is replaced by this.
6. **Reconnect: one state machine, written once in Go, used by `pb` and by the WASM** (the WASM is Go already).
   - States: connecting, attached, reconnecting (reason, attempt, next retry), failed (reason, action).
   - Backoff 0.5 s doubling to 15 s per outage, reset on success (today a 6-attempt budget for the life of the terminal,
     `shared/reattach/recovery.go:22-23`).
   - Immediate retry on network change, wake, `online` or `visible`.
   - "Failed" after 2 min of continuous failure, or on a permanent reason (revoked, ended, no access, limit), with the
     reason and a Reconnect action.
   - The status is shown even when the native status bar is off (K6).
   - Prior art: wezterm client reconnect loop (`~/ref/wezterm/wezterm-client`); VS Code reconnect grace
     (`~/ref/vscode/src/vs/platform/terminal`).
7. **Errors: typed, end to end (contract with maintainer and maintainer, 2026-10-26).**
   - The edge closes the browser WS only after accept, with a reason that is a typed code, optionally followed by
     ": <human text>". The code is what everyone branches on; the WS number is a fallback class for tools.
     | reason | WS | meaning |
     |---|---|---|
     | `no_access` | 4404 | missing, not shared, or access changed |
     | `ended` | 4409 | the terminal closed or exited, or its session ended |
     | `revoked` | 4401 | access ended while attached |
     | `expired` | 4408 | ticket lapsed or used: fetch a fresh one silently |
     | `limit` | 4402 | plan limit of open terminals |
     | `bandwidth_quota_exhausted` | 4403 | hosted allowance used up |
     | `too_many_pending` | 4429 | retry after a wait |
     | `machine_unavailable` | 4503 | the machine did not answer through its edge |
     | `edge_unavailable` | 4504 | edge or control plane cannot admit now; retry shortly |
     | `protocol` | 4500 | framing error; unknown reasons and numbers map here |
   - 1006 means the network dropped, so reconnect.
   - The WASM is the only mapper. Its state event carries `code` (typed), `message` (human text), `close` (the raw
     number, for diagnostics) and `clean`. The console branches only on `code`.
   - Ticket HTTP errors: 409 `machine_edge_unavailable` (same UI as `machine_unavailable`), 409
     `terminal_session_closed`, 429 `too_many_pending_tickets` with Retry-After, 404 only for "not yours".
   - Native: host → daemon → `pb` carry the same codes in the terminal end event; no string parsing anywhere.
8. **Admission and lifetime (server + edge).**
   - Admission credentials (tickets, attach credentials) expire only before admission. A live stream is never cut for
     credential expiry, which would bring back periodic disconnects.
   - Session lifetime (a guest code ending, a browser session ending) and revocation are separate events, pushed to the
     live stream as typed ends.
   - A ticket is issued only for a live carrier: 409 `machine_edge_unavailable`, with the other edge of the account pair
     tried first.
   - A carrier lost between ticket and redemption is tolerated: the edge retries the other carrier, then ends with
     `machine_unavailable`.
   - The pending-ticket cap returns 429 `too_many_pending_tickets`.
   - Browser JS cannot read a failed WebSocket handshake. An authenticated status endpoint
     (`GET …/terminal-sessions/{id}/browser-status`) explains a pre-upgrade refusal and when to retry.
   - Plan-limit counting reserves at admission instead of counting closed after 2 min (V5).
   - Edge and control-socket refusals are logged and counted (V8).
9. **One wire framing.**
   - The browser record layer is the native PBT1 typed record (K3 D10); one JSON frame envelope, not four (K3 D11).
   - The terminal protocol is changed with all its producers and consumers in one commit per slice.

**Per component: rewrite or fix in place.**
| Component | Verdict | Why |
|---|---|---|
| `hostruntime/pty` | fix | sound; drop the dead `WriteInput`, merge the per-OS validators |
| `hostruntime/session` locking + fanout delivery | rewrite the input/output/attachment core | one lock for output, input and state is the root cause of N6; eviction-on-lag is the wrong policy |
| `hostruntime/session` history, journal, recover, store | fix | journal GC on detach, ring size from config, delete dead APIs |
| `sessionowner`, `workloadbridge` | fix | design right (PTYs outlive updates); graceful stop must not kill shells; remove the 256/1024 machine-wide cliffs; one bridge deadline |
| `hostruntime/server` terminal stream loop | rewrite | input must not wait behind output; heartbeats both ways; typed ends; deadlines |
| `hostruntime/protocol` | fix | delete duplicates (ack text frame, replay action, WriteBinary ×3, four envelopes) |
| `browserbroadcast(server)` | fix | atomic checkpoint+join, key queue of 4, soft drain, lag → resync, viewer cap from config |
| native transport (`tunnel/*`, `resolver` queue, `localdaemon/peer_stream`) | rewrite | three framings and three retry policies for one byte stream; queue in the wrong process; dead tailnet/WSS/unified paths |
| `cmd/pb` connect/session/exec | fix | one attach path (no argv re-entry or hidden `--session`), reconnect status always shown, deletes (K6 §5) |
| WASM connection + `use-terminal-connection.ts` | rewrite | becomes the shared state machine (6) plus a thin view; maintainer's parked state machine (962dccb8) is folded in |
| server tickets, admission, error mapping | fix | typed codes, live-carrier check, pre-upgrade status endpoint, lifetime ends, limit reservation |
| edge browser-terminal bridge | fix | keepalive and deadlines, close reasons, failover, logs |
| server terminal op queue | fix | V F3 |

**Delete (with the slice that removes it).** From K1 §5, K2 §5, K3 §5, K4 §5, K5 §5, K6 §5, C §5, V §5:
- Host: `WriteStream`, `QueryInput`, `NewInputJournal`, `history.SetLimit`/`Acknowledged`, `ResumeSequence`,
  `fanout.Publish`/`Enqueue`, `BeginUpdate`/`EndUpdate`, `pty.WriteInput`, legacy `InputID` path, the bare
  `Restart` dispatch, `ShutdownForRecovery` (wired in or deleted), `ActiveRequests`, `StartStable`.
- Protocol: text `ack` branch, `replay` action, legacy input handler, `WriteBinary` ×3, unreachable `Read`/`Write`,
  `terminalState.done`, the duplicate duplicate-key walker and error-tree walker, the browser record framing (→ PBT1),
  three of four JSON envelopes, the exec double sequence.
- Broadcast and carrier: duplicate error walk and normal-termination lists, the second carrier composition root, the
  second endpoint-identity build, the double goaway, the browser control attachment that is never read, the second Join
  insert path.
- Native transport: `TailnetTerminalTunnel`, the unified handshake, the WebSocket helper transport, WSS endpoint fields,
  non-terminal local serve variants, `localPeerNetConn`, the helper response map, the input result channel, `inputSeq`,
  `MarkTransportLost`, the duplicate queue creation, the output batching knob (default 0), two of three retry policies.
- Commands: `WithReadOnly`, `ErrNotTerminal`, `RecoverFailure`, `PrepareRemoteViewport`, `statusbar.Text`, layout state
  `disconnected`, `friendlyCommandError` (identity, 125 calls), argv re-entry from menus, hidden `--session`, duplicate
  close/resolve paths and constants.
- Web and server: the duplicate TS sequence/ack layer and `from_sequence`/`replay_gap`, duplicate Decision structs,
  repeated role resolution, unused config and queries (C §5, V §5).

**Add.**
- Input/output lock split.
- Display acks plus lag resync.
- Client-process input queue.
- Heartbeats and deadlines on every hop.
- The shared reconnect state machine.
- Typed end and refusal codes.
- Live-carrier ticket check and edge failover.
- Session-lifetime and revocation ends pushed to live streams (no credential re-check).
- Browser status endpoint for refusals before the WebSocket upgrade.
- Plan-limit reservation.
- Journal GC.
- Owner graceful stop that keeps shells.
- The size rule.
- Observability (ticket issued/consumed, admission outcome, refusal reasons).

**Acceptance bar (user, 2026-10-26).** The world's best terminal experience: the user almost never feels a reconnect or
slowness, whether switching browser tabs, OS windows or anything else. No cheating: no hidden reconnects, no "only
reconnect if gone N seconds".
1. A tab hidden or a window switched for any length of time causes zero transport reconnects.
2. The backlog on return is drawn at once. Output arrives while hidden, and only rendering waits, so there is no
   replay round trip while the socket stayed up.
3. Sleep or a network change resumes with no lost or duplicated bytes and no visible reset, as fast as the network
   allows.
4. Measured, not assumed:
   - every check counts transport reconnects (web: `data-terminal-reconnects`; native: the daemon's stream opens);
   - N hours with a hidden tab and repeated tab and window switches show 0;
   - input-to-echo latency is reported as p50/p99, idle and under an output flood.

**Slices, in order.** Each slice is one or more commits that change producers and consumers together. Each is ticked
only when its checks pass on L (later M). Owners: cli/ with maintainer review; console with maintainer; server/edge with
maintainer.
| # | Slice | Owner | Acceptance (matrix rows; new rows are added to the scripts first) |
|---|---|---|---|
| 0 | Prod browser-terminal outage: drained carrier and unredeemed tickets | maintainer (now) | W2 W5 W6 W7 W10 W11 W12 attach on prod; tickets redeemed ≥ 95 % |
| 1 | Input never waits for output (latency acceptance: input-to-echo p50/p99 under a flood): input off the output lock; no state lock held across a PTY write; bounded input chunks and queues; acks, input results and heartbeats ahead of output on every writer | K (+A review) | N6: Ctrl-C under `yes`, together with a 256 KiB paste and a stalled viewer, stops output < 1 s and the shell answers; workspace test TestInputReachesThePTYWhileOutputFloods |
| 2 | Delete the input result channel; rejected/uncertain input → visible notice | K (+A) | N11: 2 000 single keystrokes, 0 reopens |
| 3 | Input queue in the client process, resend from the host cursor (replaces 03990d13), journal GC on detach | K (+A) | N3, N4 with keys typed during the gap run exactly once; N12: daemon restart mid-typing loses and repeats nothing; 10 000+ keys in one attachment |
| 4 | Liveness (protocol pings in the browser, heartbeats native) and the shared reconnect state machine (Go, used by `pb` and the WASM), per-outage backoff, typed reasons shown | K + C (+ edge ping: M/B) | W4, W4b, W16: 4 h hidden plus 200 tab and window switches give 0 reconnects and the backlog drawn on return; N4, N5, W6; N13/W14: a 60 s black hole is detected < 30 s on a visible tab and recovers on its own; never "Reconnecting" for more than 2 min without a reason and a Reconnect action |
| 5 | Output: display acks, 1 MiB ring, lag → resync instead of eviction, atomic browser checkpoint+join | K | N6 (stalled window recovers), W9, N2/W2 replay exactly once |
| 6 | Size rule | K + C | N1, W1, W5: PTY size follows the last active controller; viewers never resize; the rule survives reconnects |
| 7 | Browser admission and errors: live-carrier tickets, edge failover, 409/429 codes, pre-upgrade status endpoint, close reasons, edge keepalive, lifetime ends, limit reservation, logging | V/B/M + C | W1–W13 on prod; W15: a drained carrier shows "machine unavailable" in < 5 s and recovers when it returns |
| 8 | Server op queue: revoked or deleted machines are a terminal outcome; attempt and age bounds for transient failures; one report per op; retention | V (in progress) | no terminal_session_operation failures for 24 h; the 133 rows resolved; an idempotent completion test (completing twice changes nothing) |
| 9 | Session owner: an owner is replaced only by draining (new sessions go to the new owner; the old one exits when its last PTY ends), because nothing else can hold its PTYs; no machine-wide 256/1024 cliffs; one bridge deadline | K (+A) | N14: a runtime, worker or hostd restart and an update keep the shell and its scrollback; an owner crash ends its shells with a typed `owner_restarted` end; listing works with 2 000 closed sessions |
| 10 | Native transport collapse (rewrite): one framing, one reconnect owner, dead paths deleted | K (+A, R) | full native matrix N1–N14 before and after; switch-over deletes the old stack |
| 11 | Command layer deletes and one attach path | K (+A) | CLI reference/man drift checks; N1–N10 |
| 12 | Wire framing unified (browser on PBT1, one JSON envelope) | K + C | full matrix on L |

**Order (coordinator + GPT review, 2026-10-26).**
1. Containment: slice 0 (prod outage, maintainer) and slice 8 (server queue repair, maintainer).
2. Pure bug fixes with device repros: slices 1 and 2.
3. Typed errors and writer fairness early (slice 7 and the writer part of slice 1).
4. The identity/ack, expiry, checkpoint and lifecycle contracts above are settled before slices 3, 5 and 9.
5. Transport collapse (10), command deletions (11) and wire unification (12) come last.

Linux is validated per slice now; macOS validation per slice starts when macOS starts.

The matrix scripts are the gate. A slice that changes shared code reruns the whole matrix (native and web) on a build
of HEAD.
