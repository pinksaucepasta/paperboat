#!/usr/bin/env bash
# dispatch.sh [--no-web] <ref>...   Run the quick update run for each ref on its own GitHub runner, in parallel.
#   tools/quick-update/dispatch.sh origin/main fix-a fix-b
# Pushes each ref's commit to a throwaway branch ci/quick-<sha8>-<time> (so the runner can fetch it), dispatches
# .github/workflows/quick-update.yml from main, prints each job's per-feature table, then deletes the branches.
# Needs gh. The harness always comes from main: merge harness changes first.
set -euo pipefail
web=true; refs=()
for a in "$@"; do case $a in --no-web) web=false ;; -h|--help) sed -n 2,7p "$0" | sed 's/^# \{0,1\}//'; exit 0 ;; *) refs+=("$a") ;; esac; done
[ ${#refs[@]} -gt 0 ] || { echo "dispatch: give at least one ref" >&2; exit 2; }
command -v gh >/dev/null || { echo "dispatch: gh is required" >&2; exit 2; }
stamp=$(date +%H%M%S); branches=(); shas=()
trap 'for b in "${branches[@]}"; do git push -q --no-verify origin --delete "$b" >/dev/null 2>&1 || true; done' EXIT
for r in "${refs[@]}"; do
  sha=$(git rev-parse --verify -q "$r^{commit}") || { echo "dispatch: unknown ref $r" >&2; exit 2; }
  b="ci/quick-${sha:0:8}-$stamp-${#branches[@]}"
  git push -q --no-verify origin "$sha:refs/heads/$b" || { echo "dispatch: could not push $b" >&2; exit 1; }
  branches+=("$b"); shas+=("$sha")
done
gh workflow run quick-update.yml --ref main -f "refs=${shas[*]}" -f "web=$web"
id=""
for _ in $(seq 30); do
  id=$(gh run list --workflow quick-update.yml --branch main --event workflow_dispatch -L1 --json databaseId,createdAt \
    --jq "map(select(.createdAt > \"$(date -u -d '-2 minutes' +%Y-%m-%dT%H:%M:%SZ)\")) | .[0].databaseId // empty")
  [ -n "$id" ] && break; sleep 3
done
[ -n "$id" ] || { echo "dispatch: the run did not start" >&2; exit 1; }
echo "dispatch: $(gh run view "$id" --json url --jq .url)" >&2
deadline=$((SECONDS + 5400))
until [ "$(gh run view "$id" --json status --jq .status)" = completed ]; do
  [ $SECONDS -lt $deadline ] || { echo "dispatch: gave up waiting" >&2; exit 1; }; sleep 30
done
out=$(mktemp -d); gh run download "$id" -D "$out" 2>/dev/null || true
for i in "${!shas[@]}"; do
  echo; echo "== ${refs[$i]} (${shas[$i]:0:8}) =="
  cat "$out/quick-update-$i/table.txt" 2>/dev/null || echo "no table (job failed before the run: $(gh run view "$id" --json url --jq .url))"
done
rm -rf "$out"
[ "$(gh run view "$id" --json conclusion --jq .conclusion)" = success ]
