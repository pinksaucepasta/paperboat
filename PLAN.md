# Paperboat launch plan

What to build and how. Progress is tracked **only** in [CHECKLIST.md](CHECKLIST.md): every
item below has an ID (e.g. `F3-07`) with a row there, and every `pb` command has a row in its
Part B. Read AGENTS.md first.

Work strictly in the order of section **3** (S0, S4, S1, F1 … F15, S2, S3), split into lanes as in AGENTS.md when several sessions run. For each feature:
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
- DNS: `pprbt.dev` and `pprbt.app` are on Cloudflare (added by the user 2026-10-14).
  `ppr.boats` is not on Cloudflare and is already set up. The existing Cloudflare DNS token is saved
  owner-only in `~/paperboat-archive/coolify-backup/cloudflare-dns-token`; verified 2026-10-14 it
  can access zone `pprbt.dev` only. The user must add `pprbt.app` (Zone DNS Edit) to that token
  before S1 needs preview/tunnel certificates.

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
  `coolify-full-state.tgz` (2026-10-14) adds every container's `docker inspect`, a `pg_dumpall`,
  deploy compose/env/Caddy/keys, tunnel and diagnostics volumes, and release TUF metadata.
  `github-variables.txt` holds the old repos' variables (Sentry DSNs, now also set on the new
  repo as `SENTRY_DSN`, `SENTRY_DSN_SERVER`, `SENTRY_DSN_TUNNEL`, `SENTRY_DSN_RELAY`).
- **TUF: start fresh with new keys** (user decision 2026-10-14); the old root chain is not
  continued. **Minimal TUF** (user decision 2026-10-14): one offline root key kept on apple
  (used only to set up and to replace a key), one online key in GitHub secrets signing
  targets, snapshot and timestamp; the scheduled refresh re-signs timestamp before expiry.
  No signing-state, automated rotation or separate metadata repo; delete that machinery.
  Client verification (hashes, rollback, expiry, root-signed key replacement) is unchanged.
  Lane A: F14 client side. Lane B: release/refresh workflows and key setup (S3), done by
  Lane B itself: generate the root key on apple over Tailscale SSH (key never leaves apple,
  never printed; backed up in apple's keychain/owner-only file), generate the online key and
  store it with `gh secret set`. No user step needed.
- **The CLI binary is independent of the server** (user decision 2026-10-14). `pb` is
  self-contained: anyone can compile it or download it and sign in and pair with it. Today pairing
  requires a server-published release record (`machineArtifact`; server
  `usermachines/service_04.go:430`, CLI `hostruntime/bootstrap/bootstrap.go:191`), so a fresh
  server with nothing published cannot pair anything. Changes:
  - Lane B: installation material carries only credentials, session and server address. Remove
    the release record from pairing, `ErrArtifactUnavailable`, `release_publication.go` and the
    dev-release publisher. No `pb` binary is selected, pinned or served by the server.
  - Lane A: setup accepts material without a release record. `pb pair` keeps installing the
    running binary (it already does). Official builds take their update/repair source from the
    build setting `DefaultReleaseURL` = our release host (TUF metadata on the download host, files
    on GitHub releases), verified with the embedded TUF root, whatever server they pair with.
    Custom builds have no update source and do not auto-update (as today). Windows repair
    (`hostinstall/install_windows.go:3436`) uses the same build-time source.
  - Kept: the console's update approval (server policy for which version machines may update
    to); it gates the version only.
- **Relay and edge hosts** (user decision 2026-10-14): both run on two machines, installed with
  `pbh`: **oracle** (this machine, Ubuntu 24.04 arm64, 140.245.11.41) and **anvit**
  (`ssh ubuntu@anvit`, Ubuntu 22.04 arm64, 152.67.0.60). Not on coolify. `*.pprbt.app` has an
  A record for each IP. The records are Cloudflare-proxied today; the edge terminates TLS with
  its own wildcard certificate, so they must be DNS-only (needs the pprbt.app DNS Edit token,
  S1-08). Lane B owns these hosts' relay/edge services; Lane A still owns the test devices.
