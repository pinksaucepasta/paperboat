#!/usr/bin/env bash
# Paperboat device smoke suite.
#
# Runs the user-visible baseline on the real test machines after every build:
# doctor and version on every device, then from the HP client against every
# target (hp, apple, victus): short exec, long exec, terminal input, managed
# SSH (short and held), 50 MiB transfer with hash check; previews on the Unix
# devices. Optional `--update <dir>` adds the update-continuity check.
#
# Usage:
#   APPLE_PASS=... VICTUS_PASS=... tools/device-smoke/smoke.sh --expect-version <version> [options]
# Options:
#   --expect-version V   version every device must report (required)
#   --targets LIST       comma list of targets (default hp,apple,victus)
#   --long-seconds N     duration of long exec / held SSH (default 420 / 200)
#   --skip LIST          comma list of test ids to skip (e.g. preview,transfer)
#   --update DIR         after the baseline, update each target with the build in DIR
#                        (pb-linux-amd64, pb-darwin-arm64, pb-windows-amd64.exe)
#                        while a terminal and a long exec are running, then verify
#                        they recover and the version changed.
#   --update-version V   version the --update build reports (required with --update)
#
# Output: a PASS/FAIL table and .build-free JSON at
#   /home/ubuntu/pinksaucepasta/tools/device-smoke/results/<UTC timestamp>.json
# Exit status is non-zero when any test fails.
#
# Credentials: HP uses key SSH. APPLE_PASS and VICTUS_PASS must be exported by
# the caller (values are in REMOTE_TESTING.md). They are never printed.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
RESULTS_DIR="$HERE/results"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
RUN_ID="smoke$(date -u +%H%M%S)$RANDOM"

EXPECT_VERSION=""
TARGETS="hp,apple,victus"
LONG_EXEC=420
LONG_SSH=200
SKIP=""
UPDATE_DIR=""
UPDATE_VERSION=""

while [ $# -gt 0 ]; do
  case "$1" in
    --expect-version) EXPECT_VERSION=$2; shift 2 ;;
    --targets) TARGETS=$2; shift 2 ;;
    --long-seconds) LONG_EXEC=$2; shift 2 ;;
    --skip) SKIP=$2; shift 2 ;;
    --update) UPDATE_DIR=$2; shift 2 ;;
    --update-version) UPDATE_VERSION=$2; shift 2 ;;
    -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$EXPECT_VERSION" ] || { echo "--expect-version is required" >&2; exit 2; }
[ -z "$UPDATE_DIR" ] || [ -n "$UPDATE_VERSION" ] || { echo "--update requires --update-version" >&2; exit 2; }
[ -n "${APPLE_PASS:-}" ] || [[ ",$TARGETS," != *,apple,* ]] || { echo "export APPLE_PASS (see REMOTE_TESTING.md)" >&2; exit 2; }
[ -n "${VICTUS_PASS:-}" ] || [[ ",$TARGETS," != *,victus,* ]] || { echo "export VICTUS_PASS (see REMOTE_TESTING.md)" >&2; exit 2; }
command -v sshpass >/dev/null || { echo "sshpass is required" >&2; exit 2; }
mkdir -p "$RESULTS_DIR"

# ---------------------------------------------------------------- devices
PB_HP='$HOME/.local/bin/pb'
PB_APPLE='/Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb'
PB_VICTUS="${PB_VICTUS:-C:\\Program Files\\Paperboat\\bin\\pb.exe}"
SSH_OPTS=(-o ConnectTimeout=15 -o ServerAliveInterval=30 -o StrictHostKeyChecking=accept-new)

# on DEVICE CMD... : run a POSIX shell command (hp, apple) or a PowerShell
# command (victus) on the device and print its output.
on() {
  local device=$1; shift
  case "$device" in
    hp) ssh "${SSH_OPTS[@]}" -o BatchMode=yes anvit@hp "$*" ;;
    apple) SSHPASS="$APPLE_PASS" sshpass -e ssh "${SSH_OPTS[@]}" pujan.pm@apple \
      "security unlock-keychain -p '$APPLE_PASS' ~/Library/Keychains/login.keychain-db >/dev/null 2>&1; $*" ;;
    victus) local encoded
      encoded=$(printf '%s' "$*" | iconv -t UTF-16LE | base64 -w0)
      SSHPASS="$VICTUS_PASS" sshpass -e ssh "${SSH_OPTS[@]}" pujan@victus "powershell -NoProfile -NonInteractive -EncodedCommand $encoded" ;;
    *) echo "unknown device $device" >&2; return 2 ;;
  esac
}

