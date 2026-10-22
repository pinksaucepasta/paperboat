#!/bin/sh
# Push the current branch's commits to origin/main without losing the race to other lanes.
#
# Every lane pushes to main. Pushing separately, a push whose pre-push build takes minutes is rejected
# whenever another lane lands first, and the retry loses again. This takes one lock shared by every
# worktree of the repository, then fetches, rebases and pushes inside it, so pushes queue instead of
# racing. Usage: tools/push.sh   (from any lane worktree, with your commits on HEAD).
set -eu
common=$(git rev-parse --path-format=absolute --git-common-dir)
exec 9>"$common/pb-push.lock"
echo "push: waiting for the push queue…" >&2
flock 9
echo "push: rebasing onto origin/main" >&2
git fetch -q origin main
git rebase -q origin/main
git push origin HEAD:main
