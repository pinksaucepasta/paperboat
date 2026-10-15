#!/usr/bin/env bash
# Regression gate. Run before every push (AGENTS.md "Regression gate").
#   1. go vet + go test for every Go module, console lint + build.
#   2. If runtime code (cli/ relay/ tunnel/ server/ helper/) changed relative to origin/main,
#      require a device smoke result for a build of HEAD (tools/device-smoke/results/*.json
#      whose expect_version is dev-<short HEAD>) in which every check that passed in any
#      earlier result still passes. Feature status in CHECKLIST.md does not matter.
# Exit status is non-zero if anything fails.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
fail=0
step() { printf '\n== %s\n' "$1"; }

for m in cli relay tunnel server helper; do
  [ -d "$m" ] || continue
  step "go vet + test: $m"
  (cd "$m" && go vet ./... && go test ./...) || { echo "FAIL: $m"; fail=1; }
done

step "console lint + build"
(cd console && bun install --frozen-lockfile >/dev/null && bun run lint && bun run build) || { echo "FAIL: console"; fail=1; }

base=$(git merge-base HEAD origin/main 2>/dev/null || git rev-list --max-parents=0 HEAD | tail -1)
changed=$( { git diff --name-only "$base" HEAD; git diff --name-only; } | grep -E '^(cli|relay|tunnel|server|helper)/' | head -1)
if [ -n "$changed" ]; then
  step "device smoke for HEAD: no check that ever passed may fail"
  want="dev-$(git rev-parse --short HEAD)"
  python3 - "$want" <<'PY' || fail=1
import glob, json, sys
want = sys.argv[1]
passed, head = set(), None
for f in sorted(glob.glob('tools/device-smoke/results/*.json')):
    try:
        d = json.load(open(f))
    except Exception:
        continue
    if d.get('expect_version') == want:
        head = (f, d.get('results') or [])
    # Windows (victus) is after launch (PLAN.md 1.1), so its checks do not gate.
    passed |= {r['id'] for r in d.get('results') or [] if r['status'] == 'PASS' and not r['id'].endswith(':victus')}
if head is None:
    print(f"FAIL: runtime code changed but no smoke result for build {want}.")
    print(f"      Build HEAD for all devices, install it, run tools/device-smoke/smoke.sh --expect-version {want}")
    sys.exit(1)
f, results = head
status = {r['id']: r['status'] for r in results}
regressed = sorted(i for i in passed if status.get(i) != 'PASS')
if regressed:
    print(f"FAIL: regression in {f}: previously passing checks not passing now: {', '.join(regressed)}")
    sys.exit(1)
print(f"OK: {f} ({len(passed)} previously passing checks still pass)")
PY
else
  step "no runtime code changed; device smoke not required"
fi

[ "$fail" = 0 ] && echo -e "\nGATE PASS" || echo -e "\nGATE FAIL"
exit $fail
