#!/usr/bin/env bash
# R-01: branch protection for main from tools/branch-protection.json, including administrators.
#   tools/branch-protection.sh            print the rule and the rule GitHub has now (changes nothing)
#   tools/branch-protection.sh --apply    apply it (needs repository admin; pushes to main then need a PR)
#   tools/branch-protection.sh --check    exit 1 unless GitHub has exactly the required checks, admins included, and the rebase queue
set -euo pipefail
cd "$(dirname "$0")/.."
repo=${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}
endpoint="repos/$repo/branches/main/protection"
jq . tools/branch-protection.json
if [ "${1:-}" = --check ]; then
  want=$(jq -S '[.required_status_checks.checks[].context]' tools/branch-protection.json)
  have=$(gh api "$endpoint" --jq '[.required_status_checks.checks[].context]' | jq -S .)
  [ "$want" = "$have" ] || { echo "protection: required checks differ: $have" >&2; exit 1; }
  gh api "$endpoint" --jq '.enforce_admins.enabled and .required_linear_history.enabled and (.required_status_checks.strict|not) and (.allow_force_pushes.enabled|not)' | grep -qx true \
    || { echo "protection: admins, linear history, strict=false or force-push rule differs" >&2; exit 1; }
  gh api "repos/$repo/rulesets" --jq '[.[] | select(.name == "Paperboat main merge queue" and .enforcement == "active")] | length' | grep -qx 1 \
    || { echo "protection: merge queue ruleset is not active" >&2; exit 1; }
  echo "protection: ok"; exit 0
fi
if [ "${1:-}" != --apply ]; then
  echo "--- current:"; gh api "$endpoint" --jq '{checks: .required_status_checks.checks, admins: .enforce_admins.enabled}' 2>&1 || true
  exit 0
fi
gh api -X PUT "$endpoint" --input tools/branch-protection.json >/dev/null
gh api "$endpoint" --jq '{checks: [.required_status_checks.checks[].context], strict: .required_status_checks.strict, admins: .enforce_admins.enabled}'
# Public organization repository: the queue tests integration without repeated lane rebases.
queue=$(gh api "repos/$repo/rulesets" --jq '.[] | select(.name == "Paperboat main merge queue") | .id')
if [ -n "$queue" ]; then
  gh api -X PUT "repos/$repo/rulesets/$queue" --input tools/merge-queue.json >/dev/null
else
  gh api -X POST "repos/$repo/rulesets" --input tools/merge-queue.json >/dev/null
fi
