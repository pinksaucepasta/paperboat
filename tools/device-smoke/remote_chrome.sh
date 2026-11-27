#!/usr/bin/env bash
# A PW_CHROMIUM / SMOKE_PW_CHROMIUM stand-in for a controller that must not run a browser: Chrome runs on
# SMOKE_BROWSER_HOST (an ssh host with google-chrome, e.g. a linuxpool runner) and Playwright's debugging pipe
# (it writes to fd 3 and reads fd 4) travels as that ssh session's stdin and stdout. Node, the scripts and every
# session state stay on the controller; the profile directory Playwright names is made on the browser host.
# env: SMOKE_BROWSER_HOST, [SMOKE_BROWSER_BIN] (default google-chrome)
set -eu
host=${SMOKE_BROWSER_HOST:?set SMOKE_BROWSER_HOST}
bin=${SMOKE_BROWSER_BIN:-google-chrome}
[[ $host =~ ^[A-Za-z0-9][A-Za-z0-9@._-]*$ && $bin =~ ^[A-Za-z0-9/._-]+$ ]] || { echo "remote_chrome: bad SMOKE_BROWSER_HOST or SMOKE_BROWSER_BIN" >&2; exit 2; }
args=''
[ $# = 0 ] || printf -v args ' %q' "$@"
# Playwright removes its profile directory only on its own machine, so the one made over there is removed when the browser ends
profile=''
for a in "$@"; do [[ $a =~ ^--user-data-dir=(/tmp/playwright_[A-Za-z0-9._-]+)$ ]] && profile=${BASH_REMATCH[1]}; done
command="$bin$args 3<&0 4>&1 0</dev/null 1>&2"
[ -z "$profile" ] || command="$command; rc=\$?; rm -rf -- $profile; exit \$rc"
exec ssh -T -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=15 "$host" "$command" 0<&3 1>&4
