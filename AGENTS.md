# Working on Paperboat

Paperboat gives developers and agents access to their remote machines: a terminal with
replay and reconnect, remote exec, managed SSH/SCP/SFTP/rsync, file transfers, previews
and tunnels, ENV (end-to-end encrypted environment variables), teams, config sync and a
web console. Privacy matters: native access is end-to-end encrypted and peer-to-peer
where possible. UX comes first: setup, daily use, failure and recovery must be obvious.

Launch target: **2026-10-20**. [PLAN.md](PLAN.md) says what to build and in which order;
[CHECKLIST.md](CHECKLIST.md) is the only progress tracker (every scenario and every `pb`
command, per platform, with evidence). Post-launch ideas live in [WHITEBOARD.md](WHITEBOARD.md).
Machine access and credentials are in `REMOTE_TESTING.md` (local only, never committed).

## Repository

| Path | What |
|---|---|
| `cli/` | `pb` CLI and the endpoint runtime (`pb daemon`), Go |
| `server/` | control plane: identity, authorization, product state, Go + PostgreSQL |
| `relay/` | peer relay service (fallback when P2P fails), Go |
| `helper/` | `pbh` command: self-host installer/manager for relay and tunnel (moving here from `relay/` in S4-00), Go |
| `tunnel/` | public/browser edge for previews and tunnels, Go |
| `shared/` | code used by more than one module (wire protocols, common packages, helpers), Go |
| `console/` | web console (Next.js; read `console/AGENTS.md`, `DESIGN.md`, `NOTES.md`) |
| `marketing/` | marketing site (Astro) |
| `tools/device-smoke/` | device smoke suite (see below) |

Go modules are bound by the root `go.work`; build and test from the repo root or a
component directory. Go 1.27.2.

## How we work

1. **Feature by feature.** Take one feature from PLAN.md, make it work on every supported
   platform (Linux and macOS for launch; Windows after launch, PLAN.md 1.1) in every way a user can use it, including edge cases,
   failures and recovery, before moving to the next. A feature is done only when its PLAN
   rows in CHECKLIST.md are `[x]` on real devices.
2. **One commit per fix.** Message: what broke, root cause, the change, the test that proves
   it. Push `main` to `origin` (github.com/pinksaucepasta/paperboat, **public**: never commit anything private) after every
   commit. Never leave work uncommitted or unpushed at the end of a step. Never force-push or
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
   console lint/build, and, when anything under `cli/`, `relay/`, `tunnel/` or `server/`
   changed, the device smoke suite on a build of HEAD. **Every smoke check that has ever
   passed must still pass**, whether or not its feature is `[x]` yet.
3. **A previously green check that fails blocks the push.** Fix it in this lane, or revert
   the change that broke it. Never push with a known regression, never mark it `n/a`, never
   weaken the check.
4. Before changing shared code (transport, daemon, auth, server APIs used by several
   features), list the features that use it and include their checks in the run.

## Lanes (parallel sessions)

Several sessions may work at once only in separate lanes. A lane only edits the paths it
owns; a change needed in another lane's paths is written as a short request in the Log of
PLAN.md (section 5) and done by that lane. **Only Lane A uses the test devices.**

| Lane | Who | Owns | Works on (PLAN.md) |
|---|---|---|---|
| A — CLI & devices | Codex | `cli/`, `relay/`, `tools/device-smoke/`, the Tailscale fork (github.com/pinksaucepasta/tailscale) | S0 (except CI rows), F1–F12, F14, F15 device and CLI parts |
| B — Server & infrastructure | Codex | `server/`, `tunnel/`, `helper/`, `shared/`, `deploy/`, `.github/` | S0 CI rows, S1, S4, F13, server parts of every feature, S3 |
| C — Console & marketing | Claude only | `console/`, `marketing/` | all frontend work: console parts of every feature, S2 |

**Codex never edits `console/` or `marketing/`.** When a feature needs frontend work, Codex
finishes and pushes the Go/API side, then writes the frontend request (API, fields, states,
errors) in the PLAN.md Log; Claude does it. Go changes the frontend needs are done by Codex.

**Blocking rows first.** Before picking each next row, read the PLAN.md Log for lines where
another lane waits for one of your rows ("waits for S1", a request naming your lane). Those rows
come before everything else you own, including later rows, tidy-ups and other lanes' requests,
and you work them until they are `[x]` or truly `[!]`. A lane that waits writes exactly one such
line naming the rows it needs.

**Keep going until done.** A lane works its rows in PLAN.md order and does not stop until every
row it owns is `[x]` or `[!]`. Do not ask the user to confirm routine choices. `[!]` means blocked on something
specific (named in Evidence); rows that simply come later in PLAN.md order stay `[ ]`; one Log
line says which section the lane is waiting for. When a row is
blocked, log it in PLAN.md section 5, mark it `[!]` with the reason, continue with the next row,
and return when the blocker clears:
- needs another lane's code or the server: write the request in the Log;
- needs frontend work: finish the Go/API side, then log the request (endpoint, fields, states,
  errors, which page);
