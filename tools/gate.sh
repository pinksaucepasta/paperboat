#!/usr/bin/env bash
# Regression gate. Run before every push (AGENTS.md "Regression gate").
#   1. go vet + go test for every Go module, console lint + build.
#   2. If runtime code (cli/ relay/ tunnel/ server/ pbh/) changed relative to origin/main,
#      require a device smoke result for a build of HEAD with no failures:
#      tools/device-smoke/results/*.json whose expect_version is dev-<short HEAD>.
# Exit status is non-zero if anything fails.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
fail=0
step() { printf '\n== %s\n' "$1"; }

for m in cli relay tunnel server pbh; do
  [ -d "$m" ] || continue
  step "go vet + test: $m"
  (cd "$m" && go vet ./... && go test ./...) || { echo "FAIL: $m"; fail=1; }
done

step "console lint + build"
(cd console && bun install --frozen-lockfile >/dev/null && bun run lint && bun run build) || { echo "FAIL: console"; fail=1; }

base=$(git merge-base HEAD origin/main 2>/dev/null || git rev-list --max-parents=0 HEAD | tail -1)
changed=$( { git diff --name-only "$base" HEAD; git diff --name-only; } | grep -E '^(cli|relay|tunnel|server|pbh)/' | head -1)
if [ -n "$changed" ]; then
  step "device smoke evidence for HEAD"
  want="dev-$(git rev-parse --short HEAD)"
  ok=$(python3 - "$want" <<'PY'
import glob, json, sys
want = sys.argv[1]
for f in sorted(glob.glob('tools/device-smoke/results/*.json')):
    try:
        d = json.load(open(f))
    except Exception:
        continue
    if d.get('expect_version') == want and d.get('results') and all(r['status'] in ('PASS', 'SKIP') for r in d['results']):
        print(f)
PY
)
  if [ -n "$ok" ]; then
    echo "OK: $(echo "$ok" | tail -1)"
  else
    echo "FAIL: runtime code changed but no all-PASS smoke result for build $want."
    echo "      Build HEAD for all devices, install it, run tools/device-smoke/smoke.sh --expect-version $want"
    fail=1
  fi
else
  step "no runtime code changed; device smoke not required"
fi

[ "$fail" = 0 ] && echo -e "\nGATE PASS" || echo -e "\nGATE FAIL"
exit $fail
