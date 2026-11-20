# Working on Paperboat

Paperboat gives developers and agents access to their remote machines: a terminal with
replay and reconnect, remote exec, managed SSH/SCP/SFTP/rsync, file transfers, previews
and tunnels, ENV (end-to-end encrypted environment variables), teams, config sync and a
web console. Privacy matters: native access is end-to-end encrypted and peer-to-peer
where possible. UX comes first: setup, daily use, failure and recovery must be obvious.

Launch target: **2026-10-20**. [PLAN.md](PLAN.md) says what to build and in which order;
[CHECKLIST.md](CHECKLIST.md) is the only progress tracker (every scenario and every `pb`
command, per platform, with evidence). Post-launch ideas live in [WHITEBOARD.md](WHITEBOARD.md).
Keep development targets, credentials and operator runbooks outside the checkout.

**Zero users, zero compatibility (user rule, 2026-10-26).** Paperboat is pre-launch with no users. Only the
current `main` must work. Never write, propose or wait for anything that exists only for older builds,
releases or machine state: no migrations or cleanup of old on-machine state, no fallbacks or old protocol
paths, no deprecation periods, no staged or ordered deploys for old clients, no keeping endpoints alive for
released CLIs, no waiting for machines to update. Change producer and consumer together and deploy together.
A test machine broken by old state is reinstalled. Regressions: diff the last good build against the first
bad one on the paths the failing check touches, then fix.

**Fix efficiently (user rule, 2026-10-27).** Before changing code for a bug: (1) if it ever worked, name the
last working commit and read `git diff good..bad` on the touched paths; (2) read how the reference products
in `~/ref` solve the same thing (tailscale, headscale, netbird, teleport, cloudflared, ngrok-go, mosh, tmux,
EternalTerminal, ttyd) and cite the file. Copy proven designs instead of inventing new ones.

**Fast loop (user rule, 2026-10-26).** Before a push, CI runs only the packages the commit touches
(`tools/ci-test.sh <module> -run '<their tests>'`, minutes); the full all-module run is the CI health worktree's job
after each push batch. Each worktree keeps one paired pair of test machines and installs every new build over it
(`pb install` of the new binary); a fresh pair only for rows about install, pairing or enrollment.

## Repository

