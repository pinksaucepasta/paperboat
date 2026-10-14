# Working on Paperboat

Paperboat gives developers and agents access to their remote machines: a terminal with
replay and reconnect, remote exec, managed SSH/SCP/SFTP/rsync, file transfers, previews
and tunnels, ENV (end-to-end encrypted environment variables), teams, config sync and a
web console. Privacy matters: native access is end-to-end encrypted and peer-to-peer
where possible. UX comes first: setup, daily use, failure and recovery must be obvious.

Launch target: **2026-10-20**. The plan and its status live in [PLAN.md](PLAN.md); it is
the only planning/status document. Post-launch ideas live in [WHITEBOARD.md](WHITEBOARD.md).
Machine access and credentials are in `REMOTE_TESTING.md` (local only, never committed).

## Repository

| Path | What |
|---|---|
| `cli/` | `pb` CLI and the endpoint runtime (`pb daemon`), Go |
| `server/` | control plane: identity, authorization, product state, Go + PostgreSQL |
| `relay/` | peer relay service (fallback when P2P fails), Go |
| `tunnel/` | public/browser edge for previews and tunnels, Go |
| `console/` | web console (Next.js; read `console/AGENTS.md`, `DESIGN.md`, `NOTES.md`) |
| `marketing/` | marketing site (Astro) |
| `tools/device-smoke/` | device smoke suite (see below) |

Go modules are bound by the root `go.work`; build and test from the repo root or a
component directory. Go 1.27.1.

## How we work

1. **Feature by feature.** Take one feature from PLAN.md, make it work on every supported
   platform (Linux, macOS, Windows) in every way a user can use it, including edge cases,
   failures and recovery, before moving to the next. A feature is done only when its PLAN
   checklist is green on real devices.
2. **One commit per fix.** Message: what broke, root cause, the change, the test that proves
   it. Push `main` to `origin` (github.com/pinksaucepasta/paperboat, private) after every
   commit. Never leave work uncommitted or unpushed at the end of a step. Never force-push or
   rewrite history.
3. **Regressions first.** Anything that worked on the last accepted build and fails now is
   fixed before any other work. Never call a regression a "remaining issue".
4. **No guessing.** Read the code path in execution order before changing it. Reproduce,
   find the cause, then fix. Never retry an unchanged command hoping for a different result.
   After two failed attempts at the same thing, stop and write down what you know.
5. **One session on the devices at a time.** Only the session that owns PLAN.md's current
   step installs builds on or tests against the test machines.
6. **Report results, not prose.** Lead with what works and what doesn't, with evidence
   (smoke table, test names, commit hashes). Plain language, short.
7. **Keep PLAN.md current** after each step (status + commit). Do not create other plan,
   tracker, handoff or report files.

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
- **Every real-device failure gets a workspace test** that reproduces it (fails before the
  fix, passes after), so it is caught without devices next time.
- **Device smoke suite** after every device build:
  `tools/device-smoke/smoke.sh --expect-version <version>` (needs `APPLE_PASS`, `VICTUS_PASS`
  from `REMOTE_TESTING.md`). Report its PASS/FAIL table and record the result file and the
  build's commit hash in PLAN.md. Extend the suite when a feature's checks are added; never
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
- Console development is served at https://oracle.newt-mermaid.ts.net (Tailscale Funnel).
  Use that origin for sign-in; do not change WorkOS settings or create other public tunnels.

## Security and privacy

- Never print or commit credentials, tokens, keys, ENV values, terminal or file contents.
- Authenticate and authorize the exact resource before forwarding or acting on it.
- Native private access is end-to-end encrypted; browser HTTP terminates TLS at the edge.
  Never silently switch trust boundaries or call edge-decrypted traffic end-to-end encrypted.
- Installing a missing tool a task needs is fine (official sources, user-scoped where
  practical). Paid, destructive or security-policy changes need the user's approval.
