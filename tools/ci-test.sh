#!/usr/bin/env bash
# Run the heavy test suites for the committed HEAD on GitHub Actions instead of this machine.
#   tools/ci-test.sh [module...] [-run PATTERN] [-count N]
#   tools/ci-test.sh cli                       # vet + go test -race for cli
#   tools/ci-test.sh server -run 'TestTeams'   # server incl. its PostgreSQL suites, narrowed
#   tools/ci-test.sh cli -run TestX -count 5   # repeat each matching test 5 times (flake check)
#   tools/ci-test.sh smoke                     # the device-smoke harness's Python unit tests
#   tools/ci-test.sh                           # every module and the console
# Modules: shared cli relay tunnel helper server console sdk/go smoke. server and tunnel also run their PostgreSQL
# suites against a throwaway postgres:17 service container. Only committed work is tested.
# The script pushes HEAD to a throwaway branch ci/<worktree>-<sha>-<time> (never main, with no
# build here and "[skip ci]" so only the dispatched run starts), waits, prints a pass/fail line and
# duration per job plus the failing tests, and deletes the branch. Exit status is 0 only if all pass.
set -euo pipefail

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

modules=() run="" count=1
while [ $# -gt 0 ]; do
  case $1 in
    -run | --run) [ $# -ge 2 ] || usage; run=$2; shift 2 ;;
    -count | --count) [ $# -ge 2 ] || usage; count=$2; shift 2 ;;
    -h | --help) usage ;;
    shared | cli | relay | tunnel | helper | server | console | sdk/go | smoke) modules+=("$1"); shift ;;
    *) echo "ci-test: unknown argument '$1'" >&2; usage ;;
  esac
done

root=$(git rev-parse --show-toplevel)
cd "$root"
command -v gh >/dev/null || { echo "ci-test: gh is required" >&2; exit 2; }
git cat-file -e HEAD:.github/workflows/lane-test.yml 2>/dev/null ||
  { echo "ci-test: HEAD has no .github/workflows/lane-test.yml; rebase onto origin/main" >&2; exit 2; }
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "ci-test: note: uncommitted changes are NOT tested (only HEAD $(git rev-parse --short HEAD))" >&2
fi

lane=$(printf %s "${root##*/}" | tr -c 'A-Za-z0-9._-' _)
sha=$(git rev-parse --short HEAD)
branch="ci/$lane-$sha-$(date +%H%M%S)-$$"

# A ci branch is deleted only once no run on it is queued or in progress: deleting it under a queued run
# wasted that run (2026-11-05, GitHub took longer than this script waited to list a dispatched run).
# Unknown (the API call failed) keeps the branch too. Only the branch this invocation created is ever
# deleted: other ci/<lane>-* branches belong to concurrent runs (cross-platform dispatches) that still use them.
active_runs() {
  gh run list --workflow lane-test.yml --branch "$1" --json status --jq '[.[] | select(.status != "completed")] | length' 2>/dev/null ||
    echo unknown
}
remove_branch() {
  local active
  active=$(active_runs "$1")
  if [ "$active" = 0 ]; then
    git push -q --no-verify origin --delete "$1" >/dev/null 2>&1 || true
  else
    echo "ci-test: kept $1: its run is not finished ($active active); delete it by hand once the run ends" >&2
  fi
}
trap 'remove_branch "$branch"' EXIT

# Same tree as HEAD in a child commit whose message skips the push-triggered run: the dispatch below
# is the only run, and it carries the module/-run inputs.
commit=$(git commit-tree "HEAD^{tree}" -p HEAD -m "ci-test $lane $sha [skip ci]")
# Retry once (GitHub sometimes refuses a push for a moment) and show git's own error if it still fails.
push_err=$(git push -q --no-verify origin "$commit:refs/heads/$branch" 2>&1) ||
  { sleep 5; push_err=$(git push -q --no-verify origin "$commit:refs/heads/$branch" 2>&1); } ||
  { echo "ci-test: could not push the ci branch $branch: $push_err" >&2; exit 1; }

args=(--ref "$branch" -f "modules=${modules[*]:-}" -f "run=$run" -f "count=$count")
gh workflow run lane-test.yml "${args[@]}"

# GitHub can take minutes to list a dispatched run when its queue is long: look for 15 min, one API call
# every 30 s (the 5000/h budget is shared by every lane), and survive a failed call.
id=""
for attempt in $(seq 30); do
  sleep 30
  id=$(gh run list --workflow lane-test.yml --branch "$branch" --event workflow_dispatch -L1 \
    --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null) || id=""
  [ -n "$id" ] && break
done
[ -n "$id" ] || { echo "ci-test: GitHub listed no run for $branch within 15 min" >&2; exit 1; }
url=$(gh run view "$id" --json url --jq .url)
echo "ci-test: ${modules[*]:-all modules}${run:+ -run $run} at $sha: $url" >&2

# Poll (no screen refresh), printing a line whenever a job finishes.
seen=""
deadline=$((SECONDS + 5400))
while :; do
  # A failed poll (GitHub API rate limit, network) must not exit: the EXIT trap would delete the branch and
  # cancel the run being watched. Retry within the deadline instead.
  state=$(gh run view "$id" --json status,jobs) || {
    [ $SECONDS -lt $deadline ] || { echo "ci-test: gave up waiting after 90 min: $url" >&2; exit 1; }
    echo "ci-test: polling the run failed; retrying in 180 s" >&2; sleep 180; continue; }
  for line in $(jq -r '.jobs[] | select(.status == "completed") | "\(.name | gsub(" "; "_"))=\(.conclusion)"' <<<"$state"); do
    case $seen in *"|$line|"*) ;; *) seen="$seen|$line|"; echo "ci-test: ${line%%=*}: ${line#*=}" | tr _ ' ' >&2 ;; esac
  done
  [ "$(jq -r .status <<<"$state")" = completed ] && break
  [ $SECONDS -lt $deadline ] || { echo "ci-test: gave up waiting after 90 min: $url" >&2; exit 1; }
  sleep 120  # one GitHub API call per run per 2 min: ~20 lanes poll at once and 5000/h is shared (hit 11-05)
done

for attempt in $(seq 30); do
  final=$(gh run view "$id" --json conclusion,jobs) && break
  [ "$attempt" -lt 30 ] || { echo "ci-test: could not read the result: $url" >&2; exit 1; }
  echo "ci-test: reading the result failed; retrying in 60 s" >&2; sleep 60
done
echo
jq -r '.jobs[] | select(.name != "Plan" and .conclusion != "skipped") | [.name, (.conclusion // .status),
  (if .completedAt and .startedAt then ((.completedAt | fromdate) - (.startedAt | fromdate)) else 0 end | "\(. / 60 | floor)m\(. % 60)s")] | @tsv' <<<"$final" |
  column -t -s "$(printf '\t')"
conclusion=$(jq -r .conclusion <<<"$final")
if [ "$conclusion" != success ]; then
  echo
  echo "failing tests and packages:"
  gh run view "$id" --log-failed 2>/dev/null |
    grep -E -- '--- FAIL|^[^	]*	[^	]*	[^ ]+ FAIL|panic:|DATA RACE|\[build failed\]|##\[error\]' |
    sed -E 's/^([^	]*)	[^	]*	[^ ]+ /\1: /' | head -60 || true
fi
echo
echo "ci-test: $conclusion ($url)"
[ "$conclusion" = success ]
