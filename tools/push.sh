#!/bin/sh
# Submit a lane batch through a PR; success means GitHub merged it.
# GitHub auto-merge enforces the required checks. Never push or force-push main.
set -eu
base=${PB_PUSH_BASE:-main}
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
# Fail closed during rollout too: auto-merge on an unprotected branch can merge immediately.
gh api "repos/$repo/branches/$base/protection" --jq '
  .enforce_admins.enabled and (.required_pull_request_reviews != null) and
  (.required_status_checks.checks | length > 0) and .required_linear_history.enabled' | grep -qx true || { echo "push: $base must require PRs and CI for admins first" >&2; exit 1; }
common=$(git rev-parse --path-format=absolute --git-common-dir)
exec 9>"$common/pb-push.lock"
echo 'push: waiting for the PR queue…' >&2
flock -w 3600 9
[ -z "$(git status --porcelain)" ] || { echo 'push: commit your changes first' >&2; exit 1; }
git fetch -q origin "$base"
git rebase -q "origin/$base"
if [ "$(git rev-list --count "origin/$base"..HEAD)" = 0 ]; then
  echo 'push: no commits to submit' >&2
  exit 0
fi
dups=$(git ls-tree --name-only HEAD server/internal/db/migrations/ | sed -n 's#.*/\([0-9][0-9]*\)_.*#\1#p' | sort | uniq -d)
if [ -n "$dups" ]; then
  echo "push: duplicate migration numbers: $dups; renumber before submitting" >&2
  exit 1
fi
head=$(git rev-parse HEAD)
branch="lane-submit/$base/$head"
git push origin "HEAD:refs/heads/$branch"
url=$(gh pr list --repo "$repo" --head "$branch" --base "$base" --state all --json url --jq '.[0].url // empty')
if [ -z "$url" ]; then
  url=$(gh pr create --repo "$repo" --base "$base" --head "$branch" \
    --title "$(git log -1 --format=%s)" \
    --body "Lane batch ending at $head. Required CI checks gate the rebase merge; individual fix commits are preserved.")
fi
printf 'push: PR %s\n' "$url"
gh pr merge "$url" --repo "$repo" --auto --rebase --delete-branch --match-head-commit "$head"
# Keep the lock until merged: the next lane rebases on the landed batch.
# A timeout leaves the PR intact for inspection, and never reports a pending PR as success.
end=$(( $(date +%s) + 3600 ))
while [ "$(date +%s)" -lt "$end" ]; do
  state=$(gh pr view "$url" --repo "$repo" --json state --jq .state)
  case "$state" in
    MERGED)
      git fetch -q origin "$base"
      git rebase -q "origin/$base"
      printf 'push: merged %s\n' "$url"
      exit 0 ;;
    CLOSED) echo "push: PR closed without merging: $url" >&2; exit 1 ;;
  esac
  failed=$(gh pr view "$url" --repo "$repo" --json statusCheckRollup --jq '
    [.statusCheckRollup[] | (.conclusion // .state)] |
    any(. == "FAILURE" or . == "ERROR" or . == "CANCELLED" or . == "TIMED_OUT" or . == "ACTION_REQUIRED" or . == "STARTUP_FAILURE")')
  if [ "$failed" = true ]; then
    echo "push: CI failed for $url; fix the failing check before resubmitting" >&2
    exit 1
  fi
  sleep 60
done
echo "push: timed out waiting for $url; inspect its checks before retrying" >&2
exit 1
