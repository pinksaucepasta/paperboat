#!/usr/bin/env bash
# Run one module's Go tests for the committed HEAD on a winpool machine, in the same kind of session CI uses.
#   tools/win-remote-test.sh <winpool-N> <cli|shared> [package...]       env: RUN (-run regexp) COUNT RACE (-race) NATIVE=1 (OpenSSH native qualification, see win-test.sh)
# A raw SSH logon is a network session in session 0: the suite shows over a hundred failures there that are not product
# bugs (no console, pipes, child processes; git cannot create folders). So this starts tools/win-test.sh as a scheduled
# task of the machine's own user, interactive and highest: it runs in the logged-on console session, as the GitHub
# runner's steps do (cross-platform.yml). Only committed files are tested. Each host runs one job at a time,
# and a host another lane holds on the runner board is refused (tools/runner-board.sh).
# Prints the log; the go test -json events and the log are copied to ${WIN_TEST_OUT:-$TMPDIR/pb-win-test-<host>-<sha>}.
set -euo pipefail

[ $# -ge 2 ] || { sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
host=$1 module=$2
shift 2
case $module in cli | shared) ;; *) echo "win-remote-test: module must be cli or shared (the others have no Windows code)" >&2; exit 2 ;; esac
case $host in winpool-*) ;; *) echo "win-remote-test: host must be a winpool machine (tools/device-smoke/winpool.sh start)" >&2; exit 2 ;; esac

root=$(git rev-parse --show-toplevel)
cd "$root"
sha=$(git rev-parse --short HEAD)
worktree=$(printf %s "${root##*/}" | tr -c 'A-Za-z0-9._-' _)
[ -z "$(git status --porcelain --untracked-files=no)" ] || echo "win-remote-test: note: uncommitted changes are NOT tested (only HEAD $sha)" >&2
exec 8>"${TMPDIR:-/tmp}/pb-win-remote-test-$host.lock"
flock 8
. tools/runner-board.sh
board_guard "win-remote-test: $worktree $module $sha" "$host"
trap board_release EXIT

dir="pb-remote-test\\$worktree" task="pbtest-$worktree"
out=${WIN_TEST_OUT:-${TMPDIR:-/tmp}/pb-win-test-$host-$sha}
ssh_() { ssh -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=30 "$host" "$@"; }
cleanup() { ssh_ "schtasks /end /tn $task & schtasks /delete /tn $task /f" >/dev/null 2>&1 || true; }
trap 'cleanup; board_release' EXIT

stage=$(mktemp -d "${TMPDIR:-/tmp}/pb-win-remote-test.XXXXXX")
trap 'rm -rf "$stage"; cleanup; board_release' EXIT
git archive HEAD -- . ':!console' ':!marketing' | tar x -C "$stage"
printf -v args ' %q' "$@"
cat > "$stage/.pb-run.sh" <<RUN
set -uo pipefail
here=\$(cd "\$(dirname "\$0")" && pwd)
cd "\$here/$module"
mkdir -p "\$here/tmp" 
export RUNNER_TEMP=\$(cygpath -m "\$here/tmp") # a long path: the user TEMP is the 8.3 form (RUNNER~1), which two tests reject (CI has D:\\a\\_temp)
export PACKAGES=$(printf %q "${*:-./...}") RUN=$(printf %q "${RUN:-}") COUNT=$(printf %q "${COUNT:-1}") RACE=$(printf %q "${RACE:-}") NATIVE=$(printf %q "${NATIVE:-}") EVENTS="\$here/events"
"\$here/tools/win-test.sh" > "\$here/log.txt" 2>&1
echo \$? > "\$here/exit"
RUN
printf '@echo off\r\n"C:\\Program Files\\Git\\bin\\bash.exe" --noprofile --norc "%%~dp0.pb-run.sh"\r\n' > "$stage/.pb-run.cmd"

echo "win-remote-test: syncing $sha to $host:$dir" >&2
ssh_ "rmdir /s /q $dir 2>nul & if not exist $dir mkdir $dir & del /q $dir\\exit 2>nul" >/dev/null # a run that cannot remove the directory must not leave its result file behind
tar -C "$stage" -cf - . | ssh_ "tar -xf - -C $dir"

echo "win-remote-test: $host: $module${args:+ $args} in the console session" >&2
# /IT runs in the user's logged-on session without a stored password; the task is the machine's own user, elevated.
ssh_ "schtasks /create /tn $task /tr \"%USERPROFILE%\\$dir\\.pb-run.cmd\" /sc once /st 00:00 /ru %USERNAME% /it /rl highest /f & schtasks /run /tn $task" >/dev/null
deadline=$((SECONDS + 5400))
until ssh_ "if exist $dir\\exit (exit 0) else (exit 1)" >/dev/null 2>&1; do
  [ $SECONDS -lt $deadline ] || { echo "win-remote-test: no result after 90 minutes on $host" >&2; exit 1; }
  sleep 30
done
mkdir -p "$out"
remote=${dir//\\//} # scp wants forward slashes
scp -q -r "$host:$remote/events" "$host:$remote/log.txt" "$host:$remote/exit" "$out/"
cat "$out/log.txt"
echo "win-remote-test: $host $sha: events and log in $out" >&2
exit "$(tr -d '\r\n ' < "$out/exit")"
