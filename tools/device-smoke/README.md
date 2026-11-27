# Device smoke

Copy [targets.example.json](targets.example.json) to `~/.config/paperboat-dev/targets.json`,
restrict access to the file to your user, and fill in your test machine names and SSH logins.
Set `PB_DEV_TARGETS_FILE` to use a different private file. Shell environment values take
precedence over the corresponding entries in the `smoke` object.

- Set `results_dir` to an absolute path to share local receipts across worktrees. Smoke
  and the regression gate use that directory. The default is the checkout's ignored
  `tools/device-smoke/results/` directory.
- `remote_hosts` maps roles to SSH destinations for remote tests and skew preflight.
  `skew_host` optionally selects a default skew build host. If it is unset, the skew check
  starts a disposable Linux CI runner through `pool_jump`.
- Keep test-account IDs in `cleanup_accounts`. Use the configured test account for
  throwaway machines and keep account credentials out of the checkout.


## Fixtures (permanent, verified before every run)

A check that needs a fixture never SKIPs for a missing one: `smoke.sh` and `windows_checks.py` run
`preflight_fixtures.py` first, which reports every missing or broken fixture (what, and the command that fixes it) in one
list and exits 2 before any check starts. `preflight_fixtures.py --list` shows which check needs which fixture.
The settings live in the `smoke` object of `~/.config/paperboat-dev/targets.json` (keys must start with `SMOKE_`):