- needs credentials or a **USER DECIDES** item: log exactly what is needed;
- two failed attempts at the same thing: log what you know and what you tried.
A commit, push, fixed blocker or finished row is never a stopping point: continue with the next
row in the same turn. End the turn only when every owned row is `[x]` or `[!]`, or every
remaining row waits on an earlier section another lane is still doing; then report a
short table: rows done, rows `[!]` with reasons, commits. Root files no lane owns (`go.work`,
`Makefile`, `tools/gate.sh`) are edited by the lane whose change needs them, with `make gate`;
do not ask.

**Shared branch.** All lanes push to `main`. If a push is rejected because another lane pushed,
run `git pull --rebase`, re-run `make gate` if the incoming changes touch your paths or shared code,
then push. Never force-push.

Contracts between lanes (server API, discovery response) are changed by the lane that owns
the producer first, committed and pushed, then consumers update. A feature becomes `[x]`
only when all its rows across lanes are done; Lane A runs its device verification last.

## Code quality

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
- No stubs, placeholders or TODOs standing in for required behaviour.

## Testing

- **Unit tests** for every change, in the package that owns the behaviour. They must not
  be weakened to pass. Run the affected packages (`go vet`, `go test -race` for concurrency).
- **A check that passed before and fails now is found by bisection, not by reading logs alone.**
  First re-run only that check (`smoke.sh --targets <device>` with `--skip` for the rest) on the
  last build where it passed, to confirm it is a regression and not a flake. Then bisect the
  commits between that build and HEAD with the same single check (`git bisect`, one device
  build per step) until one commit is named. Diagnose and fix that commit's change. Never rerun
  the full suite to investigate one failing check.
- **Every real-device failure gets a workspace test** that reproduces it (fails before the
  fix, passes after), so it is caught without devices next time.
- **Device smoke suite** after every device build:
  `tools/device-smoke/smoke.sh --expect-version <version>` (needs `APPLE_PASS`, `VICTUS_PASS`
  from `REMOTE_TESTING.md`). Report its PASS/FAIL table and record the result file and the
  build's commit hash in CHECKLIST.md evidence. Extend the suite when a feature's checks are added; never
  weaken a check to pass.
- Device builds are made from a clean, committed tree, once for all platforms:
  ```sh
  V=dev-$(git rev-parse --short HEAD)
  L="-X github.com/pinksaucepasta/paperboat/internal/buildinfo.Version=$V -X github.com/pinksaucepasta/paperboat/internal/buildinfo.Commit=$(git rev-parse HEAD) -X github.com/pinksaucepasta/paperboat/internal/buildinfo.ProtocolVersion=1 -X github.com/pinksaucepasta/paperboat/internal/buildinfo.DefaultServerURL=https://api.pprbt.dev -X github.com/pinksaucepasta/paperboat/internal/buildinfo.DefaultReleaseURL=https://api.pprbt.dev"
  cd cli && for t in linux/amd64 darwin/arm64 windows/amd64; do CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags "$L" -o ../.build/$V/pb-${t%/*}-${t#*/}$([ ${t%/*} = windows ] && echo .exe) ./cmd/pb; done
  ```

## Machines and services

- Test machines: **hp** (Linux), **apple** (macOS), **victus** (Windows). Access and passwords:
  `REMOTE_TESTING.md`. Server and database run on the Hetzner VPS (`ssh root@coolify`); do
  not run local backends or databases.
- Install custom builds with the build's own `pb install` and `pb pair`, never the old
  official release. Enrollment tokens: console "Add machine", or `pb machine add --json`
  on a paired machine (use the Windows token for Windows).
- Leave every device either fully working or cleanly uninstalled at the end of a step.
  Remove temporary files, processes and test binaries you created, locally and remotely.
- The live console (dashboard) is served at https://oracle.newt-mermaid.ts.net, a Tailscale
  Funnel to `next dev` on http://100.95.70.63:3000 (systemd user unit
  `paperboat-console.service`, running from `~/paperboat/console`). WorkOS redirects to
  this origin, so sign in there; do not change WorkOS settings, the Funnel, or create other
  public tunnels. Console changes go live on save; restart the unit after dependency changes.

## Security and privacy

- Never print or commit credentials, tokens, keys, ENV values, terminal or file contents.
- Authenticate and authorize the exact resource before forwarding or acting on it.
- Native private access is end-to-end encrypted; browser HTTP terminates TLS at the edge.
  Never silently switch trust boundaries or call edge-decrypted traffic end-to-end encrypted.
- Installing a missing tool a task needs is fine (official sources, user-scoped where
  practical). Paid, destructive or security-policy changes need the user's approval.
