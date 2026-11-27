#!/usr/bin/env bash
# The console rows' browser, off the controller: a Playwright server on BROWSER_HOST (container, loopback only, secret ws
# path) reached through an SSH forward. Idempotent. Prints the exports a console row needs; eval them.
#   eval "$(tools/device-smoke/console_browser.sh up)"     # down: remove the container and the forward
set -euo pipefail
HOST=${BROWSER_HOST:-ubuntu@anvit} PORT=${BROWSER_PORT:-3979} NAME=pb-console-pw
REAL=${PW_REAL_MODULE:-$HOME/.config/paperboat-dev/pw/node_modules/playwright-core/index.mjs}
HERE=$(cd "$(dirname "$0")" && pwd)
URLFILE=$HOME/.config/paperboat-dev/console-browser.url SOCK=$HOME/.ssh/console-browser-$PORT.sock
VERSION=$(node -p "require('$(dirname "$REAL")/package.json').version")
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=15)
forward_down() { [ ! -S "$SOCK" ] || ssh -S "$SOCK" -O exit "$HOST" >/dev/null 2>&1 || true; rm -f "$SOCK"; }
if [ "${1:-up}" = down ]; then
  forward_down; "${SSH[@]}" "$HOST" "docker rm -f $NAME" >/dev/null 2>&1 || true; rm -f "$URLFILE"; exit 0
fi
alive() { [ -s "$URLFILE" ] && PW_REAL_MODULE=$REAL PW_REMOTE_FILE=$URLFILE timeout 40 node --input-type=module -e "const {chromium}=await import('$HERE/remote_browser.mjs'); const b=await chromium.launch(); await b.close()" >/dev/null 2>&1; }
{
  flock -w 600 9
  if ! alive; then
    load=$("${SSH[@]}" "$HOST" 'cut -d. -f1 /proc/loadavg')
    [ "$load" -lt 25 ] || { echo "console browser: $HOST load is $load (limit 25)" >&2; exit 1; }
    forward_down
    (umask 077; mkdir -p "${URLFILE%/*}"; echo "ws://127.0.0.1:$PORT/$(python3 -c 'import secrets;print(secrets.token_hex(16))')" >"$URLFILE")
    ws=$(sed 's#.*/##' "$URLFILE")
    "${SSH[@]}" "$HOST" "mkdir -p pb-tools/pw-remote-$VERSION/node_modules"
    tar -C "$(dirname "$(dirname "$REAL")")" -cf - playwright-core | "${SSH[@]}" "$HOST" "tar -C pb-tools/pw-remote-$VERSION/node_modules -xf -"
    printf '%s\n' "$ws" | "${SSH[@]}" "$HOST" "read -r ws; docker rm -f $NAME >/dev/null 2>&1 || true; docker run -d --name $NAME --init --ipc=host -p 127.0.0.1:$PORT:$PORT -v \$HOME/pb-tools/pw-remote-$VERSION:/pw:ro --cpus 3 --memory 6g mcr.microsoft.com/playwright:v$VERSION-noble node /pw/node_modules/playwright-core/cli.js run-server --port $PORT --host 0.0.0.0 --path /\$ws >/dev/null"
    ssh -f -N -M -S "$SOCK" -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -L "127.0.0.1:$PORT:127.0.0.1:$PORT" "$HOST" </dev/null >/dev/null 2>&1 9>&-
    for _ in 1 2 3 4 5 6; do alive && break; sleep 5; done
    alive || { echo "console browser: the server on $HOST does not answer" >&2; exit 1; }
  fi
} 9>"$HOME/.ssh/console-browser-$PORT.lock"
printf 'export PW_REAL_MODULE=%q PW_REMOTE_FILE=%q SMOKE_PW_MODULE=%q PW_MODULE=%q NODE_OPTIONS=%q\n' "$REAL" "$URLFILE" "$HERE/remote_browser.mjs" "$HERE/remote_browser.mjs" "--import=$HERE/remote_browser_register.mjs"