| Setting | Fixture | Used by |
|---|---|---|
| `SMOKE_CONSOLE_ORIGIN`, `SMOKE_PW_CHROMIUM`, `SMOKE_PW_MODULE` | the live console, headless Chromium and `playwright-core` (`npm i playwright-core` in `~/.config/paperboat-dev/pw`) that sign accounts in and mint enrollment tokens | every fixture below |
| `SMOKE_OPERATOR_STATE` | console session (Playwright storage state, mode 600) of the smoke team's owner `pprbtdev@gmail.com`: `node console_session.mjs password pprbtdev@gmail.com ~/.config/paperboat/console-workos.password STATE` | `config-team-default`, `team-business` |
| `SMOKE_FREE_STATE` | the **Free fixture account** (never subscribes; created once with `fresh_account.mjs signup ~/.config/paperboat-dev/free-account`, which leaves `account.json` and `state.json` there) | `free-plan` |
| `SMOKE_CONFIG_REPO` | `pujan-modha/paperboat-smoke-team-config`: a private GitHub repository connected to the operator account as a config repository (the account's GitHub connection already exists; connect another with `POST /v1/config-repositories` {provider github, numeric external_ref, display_name owner/name}); keep it, `config-team-default` only sets and unsets the team default | `config-team-default` |
| `database_host` (targets.json, not in `smoke`) | operator `psql` over ssh: `workspace_usage.py` caps or seeds the current usage period of one throwaway account (`final-<hex>@`, under six hours old; anything else is refused by the SQL) | `billing-console`, `bandwidth-throttle-send` |
| `SMOKE_FIXTURE_HOST`, `SMOKE_FIXTURE_PB` | an SSH host with passwordless Docker (a linuxpool runner) and the Linux build for its architecture; `fixture_machine.sh` builds the container image there once | the four checks run by `fixture_checks.py` |
| `SMOKE_OPERATOR_API`, `~/.config/paperboat-dev/operator-cred` (the API origin; mode 600: operator email, password, authenticator key; `forward_live.py` format) | the smoke team `paperboat-smoke-paid` keeps a running paid plan and the SSO and SCIM add-ons. They are comp grants that end (the plan after at most 366 days): when preflight says the plan is gone, run `python3 operator_fixture.py ensure` (idempotent; never touches a Stripe subscription) | `config-team-default`, `team-business` |
| `~/.f19-s3ak`, `~/.f19-s3sk` (mode 600) | the S3 keys of the audit-log stream store | `team-business` |
| rsync on every Windows target (and `--peer` = the peer machine's **alias**: `pb scp`/`pb rsync` refuse a `machine_<id>` operand, a product bug recorded in the 2026-11-22 triage) | MSYS2 `rsync` on the machine `PATH` (`C:\msys64\usr\bin\pacman -S --noconfirm rsync`; `C:\msys64\usr\bin` on PATH) | `ssh-rsync` |

`fixture_checks.py free-plan|config-team-default|team-business|recording-key-rotation` (called by `smoke.sh`) gives each check its own throwaway
machines in containers on `SMOKE_FIXTURE_HOST` (`fixture_machine.sh up HOST NAME BUILD STATE`: paired as the account of the
stored console session, revoked by exact id when the check ends, on every exit path). `team-business` also starts, per run,
an S3 store (versitygw from its official image, filesystem backend, in the machine's own network namespace) because the check
reads the stored objects back as plain `.ndjson.gz` files, which MinIO's object format does not allow; nothing of it outlives the
run and no other host is involved (the server reaches it through the machine's ephemeral tunnel).
`recording-key-rotation` (PB-R1) needs a team that has no recording key (the team's first recording, started from the console, makes
generation 1), so every run makes its own, through the product: two throwaway console accounts (owner and member, `fresh_account.mjs`), a team
the owner creates with `pb team create` and pays in Stripe's **sandbox** Checkout with the 4242 test card (`stripe_checkout.mjs`; prod runs on
the Stripe sandbox until launch), the member invited and accepting, two machines of the owner (one runs `recordings_key_rotation.py`, the other is
the team machine that records) and the member's pb as a profile in the first machine's container. The recording starts only in the console, whose browser (node, Chromium,
the owner's session) is on the controller: the container asks for it through files and the controller runs `console_record_start.mjs`
(`console_start_bridge.py`). The member leaves the team, machines are revoked, and deleting the owner's account deletes the team and cancels its subscription; both accounts are gone when the run ends; a record that cannot be removed is reported as
`LEFT BEHIND` and fails the check.
`billing-console`, `bandwidth-throttle-send`, `plus-alias-signup` and `billing-team-lifecycle` (`billing_checks.py`) each make their own throwaway accounts and delete them
(which cancels the subscription). Money safety: nothing is typed into a Stripe page unless it is a test-mode one (a `cs_test_` Checkout or a `test_`
portal session AND the "Sandbox" badge, `stripe_pay.mjs`), and only the 4242 test card is used.

| Check | CHECKLIST cells | What it does |
|---|---|---|
| `billing-console` | F13-04C, F13-06, F13-04B | live console: Free plan screen and allowance card, Checkout to Pro monthly, paid card, portal switch to yearly and back with seeded spend (bandwidth window, billing day, allowance and spent bytes unchanged) |
| `bandwidth-throttle-send` | F13-03S, F13-06 | two containers, UDP blocked (relay only), fair-use amount used up: `pb send` finishes intact at 512 kbit/s and says why (the plan-limit cut was removed, 02e55fb13); the console card says "Slowed" |
| `billing-team-lifecycle` | F13-04 (part), F13-04C (Team monthly) | team bought on console Checkout; a seat added mid-cycle (charged at once: one more paid invoice) and removed, billing date unchanged; cancel and keep in the portal. Not covered: failed payment, grace, suspension, recovery (Stripe test clock and test API key needed) |
| (L37, L38 steps) | F13-04, F13-04C | `billing-console`: a Checkout left unpaid does not block the next one and can no longer be paid. `billing-team-lifecycle`: the removal survives cancel and keep, and the Seats section reads "3 seats until <date>, then 2." (" if you keep the plan" while cancelling), right after the removal and after a reload |
| `plus-alias-signup` | F13-07 | sign-ups with plus addresses of an existing account are refused by name; a plus address with no base account signs up; the base address signing up afterwards must not land in that account |

| `billing-declined-card` | F13-04 (failed payment at purchase), F13-05 (sandbox purchase) | a declined test card leaves the workspace Free and says why; the good card on the same Checkout recovers |
| `billing-topup` | F13-04T, F13-03T, F13-06A | Free refused (409), quantity limits, 200 GB bought on the console and credited once; with the plan allowance used up, relayed traffic is taken from the top-up |

### Access policy, API tokens, tunnel and preview rules (`access_checks.py`)

Each check makes its own throwaway account(s) and containers on `SMOKE_FIXTURE_HOST`: `policy-cli` (F18-01, F18-10, C244-C253, C289, C290),
`policy-token` (F18-06), `machine-tags` (F18-11, C262), `policy-team` (F18-02, F18-04, F18-05, F18-07, F18-08; a team paid in Stripe's
sandbox), `tunnel-create` (C279), `tunnel-route-rules` (F8-16), `tunnel-ip-rules` (F8-13, C280), `preview-ip-rules` (F7-09, C281),
`tunnel-origin` (F8-09..F8-12), `tunnel-origin-proxies` (F8-09). The two IP rule checks need `SMOKE_OBSERVER_SSH`, a second host with curl
and python3 whose public address differs from the controller's.

A controller that must not run a browser sets `SMOKE_PW_CHROMIUM` to `remote_chrome.sh` and `SMOKE_BROWSER_HOST` to a host with Chrome:
the browser runs there, node and every session state stay on the controller. Stripe Checkout offers a buyer in the US a list of payment
methods; `stripe_pay.mjs` picks "Card" there.

When a console session has expired, preflight says which one: sign it in again with `console_session.mjs` (operator) or
with the credentials in `free-account/account.json` (Free account).

## Sign-in preflight

Before the first check, `windows_checks.py` and `smoke.sh` ask the server through every CLI profile the selected rows use
(`pb team list`, `profile_preflight.py`; `auth status` also says signed in for an expired session): the device's own
sign-in and, for the team checks, its second profile, plus the Unix fixtures named by `--owner-cmd`/`--member-cmd`. A row
whose profile is signed out is a FAIL row naming the profile and the command that signs it in, and its check does not run;
the other rows run. `smoke.sh` also writes a `sign-in:<device>` FAIL row for a target whose own sign-in is out. The
preflight never signs in, mints or copies a token.

## Console rows (browser)

`fixture_checks.py console-team-admin|console-signout-switch|console-invite-mail|console-recording-replay` (called by `smoke.sh`,
`console_rows.py`) drive the live console in headless Chromium on the controller. Only the replay row needs machines (`SMOKE_FIXTURE_HOST`). Run one browser at a time on the controller: the full
E2E gives every shard with these rows the `browser` lock. Every row keeps its step list in `results/fixture-checks/`, addresses removed.

| Row | Cell | What it does and what it leaves |
|---|---|---|
| `console-team-admin` | F19-13 | `check-f19.js` (roles, audit log filters, CSV export), `check-f19-security.js` and `check-audit-streams.js` on the smoke team with `SMOKE_OPERATOR_STATE`. Each export adds one `team.activity_exported` audit event (the log is append-only). A configured SSO connection, directory groups and the bypass log need an identity provider (`check-f19-sso-round.js`, by hand). |
| `console-signout-switch` | F2-07 | signs the owner in anew for this run (`SMOKE_OPERATOR_PASSWORD_FILE`, default `~/.config/paperboat-dev/console-workos.password`, mode 600), runs `check-machines-paging.js`, pages the real machine table when the account has over 50 machines (`check-machines-paging-stub.js` stays by hand: its in-tab fake races the real server render and fails at random), switches Personal ↔ team and signs that session out. It refuses to sign out `SMOKE_OPERATOR_STATE` and proves it still works. |
| `console-invite-mail` | F12-02-mail-live | invites a new disposable mail.tm inbox (only this run can read it; no other address is ever invited) to the smoke team, reads the mail, Resends from the console, opens the link signed out and signs up. `SMOKE_MAIL_CONSOLE_ORIGIN` is the console the server writes into mail links (default `SMOKE_CONSOLE_ORIGIN`); a link to another host fails and is not opened. The invitation is cancelled by its id and the account deleted. |
| `console-recording-replay` | F25-05 | a throwaway owner and sandbox-paid team as in `recording-key-rotation`; `record_for_replay.py` makes a ticking, a tiny and a ~40 MB recording on the team machine and `check-recordings.mjs` replays them in the browser (decrypt, play, pause, seek, speed, unknown recording, REC in the web terminal, then every replay rule removed while the long one downloads). A skipped step fails the row (except the offline-machine state, not in the cell). Deleting the account deletes the team. Linux recordings only; Windows and macOS targets stay with X-33. |

## Recipe rows (PLAN F26)

`recipe_checks.py` (smoke.sh `recipe-lifecycle`, `recipe-errors`, `recipe-vaults`, `recipe-api-tokens`, `recipe-lifetimes`,
`recipe-install:{container,linux,macos,windows}`; CHECKLIST F26-01..04, F26-06..08, X-47). One run makes one throwaway console
account (so the smoke account's ENV password never leaves the controller, and deleting the account takes every recipe, vault,
tag and machine with it) and a paired creator container on `SMOKE_FIXTURE_HOST` (`SMOKE_FIXTURE_PB`). Linux targets are
containers there. The real-machine rows are NOT RUN unless the run names a throwaway machine **with no Paperboat on it**:
`SMOKE_RECIPE_LINUX_SSH`, `SMOKE_RECIPE_MACOS_SSH` (passwordless sudo), `SMOKE_RECIPE_WINDOWS_SSH` (key login as an
administrator, e.g. `winpool-N`). A machine that has Paperboat, a paired smoke machine or a personal PC is refused untouched;
a used one is uninstalled at the end. The one-liner is the line `pb recipe create` prints (`curl -fsSL <installer> | sh`,
`irm <installer> | iex`), with `PB_RECIPE` on stdin, pinned to `--expect-version` when that is a release. `recipe-lifetimes`
takes about 10 minutes. Not automated: the console's offer to create a missing vault (its recipe editor was removed on
2026-11-21; the CLI path is checked) and reading the server's stored vault bytes.

`recipe-apply:{container,windows}` (F26-05) pairs a machine from one commented recipe file with every section and restarts its daemon; the
config repository is a public HTTPS one (`SMOKE_RECIPE_CONFIG_URL`). A Windows target needs `python`. The script also takes
`linux` and `macos`; a variant becomes a smoke row after its first passing run on a real machine.

## Rows that need an account of their own (F10-07, F27-05, F1-03)

`account_rows.py` runs on the same harness and settings with its own throwaway account. smoke.sh rows:
`env-vault-reset-locked:{container,macos,windows}` (reset of a locked ENV vault), `held-web-ports:{container,macos,windows}` (a web
server holds 80/443 before Paperboat starts; Linux (nft) and macOS (PF) keep working behind the guard's redirect with `pb doctor`
healthy; on Windows IIS takes :80, doctor names it and recovers when IIS stops: the machine needs the IIS service installed and
stopped, and a disabled one is enabled only for the check) and `pair-token-refusals:{linux,macos,windows}` (the other OS's pasted
command, and a used, revoked and expired token; about 13 minutes because the expired token is waited out). The rows without
`:container` use the clean `SMOKE_RECIPE_<OS>_SSH` machine and are NOT RUN without it. The wrong-platform paste needs `pwsh` on
Linux and macOS and Git bash on Windows. Not automated: a pairing the server refuses as an unsupported platform (no supported test
OS can send one).


## Staging edge outage (F21-17)

`fixture_checks.py staging-edge-outage` (`smoke.sh` row `staging-edge-outage`, `staging_edge_outage.py`) runs on **staging only**.
It makes a public tunnel and a public preview on a paired staging fixture, cuts every staging edge off the staging control server
for 100 s (DROP rules inside the edge's network namespace; a timer on the staging host removes them even if the run dies), and
requires every URL to keep answering with the marker during the cut and for 60 s after it, then to drop (404) only when deleted.
It refuses to change anything unless the API host has an `stg` label and is not `SMOKE_OPERATOR_API`, the fixture's pb is signed in
to it and each edge's own `control_url` is it. One run at a time. It disturbs shared staging: nothing else may use the staging
edge during the row.

| Setting | Default |
|---|---|
| `staging_host` (top level) | none |
| `SMOKE_STAGING_API` | none, e.g. `https://REGION.stg.DOMAIN:18443` |
| `SMOKE_STAGING_MACHINE` | `pb-stg-a` (a paired container on `staging_host`) |
| `SMOKE_STAGING_EDGES` | `pb-stg-edge:18446:stg_managed_edge` (container, public HTTPS port, node id; comma separated; list every running edge). A fourth field, an IPv4 address of the staging host, is for an edge whose port is closed from outside it: the row forwards the port over ssh, so that edge is checked from the staging host only |

Needed beforehand: the staging server, edge and fixture on the build under test (`deploy/staging.py status`, `check`, `edge IMAGE`).

## Staging edge idle limit (F21-37)

`fixture_checks.py staging-edge-idle` (`smoke.sh` row `staging-edge-idle`, `staging_edge_idle.py`) runs on **staging only**.
With the routes' own idle limit at 90 s and the edges' at 120 s it holds one quiet and one busy (a byte every 10 s) stream on a
public TCP route, a TLS-passthrough route and a private route (`pb access` on the second fixture) to an origin it starts on the
first. Quiet streams must close 115-130 s after their last byte with a clean EOF (not a reset, not at 90 s), busy ones must be
alive at 200 s, and an HTTPS tunnel on the same edge must answer 200 every 30 s. It takes about 5 minutes and deletes what it
made; the public streams run from the controller straight to the edge's HTTPS and public TCP ports.

The row never restarts an edge. Every ready staging edge needs the limit (each route is on every edge, and `pb access` takes
whichever the server lists first), so set it before the row and remove it afterwards, from a checkout at current `origin/main`
(`edge-b` only when that edge runs; `STAGING_EDGE_HOST_WEB=0` while another service holds the staging host's 443 and 80):

```sh
STAGING_PUBLIC_TCP_IDLE_SECONDS=120 python3 deploy/staging.py edge IMAGE     # and: edge-b IMAGE
SMOKE_STAGING_EDGES=pb-stg-edge:18446:stg_managed_edge,pb-stg-edge-b:18456:stg_managed_edge_b \
  python3 tools/device-smoke/fixture_checks.py staging-edge-idle
python3 deploy/staging.py edge IMAGE                                          # restore; and: edge-b IMAGE
```

Without the limit the row refuses (a FAIL that names the commands). It also refuses to run from a checkout that lacks any
`origin/main` commit touching `tools/device-smoke` or `deploy/staging.py`, and changes nothing unless every target is provably
staging (the guard of `staging-edge-outage`, whose settings and lock it shares). `SMOKE_STAGING_CLIENT` names the second
fixture (default `pb-stg-b`).

## Extra Windows machines (winpool)

`winpool.sh` turns up to fourteen GitHub-hosted `windows-latest` runners into temporary Windows test machines, so
several worktrees can run Windows checks at once instead of queueing on windows-test.

```sh
tools/device-smoke/winpool.sh start 2 3     # 2 machines for 3 hours (1-5); prints winpool-1, winpool-2 when SSH answers
scp .build/$V/pb-windows-amd64.exe winpool-1:pb-new.exe
export SMOKE_WINDOWS_SSH=winpool-1; unset SMOKE_WINDOWS_PASSWORD
python3 tools/device-smoke/windows_lifecycle.py --expect-version $V --windows-install-json /tmp/winpool-1.json \
  --machine winpool-<worktree>-1 --build 'C:\Users\runneradmin\pb-new.exe' install-pair doctor status
python3 tools/device-smoke/windows_checks.py --install-json /tmp/winpool-1.json --machine winpool-<worktree>-1 --peer linux-test --only exec-main
tools/device-smoke/winpool.sh stop 1        # hands it back (POOL_END=1 ends it); then remove the machine records:
tools/device-smoke/cleanup_test_account.py --prefix winpool- --stale '5 minutes'   # dry run, then --apply
```

- Machine N is the SSH host `winpool-N` (user `runneradmin`, an administrator; key `~/.ssh/winpool_controller`).
  `winpool.sh` writes `~/.ssh/winpool.conf` and includes it from `~/.ssh/config`.
- Name the Paperboat machine `winpool-…` so the cleanup finds it. `install-pair` mints its token on linux-test; nothing
  of Paperboat is stored in GitHub.
- Start and stop only from this controller (build-host): the start lock and the record of who started which machine
  are local to it. `stop` acts only on machines started from your worktree; never stop another worktree's.
- A machine is gone after its hours, at `stop`, or when its run is cancelled; nothing on it survives.
- Evidence for a row ticked from a runner says so (`winpool, Windows Server 2025`); the final pass is on windows-test.

### Go tests on a winpool machine

```sh
tools/win-remote-test.sh winpool-1 cli                       # whole module; also: shared, ./internal/x/..., RUN=TestName, RACE=-race
```

Never run `go test` in a plain SSH session: it is a network logon in session 0 (no console, some pipes and child
processes refused, git cannot create folders), so a raw `go test ./...` of the cli failed 273 tests that pass for a user
(2026-11-22). `win-remote-test.sh` syncs the committed HEAD, then runs `tools/win-test.sh` as a scheduled task of the
machine's own user (`/IT /RL HIGHEST`), which starts in the logged-on console session, the session GitHub's runner steps
use. `win-test.sh` is also what `cross-platform.yml` runs, so a winpool result and a CI result mean the same thing:
every test binary runs through `cli/tools/win-test-exec` with an unelevated token and a private TEMP/APPDATA the token
owns; the tests in `cli/tools/win-test-exec/elevated_tests.txt` run second, elevated, and the run fails if one of them
never ran (a renamed or deleted test). Every failure is collected, none stops the run. The run prints counts and every
skipped test; the `go test -json` events and the log are copied to `$WIN_TEST_OUT` (default `/tmp/pb-win-test-<host>-<sha>`);
the CI job uploads the same events as the artifact `gotest-events-<module>-windows`. The first run on a machine downloads
the Go toolchain and modules (about 3 minutes extra); a full cli run takes about 10 minutes. One job per host at a time.

Send these to windows-test, they cannot run on a runner:

- anything that reboots, sleeps or logs the user off (the runner and its tunnel do not come back);
- anything that needs a signed-in desktop: tray or toast UI, a browser opened by `pb login`, per-user startup at logon;
- Windows Sandbox, Hyper-V guests, and rows that are about Windows 11 itself (the runner is Windows Server);
- LAN rows: direct peer-to-peer on one network, mDNS/local names, cutting the network (it cuts the tunnel);
- non-administrator installs and UAC prompts (the SSH session is already elevated, there is no second user);
- anything longer than 5 hours.

How it is wired: the runner starts sshd on loopback with the controller's public key (repository variable
`WINPOOL_CONTROLLER_PUBKEY`) and holds `ssh -N -R 127.0.0.1:2220N:127.0.0.1:22` to the jump host as user `winpool`
(secrets `WINPOOL_TUNNEL_KEY`, `WINPOOL_TUNNEL_HOST`, `WINPOOL_TUNNEL_KNOWN_HOSTS`). On the jump host that user has
no shell and `/etc/ssh/sshd_config.d/70-winpool.conf` allows it only those fourteen loopback listeners; the key's own `permitlisten=` options in `~winpool/.ssh/authorized_keys` must list the same fourteen ports (a port missing from either place makes `ssh -R` fail forever, 2026-11-10 slots 7-10). The controller
reaches a machine with `ProxyJump` through its own login on the jump host (`WINPOOL_JUMP`, default `tester@database-test-host`).

## Linux test hosts that stay up (linuxpool)

`linuxpool.sh` keeps up to twelve GitHub-hosted `ubuntu-latest` runners alive for 1-5 hours as SSH test hosts, so a
heavy suite does not wait for a new runner and set it up on every `tools/ci-test.sh` run. Each host has Go 1.27.4, gcc
(for `-race`), passwordless sudo and a `postgres:17` container behind `~/.pb-test-pg-dsn`, which is what
`tools/remote-test.sh` expects.

```sh
tools/device-smoke/linuxpool.sh start 3 2     # 3 hosts for 2 hours (1-5); prints linuxpool-1.. when SSH answers (~2 min)
tools/remote-test.sh linuxpool-1 server -run . ./internal/edgeset/   # PostgreSQL suites work; a second job reuses the host at once
tools/device-smoke/linuxpool.sh stop 1        # hands it back; it stays up for the next lane (POOL_END=1 ends it)
```

- Same design, controller key, tunnel secrets and claim records as winpool/macpool; host `linuxpool-N` is user `runner` on
  the jump host's loopback port `22400+N` (1-12), written to `~/.ssh/linuxpool.conf`. Start and stop only from the controller.
- The jump host's `winpool` tunnel account must allow ports 22401-22412 in both `70-winpool.conf` and the key's `permitlisten=`.
- `remote-test.sh` runs one job per host at a time (host lock); start several hosts to run suites in parallel.
- With `~/.config/paperboat-dev/linuxpool-gh.{repo,token}` (0600) on the controller, `start` (macpool.sh too) dispatches the same workflow in that
  repository, whose account has its own job limit and its own tunnel key (`permitlisten=` 22301-22308 and 22401-22412 only).
  After changing `linuxpool.yml` or `macpool.yml`, copy it there.
- Windows has its own file, `~/.config/paperboat-dev/winpool-gh.repo` (plus an optional `.token`): a third owner's runner repository, so the main
  repository keeps its job limit for CI and releases. GitHub runs 20 jobs per owner at once. Each tunnel key and the jump host's `PermitListen` list
  the ports it may open: a new slot number needs its port in both.
- A host is gone after its hours, at `stop`, or when its run is cancelled; nothing on it survives.

### Pool runners revoke their machine records (linuxpool, macpool, winpool)

A runner's throwaway machines (`<pool>-<build>-<slot>`) stay "online" on the server until their heartbeat times out after
the runner ends, which fails the deploy gate's "online before, offline after" check. So the end of a runner revokes its
records first, by exact machine id, through `cleanup_test_account.py --pool P --slot N` (still only throwaway machines of the
cleanup accounts, never `cleanup_protected`): `stop`, `release`, and `start` (leftovers of the slots it takes) via
`pool_revoke.sh`; `sweep` revokes the records of every slot that is no longer up and not still starting, and the pool keeper
(`paperboat-runner-pool`) runs `linuxpool.sh sweep` on every run. `<pool>.sh record N machine_<id>` remembers an id paired on
slot N so its revoke is exact even after a rename; the names are looked up on the server as well. A failed revoke still
stops the runner but exits non-zero. `POOL_REVOKE=0` turns it off. Self-test: `python3 test_pool_revoke.py`.

## Extra macOS machines (macpool)

`macpool.sh` is `winpool.sh` for macOS: up to eight GitHub-hosted `macos-latest` runners (arm64, macOS 26, 3 cores) as
temporary Mac test machines, so worktrees need not queue on the single macos-test VM.

```sh
tools/device-smoke/macpool.sh start 1 3     # 1 machine for 3 hours (1-5); prints macpool-1 when SSH answers
scp .build/$V/pb-darwin-arm64 macpool-1:pb-new
ssh macpool-1 '(umask 077; cat > tok)' < token-file     # token: pb machine add --json on linux-test (the token file must be owner-only)
ssh macpool-1 './pb-new install && ~/.local/bin/pb pair --enrollment-token-file ~/tok --name macpool-<worktree>-1'
tools/device-smoke/macpool.sh stop 1        # then revoke the machine record by exact id
```

- Machine N is the SSH host `macpool-N` (user `runner`, passwordless sudo, arm64; key `~/.ssh/winpool_controller`;
  hosts in `~/.ssh/macpool.conf`). Same controller, start lock and claim records (`~/.ssh/macpool-claims`) as winpool.
- Every login (command, shell, scp/sftp) runs in the runner user's GUI session (sshd `ForceCommand` wrapper), so the login
  keychain works: `pb install`, `pb pair` and `pb doctor` are healthy, as in the Mac's own Terminal. A plain sshd login on
  macOS is a Background session where the keychain refuses ("User interaction is not allowed", `pb pair` fails). The
  daemon itself runs as a system LaunchDaemon, so `pb exec` into the machine has no keychain.
- Limits: 8 slots, at most 5 hours, GitHub's macOS concurrency (5 jobs at once, shared with every workflow; extra
  jobs queue, and macOS arm64 runners can queue for minutes). Nothing survives the run. No reboot, sleep or logout
  (the tunnel dies), no LAN, no second OS user, no Spotlight/GUI-app rows. The final macOS pass stays on macos-test.
- Name the Paperboat machine `macpool-…` so the cleanup finds it (`cleanup_test_account.py --prefix macpool- --apply`).
- Wiring: `.github/workflows/macpool.yml` (manual dispatch only) reuses the winpool secrets and variable; the jump
  host's `winpool` user also lists loopback ports 22301-22308 (`70-winpool.conf` and `permitlisten=` in its `authorized_keys`).

## SSO from the Windows CLI (F19-03, F19-04)

`sso_windows.py` runs `pb login` (SSO sign-in, exit code read from the process) and the Require-SSO flow (a real non-SSO
member's Windows `pb team get` refused with `team_sso_required`, works when exempt and after Require SSO is off) on one
runner. Each result line has the device, the command and the exit code. Console sessions come from `console_session.mjs`
(`password EMAIL PASSWORD_FILE OUT` for a password account, `sso USER FIXTURE OUT` with `IDP_START` for an IdP user).

```sh
export SMOKE_WINDOWS_SSH=winpool-N PB_SMOKE_WINDOWS_PB='C:\Program Files\Paperboat\users\<id>\bin\pb.exe'   # pb install ran, no pair
python3 tools/device-smoke/sso_windows.py login --team T --sso-state alice.json --out f19-03.out
python3 tools/device-smoke/sso_windows.py require-sso --team T --member-state m.json --owner-state o.json --out f19-04.out
```
Each run enrolls the runner as a machine of the signed-in account: revoke that record by exact id afterwards.
