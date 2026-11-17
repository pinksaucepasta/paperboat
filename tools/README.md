# tools/

Scripts that are not part of any shipped binary. This is the caller map: who runs each file.
"manual" means a person or an agent runs it by hand; **unreferenced** means no workflow, Makefile,
gate, hook, doc or other script names it. Add a row when you add a file.

## Gates and checks

| File | What | Called by |
|---|---|---|
| `gate.sh` | the regression gate (`make gate`) | `Makefile`, AGENTS.md; stubbed by `.github/tests/test_gate.py` |
| `check-shared-imports.sh` | `shared/` imports nothing from cli or relay | `gate.sh`, `lane-test.yml` |
| `check_domains.py`, `test_check_domains.py` | no hardcoded domains | `gate.sh`, `server-ci.yml` |
| `check_doc_links.py`, `check_doc_envvars.py` and their `test_*` | docs link and env-var lint | `gate.sh`; links also `server-ci.yml` |
| `check_duplicate_packages.py`, `check_migrations.py` and their `test_*` | duplicate-package guard, migration lint | `gate.sh` |
| `check_server_deployed.py`, `test_check_server_deployed.py` | warns when a client push is ahead of the deployed server | `gate.sh`, `release.yml` |
| `pr-checks.py`, `test_pr_checks.py` | fast and heavy PR checks | `pr-checks.yml`; the test in `server-ci.yml` |
| `skip-audit.py`, `skip-allowlist.txt`, `skip-baseline.txt` | a test skipped in every CI step fails the run | `lane-test.yml`, `.github/tests/test_skip_audit.py` |
| `gotest-json.py` | `go test -json` to JUnit/JSONL for the sharded runs | `lane-test.yml`, `skip-audit.py` |
| `cli-root-tests.sh` | cli tests that need root | `lane-test.yml`, `server-ci.yml` |
| `hooks/pre-push`, `hooks/test_pre_push.py` | builds and vets the pushed modules on a clean checkout | `make hooks` installs it |

## Running tests and pushing

| File | What | Called by |
|---|---|---|
| `ci-test.sh` | heavy suites on GitHub Actions for the committed HEAD | manual (AGENTS.md); `.github/tests/test_ci_test.py` |
| `remote-test.sh` | one module's tests on a spare host | manual (AGENTS.md) |
| `push.sh`, `test_push.py` | the shared-branch push path | manual (AGENTS.md); test in `server-ci.yml` |
| `oracle-slot.sh` | fair queue for compiles on the shared build host | `hooks/pre-push` when present; manual |
| `perfdb.sh` | prints the isolated PostgreSQL DSN for the performance budgets | manual; named by `gate.sh` |

## Repository, release and live deploys

| File | What | Called by |
|---|---|---|
| `branch-protection.sh`, `branch-protection.json`, `merge-queue.json` | the required-checks and merge-queue rule | manual (repo admin); `deploy/README.md`, `e2e.yml` cite the json |
| `verify-main-protection.py` | checks the rule on GitHub | **unreferenced** |
| `sentry-release.py` | creates a Sentry release and deploy | manual (`cli/docs/release-deployment.md`) |
| `test-sentry-release.py` | its tests | **unreferenced**: `python3 tools/test-sentry-release.py` |
| `console-live.sh` | builds and restarts the live console | manual (AGENTS.md) |
| `console-prod.sh` | production console deploy | manual; `deploy/Caddyfile`, `deploy/paperboat-console.service` mention it |

## Device qualification (manual, on test machines)

| File | What | Called by |
|---|---|---|
| `fresh-host.sh` | sourced: a throwaway systemd container | the `qualify-*.sh` scripts, `helper/deploy/qualify-fresh-host.sh` |
| `qualify-full-disk.sh`, `qualify-logout-survival.sh`, `qualify-pb-installer.sh`, `qualify-revoked-machine.sh`, `qualify-ssh-trust-host.sh`, `qualify-uninstall-inventory.sh` | one scenario each, repeatable on a fresh host | **unreferenced** by code; run by hand |
| `progress.py`, `test_progress.py` | renders `CHECKLIST.md` as a progress page | **unreferenced**; goes away with `CHECKLIST.md` |

## Directories

| Directory | What | Called by |
|---|---|---|
| `device-smoke/` | the device suite and its unit tests | `make smoke`, `cross-platform.yml`, `lane-test.yml`, `final-recheck.yml`; see its README |
| `quick-update/` | parallel feature runs after a build | `quick-update.yml`, `gate.sh` |
| `probe/` | production canary and console probes | `server-ci.yml`, `deploy/*canary*` units, `deploy/staging.py` |
| `e2e/` | core end-to-end (`TestE2ECore`) | `e2e.yml` |
| `loadtest/` | scale and failover runs | manual; cited by `server/internal/app/load_postgres_test.go` and `device-smoke/perf_budgets.py` |
| `compat/` | decodes upgrade artifacts with an older release | manual; named by a comment in `server/internal/peersessions/compat_emit_test.go`; its tests are **unreferenced** |

## Device smoke results

`device-smoke/results/` holds the receipts of device runs. They are local files, ignored by git,
and name private test machines, so they never ship. Readers:

- `gate.sh` takes them as the regression baseline for pushes that touch `cli/`, `relay/` or
  `shared/`. Without that directory the step is skipped and says so. To share receipts between
  worktrees, symlink `tools/device-smoke/results` to one directory.
- `device-smoke/green_list.py` (checks that ever passed), `last_green.py` and `release_run.py`
  read the same directory (`last_green.py` also takes `--results-dir`).
- `smoke.sh` and the other device scripts write there; `final-recheck.yml` uploads its own
  `final-recheck-*` files as CI artifacts.
