#!/usr/bin/env bash
# Run the heavy test suites for the committed HEAD on GitHub Actions instead of this machine.
#   tools/ci-test.sh [module...] [-run PATTERN]
#   tools/ci-test.sh cli                       # vet + go test -race for cli
#   tools/ci-test.sh server -run 'TestTeams'   # server incl. its PostgreSQL suites, narrowed
#   tools/ci-test.sh                           # every module and the console
# Modules: shared cli relay tunnel helper server console. server and tunnel also run their PostgreSQL
# suites against a throwaway postgres:17 service container. Only committed work is tested.
# The script pushes HEAD to a throwaway branch ci/<worktree>-<sha>-<time> (never main, with no
# build here and "[skip ci]" so only the dispatched run starts), waits, prints a pass/fail line and
# duration per job plus the failing tests, and deletes the branch. Exit status is 0 only if all pass.
set -euo pipefail

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

modules=() run=""
while [ $# -gt 0 ]; do
  case $1 in
    -run | --run) [ $# -ge 2 ] || usage; run=$2; shift 2 ;;
    -h | --help) usage ;;
    shared | cli | relay | tunnel | helper | server | console) modules+=("$1"); shift ;;
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
branch="ci/$lane-$sha-$(date +%H%M%S)"
trap 'git push -q --no-verify origin --delete "$branch" >/dev/null 2>&1 || true' EXIT

# Same tree as HEAD in a child commit whose message skips the push-triggered run: the dispatch below
# is the only run, and it carries the module/-run inputs.
commit=$(git commit-tree "HEAD^{tree}" -p HEAD -m "ci-test $lane $sha [skip ci]")
git push -q --no-verify origin "$commit:refs/heads/$branch"

args=(--ref "$branch" -f "modules=${modules[*]:-}" -f "run=$run")
gh workflow run lane-test.yml "${args[@]}"

id=""
for _ in $(seq 30); do
  id=$(gh run list --workflow lane-test.yml --branch "$branch" --event workflow_dispatch -L1 \
    --json databaseId --jq '.[0].databaseId // empty')
  [ -n "$id" ] && break
  sleep 3
done
[ -n "$id" ] || { echo "ci-test: the run did not start" >&2; exit 1; }
url=$(gh run view "$id" --json url --jq .url)
echo "ci-test: ${modules[*]:-all modules}${run:+ -run $run} at $sha: $url" >&2

# Poll (no screen refresh), printing a line whenever a job finishes.
seen=""
deadline=$((SECONDS + 5400))
while :; do
  state=$(gh run view "$id" --json status,jobs)
  for line in $(jq -r '.jobs[] | select(.status == "completed") | "\(.name | gsub(" "; "_"))=\(.conclusion)"' <<<"$state"); do
    case $seen in *"|$line|"*) ;; *) seen="$seen|$line|"; echo "ci-test: ${line%%=*}: ${line#*=}" | tr _ ' ' >&2 ;; esac
  done
  [ "$(jq -r .status <<<"$state")" = completed ] && break
  [ $SECONDS -lt $deadline ] || { echo "ci-test: gave up waiting after 90 min: $url" >&2; exit 1; }
  sleep 20
done

final=$(gh run view "$id" --json conclusion,jobs)
echo
jq -r '.jobs[] | select(.name != "Plan") | [.name, (.conclusion // .status),
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
