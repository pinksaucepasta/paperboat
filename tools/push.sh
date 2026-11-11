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
if [ -n "${PB_PUSH_PR:-}" ]; then
  # Published PR commits must remain ancestors: rebase would require a forbidden force-push.
  git merge -q --no-edit "origin/$base"
else
  git rebase -q "origin/$base"
fi
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
# A follow-up commit updates the requested PR without rewriting its published history.
if [ -n "${PB_PUSH_PR:-}" ]; then
  branch=$(gh pr view "$PB_PUSH_PR" --repo "$repo" --json headRefName --jq .headRefName)
  case "$branch" in
    lane-submit/"$base"/*) ;;
    *) echo 'push: the requested PR must use a lane-submit branch for this base' >&2; exit 1 ;;
  esac
fi
git push origin "HEAD:refs/heads/$branch"
url=$(gh pr list --repo "$repo" --head "$branch" --base "$base" --state all --json url --jq '.[0].url // empty')
if [ -z "$url" ]; then
  url=$(gh pr create --repo "$repo" --base "$base" --head "$branch" \
    --title "$(git log -1 --format=%s)" \
    --body "Lane batch ending at $head. Required CI checks gate the rebase merge; individual fix commits are preserved.")
fi
printf 'push: PR %s\n' "$url"
# With a merge queue the method is the queue's: gh prints a notice (and may exit non-zero), so judge by the result.
number=${url##*/}
gh pr merge "$url" --repo "$repo" --auto --match-head-commit "$head" || true
armed=$(gh api graphql -f query='query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){state isInMergeQueue autoMergeRequest{enabledAt}}}}' \
  -F o="${repo%%/*}" -F n="${repo#*/}" -F p="$number" --jq '.data.repository.pullRequest | .state == "MERGED" or .isInMergeQueue or .autoMergeRequest != null')
[ "$armed" = true ] || { echo "push: auto-merge is not enabled on $url; run: gh pr merge $number --auto" >&2; exit 1; }
# The queue rebuilds every entry on the current main, so the next lane need not wait for this merge: release the lock
# now (holding it until merged made lanes wait one after another, ~10 minutes each, 2026-11-12). Duplicate migration
# numbers between parallel PRs are caught by the policy job on the queue's merge commit.
exec 9>&-
# A timeout leaves the PR intact for inspection, and never reports a pending PR as success.
required=$(jq -c '[.required_status_checks.checks[].context]' "$(git rev-parse --show-toplevel)/tools/branch-protection.json")
end=$(( $(date +%s) + 3600 )) lost=0
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
  # Neither queued nor armed any more while still open: the queue ejected the PR (a failed merge-group check).
  if [ "$state" = OPEN ]; then
    armed=$(gh api graphql -f query='query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){isInMergeQueue autoMergeRequest{enabledAt}}}}' \
      -F o="${repo%%/*}" -F n="${repo#*/}" -F p="$number" --jq '.data.repository.pullRequest | .isInMergeQueue or .autoMergeRequest != null' || echo true)
    if [ "$armed" = true ]; then lost=0; else lost=$((lost + 1)); fi
    [ "$lost" -lt 3 ] || { echo "push: $url left the merge queue without merging (a queue check failed); fix it and resubmit" >&2; exit 1; }
  fi
  # Only the required checks decide: a failing optional job (Cross-platform, End-to-end) does not stop the merge.
  failed=$(gh pr view "$url" --repo "$repo" --json statusCheckRollup | jq --argjson required "$required" '
    [.statusCheckRollup[] | select(.name as $n | $required | index($n)) | (.conclusion // .state)] |
    any(. == "FAILURE" or . == "ERROR" or . == "CANCELLED" or . == "TIMED_OUT" or . == "ACTION_REQUIRED" or . == "STARTUP_FAILURE")')
  if [ "$failed" = true ]; then
    echo "push: CI failed for $url; fix the failing check before resubmitting" >&2
    exit 1
  fi
  sleep "${PB_PUSH_POLL:-60}"
done
echo "push: timed out waiting for $url; inspect its checks before retrying" >&2
exit 1
