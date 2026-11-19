#!/usr/bin/env bash
# Exercises install.sh's flags and prompts. Every tool it would run is a stub, so nothing is installed.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
mkdir -p "$tmp/bin"
installer=$here/install.sh
# curl stops the installer right after its flags are accepted; the other tools only have to exist.
# systemctl stub: the unit is absent (fresh host) unless $installed is set.
for tool in tar sha256sum install timeout sudo; do printf '#!/bin/sh\nexit 0\n' > "$tmp/bin/$tool"; done
printf '#!/bin/sh\n[ -n "$installed" ]\n' > "$tmp/bin/systemctl"
printf '#!/bin/sh\necho "STUB curl" >&2\nexit 77\n' > "$tmp/bin/curl"
chmod +x "$tmp"/bin/*
run() { PATH="$tmp/bin:$PATH" setsid sh "$installer" "$@" < /dev/null 2>&1; } # setsid: no controlling terminal

out=$(run --enable both) && fail "--enable was accepted"
grep -q 'use --relay, --edge or --both' <<< "$out" || fail "--enable error: $out"
out=$(run --enable=relay) && fail "--enable=relay was accepted"
out=$(run --relay --edge) && fail "two component flags were accepted"
grep -q 'only one of' <<< "$out" || fail "two flags error: $out"

out=$(run) && fail "no flag and no terminal did not fail"
for flag in relay edge both; do
  grep -qF "curl -fsSL https://get.pprbt.dev/selfhost | sh -s -- --$flag" <<< "$out" || fail "no command for --$flag: $out"
done
! grep -q STUB <<< "$out" || fail "installer went on without a choice"
out=$(installed=1 run || true)
grep -q 'STUB curl' <<< "$out" || fail "an upgrade asked for a choice: $out"
for flag in --relay --edge --both; do
  out=$(run "$flag" --endpoint-host 203.0.113.10 || true)
  grep -q 'STUB curl' <<< "$out" || fail "$flag stopped early: $out"
done

# The prompts, on a real pseudo-terminal: choice 2 and a confirmation reach pbh as --edge.
PATH="$tmp/bin:$PATH" python3 -I - "$installer" <<'PY' || fail "interactive prompt"
import os, pty, sys, select
pid, fd = pty.fork()
if pid == 0:
    os.execvp("sh", ["sh", sys.argv[1]])
buf = b""
def until(text):
    global buf
    while text not in buf:
        if not select.select([fd], [], [], 10)[0]:
            sys.exit("timeout waiting for %r; got %r" % (text, buf))
        try:
            data = os.read(fd, 4096)
        except OSError:
            data = b""
        if not data:
            sys.exit("closed before %r; got %r" % (text, buf))
        buf += data
until(b"Choose 1-3 [3]: "); os.write(fd, b"2\n")
until(b"Install an edge on this host? [Y/n] "); os.write(fd, b"y\n")
until(b"STUB curl")
PY
echo "ok"