pb_on() { # pb_on DEVICE ARGS... (args are already quoted for the remote shell)
  local device=$1; shift
  case "$device" in
    hp) on hp "$PB_HP $*" ;;
    apple) on apple "$PB_APPLE $*" ;;
    victus) on victus "& '$PB_VICTUS' $*" ;;
  esac
}

# ---------------------------------------------------------------- results
declare -a ROWS=()
FAILED=0
record() { # record ID STATUS SECONDS DETAIL
  local id=$1 status=$2 secs=$3 detail=${4:-}
  ROWS+=("$id|$status|$secs|${detail//|//}")
  [ "$status" = PASS ] || [ "$status" = SKIP ] || FAILED=1
  printf '%-28s %-5s %6ss  %s\n' "$id" "$status" "$secs" "${detail:0:150}"
}
skipped() { [[ ",$SKIP," == *",$1,"* ]]; }
now() { date +%s; }

# ---------------------------------------------------------------- tests
check_device() { # doctor + version on the device itself
  local d=$1 t0 out
  t0=$(now); out=$(pb_on "$d" --version 2>&1)
  if grep -q "$EXPECT_VERSION" <<<"$out"; then record "version:$d" PASS $(( $(now)-t0 ))
  else record "version:$d" FAIL $(( $(now)-t0 )) "$(tr '\n' ' ' <<<"$out" | tail -c 200)"; fi
  t0=$(now); out=$(pb_on "$d" doctor 2>&1)
  if grep -q "doctor: healthy" <<<"$out"; then record "doctor:$d" PASS $(( $(now)-t0 ))
  else record "doctor:$d" FAIL $(( $(now)-t0 )) "$(grep -m3 -iE 'fail|warning|unhealthy|degraded' <<<"$out" | tr '\n' ' ')"; fi
}

remote_echo_cmd() { # command that prints $2 on target $1
  if [ "$1" = victus ]; then echo "cmd /c echo $2"; else echo "echo $2"; fi
}

test_exec() { # test_exec TARGET [ID-PREFIX]
  local t=$1 prefix=${2:-exec} marker="ex-$RUN_ID" t0 out
  t0=$(now); out=$(on hp "timeout 120 $PB_HP exec $t -- $(remote_echo_cmd "$t" "$marker"); echo rc=\$?" 2>&1)
  if grep -q "$marker" <<<"$out" && grep -q "rc=0" <<<"$out"; then record "$prefix:$t" PASS $(( $(now)-t0 ))
  else record "$prefix:$t" FAIL $(( $(now)-t0 )) "$(tr '\n' ' ' <<<"$out" | tail -c 200)"; fi
}