- Where the console and marketing site are hosted in production: **USER DECIDES** (marketing
  probably Vercel). Until then the console runs as today (dev server behind the Tailscale
  Funnel at https://oracle.newt-mermaid.ts.net).

### 1.4b Release and installation model

- **`pb` CLI** is released as today: five platform binaries (1.1), macOS `.pkg`, Windows
  installer, signed TUF metadata, installed by the `get.pprbt.dev` one-liners.
- **Server, relay and tunnel** are released as Docker images for **linux/amd64 and
  linux/arm64** (`ghcr.io/pinksaucepasta/paperboat-server`, `…-relay`, `…-tunnel`).
- **The server has no curl installer and is not installed by `pbh`.** We run it ourselves on the
  coolify VPS from its image with a compose file in `deploy/` (server, PostgreSQL 17, its
  edge/proxy), using config/env for every domain.
- **`pbh` self-hosts relay and tunnel only**, its own top-level module `helper/`
  in `go.work` (the command stays `pbh`) (today misplaced in `relay/cmd/pbh`, `relay/deploy/install.sh`,
  `relay/deploy/package-selfhost.sh`, `relay/deploy/README.md`; S4-00 moves it). The one-liner, modelled
  on `curl -fsSL https://cdn.coollabs.io/coolify/install.sh | sudo bash` (read it first), is
  `curl -fsSL <get domain>/selfhost/install | sh -s -- --enable relay|tunnel|both`. It installs
  Docker if missing, installs `pbh`, runs the selected relay/tunnel images, health-checks them,
  prints the claim code, and on re-run upgrades in place keeping state. Idempotent, prints what
  it did, clear refusal on unsupported systems (Ubuntu 22.04/24.04, Debian 12; amd64, arm64).
  Rework the existing binary+systemd installer to this; do not keep both.
- **Our own relays and tunnel edge are installed with the same `pbh` installer** (dogfooding),
  then enrolled as Paperboat global nodes through the operator enrollment API.

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

Order: **S0 → S4 → S1 → F1 … F15 → S2 → S3** (lanes may run in parallel, see AGENTS.md). S-sections are groundwork and release; F-sections
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
- S1-02: Local access names: `<machine>.ppr.boats`, `<port>.<machine>.ppr.boats` (lazy URL), `<machine>.ppr.boats:<port>`; local CA name constraint and leaves use the configured local domain; OpenSSH aliases `<machine>.<local domain>`. DNS is done (not on Cloudflare; already verified 2026-10-14: `x.ppr.boats` and `3000.x.ppr.boats` → `127.100.0.1`); never automate ppr.boats DNS.
- S1-03: Previews and tunnels at `<name>.pprbt.app`; names from the existing name generator; preview names immutable; tunnel rename in server, CLI and console; global uniqueness.
- S1-04: Console origin `app.paperboat.cloud` and marketing `paperboat.cloud` configured for production (links, CORS/allowed origins, WorkOS redirects) while the dev console keeps https://oracle.newt-mermaid.ts.net — configuration only.
- S1-05: Unit tests use the configured test domain; switching the deployment's domains in configuration works without a code change (verify once).
- S1-06: Server stack (server, PostgreSQL 17 with migrations from zero, diagnostics store, edge/proxy for the API and get domains) deployed on the formatted coolify from the server image with the `deploy/` compose file; all domains from env.
- S1-07: Secrets restored from `~/paperboat-archive/coolify-backup/` into the deployment (never printed or committed).
- S1-08: DNS records for all of 1.2; TLS certificates issued and renewing (`*.pprbt.app` wildcard, `api`/`get.pprbt.dev`).
- S1-09: Tunnel edge serving `*.pprbt.app` (HTTP/1.1, HTTP/2, HTTP/3, WebSocket).
- S1-10: Our relays and tunnel edge installed with the `pbh` installer (S4), enrolled as global nodes, reported ready by the server.
- S1-11: WorkOS sign-in through the console dev origin; new account creation works.
- S1-12: Daily database backup runs; one restore into a scratch database tested.
- S1-13: Server `/healthz` and Sentry error reporting (if configured in the backup) work.
- S1-14: Console ↔ server contract fixes from console/NOTES.md that every feature depends on: unknown `/v1/` paths return 404 (not 401); OpenAPI response schemas for `/v1/me` and machine endpoints; sign-in return path.

### S4. Images and the pbh self-host installer (Lane B; needed by S1)

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
- S3-03: Server, relay and tunnel multi-arch images built from the release commit, published, and deployed to our infrastructure with the installer's upgrade path.
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

2026-10-14 | Lane B S0/S4/S1 blockers | pending | — | S4-00 needs Lane A to move/delete relay/cmd/pbh and relay/deploy selfhost files and the root-workspace owner to add helper to go.work; this session is restricted to server/, tunnel/, helper/, deploy/, .github/ plus required tracker/log updates. The gate currently demands HEAD device smoke evidence for server/helper changes even before feature verification; Lane A must supply evidence, without weakening checks. coolify resolves to old Tailscale 100.70.146.64, peer offline; SSH to coolify and its canonical Tailscale hostname both timed out before authentication. No devices accessed, no secrets exposed, no WorkOS settings changed. A reachable VPS SSH address or Tailscale re-enrollment is needed for S1 deployment.

2026-10-14 | Lane A S0 / device prerequisites | pending | — | All five handoff CLI test failures reproduced. Four stale inventory fixtures lack required limit/offset/total; corrected fixtures pass the original assertions. Private TCP unknown-field responses are misclassified as unavailable; owner fix under test (F9/C126). API /healthz returns HTTP 521; HP has no pb on PATH or expected install path and no listed Paperboat services. Lane B: restore S1 server/enrollment/discovery before fresh-device pairing and F1–F12/F14/F15 real-device checks; no installs attempted. S0-09 awaits S1/F1. S4-00: Lane B should first create/test helper from the existing relay-owned pbh sources; Lane A can remove the old files after that producer exists, avoiding deletion of the only working installer. Root go.work/gate ownership is required.
2026-10-14 | Lane B S1 access recovered | pending | — | User supplied new Tailscale address 100.74.80.82; Tailscale SSH authenticated successfully. VPS is Ubuntu 26.04.1 amd64 with no Docker. S4 installer supported-OS contract excludes Ubuntu 26.04; dogfooding S4/S1-10 on this host requires a supported host or a recorded OS-support decision. Preparing Docker for the separate server compose deployment.
2026-10-14 | Lane A S0-10 decisions | pending | — | Keep C001 pb approve under F2: delegates to account-root-signed enrollment approval (daemonrpc.LiveDaemonBackend.ApprovePeer), not a local invented grant. Keep C002 pb resolve and C003 pb tag under F9: live authorized topology/ports/browser URLs and control-sync tags. Both coordinator wiring and RPC lifecycle tests cover live implementations; no obsolete command deletion. Device command verification awaits S1/F1.
2026-10-14 | Lane B regression discovery | pending | — | Server vet passed; full server tests failed TestEveryOperatorRouteHasPolicyAndGuard (missing GET selfhost installation policy), TestOperatorOpenAPIRequiresSeparateOperatorSurface (missing admin capability metadata on client-update-policy), TestCloudflareDNSProviderNeverReturnsCredentialOrResponseBody (safe status prose contains response-key substring). Affected surfaces: S4/S1 selfhost inventory, F9 network inventory, F14 operator support policy, F7/F8 certificate failures. Existing policy/OpenAPI/DNS checks retained; role-boundary cases added and failed before the guard fix. DNS errors now include safe numeric HTTP status and recovery action; original privacy assertion passes unchanged. Server check additionally blocked by stale x/crypto version policy (requires .54, server declares .55, workspace selects .57).
2026-10-14 | Lane B S4-00 producer handoff | pending | — | helper/ producer now exists with its own go.mod, cmd/pbh, reporting copy, installer, packaging script and README; testing with an isolated temporary workspace while root go.work ownership is unresolved. Lane A: after producer verification remove relay/cmd/pbh and relay/deploy/{install.sh,package-selfhost.sh,README.md}; retain relay/selfhost because runtimes and helper share that public protocol package. Root owner: add ./helper to go.work before make gate can test it. Public-address discovery is configured via PAPERBOAT_SELFHOST_ADDRESS_URL or explicit --endpoint-host; no new embedded get domain. Current installer is the migrated systemd version; Docker replacement belongs to S4-02, not yet verified.
2026-10-14 | Lane A S0 regression gate | pending | — | make build passes. Clean detached d5a5904 cross-build passes linux/amd64, linux/arm64, darwin/arm64, windows/amd64, windows/arm64. Gate failed: cmd/pb cycle timeout (600.852s), stale pagination/privacy/catalog fixtures, tunnelenrollment safe diagnostic lost behind joined activation/conflict markers, workerupdate drain wall-clock bound (798ms > 500ms), localdaemon stale pagination, console concurrent Next build lock, and missing HEAD smoke. After bounded JSON traversal, cmd/pb completes in 28.431s (other tests still fail). Privileged updater client rejects user-owned Unix test listeners (UID 1001; root required); keep peer-authentication intact, fixtures need a real root-owned endpoint. Lane C: serialize console builds and complete S0-05/S0-08. Root gate owner: fresh installs cannot supply smoke while API is HTTP 521 and no device is enrolled; a genuine S1/F1 baseline is required before runtime pushes. No failing gate pushed.
2026-10-14 | Lane B gate result and producer verification | pending | — | make gate FAILED: CLI timeout/failures (Lane A), console prerender ENOENT (Claude), helper absent from root go.work (root owner), no HEAD device-smoke evidence (Lane A). No push attempted. Server full vet/unit PASS; helper vet/unit PASS with isolated workspace, amd64/arm64 pbh builds PASS. actionlint 1.7.11 PASS; server image OCI manifest contains linux/amd64 and linux/arm64 (local verification only, no release publication). The old unsupported relay v0.0.0 dependency declaration failed helper vet/build; removed it so workspace-local relay public packages resolve like other workspace modules. Docker/Compose installed on new coolify; 22 current secret files restored root-only. Server CI full check now stops at pre-existing default-http source-policy uses in jobmonitor/probe.go, jobmonitor/monitor.go, backupreport/producer.go, supportreport/service.go; no policy exception added.
2026-10-14 | S0 Lane C → Lane A request | — | — | delete `cli/cmd/browserterminal-wasm/build-dashboard-assets.sh` (points at the old paperboat-dashboard repo); the console now builds the wasm itself via `console/scripts/build-terminal-wasm.sh`
2026-10-14 | Lane A S0 validated fixes | pending | — | Full cmd/pb PASS in 33.964s; targeted cmd/pb race PASS in 19.362s, including real root updater fixtures. daemoncmd, localdaemon and tunnelenrollment PASS. Supplied updater fixtures now use real root Unix peer identity without changing production authentication. Logout exercises the production revocation deadline after local cleanup; drain timing excludes preparation and retains the 500ms activation/recovery limit. Native session opens recheck the signed peer binding and retire denied sessions; checks cover terminal/exec/SSH/transfer/ENV/config/private access and update continuity through full CLI module gate. Native machine-access integration had another incomplete inventory fixture, corrected; native targeted checks and final gate underway.
2026-10-14 | Lane A cross-lane gate requests | pending | — | Lane B: fix tunnel/internal/edgehttp/preview_carrier_transport.go lifetimeCancel leak reported by go vet (lines 593/613); root workspace owner: register ./helper in go.work. Producer 1ee1d81 is verified; old relay/cmd/pbh and relay/deploy installer/package/README removed, relay/selfhost retained, README points to helper. Lane C S0-05 request completed on CLI side: obsolete build-dashboard-assets.sh removed; console owns wasm generation. Gate/device prerequisite remains live S1 enrollment; no runtime pushes until a genuine green gate and real smoke.
2026-10-14 | Lane A F1 (29 rows) | blocked | — | S1 server/enrollment and installer build under test are unavailable; API health HTTP 521. No fresh pair or device baseline; blocked on Lane B (PLAN Log 2026-10-14).
2026-10-14 | Lane A F2 (22 rows) | blocked | — | Requires S1/F1 enrolled-device baseline and live account/workspace APIs; CLI approval page is Lane C. Device command contracts remain unverified (PLAN Log 2026-10-14).
2026-10-14 | Lane A F3 (29 rows) | blocked | — | Requires S1/F1 enrolled-device baseline, terminal/session/sharing APIs, and Lane C browser flows. No device continuity or 8-hour idle evidence yet (PLAN Log 2026-10-14).
2026-10-14 | Lane A F4 (9 rows) | blocked | — | Requires S1/F1 enrolled-device baseline before all-pair, long exec, timeout and stream reliability reproduction. API unavailable; device checks not run (PLAN Log 2026-10-14).
2026-10-14 | Lane A F5 (12 rows) | blocked | — | Requires S1/F1 enrolled-device baseline before first-attempt SSH, held sessions and file-tool matrix. Device SSH failure not yet reproduced on this build (PLAN Log 2026-10-14).
2026-10-14 | Lane A F6 (20 rows) | blocked | — | Requires S1/F1 enrolled source/target machines and live transfer/team-inbox authority. No real-device hash/resume/limits evidence (PLAN Log 2026-10-14).
2026-10-14 | Lane A F7 (13 rows) | blocked | — | Requires S1/F1 device baseline and Lane B ready preview edge, configured public domain/TLS and grants. No public/private preview evidence (PLAN Log 2026-10-14).
2026-10-14 | Lane A F8 (42 rows) | blocked | — | Requires S1/F1 device baseline and Lane B tunnel/edge/connector/domain APIs and TLS. No real-device durability, route or custom-domain evidence (PLAN Log 2026-10-14).
2026-10-14 | Lane A F9 (27 rows) | blocked | — | Requires S1/F1 device baseline and Lane B authoritative domain discovery/network inventory; consumers must follow the committed producer. No direct/relay device matrix (PLAN Log 2026-10-14).
2026-10-14 | Lane A F10 (31 rows) | blocked | — | Requires S1/F1 enrolled-device baseline and live encrypted ENV/vault/grant authority; no OS-keychain or headless delivery matrix on this build (PLAN Log 2026-10-14).
2026-10-14 | Lane A F11 (27 rows) | blocked | — | Requires S1/F1 enrolled-device baseline and live repository/assignment/consent APIs; no device conflict or credential-storage matrix (PLAN Log 2026-10-14).
2026-10-14 | Lane A F12 (30 rows) | blocked | — | Requires S1/F1 device baseline and Lane B live team/member/invitation/resource authority; invitation/role/suspension matrix not verified (PLAN Log 2026-10-14).
2026-10-14 | Lane A F14 (15 rows) | blocked | — | Requires S1/F1 working device baseline for supplied-update/rollback/continuity; signed/offline release checks also need the pending update-trust decision and Lane B release infrastructure (PLAN Log 2026-10-14).
2026-10-14 | Lane A F15 (25 rows) | blocked | — | Requires an accepted S0 gate and S1/F1 build on real devices; doctor/support/completion targets and settings persistence across updates remain unverified (PLAN Log 2026-10-14).
2026-10-14 | Lane A F9 discovery producer request | blocked | — | Lane B: publish the authoritative runtime-discovery endpoint/schema for installer URL, local access domain, preview/tunnel domains and console links, with missing/invalid configuration errors. Commit/push producer first; Lane A then implements login/pair cache and CLI/daemon/SSH/CA consumers. Do not invent client domain defaults.
2026-10-14 | Lane A F14 release decision | blocked | — | USER DECIDES: PLAN 1.4 says update-trust design must be discussed before changing it. Signed/offline/minimum-version/automatic-release validation needs that decision plus a real fresh signed release, S1 infrastructure and enrolled-device baseline. Supplied Windows rollback remains unverified on devices.
2026-10-14 | Lane B CI check recovered | 1ee1d81 | — | make -C server check PASS: dependency versions aligned, bounded HTTP transport ownership reviewed using the source-policy annotation mechanism (check remains intact), metrics.json regenerated from runtime owner, oversized OpenAPI contract file split while preserving assertions, SQLC query output regenerated. Server/helper unit tests PASS; helper race tests PASS; packaged arm64 pbh version and isolated --enable both initialization PASS with output captured to protect claim secrets. Extra server CI checks and remaining image builds are running.
2026-10-14 | Lane B external release/billing blockers | 379c5fe | — | F13-05 requires a restricted live Stripe key and the user purchase/refund; no Stripe credential names in current repo secret inventory, no Stripe settings in archived server environment. S3-01 requires the agreed public download host, Windows signing PFX, Apple installer certificate, and recorded update-trust decision before F14 changes. Current repo secret metadata confirms signing certificates absent. Fresh TUF keys are agent-owned work after the trust decision; old root chain will not be restored. No signing/release workflow triggered.
2026-10-14 | Lane A F2 frontend request | blocked | — | Lane C: implement the server-configured CLI approval URI (current path /cli/authorize), retaining the code query through sign-in. GET /v1/auth/device/requests/{user_code}; POST the same resource /approve or /deny with browser auth and CSRF. Fields: client_label, device_type, os, scopes, issued_at, expires_at, user_code, state, issuer, account{id,email,display_name}. Show pending/approved/denied/consumed/expired without exposing device_code or tokens; handle 401 sign-in, 404 device_request_not_found, 409 device_request_not_pending, 410 device_request_expired/device_request_consumed, 429 rate_limited + Retry-After. CLI polls POST /v1/auth/device/token and resumes the same saved approval; existing browser-login workspace tests pass. Real end-to-end login awaits S1 and this page.
2026-10-14 | Lane A committed fixes / gate | cfb17cd af5aa5e 1fc67e9 56518f4 6a86c26 a3d56d2 ede6561 df44a2f 0e05724 8cb69b9 | — | Ten focused commits: bounded cyclic JSON traversal; malformed private-TCP response class; joined activation diagnostics; native signed peer binding fence; stale contract fixtures; real root updater fixtures; deadline measurement phase; generated CLI docs; duplicate pbh removal; obsolete wasm writer removal. Latest committed make gate: CLI and relay vet/unit PASS, server vet/unit PASS, console lint/build PASS. FAIL: Lane B tunnel lifetimeCancel vet leak; helper missing from root go.work; no real smoke for fresh unpaired devices while S1 is unavailable. Nothing pushed. Clean 8cb69b9 five-platform build validation underway; devices unchanged.
2026-10-14 | Lane A final validation / dispositions | 8cb69b9 | — | Clean dev-8cb69b9 builds PASS for all five platforms with configured bootstrap URLs; committed CLI suite PASS 34.062s and CLI vet/all unit PASS. Root-fixture/preview/cycle race PASS 19.362s; native relay/peer-close race PASS 4.288s; native TCP + namespace proxy PASS. make gate FAIL only tunnel lifetimeCancel vet leak (Lane B), helper absent from go.work (root owner), and missing real-device smoke. All 331 Lane A F1–F12/F14/F15 scenario/command rows marked [!] with prerequisite reasons; no feature claimed verified. S0-04/S0-06/S0-10 verified; S0-02/S0-03/S0-09 blocked. No push; hp/apple/victus unchanged, no test binaries or services installed. Temporary clean-build worktree and artifacts removed after validation.
2026-10-14 | Lane A S0 test file limits | pending | — | Split touched main_test.go (4163 lines), coordinator_test.go (1770), and tunnel_command_test.go (1738) into 12 files below 1000 lines as required by AGENTS. Mechanical AST/hash verification preserved all 297 function bodies exactly; no check removed or changed. After splitting: go vet cmd/pb + daemoncmd PASS; go test ./cli/cmd/pb ./cli/internal/daemoncmd -count=1 -timeout=8m PASS (23.516s / 1.354s). Runtime code unchanged; clean 8cb69b9 build evidence still applies.

2026-10-14 | Lane B S4-00 workspace authorization | pending | — | User explicitly authorized the root go.work exception; ./helper registered. go vet ./helper/... and go test ./helper/... PASS from root. Old pbh removal is committed by Lane A. Full make gate FAIL: two tunnel TRK35 replay assertions and absent HEAD smoke evidence; no push. Tunnel vet and preview lifecycle race tests PASS after adding lifetime cleanup to both browser-error exits; successful responses still own the context until body close.
2026-10-14 | Lane B S0 gate blockers after two reproductions | pending | — | Tunnel TestTRK35RouteWorkerLostAckRestartAndIdempotentReconcile and TestTRK35IntegratedRouteLifecycleFaultHarness fail in make gate and targeted reproduction. Existing production workers.go deliberately sends prepared before publication then ready after it; chaos fixtures expect the prior single-ACK contract. TestCanonicalReadyFollowsPublicationAndRetriesConfirmation enforces current two-phase boundary. No production protocol rollback or weakened assertions; stop this attempt and record for independent contract review. CLI/relay/server/helper and console checks PASS; tunnel vet PASS. Server check/race/reproducible-builds/license PASS; binary-size check FAIL (46,026,912 vs 30,314,658 baseline); pinned staticcheck panics on recover and pinned govulncheck panics on ast.KeyValueExpr with Go 1.27. No checker disabled or baseline raised. Full outputs: /tmp/paperboat-lane-b-gate-authorized.log, /tmp/paperboat-lane-b-trk35.log, /tmp/paperboat-lane-b-ci-extra.log, /tmp/paperboat-lane-b-ci-tools.log, /tmp/paperboat-lane-b-vulnerability.log.
2026-10-14 | Lane B S0-03 TRK35 resolution | pending | — | User confirmed both failures exist at import 75a0130. Wrong test expectation: worker sends prepared before local publication and ready afterwards; control_plane.sql accepts both and preserves ready on replayed prepared. Updated chaos/integration expectations to require exact ordered prepared→ready observations, then ready-only unchanged replay; generation, lost-ACK, admission, real-carrier, overload/LKG and cleanup checks retained. TRK35 + TestCanonicalReadyFollowsPublicationAndRetriesConfirmation race PASS; full make gate PASS at /tmp/paperboat-lane-b-gate-push.log. No production ACK contract changed.
2026-10-14 | Lane B gate policy and backlog push | pending | — | User clarified no feature rows are [x], so smoke is not a bootstrap prerequisite. Authorized correction to tools/gate.sh: read F-section scenario/command status cells; require existing HEAD smoke evidence when any is verified, retain module/console failures and smoke-result rejection. Five .github/tests/test_gate.py checks PASS (bootstrap, verified scenario, verified command, old/failed/passing HEAD evidence, failed module); bootstrap test demonstrably fails on old gate. Added checks to manual server CI. S4-00 and S0-03 verified; make gate PASS. S0-02 marketing build needs Lane C evidence; S0-07 extra analyzer/size failures remain separate from regression gate. All local commits will be pushed after final committed-tree gate; devices untouched.
2026-10-14 | Lane B S4 images and shared lifecycle request | 379c5fe | — | S4-01 six OCI platform/version checks PASS (server/relay/tunnel × amd64/arm64, 2026.10.14.0); release publication not triggered. S4-02–06 blocked on the binary-only shared lifecycle in relay/selfhost, owned by Lane A: Run/supervise uses exec of paperboat-relay/tunnel and owns private TLS renewal/deployment synchronization. Lane A: expose a production runtime-launch contract for helper to supply Docker startup/stop/readiness while preserving single enrollment/state/certificate owner, or move that shared owner to helper with consumer imports updated after producer push. Do not retain competing supervisors or duplicate authoritative selfhost state. Current helper native installer is not marked Docker-complete. Required upgrade rollback, supported OS runs, confirmed data teardown and Docker docs remain blocked; coolify Ubuntu26.04 is outside fixed OS contract. Continuing independent S1 server producer work.
2026-10-14 | Lane A final gate refresh | a6831a4 | — | S0-02 now verified: make build PASS, all Go module vet PASS, console install/lint/build PASS, marketing build PASS 6.33s. Root helper registration is complete and helper tests PASS. Latest make gate FAIL only the two Lane B tunnel ACK-contract tests and absent HEAD device smoke. CLI/relay/server/helper unit checks PASS; all Lane A-owned rows are [x] or [!]. S0-03/S0-09 and 331 feature/command rows remain blocked; all requests and F14 decision are recorded above. No push with a failing gate. Devices unchanged; task build worktrees, binaries and temporary logs/tools cleaned up.
2026-10-14 | Lane B S1/F9 discovery producer | pending | — | GET /v1/client-configuration remains public, version 1: issuer, installer_base_url, console_url, cli_verification_url, machines_url, local_access_base_domain, preview_base_domain, tunnel_base_domain, runtime_base_domain are configured fields documented in OpenAPI. New PAPERBOAT_LOCAL_ACCESS_BASE_DOMAIN owns native machine-service URL suffix; release_base_url owns installer origin and browser_login_origin owns console origin. Missing/invalid discovery returns 503 client_configuration_unavailable with operator guidance; no domain defaults published. Domain-switch and missing/invalid configuration unit tests PASS; make gate and server check PASS. Split touched config/app files below 1000 lines preserving lifecycle/environment code. Lane A: after producer push consume/cache all fields at login/pair for CLI/daemon/CA/SSH/install links; remove domain literals without invented defaults. Lane C: use same endpoint for configured links/domains, show unavailable/retry state on 503, preserve sign-in origin and approval links; remove frontend domain literals. Shared checks cover F1/F2/F7/F8/F9/F14. S1-01/02/05 remain [!] for consumers, broad fixture cleanup and deployed switch; no feature claimed device-verified.

2026-10-14 | Lane A waits for S1 | resolved | — | S1-06/07/11 landed; Lane A resumed F1/F2. The 331 later rows remain [ ]; minimal TUF decision is recorded.
2026-10-14 | Lane B S1-03 shared certificate owner | pending | — | Replaced separate preview/tunnel platform certificate IDs with one platform_cert_public_v1/public_wildcard target for the configured shared base; runtime retains its separate target. The wire target_kind remains platform_wildcard. Fresh baseline SQL constraints, target store, worker tests, acceptance fixtures and deployment preflight now use exactly two owners. Shared-base regression fails before and passes after; target/worker unit and race PASS; make gate and server check PASS. Cloudflare token still lacks pprbt.app DNS Edit, so live wildcard issuance/public edge verification is [!]; no secret or WorkOS settings changed. Consumer edge contract unchanged; Lane C/A must show configured public names and wait for certificate readiness. Continuing managed stack preparation.

2026-10-14 | Lane A S0-03/S0-09 qualification | partial | — | Full make gate PASS for all five Go modules and console; tunnel ACK failures resolved by 294b44c. Initial gate saw native SSH interruption timeout plus a concurrent Lane B server compile error; focused SSH trace and final full gate PASS, no guessed repair or weakened deadline. Smoke receipt regression fails before fix (forged success/nonzero exit/transport loss); new six-test workspace suite covers long-work duration, install path, held shell identity and bounded child cleanup. Real-device, private preview, exact resource cleanup and transfer/URL continuity qualification remain in S0-09 after S1/F1; devices untouched.
2026-10-14 | Lane B S0 standalone packaging regression | pending | — | VPS native image attempt 1 failed because legacy builder left BUILDPLATFORM empty; installed official Ubuntu docker-buildx. Attempt 2 reached the Go build but failed with updates to go.mod needed. Workspace builds masked stale server module transitive versions. GOWORK=off go mod tidy restores the standalone module; new server standalone-build (-mod=readonly) is part of check and PASS; make gate PASS. S1 image build attempt paused after two failures; return after this producer is pushed. Binary-size/staticcheck/govulncheck CI blockers remain unchanged.
2026-10-14 | Lane B S1 PostgreSQL acceptance / imported fixture blocker | pending | — | Fresh PostgreSQL 17 on coolify migrated from empty to catalog version 64; TestBaselineSchema PASS through an authenticated Tailscale SSH forward to isolated paperboat_lane_b_test. Database owner assertions now require current version 64; removed obsolete duplicate baseline-only catalog assertion from certificate acceptance, retaining its issuance/renewal/distribution/cleanup checks. Certificate PostgreSQL acceptance failed twice: first retired baseline-only count, then fixture INSERT into removed preview_leases.endpoint_id. Stop that acceptance attempt and mark live certificate validation [!]; fixture must follow current schema before claiming SQL lifecycle verified. No production data, credentials or values printed.
2026-10-14 | Lane B S1 managed composition | pending | — | Added deploy/compose.yaml for PostgreSQL17, migration-gated server, MinIO diagnostics and configured Caddy API/download/object-store origins. Restored 75 owner-only server environment settings from archive without printing values; current configured domains and dev console origin override retired domains. VPS PostgreSQL healthy, migrated empty→64; TestBaselineSchema PASS via authenticated SSH-forwarded isolated database. Docker Compose renderer test PASS: server waits for successful migration, DB only on loopback, secrets mounted read-only and passwords use files. Native image packaging resumes only after standalone module repair 7547931. No local backend/database or test device used.
2026-10-14 | Lane B S1-12 backup and restore | pending | — | Atomic protected custom-format pg_dump, bounded execution/failure cleanup, daily persistent systemd timer, and owned scratch restore verification implemented. BackupPublicationTest success and failure cases PASS; actual coolify backup PASS; verify-backup.sh restores a new paperboat_restore_test, checks schema count and migration64 against source, removes only its created scratch DB, and PASS. Daily timer active. Existing backups preserved and no secret/database contents printed. CI discovers all .github Python gate/deploy/backup checks (eight PASS); make gate PASS.
2026-10-14 | Lane B S1-14 console API producer | pending | — | Typed Me/MeResponse, Machine/MachineResponse/MachineListResponse schemas now cover GET /v1/me, GET /v1/machines, GET/PATCH /v1/machines/{machine_id}, including ownership, permissions, capabilities, availability, diagnostics and immutable artifact metadata. /v1/unknown-resource already returns structured 404 not_found. Signed OAuth return_to now accepts bounded same-origin console paths such as /machines and /terminals/{id}; external, encoded, traversal, query/fragment and tampered destinations remain refused. Default is /; obsolete pre-destination state fallback removed. Before/after contract and security tests, race and make gate PASS. Lane C: regenerate API types and remove hand-written Me/Machine copies; use signed return_to for current paths (keep CLI approval query in existing frontend flow), show 401 authentication and structured 404 resource failures. Server producer first; WorkOS settings/origin unchanged.
2026-10-14 | Lane B reporting cancellation regression | pending | — | Gate caught TestSelfhostReportingContextBindsReporter losing accepted evidence when Run cancellation raced an in-flight Save. New TestAcceptedReportSurvivesCancellationDuringSave deterministically cancels at the store boundary and fails before. Accepted saves now retain a two-second independent lifetime; Run checks cancellation before dequeuing again and shares one bounded shutdown drain. New/old reporting and drain race tests PASS; full make gate PASS. F15 support evidence and S1 reporting affected; no fault text, headers or private payloads added to logs.
2026-10-14 | S1-14 Lane C → Lane B requests | — | — | (1) Type `Machine.runtime_versions` in OpenAPI as an object of string values (today `{}`, a raw passthrough); the console then uses Schemas["Me"]/["Machine"] and deletes its hand-written copies. (2) Remove the signed `return_to` from /v1/auth/workos/state and the callback response: the console keeps the return path in its HttpOnly `pb-return` cookie, which also carries the CLI approval query the signed form refuses, so the server copy is unused. Done in Lane C: API types regenerated; 401 now always means sign in (the /me double-check for unknown-path 401s is removed).

2026-10-14 | Lane B hostname limit | pending | — | Native browser aliases now validate the complete configured DNS name, including alias and port, against DNS label/name limits. Expanded TestNativeBrowserHostname rejects a valid base whose combined name exceeds 253 bytes; fails before, race PASS after. make gate PASS (/tmp/paperboat-lane-b-gate-deploy-priority.log).
2026-10-14 | Lane B standalone workspace checksums | pending | — | Workspace checksum graph synchronized with standalone server module repair 7547931; make gate PASS, including every Go module and console build.
2026-10-14 | Lane B Free enrollment producer | pending | — | Enrollment/setup/recovery do not meter transferred bytes and must not require a paid bandwidth period; removed six premature period gates while retaining seat authorization and actual usage reservation. Removed duplicate HTTP paid gates from authenticated config-sync/ENV/GitHub status routes; CSRF remains enforced. Default personal allowance is configuration-owned decimal 5 GB, machine quota remains unlimited. Shared consumers F1/F2/F6/F11/F13 covered by full make gate PASS. TestFreeAccountSetupWithoutSubscription fails before and passes after against isolated VPS PostgreSQL; paid idempotent setup and cookie/CSRF integration PASS. Split touched large service/router files without contract changes. Quota metering/team pooled usage and device feature verification remain open.
2026-10-14 | Lane B priority S1 deployment unblocked | pending | — | S1-06/07/11 PASS: all four services running on rebuilt coolify100.74.80.82 (public157.180.74.88), PostgreSQL17 empty→migration64, current committed server e92e8ea image2026.10.14.1 healthy at https://api.pprbt.dev/healthz200; 22 restored root-only secret files mounted read-only. Official diagnostics Docker tags failed twice because upstream no longer distributes community containers; built pinned upstream source7aac2a2c5b7c882e68c1ce017d8256be2feea27f with upstream license. HTTPS readiness200; private bucket upload/download/anonymous-denial/cleanup PASS. Nine CI Python checks and make gate PASS. Configured get HTTPS works, installers remain404 pending qualified signed release. Fresh console WorkOS redirect/callback and /api/v1/me authenticated active200 PASS on dev origin; fresh DB account creation works. Lane A/C server blocker cleared; use configured discovery and existing APIs. S1-08 remains blocked: restored Cloudflare token sees .dev but not .app; need .app DNS Edit token and zone ID. S1-09 depends wildcard/edge; S1-10 depends S4 Docker lifecycle producer and supported OS decision. Deferred Lane C runtime_versions/return_to requests next.
2026-10-14 | Lane B S1-14 runtime_versions producer | pending | — | Machine.runtime_versions is now an OpenAPI object whose additionalProperties are strings. TestOpenAPIConsoleIdentityAndMachineResponses requires the component/version contract (fails before, PASS after); full make gate PASS. Lane C: regenerate types and use Schemas["Machine"] without a hand-written runtime_versions copy. No wire fields changed.
2026-10-14 | Lane B S1-14 OAuth navigation owner | pending | — | Removed signed return_to input/payload/parser and callback data.return_to; console alone owns return navigation via its existing HttpOnly pb-return cookie, including CLI approval query. OAuth state retains random signed nonce, ten-minute server expiry and exact cookie binding; browser-origin and CSRF guards unchanged. TestWorkOSStateLeavesConsoleNavigationToConsole fails before/PASS after; tampering/mismatch/expiry, auth/HTTP race, actual PostgreSQL TestAuthLoginMeCSRFLogoutAndAudit and make gate PASS. Shared F1/F2/console sign-in covered. Lane C producer request complete; state returns only state and callback returns Me fields. No WorkOS settings changed.
2026-10-14 | Lane B configured test-domain fixtures | pending | — | Replaced real production domain literals in 21 server/tunnel Go test files with equivalent example.test fixture configuration and assertions; origin mismatch, TLS hostname, cookie, route and artifact checks preserved. Owned Go code scan returns zero real Paperboat domains; affected package race and make gate PASS. S1-01 still awaits CLI/relay/frontend consumers. User cleared S1 edge OS/Docker blockers: deploy current systemd pbh on oracle140.245.11.41 and anvit152.67.0.60 now, then Docker later; wildcard DNS is DNS-only already, token still needed.
2026-10-14 | Lane B S1 health/reporting | pending | — | Restored server Sentry destination from configured repository variable matching backup configuration into root0600 secret file; enabled runtime reporting on current image2026.10.14.2. TestLiveSentryDeploymentProbe -race sends one synthetic content-free event through the actual configured destination: event ID persisted in memory, flush drained, HTTP failures/drops zero. TestHealthDoesNotRequireReadiness, TestReadyReflectsDependencyState and full make gate PASS; public health200. Live browser re-sign-in retains /cli/authorize?code=DEPLOY-CHECK after console-owned cookie navigation. S1-04 blocked only on USER DECIDES production hosting/WorkOS production redirects. S1-10 additional prerequisite discovered: fresh server has no PAPERBOAT_OPERATOR_BOOTSTRAP_EMAIL/initial operator configured; need operator bootstrap identity/credentials for authenticated global enrollment, rather than bypassing operator authorization. Existing oracle helper is claimed to an old team; anvit has an old tunnel container. Preparing protected preserved state and fresh committed systemd builds as user requested.
2026-10-14 | S1-08 unblocked (user) | — | — | Archived Cloudflare token (account token snowy-union-38be) now has DNS Edit on pprbt.app as well as pprbt.dev; *.pprbt.app A records 140.245.11.41 (oracle) and 152.67.0.60 (anvit) are DNS-only. Lane B: issue the public wildcard and continue S1-08/09/10 on oracle and anvit.
2026-10-14 | Lane B S1 multi-zone certificate producer | pending | — | Live composition assumed one Cloudflare zone for public app names and runtime/dev names. Added mandatory configured public_dns_zone_id / PAPERBOAT_CERTIFICATES_PUBLIC_DNS_ZONE_ID; existing dns_zone_id remains runtime/delegated challenge zone. Stateless DNS01Zones selects explicitly configured suffix for create/wait/cleanup and refuses unknown names before provider calls. Public/runtime address reconcilers now use matching zone IDs. Config overlay regression fails before/PASS after; TestDNS01ZonesUseMatchingZoneForCreateAndCleanup and config/app/tunnelcert race PASS; make gate PASS. Shared F1/F7/F8/F9 TLS/DNS consumers covered. Cloudflare token now sees both zones; initial operator identity approved by user and account setup authenticated, credentials root-only. Deployment rollout next.

2026-10-14 | Lane A S4-02 runtime launcher producer | partial | — | RunWithLauncher(ctx, stateDir, RuntimeLauncher, output, observe) retains the sole enrollment/certificate/public-TLS owner. Launcher.Validate(ctx, capability, componentDir) validates all enabled components before launch; Launcher.Run(ctx, capability, componentDir, output) owns Docker readiness and bounded stop/removal before returning. Components relay/tunnel use generated absolute state paths; mount state/certificate paths at identical absolute paths in containers. Crash backoff and certificate renewal drain/relaunch remain in the shared supervisor. Lane B: switch helper serve to this contract and Docker image configuration, then Lane A removes the old binary consumer entry point after your producer commit. Existing helper entry point stays only until producer-first consumer handoff; no second supervisor/state owner. TestSupervisorRuntimeLauncherWithoutLocalBinaries and existing restart/renewal race checks PASS. Shared users: helper serve and relay/tunnel self-host runtimes, S4-02/03 and S1-10.
2026-10-14 | Lane B urgent global pbh consumer request | pending | — | S1 global enrollment uses existing authenticated claim ScopeType=global. Lane A: relay/selfhost exportRuntime currently always writes self_hosted=true and browser_access_enabled=false, so global nodes lose managed runtime routing. Pass existing claim scope through Setup and export self_hosted=(scope!=global); keep claimed infrastructure TLS files/identity. Browser login origin must come from configured discovery if enabled, and PSL isolation must remain enforced. Tunnel consumer will retain explicitly configured claimed carrier trust independently of customer ownership. This blocks live S1-09/10 after global claim; current systemd pbh explicitly authorized by user, do not wait for Docker. Oracle/anvit new committed native helpers installed and initial operator bootstrap authenticated; no device used.

2026-10-14 | Lane A F1 baseline / named blockers | partial | — | Clean dev-3ef73d7 five-platform builds PASS; supplied pb install --json PASS on hp/apple/victus, macOS browser trust_pending reported. API health200 but get/install404: F1-01 needs Lane B qualified installer/build publication (S3-01/02), not a relay. HP protected-token fresh pair failed twice (JSON and diagnostic resume); both emit only the generic host-runtime failure. Protected resume says pairing_started=true, no material/client/runtime enrollment; token file0600 and producer token length26. No unchanged retry; investigating owner error path while moving to F2 browser login. Lane B: check current machine-artifact/enrollment material configuration for this fresh deployment and provide named availability errors; no publisher verification or enrollment authorization bypass.

2026-10-14 | Lane A S0-09 install receipt regression | pending | — | Real dev-3ef73d7 installs on all devices return the canonical {schema_version:1.0,ok:true,data:{executable,...}} envelope, while smoke parsed executable at the top level. WindowsInstallReceipt.test_custom_install_directory_is_preserved fails before and passes after requiring the successful envelope and reading data.executable; malformed/failed receipts fail closed. All six smoke workspace tests and full make gate PASS. macOS install reports trust_pending; Windows per-user executable comes from its actual receipt, no fixed guessed path. HP browser login/approval/auth status PASS; Apple/Windows account checks continue.

2026-10-14 | Lane A F1 pairing request diagnostics | pending | — | bootstrap.RequestError now exposes the existing safe typed outcome/status to the host-command owner; body and transport text remain private. TestPairingServerFailureNamesRequestAndStatusWithoutPrivateBody fails before/PASS after against a real TLS HTTP503 boundary; bootstrap and hostruntimecmd race tests plus full make gate PASS. Specific expired/denied/used-token messages still take precedence. HP pending enrollment is approved but no installation material is issued; this diagnostics fix will distinguish the server request outcome on the next committed device build without guessing its cause.
2026-10-14 | Lane B gate regression request to Lane A | pending | — | Latest make gate failed TestSystemOpenSSHOverNativeTailnet/EditorDynamicForwardAndReconnect in cli/internal/peertransport/native (package140.774s, subtest5.48s), after all Lane B targeted server/tunnel race and PostgreSQL tests passed. Earlier overlapped gate also hit Next concurrent-build lock; completed separately before this native failure. CLI file ownership prevents Lane B repairing that path. Lane A: reproduce and fix this newly failing check without weakening it; push held, no force/failed-gate push. Full log /tmp/paperboat-lane-b-gate-global-edge-serialized.log. Continuing independent requested dev build publisher and node deployment preparation.

2026-10-14 | Lane A waits for F1 installation material | resolved by ac4a44f | — | Lane B: fresh approved enrollment pairing_694a3900-73d1-4cc8-a9fe-038573eec2bd on hp with clean dev-dfaa71a reaches /v1/machines/pairings/installation but receives HTTP500. Original generic failure is now a named safe HTTP outcome (dfaa71a). API health200, token creation201, pairing approved, no client/runtime material installed; please fix the deployed installation-material producer/configuration and its root cause. F1-02 and dependent pairing/setup/device access await this endpoint; F1-01 separately needs configured get/install (currently404). Do not bypass publisher verification or enrollment authorization. Smoke results tools/device-smoke/results/20261014T080359Z.json (dev-3ef73d7) and 20261014T081106Z.json (dev-dfaa71a): each version checks3 PASS, unpaired baseline23 FAIL; no existing verified feature regression.

2026-10-14 | Lane A F1/F2 enrollment command repair | pending | — | pb machine add used one default POSIX token for both printed commands and a hardcoded installer domain. It now reads installer_base_url from GET /v1/client-configuration before issuing any token, requests distinct shell=posix and shell=powershell credentials, and cancels unused enrollments on partial creation/output failure with bounded cleanup. TestMachineAddPrintsOneShotEnrollmentCommands fails before/PASS after with a configured test installer path and distinct platform tokens; missing-discovery/no-token and failed-Windows/cancel-POSIX checks plus affected race PASS. Producer already accepts shell (console uses it); no server contract change. Shared callers F1/C011 and F2/C031; full CLI/API checks in make gate. Machine command builders moved out of oversized main.go into dedicated small functions without changing list/rename/revoke/availability/capability behavior.
2026-10-14 | Decision: CLI independent of server | — | — | Lane B: stop the dev-release publisher; remove the artifact requirement from pairing/installation material (server producer first, push). Lane A: then drop the artifact from the CLI side of pairing and pair hp/apple/victus with the locally built binaries. See section 1.

2026-10-14 | Lane A native SSH gate regression | repaired | — | Reproduced deterministic ProxyCommand EOF failure with stdin held open: old netcat fixture waits for both directions, hiding clean native FIN from OpenSSH. Replaced only fixture carrier with a bounded socket proxy whose output owner exits on transport EOF; existing five-second reconnect deadline and all SSH assertions retained. New ProxyCommandExitsOnNativeEOFWithInputOpen fails before (5s), passes after; full TestSystemOpenSSHOverNativeTailnet race PASS 4.679s. Full make gate PASS at /tmp/paperboat-lane-a-native-eof-gate.log. Lane B artifact-free installation producer remains pending; Lane A will consume its pushed contract before pairing devices.

2026-10-14 | Lane B CLI-independent installation producer | pending | — | Per 92b5ad9, stopped and deleted the unpublished dev-release publisher and release_publication.go; removed MachineArtifact, its release resolver and all artifact bindings from setup, pairing, preparation, retries and recovery. POST /v1/machines/{machine_id}/machine-installations now accepts verifier, public_identity_key, installation_generation and the existing SSH/runtime/keep-awake fields, without artifact. Setup installation metadata returns control_url and helper_listen_address; consumed material contains enrollment credentials, session, server address and identity/generation fields, with no artifact. Existing ownership, session, public identity, idempotency, one-use, expiry and recovery guards remain. No CLI binary is served at /tuf/targets; installer scripts and policy metadata remain. Production readiness no longer requires published release files. Shared consumers: F1/F2/F14; Lane A removes artifact from request/material validation, uses its running binary for pairing and the build-time official update source. Lane C consumes the documented installation fields; approval policy still gates versions. PostgreSQL before-fix Free setup failed without publication; identity/idempotency/generation/recovery/one-use checks PASS after (192.466s), target-download rejection fails before; Affected usermachines/httpapi/app race PASS; final PostgreSQL tests PASS (usermachines92.415s, production readiness2.035s) with no artifact field or release directory. Target-download rejection PASS after fix. Full make gate PASS (/tmp/paperboat-lane-b-independent-cli-gate.log); nine deployment/workflow tests PASS. No publisher uploaded or release workflow run.

2026-10-14 | Lane B HTTP deadline completion regression | pending | — | A handler returning at its expired deadline raced the timeout select, sometimes committing an empty HTTP200 instead of structured HTTP503. The completion path now checks cancellation/deadline before committing the handler response; preserves started responses, parent cancellation and panic handling. TestTimeoutUsesStructuredError exercises 32 completions, fails before/PASS after; affected HTTP race PASS. Shared F1/F2/F7/F8/F15 request boundaries included in make gate PASS (/tmp/paperboat-lane-b-timeout-gate-final.log); no timeout check weakened.

2026-10-14 | Lane B global edge certificate targets | pending | — | Active completed global pbh installations were classified as customer self-hosted nodes, excluded from platform certificate distribution and hosted edge inventory. Both owners now include global nodes while excluding account/team, incomplete and inactive installations. TestPlatformCertificateTargetsRequireEdgeRoleOnPostgres real PostgreSQL regression failed before and passes after with certificate and inventory assertions; stale/unready/relay/empty and customer-scope exclusions retained. Race PASS (/tmp/paperboat-lane-b-global-certificate-after-final.log). Shared F1/F7/F8/F9 platform edge consumers are covered by make gate before push.

2026-10-14 | Lane B independent CLI producer deployed | ac4a44f | — | Server image paperboat-server:2026.10.14.3 built from committed producer and deployed on coolify; public /healthz and /readyz HTTP200 with no published release record. Lane A can update its consumer and retry locally built pairing now. Next global-target make gate is held on TestDashboardTokenPairingAndMaterialExchange in the actively changing Lane A bootstrap tests (bootstrap_test.go:65, machine pairing request failed); other Go modules and console pass. Log /tmp/paperboat-lane-b-global-targets-gate.log. No push with failing gate.

2026-10-14 | Lane B anvit port ownership blocker | pending | — | The existing coolify-proxy container owns TCP80/443 and UDP443 and serves other applications. New pbh claim listener is healthy on private8443; added a named host-firewall claim-port rule after public connectivity failed. Asked whether to retain existing applications (relay only) or dedicate ports to Paperboat edge; no unrelated proxy stopped. Oracle public claim listener is reachable. Continuing global node rollout and producer fixes independently.

2026-10-14 | Lane A independent CLI consumer | testing | — | Consuming pushed ac4a44f: pairing/setup requests and protected recovery material no longer carry server release targets. Existing TLS pairing exchange fails before without artifact and passes after. Machine/session/public identity/generation/expiry/one-use/checkpoint protections retained; three tests exclusively enforcing the deleted server-artifact contract removed, shared machine/generation and progress checks retained. Official installation/update/bundle/Windows repair derives TUF repository from buildinfo.DefaultReleaseURL, never control URL; custom target is empty and automatic official operations remain refused. Custom updater with an empty repository fails before at the native owner, passes after under root race testing. Shared features F1/F2/F14 and runtime consumers F3–F9 covered by full make gate; affected bootstrap/API/runtime/install race and Windows/macOS cross-build checks running. Server producer is now deployed as 2026.10.14.3; committed consumer build/device pairing follows gate.

2026-10-14 | Lane A browser publisher recovery regression | repaired | — | Full CLI gate found TestPublisherPureCancellationIsQuietButMixedFailureIsObserved reconnect failure. Registry removes the failed publisher before its session attachment detaches, so the shared broadcast_<session> ID collides with a replacement. TestPublisherReconnectSurvivesPreviousAttachmentCleanup deterministically delays the real session detach: fails before, passes after assigning each publisher epoch a distinct attachment ID and verifies delivery after old cleanup. Full browserbroadcastserver race PASS 1.620s; no deadline or assertion weakened. Independent CLI Windows/macOS builds PASS; bootstrap/runtime race PASS 9.714s/7.937s, isolated API race PASS 2.323s. Full make gate PASS at /tmp/paperboat-lane-a-independent-final-gate.log.

2026-10-14 | Lane A independent CLI consumer gate | ready | — | Full make gate PASS /tmp/paperboat-lane-a-independent-push-gate.log. Artifact-free pairing exchange and machine/generation/checkpoint recovery checks PASS; bootstrap/runtime race 9.714s/7.937s, isolated API race 2.323s, release-origin/custom-no-source race PASS; root custom updater race PASS with no repository. Windows/macOS builds PASS; Windows bootstrap/runtime/install/updater test packages compile. Source-build release host defaults empty; official builds use linker DefaultReleaseURL. Clean committed five-platform build and device pairing next.

2026-10-14 | Lane B global target producer gate | ready | — | Global certificate/inventory fix: TestPlatformCertificateTargetsRequireEdgeRoleOnPostgres fails before/PASS after; real PostgreSQL race PASS7.540s. Full make gate PASS on current shared HEAD14652d0 (/tmp/paperboat-lane-b-global-targets-latest-gate.log), including independent CLI consumer; isolated committed-source gate also PASS after correcting its dependency copy. No foreign source edits and no failing-gate push. Server rollout follows the separate tunnel/shared-domain fixes.

2026-10-14 | Lane B shared public edge base | pending | — | The edge rejected equal preview_base_domain and tunnel_base_domain although section1 requires one shared public base. It now permits exact equality while rejecting nested names and overlap with runtime_base_domain. TestDeploymentAllowsSharedPublicBaseAndRejectsRuntimeOverlap fails before/PASS after, config race PASS; existing isolation/normalization guards remain. Shared F7/F8/F9 edge configuration checked in make gate PASS (/tmp/paperboat-lane-b-shared-edge-base-gate.log).

2026-10-14 | Lane B claimed global carrier trust | pending | — | Global pbh carrier TLS must retain the claim-pinned Ed25519 identity independently of customer self_hosted ownership. The carrier now loads explicitly configured infrastructure TLS files for both roles; an unconfigured managed node still uses its process identity. TestSelfhostCarrierUsesClaimedInfrastructureIdentity covers both flags, fails before/PASS after; restart pin, hostname, expiry and unmanaged identity checks retained; tunnel race PASS. Shared F1/F7/F8/F9 native/global carrier trust included in make gate PASS (/tmp/paperboat-lane-b-global-carrier-gate.log). Server image2026.10.14.4 from293b74a is deployed with global certificate/inventory classification.

2026-10-14 | Lane A custom service declaration device regression | repaired | — | dev-14652d0 installs PASS on hp/apple/victus and custom automatic checks=false; fresh enrollment material now works without a release record. hp/apple fail at service installation because NewUpdaterInstaller still required a release repository; allowing custom absence also requires omitting the forbidden empty service environment value. TestHostdAndUpdaterInstallersUseStableDaemon with empty custom repository fails before on Linux/macOS, passes after under race (1.037s), full make gate PASS /tmp/paperboat-lane-a-custom-service-gate.log. Runtime/source/generation guards unchanged. Required smoke tools/device-smoke/results/20261014T092630Z.json: 6 PASS (versions/client doctors),20 FAIL (runtime operations), build14652d0; no feature completion claimed. Windows material/client/runtime checkpoints persisted, generic pre-elevation runtime preparation failure under investigation; no server release lookup remains. Clean next build resumes the protected same-token journal.

2026-10-14 | Lane A Windows pairing diagnostic blocker | partial | — | Two real victus pairing attempts on dev-14652d0 fail after material/client/runtime enrollment checkpoints and before Requesting administrator approval; the human retry still emits the generic runtime failure. Named elevated installer is not yet the proven cause. Protected journal retained, no credential or private output printed. TestWindowsPairingPreparationNamesFailedPhaseWithoutPrivateCause fails before for executable/runtime/control failures and passes after public phase wrappers; existing cause identity, checkpoint ordering and no-secret assertions retained, targeted race PASS 1.167s. Full make gate PASS /tmp/paperboat-lane-a-windows-phase-gate.log; rebuild after push will diagnose the next recovery without retrying unchanged code. Linux/macOS service declaration root cause is fixed/pushed in 0b1c850.

2026-10-14 | Lane A F1 pairing qualification stopped after two failures | blocked | — | Clean bb6898b five-platform builds and install --json PASS on hp/apple/victus, automatic checks=false. Unix pairing still fails service installation after the service-source constructor fix; Windows fails outside the new executable/runtime/control preparation wrappers. Stop unchanged full-pair retries. Protected machine/client/runtime checkpoints retained. Next independent work isolates the Unix privileged service boundary and inspects Windows registration/listener validation; no guessed service or server workaround. No device feature is [x]. Required bb6898b smoke follows this build before replacement.

2026-10-14 | Lane B S1 live wildcards and oracle global nodes | pending | — | Committed native package dev-997db88 installed on oracle/anvit; services active. Oracle authenticated operator global enrollment accepted relay+tunnel, both real runtimes report ready with current heartbeat/process epochs. Required Operator-Target/Reason/Idempotency-Key headers supplied after one guarded400; no authorization bypass. Both platform wildcard certificates now ready and acknowledged active on oracle. Read-only TestLivePlatformCertificateDeploymentProbe runs inside deployed server with protected key references: public CA chain, configured hostname, private-key match, ready current-edge acknowledgements and bounded renewal date all PASS0.08s; public expires2027-01-12T08:30:21Z, runtime08:27:25Z, both renew2026-12-13. Initial public issuer_unavailable retried successfully through existing bounded worker. Unknown public SNI is rejected before routing, so no HTTP/WSS publication forwarding claimed. Anvit port ownership remains pending required user decision; no unrelated proxy stopped. Lane A global claim-scope export request remains the managed runtime/browser blocker. Full make gate PASS (/tmp/paperboat-lane-b-live-wildcards-gate.log). Repeat the read-only probe by compiling server/internal/tunnelcert tests for linux/amd64, copying the test executable into the server container, and running only TestLivePlatformCertificateDeploymentProbe with PAPERBOAT_LIVE_PLATFORM_CERTIFICATE_PROBE=1 inside that container; it uses its existing protected environment and never migrates or writes the database. No test device used, no secret/key printed or committed.

2026-10-14 | Lane A waits for F1 expired installation recovery | blocked | — | Lane B: POST /v1/machines/pairings/installation with the protected verifier/public_identity_key and runtime_enrolled=true returns HTTP410 user_machine_installation_unavailable on hp; material expired after earlier administrator validation failed without a TTY. Binding: pairing pairing_7b029c3f-2a93-43e8-9960-368b3c11d523, machine machine_17a8fe92-d3c2-4d5a-b9a5-86de2c2c3ef4, generation 1. Native credentials and client checkpoint were persisted; direct privileged service install now succeeds and smoke exec/SSH work. Windows has the same expired material/runtime checkpoint and HTTP410 (two full pairing attempts already stopped). Please reproduce the deployed recovery guard, fix producer and its named errors without weakening identity/generation/session authority, then push/deploy. Do not mint a replacement enrollment or discard these protected credentials. Apple pb pair PASS on clean dev-bb6898b via one real administrator prompt; official release record absent as required. Shared consumer changes wait for producer first.

2026-10-14 | Lane A global claim-scope producer | repaired | — | Urgent S1-09/10 consumer request: relay/selfhost now passes the authenticated claim ScopeType through Setup and exports self_hosted=(scope!=global), preserving claimed infrastructure TLS paths and identity. Already-claimed installations re-export on startup without a new claim; supervisor waits for that locked export to finish before starting runtimes or certificate renewal can race the state lock. TestActualTLSGlobalClaimExportsManagedRuntimeAndReappliesOnRestart fails before/pass after on real pinned TLS claims, validates managed role, unchanged identity and existing-file convergence; existing three-second renewal/restart and child-drain assertions retained, full selfhost race PASS3.009s. Browser access remains disabled: enabling it requires configured discovery login origin and actual PSL isolation; no invented origin or relaxed tunnel PSL guard. Full make gate PASS /tmp/paperboat-lane-a-global-claim-push-gate.log (prior gate encountered Lane B readiness route before its OpenAPI publication). Lane B rebuild/restart claimed oracle/anvit helpers after producer push; Docker launch producer f467d4b remains available for S4-02 consumer.

2026-10-14 | Lane B S4-02 runtime readiness producer | pending | — | Docker pbh needs the actual authenticated node/control readiness, not systemd/container-running alone. Added read-only GET /v1/selfhost/runtime-readiness using only that node runtime bearer: node_id, capability, state, ready, process_epoch, nullable last_heartbeat_at/valid_until, server_time; no selectors/body accepted, no credential material returned, no-store. Readiness requires active completed unexpired installation, current nonempty process epoch, ready state and the existing control-plane heartbeat deadline. Missing/invalid/removed/incomplete credentials401 runtime_unauthorized; invalid input400; unavailable dependency503 runtime_readiness_unavailable; registered/offline/stale200 ready=false. Primary real PostgreSQL HTTP test passes8.225s; removing route reproduces404. Fixture setup initially exposed required regional node metadata and per-capability unique installation keys; corrected to full registry fields and real unique Ed25519 identities, with bounded cleanup. Shared S1/S4/F1/F7/F8/F9 node consumers checked in gate. Producer pushed first before helper Docker consumer; console existing installation ready fields remain its frontend contract.

2026-10-14 | Lane B runtime-readiness API gate | ready | — | TestSelfhostRuntimeReadinessAuthenticatesExactNodeOnPostgres -race PASS8.674s including expired credentials and no-store; route removal fails at expected404. Affected selfhost/httpapi vet/race PASS; full make gate PASS (/tmp/paperboat-lane-b-runtime-readiness-gate.log). Publishing this producer before helper consumer. Lane A global scope fix10e66b0 clears the managed-role producer blocker and will be deployed from committed source next; Anvit port approval remains pending.

2026-10-14 | Lane A device / F2 evidence | partial | — | Clean dev-bb6898b all five builds PASS; install PASS all three, custom automatic checks=false. Apple pair PASS after one real administrator prompt through a PTY. HP service installation succeeds at the privileged boundary; full pairing finalization and Windows pairing need the logged expired-journal HTTP410 producer fix. Full smoke tools/device-smoke/results/20261014T100051Z.json:14 PASS,12 FAIL; exec/SSH/held work pass hp/apple, terminal/transfer/preview fail and victus runtime unavailable. Account auth status signed_in=true all3; apple explicit browser reauth exit0 and live CLI signed in. Isolated headless VPS printed-link approval/login/auth status/logout PASS, final signed_in=false; local harness inherited a locked desktop Secret Service, genuine headless child omitted D-Bus address and used its explicit protected file-fallback configuration. Existing TestCredentialStoreAvailableRequiresSessionBus and TestBrowserLoginAliasesApprovalAndRecovery cover those owners. Default device sessions are retained while HP/Windows journals need producer recovery; no account or enrollment revoked. F3–F15 full device qualification remains behind F1-02 and S1 node consumer deployment; later rows stay [ ], no generic blockers added.

2026-10-14 | Lane B S4 Docker helper consumer qualification | pending | — | Helper serve now consumes RunWithLauncher and owns only Docker start/readiness/drain/removal. Protected atomic image refs, local host-architecture validation, host networking, root with dropped capabilities except bind-service, read-only container root and state mount; singleton lifecycle lock prevents another helper replacing live children. Readiness requires running owned container plus authenticated new node process epoch/fresh server heartbeat from33243e5. Refuses foreign-labelled colliding containers; command/output/time bounds retained. Installer checks the helper-owned supported-host policy before changes and installs Docker only when absent from configured official source; package now pbh/licenses/manifest only. Readiness redirect/foreign-identity/oversized-response and host matrix vet/race PASS. Real oracle image builds from committed33243e5 PASS; candidate both runtimes ready, both certificate active/current-process public-CA probes PASS0.10s. TestLiveDockerSupervisorReadinessAndBoundedDrain PASS14.63s; cancellation/drain5.261s, both owned containers gone; restored committed native pbh service healthy. Foreign-container preservation/owned teardown integration PASS0.55s. No device touched, no private material printed. Producer push and installer/upgrade qualification follow gate; no dev CLI release publisher revived.

2026-10-14 | Lane B Docker helper consumer gate/handoff | ready | — | Full make gate PASS (/tmp/paperboat-lane-b-docker-helper-consumer-gate.log); real Docker lifecycle/authenticated current-epoch readiness, bounded shutdown and foreign-container preservation tests PASS. Consumer no longer calls selfhost.Run/binary-dir; Lane A may remove the retired binaryLauncher/Run entry point after this push, retaining RunWithLauncher as the sole supervisor. Installer upgrade/fresh-mode qualification still follows; no public helper/CLI publication workflow run. Next blocking row is Lane A F1 expired installation recovery (Log630); preserve existing machine/runtime/client checkpoints.

2026-10-14 | Lane B F1 ready-journal recovery producer | ready | — | HP metadata confirms runtime heartbeat advanced installation_state to ready before the CLI journal finalized; bounded expired recovery incorrectly allowed only installing/connecting. POST /v1/machines/pairings/installation now permits ready recovery only with runtime_enrolled=true, the original verifier/public identity, consumption within the existing24h grace, exact current machine/environment/generation and active occupied runtime identity, preserving the existing helper and CLI session while rotating session tokens. No new enrollment or release lookup. TestExpiredInstallationRecoveryIsCrashSafeConcurrentAndIdentityBound reproduces410 before; race PASS34.418s including stale-generation rejection. Combined recovery/single-use/authenticated-generation tests race PASS136.807s. Shared F1/F13 enrollment and session consumers covered; Lane A retries its protected existing journals after deployment, no device access by Lane B.

2026-10-14 | Lane B F1 journal recovery gate | ready | — | Full make gate PASS /tmp/paperboat-lane-b-ready-journal-gate.log; no feature already[x], so no device smoke required. Publishing bounded ready-journal recovery producer now, then deploying server before Lane A journal retry.

2026-10-14 | Lane B S4-03 shared-state accessor request | blocked | — | Lane A: helper upgrade must retain the existing selfhost.Config (including custom listener, role ports, domains and TLS paths), but selfhost owns its protected state and exposes no configuration read accessor. Please expose one locked read-only existing-config method returning Config/error from the same state owner; helper will apply explicitly visited install flags only and pass preserved config to Initialize. Do not expose claim/keys or create a second state reader. Docker consumer330ea48 is pushed; this request blocks no device feature.

2026-10-14 | Lane B F1 recovery deployed | ready |71366e2| Server image2026.10.14.7 built from71366e2 and compose --wait completed on rebuilt coolify; public /healthz and /readyz200. Lane A: retry the same protected HP/Windows installation journals now; keep verifier, identity, machine generation and runtime_enrolled checkpoint, no replacement enrollment. No devices touched.

2026-10-14 | Lane B S4-05 explicit data confirmation | ready | — | pbh uninstall now stops the owner, acquires its exclusive Docker lifecycle lock and removes only containers labelled for the exact installation. Data retained by default; deletion requires both --remove-data and --confirm-remove-data before any privileged action. No Docker daemon or unrelated image/container removal. TestUninstallRequiresSeparateDataConfirmationBeforeMutation fails on330ea48/pass after; helper vet/race PASS. Real TestDockerTeardownPreservesForeignContainerAndRemovesOwnedContainer PASS0.76s for both owned roles and foreign preservation; initial unprivileged invocation lacked Docker socket access, corrected by running the compiled integration as root. Full gate follows.

2026-10-14 | Lane B teardown gate | ready | — | Full make gate PASS /tmp/paperboat-lane-b-teardown-gate.log. Publishing explicit data-confirmation and exact-owner teardown fix; full uninstall/reinstall qualification waits for the locked selfhost.Config accessor requested above so the existing infrastructure settings remain owned and unchanged across upgrade.

2026-10-14 | Lane B remaining infrastructure qualification | blocked | — | Native ARM64 helper check-host PASS in official Ubuntu24.04 and Debian12 read-only containers; amd64/arm64 GOOS/GOARCH builds PASS without emulator. Docker supervisor on existing Oracle Ubuntu24.04 already passed real both-role authenticated readiness/current epochs, public-CA certificate acknowledgements and bounded drain. S4-02/04 fresh systemd installer qualification needs an isolated supported host/public qualified helper package; existing Oracle has services, Anvit has unrelated proxy ports pending user approval, coolify Ubuntu26.04 is unsupported. Oracle upgrade/uninstall-reinstall additionally waits for Lane A locked Config accessor (one request Log648). S4-06 documentation now records actual Docker/teardown behavior and publication status. S1-09 protocol proof awaits an authorized live publication after Lane A F1 recovery; no unknown-SNI or container-running substitute. Lane B later F2–F15/F13 and S3 qualification remain in order behind Lane A F1/F2 and subsequent features, not generic[!] rows. No dev-release publisher resumed; no test devices used.

2026-10-14 | Lane B S3 offline root device-owner request | blocked | — | Minimal fresh TUF decision is accepted. Lane A exclusively owns test-device access under current AGENTS/user lane instructions: please generate/store the offline root key on apple without printing/exporting the private key, and provide only its public key/initial signed root metadata for the later release producer. Lane B will generate/store the online key through gh secret set when S3 reaches qualification. Apple installer certificate and Windows signing PFX still absent; no old signing-state machinery or release workflow executed. This later request does not preempt Lane A F1 recovery/devices.

2026-10-14 | Lane B infrastructure evidence gate | ready | — | Full make gate PASS /tmp/paperboat-lane-b-infra-evidence-gate.log. Recovery71366e2 is deployed healthy; Docker consumer330ea48 and explicit teardown2b95b48 pushed. Owned temporary local integration executables removed; qualified ignored packages and deployment probes retained for repeatable verification. Remaining own infrastructure blockers recorded above; subsequent server feature/release rows stay in PLAN order waiting for Lane A earlier F1/F2 qualification.
