#!/usr/bin/env bash
# Regression gate. Run before every push (AGENTS.md "Regression gate").
#   1. go vet + go test for every Go module, console lint + build.
#   2. If device-build code (cli/ relay/ shared/) changed relative to origin/main,
#      require a device smoke result for a build of HEAD (tools/device-smoke/results/*.json
#      whose expect_version is dev-<short HEAD>) in which every check that passed in any
#      earlier result still passes. Feature status in CHECKLIST.md does not matter.
#      server/ tunnel/ helper/ changes push on unit and PostgreSQL tests; their deploy is what
#      the next device smoke checks (AGENTS.md "Regression gate").
# Exit status is non-zero if anything fails.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
fail=0
step() { printf '\n== %s\n' "$1"; }

for m in shared cli relay tunnel server helper; do
  [ -d "$m" ] || continue
  step "go vet + test: $m"
  (cd "$m" && go vet ./... && go test ./...) || { echo "FAIL: $m"; fail=1; }
done

step "server performance budgets on the seeded PostgreSQL (internal/perf/budgets.json)"
if [ -n "${PAPERBOAT_PERF_DATABASE_DSN:-}" ]; then
  (cd server && go test -count=1 -timeout 30m -run TestPerformanceBudgetsOnSeededPostgres ./internal/app) || { echo "FAIL: performance budgets"; fail=1; }
else
  echo "skipped: set PAPERBOAT_PERF_DATABASE_DSN to an isolated *_test database (tools/perfdb.sh prints it on oracle)"
fi

step "no hardcoded domains (AGENTS.md)"
python3 tools/check_domains.py && (cd tools && python3 -m unittest test_check_domains) || { echo "FAIL: hardcoded domains"; fail=1; }

step "documentation links"
python3 tools/check_doc_links.py && python3 tools/check_doc_envvars.py && (cd tools && python3 -m unittest test_check_doc_links test_check_doc_envvars) || { echo "FAIL: documentation links or environment variables"; fail=1; }

step "migrations: expand/contract and index locks (F21-04)"
python3 tools/check_migrations.py && (cd tools && python3 -m unittest test_check_migrations) || { echo "FAIL: migration lint"; fail=1; }

step "signed artifacts decode with the oldest supported official pb (tools/compat)"
sh tools/compat/run.sh || { echo "FAIL: compat with official $(cat tools/compat/OLDEST_OFFICIAL)"; fail=1; }

step "server-first check tests"
(cd tools && python3 -m unittest test_check_server_deployed) || { echo "FAIL: server-first check"; fail=1; }

step "managed deployment lifecycle tests"
python3 -m unittest discover -s deploy -p 'test_*.py' || { echo "FAIL: deployment"; fail=1; }

step "console lint + build"
(cd console && bun install --frozen-lockfile >/dev/null && bun run lint && bun run build) || { echo "FAIL: console"; fail=1; }

base=$(git merge-base HEAD origin/main 2>/dev/null || git rev-list --max-parents=0 HEAD | tail -1)
changed=$( { git diff --name-only "$base" HEAD; git diff --name-only; } | grep -E '^(cli|relay|shared)/' | head -1)
# Server first, then client (warning only): a client push must not get ahead of the deployed server.
if git diff --name-only "$base" HEAD | grep -q '^cli/'; then
  step "server first: is every server/ and shared/ commit of this push deployed? (warning)"
  if [ -n "${PAPERBOAT_SERVER_URL:-}" ]; then
    python3 tools/check_server_deployed.py --warn --server-url "$PAPERBOAT_SERVER_URL"
  else
    echo "skipped: set PAPERBOAT_SERVER_URL to the server's https origin to compare with the deployed commit"
  fi
fi

if [ -n "$changed" ]; then
  step "device smoke for HEAD: no check that ever passed may fail"
  want="dev-$(git rev-parse --short HEAD)"
  python3 - "$want" <<'PY' || fail=1
import glob, json, subprocess, sys
want = sys.argv[1]
passed, current, receipts, fresh = set(), {}, [], False
runtime_paths = ['cli/', 'relay/', 'shared/']
def runtime_tree(revision):
    return subprocess.check_output(['git', 'ls-tree', revision, '--', *runtime_paths], text=True)

# Rebasing changes commit IDs, but not the source that was built and accepted.
# Only exact runtime trees already published in our common history are baselines.
common = subprocess.run(['git', 'merge-base', 'origin/main', 'HEAD'], capture_output=True, text=True)
published_trees = set()
if common.returncode == 0:
    revisions = subprocess.check_output(['git', 'log', '--format=%H', common.stdout.strip(), '--',
                                        *runtime_paths], text=True).splitlines()
    published_trees = {runtime_tree(revision) for revision in revisions}
head_tree = runtime_tree('HEAD')
def receipt_source(version):
    if not version.startswith('dev-'):
        return False, False
    revision = version[4:]
    resolved = subprocess.run(['git', 'rev-parse', '--verify', revision + '^{commit}'], capture_output=True, text=True)
    if resolved.returncode != 0:
        return False, False
    revision = resolved.stdout.strip()
    tree = runtime_tree(revision)
    # Fresh proof is still required for this candidate's exact runtime tree.
    return tree == head_tree, tree in published_trees

for f in sorted(glob.glob('tools/device-smoke/results/*.json')):
    try:
        d = json.load(open(f))
    except Exception:
        continue
    same, published = receipt_source(d.get('expect_version', ''))
    if same or published:
        fresh = fresh or same
        receipts.append(f)
        current.update({r['id']: r['status'] for r in d.get('results') or []})
    # Windows (victus) is after launch (PLAN.md 1.1), so its checks do not gate.
    passed |= {r['id'] for r in d.get('results') or [] if r['status'] == 'PASS' and not r['id'].endswith(':victus')}
if not fresh:
    print(f"FAIL: runtime code changed but no smoke result for build {want}.")
    print(f"      Build HEAD for all devices, install it, run tools/device-smoke/smoke.sh --expect-version {want}")
    sys.exit(1)
regressed = sorted(i for i in passed if current.get(i) != 'PASS')
if regressed:
    print(f"FAIL: regression in {receipts[-1]}: previously passing checks not passing now: {', '.join(regressed)}")
    sys.exit(1)
print(f"OK: {len(receipts)} accepted-baseline/targeted receipts ({len(passed)} previously passing checks still pass)")
PY
else
  step "no runtime code changed; device smoke not required"
fi

[ "$fail" = 0 ] && echo -e "\nGATE PASS" || echo -e "\nGATE FAIL"
exit $fail