# Long work runs on HP in the background as uploaded job scripts, so remote
# commands never need nested shell quoting. Results are collected later.
hp_job() { # hp_job TAG  (job body on stdin)
  local tag=$1
  on hp "mkdir -p /tmp/$RUN_ID && cat > /tmp/$RUN_ID/$tag.sh" >/dev/null
  on hp "cd /tmp/$RUN_ID && (nohup sh -c 'date +%s > $tag.start; sh $tag.sh > $tag.out 2>&1; echo rc=\$? >> $tag.out; date +%s > $tag.end' >/dev/null 2>&1 &)" >/dev/null
}
start_long_exec() { # start_long_exec TARGET TAG SECONDS
  local t=$1 tag=$2 secs=$3
  if [ "$t" = victus ]; then
    printf '%s\n' "timeout $((secs+300)) $PB_HP exec $t -- powershell -NoProfile -Command \"Start-Sleep $secs; 'FINAL'\"" | hp_job "$tag"
  else
    printf '%s\n' "timeout $((secs+300)) $PB_HP exec $t -- sh -c 'sleep $secs; echo FINAL'" | hp_job "$tag"
  fi
}
start_long_ssh() {
  local t=$1 tag=$2 secs=$3
  if [ "$t" = victus ]; then
    printf '%s\n' "timeout $((secs+300)) $PB_HP ssh $t -- powershell -NoProfile -Command \"Start-Sleep $secs; 'SSHFINAL'\" < /dev/null" | hp_job "$tag"
  else
    printf '%s\n' "timeout $((secs+300)) $PB_HP ssh $t -- \"sleep $secs; echo SSHFINAL\" < /dev/null" | hp_job "$tag"
  fi
}
collect() { # collect ID TAG EXPECT-MARKER : waits for the background job
  local id=$1 tag=$2 want=$3 out
  for _ in $(seq 1 200); do
    on hp "test -f /tmp/$RUN_ID/$tag.end" && break
    sleep 15
  done
  out=$(on hp "cat /tmp/$RUN_ID/$tag.out; echo; echo dur=\$(( \$(cat /tmp/$RUN_ID/$tag.end 2>/dev/null || date +%s) - \$(cat /tmp/$RUN_ID/$tag.start) ))" 2>&1)
  local dur; dur=$(grep -oE 'dur=[0-9]+' <<<"$out" | cut -d= -f2)
  if grep -q "$want" <<<"$out" && grep -q "rc=0" <<<"$out"; then record "$id" PASS "${dur:-?}"
  else record "$id" FAIL "${dur:-?}" "$(grep -v '^dur=' <<<"$out" | tr '\n' ' ' | tail -c 220)"; fi
}

test_ssh_short() {
  local t=$1 marker="ssh-$RUN_ID" t0 out
  t0=$(now); out=$(on hp "timeout 120 $PB_HP ssh $t -- $(remote_echo_cmd "$t" "$marker") < /dev/null; echo rc=\$?" 2>&1)
  if grep -q "$marker" <<<"$out" && grep -q "rc=0" <<<"$out"; then record "ssh:$t" PASS $(( $(now)-t0 ))
  else record "ssh:$t" FAIL $(( $(now)-t0 )) "$(tr '\n' ' ' <<<"$out" | tail -c 200)"; fi
}

# Terminal: a real PTY session from HP. The driver is uploaded once; it sends
# an input line that computes 6*7 and waits for the computed answer.
TERM_DRIVER=$(cat <<'PY'
import os, pty, sys, time, select, signal
pb, target, name, line, token = sys.argv[1:6]
pid, fd = pty.fork()
if pid == 0:
    os.execv(pb, [pb, "connect", target, "--name", name, "--status-bar", "off"])
buf = b""
def read_until(tok, deadline):
    global buf
    while time.time() < deadline:
        r, _, _ = select.select([fd], [], [], 1)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                return False
            if not data:
                return False
            buf += data
            if tok in buf:
                return True
    return False
time.sleep(8)
os.write(fd, (line + "\r").encode())
ok = read_until(token.encode(), time.time() + 90)
os.write(fd, b"exit\r")
time.sleep(3)
try:
    os.kill(pid, signal.SIGTERM)
except OSError:
    pass
print("TERMINAL_OK" if ok else "TERMINAL_FAIL " + buf[-300:].decode(errors="replace").replace("\n", " "))
PY
)
DRIVER_UPLOADED=0
test_terminal() { # test_terminal TARGET [ID-PREFIX]
  local t=$1 prefix=${2:-terminal} t0 out marker="tm$RUN_ID$RANDOM" line
  if [ $DRIVER_UPLOADED = 0 ]; then
    printf '%s\n' "$TERM_DRIVER" | on hp "mkdir -p /tmp/$RUN_ID && cat > /tmp/$RUN_ID/term.py" && DRIVER_UPLOADED=1
  fi
  # POSIX shells and PowerShell both print MARKER-42 for these lines.
  if [ "$t" = victus ]; then line="echo $marker-\$(6*7)"; else line="echo $marker-\$((6*7))"; fi
  t0=$(now)
  out=$(on hp "python3 /tmp/$RUN_ID/term.py $PB_HP $t smoke-$marker '$line' $marker-42" 2>&1)
  if grep -q TERMINAL_OK <<<"$out"; then record "$prefix:$t" PASS $(( $(now)-t0 ))
  else record "$prefix:$t" FAIL $(( $(now)-t0 )) "$(tr '\n' ' ' <<<"$out" | tail -c 220)"; fi
}