| Path | What |
|---|---|
| `cli/` | `pb` CLI and the endpoint runtime (`pb daemon`), Go |
| `server/` | control plane: identity, authorization, product state, Go + PostgreSQL |
| `relay/` | peer relay service (fallback when P2P fails), Go |
| `helper/` | `pbh` command: self-host installer/manager for relay and edge (moving here from `relay/` in S4-00), Go |
| `edge/` | public/browser edge for previews and tunnels, Go |
| `shared/` | code used by more than one module (wire protocols, common packages, helpers), Go |
| `console/` | web console (Next.js; read `console/AGENTS.md`, `DESIGN.md`, `NOTES.md`) |
| `admin/` | operator admin panel (Next.js, copied from `console/`'s patterns; operator sign-in, own origin; `tools/admin-prod.sh`) |
| `marketing/` | marketing site (Astro) |
| `tools/device-smoke/` | device smoke suite (see below) |

Go modules are bound by the root `go.work`; build and test from the repo root or a
component directory. Go 1.27.4.

## How we work

1. **Main paths of every feature first, then edge cases** (PLAN.md section 3 overrides the rest
   of this point until pass 1 is done). **Feature by feature.** Take one feature from PLAN.md, make it work on every supported
   platform (Linux and macOS for launch; Windows after launch, PLAN.md 1.1) in every way a user can use it, including edge cases,
   failures and recovery, before moving to the next. A feature is done only when its PLAN
   rows in CHECKLIST.md are `[x]` on real devices.
2. **One commit per fix.** Message: what broke, root cause, the change, the test that proves
   it. Submit the commit with `tools/push.sh` after verifying it. Never leave work uncommitted or unpushed at the end of a step. Never force-push or
   rewrite history.
3. **Regressions first.** Anything that worked on the last accepted build and fails now is
   fixed before any other work. Never call a regression a "remaining issue".
4. **No guessing.** Read the code path in execution order before changing it. Reproduce,
   find the cause, then fix. Never retry an unchanged command hoping for a different result.
   After two failed attempts at the same thing, write down what you know, mark the row `[!]`
   and move on to the next row.
5. **One session on the devices at a time.** Only the session that owns PLAN.md's current
   step installs builds on or tests against the test machines.
6. **Report results, not prose.** Lead with what works and what doesn't, with evidence
   (smoke table, test names, commit hashes). Plain language, short.
7. **Mark CHECKLIST.md as you go** (cell + evidence) and commit it with the work. Do not
   create other plan, tracker, handoff or report files.

## Regression gate (mandatory before every push)

Regressions are the most expensive failure in this project. They are prevented mechanically:

1. **Every `[x]` row in CHECKLIST.md has an automated check**: a smoke-suite test id or a
   named unit/integration test. A row that can only be checked by hand cannot be `[x]`
   (console UI rows use a scripted browser check or a recorded manual step re-run at each
   feature completion; say which).
2. **`make gate` before every push.** It runs vet and unit tests for every Go module, the
   console lint/build, and, when anything under `cli/`, `relay/` or `shared/`
   changed, the device smoke suite on a build of HEAD. `server/`, `edge/` and `helper/` changes
   push on their unit and PostgreSQL tests; after deploying them, run smoke against the deployed server, and a regression there is reverted or fixed first. **Every smoke check that has ever
   passed must still pass**, whether or not its feature is `[x]` yet.
3. **A previously green check that fails blocks the push.** Fix it in this worktree, or revert
   the change that broke it. Never push with a known regression, never mark it `n/a`, never
   weaken the check.
4. Before changing shared code (transport, daemon, auth, server APIs used by several
   features), list the features that use it and include their checks in the run.

**Pre-push hook.** `make hooks` (once per clone) installs `tools/hooks/pre-push`: it builds and vets the Go
modules your push touches on a clean checkout of the commit being pushed, so a file left out of the commit
cannot reach `main` (the working tree it passes in still has the file). The first run in a clone takes about a
minute, later runs a few seconds. `PB_PREPUSH_CROSS=1` also builds the cli for darwin and windows;
`PB_PREPUSH_SKIP=1` skips it (never push a tree you have not built).

## Concurrent work

Coordinate ownership before using shared development resources. Use `tools/push.sh` for submissions.
The script serializes pushes across worktrees and submits through the required GitHub checks.
For runtime changes, run the focused workspace checks before pushing and verify device smoke on the
pushed batch. Preserve the baseline checks during targeted reruns.

## Code quality

- **Ponytail (user decision 2026-10-26): use the ponytail skill (full) for every change; agents without that skill (Codex) follow the rule below directly.** The smallest complete change: reuse what exists, no options or abstractions nobody asked for, deletion beats addition, finish every caller/test the change touches, non-trivial logic leaves a test, never cut validation, data-loss handling, security or anything asked for. Replies end with one line on what was skipped or not checked.
- Simple and small beats clever and defensive. Prefer deleting code to adding it. Remove
  duplicate paths instead of adding another one.
- Functions under ~100 lines, files under ~1000 lines. Split when touching something larger.
  Lines readable without horizontal scrolling.
- One owner for each piece of state; one way to do each thing. No compatibility shims,
  hidden fallbacks, v2 contracts or dead code. This is unreleased software: change the
  contract and all its producers, consumers and tests together.
- Errors name the step that failed and what the user can do. Never hide a primary error
  behind a cleanup error. Typed errors where callers branch; never parse error strings.
- Bound everything: timeouts, sizes, retries with backoff. Clean up processes, files and
  locks on every exit path.
- **No hardcoded domains, hostnames or URLs anywhere in code**, including tests' production
  paths. Domains come from configuration: server config/environment for the server and edge,
  the server's discovery response for clients (CLI, console) at runtime, and build/deploy
  configuration only for the initial API address. Tests use a configured test domain
  (e.g. `example.test`), never a real one.
- Changeable values (limits, prices, policies) live in one configuration place, not copied.
- Clients ignore unknown fields in server responses (`internal/wirejson`) and keep checking types and
  required values; servers may stay strict on requests. Signed authorization claims stay exact, because
  an unknown claim could be a restriction the client does not understand.
- No stubs, placeholders or TODOs standing in for required behaviour.

## Migrations on a live database

- Follow [GitLab](https://docs.gitlab.com/development/migration_style_guide/) and [strong_migrations](https://github.com/ankane/strong_migrations): keep lock holding brief.
- Never mix data DML (INSERT/UPDATE/DELETE backfills) with ACCESS EXCLUSIVE DDL (constraint changes, column type changes, etc.).
- Add new foreign keys and CHECK constraints NOT VALID; VALIDATE in a later migration.
- Prefer one table's DDL per migration; run data backfills in bounded batches.
- Start DDL migrations with `SET lock_timeout = '5s'`; retry lock timeouts/deadlocks with bounded backoff.
- A column the previous server reads is dropped one deploy after the code stops reading it (post-deploy migration), never in the same deploy ([GitLab post-deployment migrations](https://docs.gitlab.com/development/database/post_deployment_migrations/); 2026-11-06 .06.2 dropped columns .06.1 still read: 19 × 503 in 2 s).
- Before touching tables whose triggers lock shared 443 names, take `SELECT pg_advisory_xact_lock(1885686843)` first.

## Testing

- **Unit tests** for every change, in the package that owns the behaviour. They must not
  be weakened to pass. Run the affected packages (`go vet`, `go test -race` for concurrency).
- Use `tools/remote-test.sh <host> <module> [go test args]` for committed Go changes on a test host,
  or `tools/ci-test.sh <module> [-run PATTERN]` for GitHub Actions.
- After a device run, use `tools/device-smoke/cleanup_test_account.py --prefix <run-prefix>`
  to review stale throwaway machines in the configured test account. Add `--apply` to revoke them.
- **A check that passed before and fails now is found by bisection, not by reading logs alone.**
  First re-run only that check (`smoke.sh --targets <device>` with `--skip` for the rest) on the
  last build where it passed, to confirm it is a regression and not a flake. Then bisect the
  commits between that build and HEAD with the same single check (`git bisect`, one device
  build per step) until one commit is named. Diagnose and fix that commit's change. Never rerun
  the full suite to investigate one failing check.
- **Every real-device failure gets a workspace test** that reproduces it (fails before the
  fix, passes after), so it is caught without devices next time.
- **Device smoke suite** after every device build:
  `tools/device-smoke/smoke.sh --expect-version <version>`. Report its PASS/FAIL table and record the result file and the
  build's commit hash in CHECKLIST.md evidence. Extend the suite when a feature's checks are added; never
  weaken a check to pass.
- Device builds are made from a clean, committed tree, once for all platforms:
  ```sh
  V=dev-$(git rev-parse --short HEAD)
  L="-X github.com/pinksaucepasta/paperboat/cli/internal/buildinfo.Version=$V -X github.com/pinksaucepasta/paperboat/cli/internal/buildinfo.Commit=$(git rev-parse HEAD) -X github.com/pinksaucepasta/paperboat/cli/internal/buildinfo.ProtocolVersion=1 -X github.com/pinksaucepasta/paperboat/cli/internal/buildinfo.DefaultServerURL=https://api.pprbt.dev -X github.com/pinksaucepasta/paperboat/cli/internal/buildinfo.DefaultReleaseURL=https://api.pprbt.dev"
  cd cli && for t in linux/amd64 darwin/arm64 windows/amd64; do CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags "$L" -o ../.build/$V/pb-${t%/*}-${t#*/}$([ ${t%/*} = windows ] && echo .exe) ./cmd/pb; done
  ```

## Test devices

Configure the Linux, macOS, Windows and teammate test targets locally. See the
[device smoke setup](tools/device-smoke/README.md). Use the build's own `pb install` and
`pb pair` on test devices. Leave every device fully working or cleanly uninstalled,
and remove temporary files, processes and test binaries you created.

## Security and privacy

- Never print or commit credentials, tokens, keys, ENV values, terminal or file contents.
- Authenticate and authorize the exact resource before forwarding or acting on it.
- Native private access is end-to-end encrypted; browser HTTP terminates TLS at the edge.
  Never silently switch trust boundaries or call edge-decrypted traffic end-to-end encrypted.
- Installing a missing tool a task needs is fine (official sources, user-scoped where
  practical). Paid, destructive or security-policy changes need the user's approval.