test_transfer() {
  local t=$1
  local name="smoke-$RUN_ID-$t.bin" t0 src_sha out dst_sha
  t0=$(now)
  src_sha=$(on hp "mkdir -p /tmp/$RUN_ID && head -c 52428800 /dev/urandom > /tmp/$RUN_ID/$name && sha256sum /tmp/$RUN_ID/$name | cut -d' ' -f1")
  out=$(on hp "timeout 900 $PB_HP send /tmp/$RUN_ID/$name --to $t; echo rc=\$?" 2>&1)
  if ! grep -q "rc=0" <<<"$out"; then record "transfer:$t" FAIL $(( $(now)-t0 )) "$(tr '\n' ' ' <<<"$out" | tail -c 200)"; return; fi
  case "$t" in
    victus) dst_sha=$(on victus "\$f = Get-ChildItem -Path \$HOME -Recurse -Filter '$name' -ErrorAction SilentlyContinue -Depth 3 | Select-Object -First 1; if (\$f) { (Get-FileHash \$f.FullName -Algorithm SHA256).Hash.ToLower(); Remove-Item \$f.FullName }" | tr -d '\r') ;;
    apple) dst_sha=$(on apple "f=\$(find ~ -maxdepth 3 -name '$name' 2>/dev/null | head -1); [ -n \"\$f\" ] && shasum -a 256 \"\$f\" | cut -d' ' -f1 && rm -f \"\$f\"") ;;
    hp) dst_sha=$(on hp "f=\$(find ~ -maxdepth 3 -name '$name' -not -path '/tmp/*' 2>/dev/null | grep -v '^/tmp' | head -1); [ -n \"\$f\" ] && sha256sum \"\$f\" | cut -d' ' -f1 && rm -f \"\$f\"") ;;
  esac
  if [ -n "$dst_sha" ] && [ "$dst_sha" = "$src_sha" ]; then record "transfer:$t" PASS $(( $(now)-t0 ))
  else record "transfer:$t" FAIL $(( $(now)-t0 )) "received sha '${dst_sha:-missing}' != source"; fi
}

test_preview() { # on a Unix device: serve a marker on loopback, publish, fetch from the workspace
  local d=$1 port=$(( 18000 + RANDOM % 1000 )) marker="pv-$RUN_ID" t0 out url
  t0=$(now)
  out=$(on "$d" "mkdir -p /tmp/$RUN_ID-pv && cd /tmp/$RUN_ID-pv && echo $marker > index.html && (nohup python3 -m http.server $port --bind 127.0.0.1 >/dev/null 2>&1 & echo \$! > srv.pid) && sleep 1 && \
    $( [ "$d" = apple ] && echo "$PB_APPLE" || echo "$PB_HP" ) preview $port --background --ttl 10m --json" 2>&1)
  url=$(grep -oE 'https://[^"]+' <<<"$out" | head -1)
  if [ -z "$url" ]; then record "preview:$d" FAIL $(( $(now)-t0 )) "no URL: $(tr '\n' ' ' <<<"$out" | tail -c 200)"
  else
    local body=""
    for _ in $(seq 1 12); do body=$(curl -fsS --max-time 10 "$url" 2>/dev/null) && grep -q "$marker" <<<"$body" && break; sleep 5; done
    if grep -q "$marker" <<<"$body"; then record "preview:$d" PASS $(( $(now)-t0 ))
    else record "preview:$d" FAIL $(( $(now)-t0 )) "URL did not serve marker"; fi
  fi
  on "$d" "kill \$(cat /tmp/$RUN_ID-pv/srv.pid) 2>/dev/null; rm -rf /tmp/$RUN_ID-pv" >/dev/null 2>&1
}

update_target() { # update TARGET with the build in UPDATE_DIR while work is live
  local t=$1 bin dst t0 out
  case "$t" in
    hp) bin=pb-linux-amd64; dst=/tmp/$RUN_ID-upd ;;
    apple) bin=pb-darwin-arm64; dst=/tmp/$RUN_ID-upd ;;
    victus) bin=pb-windows-amd64.exe; dst='C:/Users/Pujan/smoke-upd.exe' ;;
  esac
  [ -f "$UPDATE_DIR/$bin" ] || { record "update:$t" FAIL 0 "missing $UPDATE_DIR/$bin"; return; }
  case "$t" in
    hp) scp -q "${SSH_OPTS[@]}" "$UPDATE_DIR/$bin" "anvit@hp:$dst" ;;
    apple) SSHPASS="$APPLE_PASS" sshpass -e scp -q "${SSH_OPTS[@]}" "$UPDATE_DIR/$bin" "pujan.pm@apple:$dst" ;;
    victus) SSHPASS="$VICTUS_PASS" sshpass -e scp -q "${SSH_OPTS[@]}" "$UPDATE_DIR/$bin" "pujan@victus:$dst" ;;
  esac
  # Live work during the update: a long exec from HP and a held terminal.
  start_long_exec "$t" "upd-exec-$t" 240
  sleep 20
  t0=$(now)
  case "$t" in
    victus) out=$(pb_on victus "update --from '$dst'" 2>&1) ;;
    *) out=$(on "$t" "chmod +x $dst && $( [ "$t" = apple ] && echo "$PB_APPLE" || echo "$PB_HP" ) update --from $dst; echo rc=\$?" 2>&1) ;;
  esac
  local ver; ver=$(pb_on "$t" --version 2>&1)
  if grep -q "$UPDATE_VERSION" <<<"$ver"; then record "update:$t" PASS $(( $(now)-t0 ))
  else record "update:$t" FAIL $(( $(now)-t0 )) "$(tr '\n' ' ' <<<"$out" | tail -c 200)"; fi
  test_terminal "$t" after-update-terminal
  test_exec "$t" after-update-exec
  collect "during-update-exec:$t" "upd-exec-$t" FINAL
}

# ---------------------------------------------------------------- run
IFS=, read -r -a TLIST <<<"$TARGETS"
echo "Paperboat device smoke  run=$RUN_ID  expect=$EXPECT_VERSION  targets=$TARGETS"
printf '%-28s %-5s %7s  %s\n' TEST RESULT TIME DETAIL

for d in "${TLIST[@]}"; do check_device "$d"; done

# Long-running work starts first so it overlaps the short checks.
for t in "${TLIST[@]}"; do
  skipped exec-long || start_long_exec "$t" "long-exec-$t" "$LONG_EXEC"
  skipped ssh-long || start_long_ssh "$t" "long-ssh-$t" "$LONG_SSH"
done

for t in "${TLIST[@]}"; do
  skipped exec || test_exec "$t"
  skipped terminal || test_terminal "$t"
  skipped ssh || test_ssh_short "$t"
  skipped transfer || test_transfer "$t"
done
for d in "${TLIST[@]}"; do
  [ "$d" = victus ] && continue
  skipped preview || test_preview "$d"
done
for t in "${TLIST[@]}"; do
  skipped ssh-long || collect "ssh-long:$t" "long-ssh-$t" SSHFINAL
  skipped exec-long || collect "exec-long:$t" "long-exec-$t" FINAL
done

if [ -n "$UPDATE_DIR" ]; then
  for t in "${TLIST[@]}"; do update_target "$t"; done
fi

# ---------------------------------------------------------------- report
on hp "rm -rf /tmp/$RUN_ID" >/dev/null 2>&1
{
  printf '{"run":"%s","at":"%s","expect_version":"%s","update_version":"%s","results":[' "$RUN_ID" "$STAMP" "$EXPECT_VERSION" "$UPDATE_VERSION"
  first=1
  for row in "${ROWS[@]}"; do
    IFS='|' read -r id status secs detail <<<"$row"
    [ $first = 1 ] || printf ','
    first=0
    printf '{"id":"%s","status":"%s","seconds":"%s","detail":%s}' "$id" "$status" "$secs" "$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$detail")"
  done
  printf ']}\n'
} > "$RESULTS_DIR/$STAMP.json"
echo
pass=$(printf '%s\n' "${ROWS[@]}" | grep -c '|PASS|'); fail=$(printf '%s\n' "${ROWS[@]}" | grep -c '|FAIL|')
echo "PASS $pass  FAIL $fail   results: $RESULTS_DIR/$STAMP.json"
exit $FAILED
