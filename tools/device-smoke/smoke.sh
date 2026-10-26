#!/usr/bin/env bash
# Paperboat device smoke suite.
#
# Runs the user-visible baseline on the real test machines after every build:
# doctor and version on every device, then from the HP client against every
# launch target (hp, apple): short exec, long exec, terminal input, managed
# SSH (short and held), 50 MiB transfer with hash check; all-pairs interactive
# terminals (resize, controls, paste, Unicode, Vim/less/htop) with exact cleanup; previews on the Unix
# devices. Optional `--update <dir>` adds the update-continuity check.
#
# Usage:
#   APPLE_PASS=... tools/device-smoke/smoke.sh --expect-version <version> [options]
# Options:
#   --expect-version V   version every device must report (required)
#   --windows-install-json FILE  saved Windows pb install --json output (required for victus)
#   --targets LIST       comma list of launch targets (default hp,apple)
#   --long-seconds N     duration of long exec (default 420)
#   --ssh-seconds N      duration of held SSH (default 200)
#   --skip LIST          comma list of test ids to skip (e.g. preview,transfer)
#   --only-check ID      run one exact feature check, plus version/doctor baselines
#   --only-list FILE     run exactly the check ids listed in FILE (one per line), plus baselines
#   --update DIR         after the baseline, update each target with the build in DIR
#                        (pb-linux-amd64, pb-darwin-arm64, pb-windows-amd64.exe)
#                        while a terminal and a long exec are running, then verify
#                        they recover and the version changed.
#   --update-version V   version the --update build reports (required with --update)
#
# Output: a PASS/FAIL table and .build-free JSON at
#   tools/device-smoke/results/<UTC timestamp>.json
# Exit status is non-zero when any test fails.
#
# Credentials: HP uses key SSH. APPLE_PASS and VICTUS_PASS must be exported by
# the caller (values are in REMOTE_TESTING.md). They are never printed.

if [ "${PAPERBOAT_SMOKE_SNAPSHOT:-}" != 1 ]; then
  exec python3 "$(dirname "$0")/snapshot_run.py" "$0" "$@"
fi
set -uo pipefail

HERE=$PAPERBOAT_SMOKE_SOURCE_DIR
# Team creation needs a paid Checkout: team checks reuse this retained paid team.
SMOKE_TEAM=${PAPERBOAT_SMOKE_TEAM:-paperboat-smoke-paid}
RESULTS_DIR="$HERE/results"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
RUN_ID="smoke$(date -u +%H%M%S)$RANDOM"

EXPECT_VERSION=""
TARGETS="hp,apple"
# CLIENT is the Linux device that drives every pair; PEER is the second Linux device. SMOKE_CT_MAP
# (name=host:container,...) stands a disposable systemd container on a Docker host in for a device name;
# SMOKE_CT_USER / SMOKE_CT_UID / SMOKE_CT_DOCKER set the container user (pbt/1000) and docker command ("sudo docker").
CLIENT=${SMOKE_CLIENT:-hp}
PEER=${SMOKE_PEER:-dadape}
LONG_EXEC=420
LONG_SSH=200
SKIP=""
ONLY_CHECK=""
ONLY_LIST=""
UPDATE_DIR=""
UPDATE_VERSION=""
WINDOWS_INSTALL_JSON=""

while [ $# -gt 0 ]; do
  case "$1" in
    --expect-version) EXPECT_VERSION=$2; shift 2 ;;
    --windows-install-json) WINDOWS_INSTALL_JSON=$2; shift 2 ;;
    --targets) TARGETS=$2; shift 2 ;;
    --long-seconds) LONG_EXEC=$2; shift 2 ;;
    --ssh-seconds) LONG_SSH=$2; shift 2 ;;
    --skip) SKIP=$2; shift 2 ;;
    --only-check) ONLY_CHECK=$2; shift 2 ;;
    --only-list) ONLY_LIST=$2; shift 2 ;;
    --update) UPDATE_DIR=$2; shift 2 ;;
    --update-version) UPDATE_VERSION=$2; shift 2 ;;
    -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
for duration in "$LONG_EXEC" "$LONG_SSH"; do
  if ! [[ "$duration" =~ ^[0-9]+$ ]] || [ "$duration" -lt 1 ] || [ "$duration" -gt 86400 ]; then
    echo "long-check durations must be 1..86400 seconds" >&2
    exit 2
  fi
done
# PLAN.md 1.1: Windows device work resumes after launch.
if [[ ",$TARGETS," == *,victus,* ]]; then
  echo "Windows device smoke is deferred until after launch (PLAN.md 1.1)." >&2
  exit 2
fi
[ -n "$EXPECT_VERSION" ] || { echo "--expect-version is required" >&2; exit 2; }
ONLY_IDS="|" ONLY_GROUPS="|"
if [ -n "$ONLY_LIST" ]; then
  [ -r "$ONLY_LIST" ] || { echo "--only-list file is not readable: $ONLY_LIST" >&2; exit 2; }
  while IFS= read -r listed; do
    case "$listed" in ''|'#'*) continue ;; esac
    ONLY_IDS+="$listed|"; ONLY_GROUPS+="${listed%%:*}|"
  done < "$ONLY_LIST"
fi
[ -z "$UPDATE_DIR" ] || [ -n "$UPDATE_VERSION" ] || { echo "--update requires --update-version" >&2; exit 2; }
[ -n "${APPLE_PASS:-}" ] || [[ ",$TARGETS," != *,apple,* ]] || { echo "export APPLE_PASS (see REMOTE_TESTING.md)" >&2; exit 2; }
[ -n "${DADAPE_PASS:-}" ] || [ -n "${SMOKE_CT_MAP:-}" ] || [[ ",$TARGETS," != *,dadape,* ]] || { echo "export DADAPE_PASS (see REMOTE_TESTING.md)" >&2; exit 2; }
[ -n "${VICTUS_PASS:-}" ] || [[ ",$TARGETS," != *,victus,* ]] || { echo "export VICTUS_PASS (see REMOTE_TESTING.md)" >&2; exit 2; }
command -v sshpass >/dev/null || { echo "sshpass is required" >&2; exit 2; }
mkdir -p "$RESULTS_DIR"

# ---------------------------------------------------------------- devices
PB_HP='$HOME/.local/bin/pb'
PB_APPLE='/Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb'
PB_VICTUS=""
if [[ ",$TARGETS," == *,victus,* ]]; then
  PB_VICTUS=$(python3 "$HERE/install_path.py" "$WINDOWS_INSTALL_JSON") || exit 2
fi
SSH_OPTS=(-o ConnectTimeout=15 -o ServerAliveInterval=30 -o StrictHostKeyChecking=accept-new)

ct_of() { # ct_of DEVICE: host:container for a device that is a container, else nothing
  local pair map=${SMOKE_CT_MAP:-}
  for pair in ${map//,/ }; do [ "${pair%%=*}" = "$1" ] && { echo "${pair#*=}"; return 0; }; done
  return 1
}
same_ct_host() { # same_ct_host A B: both devices are containers on one Docker host
  local a b
  a=$(ct_of "$1") && b=$(ct_of "$2") && [ "${a%%:*}" = "${b%%:*}" ]
}
container_on() { # container_on HOST:CONTAINER CMD... : run as the enrolled user inside the container
  local host=${1%%:*} ct=${1#*:}; shift
  local user=${SMOKE_CT_USER:-pbt} uid=${SMOKE_CT_UID:-1000} docker=${SMOKE_CT_DOCKER:-sudo docker}
  ssh "${SSH_OPTS[@]}" -o BatchMode=yes "$host" "$docker exec -i -u $user -e HOME=/home/$user -e XDG_RUNTIME_DIR=/run/user/$uid $ct bash -lc $(printf '%q' "$*")"
}

# on DEVICE CMD... : run a POSIX shell command (hp, apple) or a PowerShell
# command (victus) on the device and print its output.
on() {
  local device=$1 where; shift
  if where=$(ct_of "$device"); then container_on "$where" "$*"; return; fi
  case "$device" in
    hp) ssh "${SSH_OPTS[@]}" -o BatchMode=yes anvit@hp "$*" ;;
    ubuntu24) ssh "${SSH_OPTS[@]}" -o BatchMode=yes pujan@ubuntu24 "$*" ;;
    dadape) SSHPASS="$DADAPE_PASS" sshpass -e ssh "${SSH_OPTS[@]}" anvit@dadape "$*" ;;
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
  ct_of "$device" >/dev/null && { on "$device" "$PB_HP $*"; return; }
  case "$device" in
    hp) on hp "$PB_HP $*" ;;
    dadape) on dadape "$PB_HP $*" ;;
    apple) on apple "$PB_APPLE $*" ;;
    victus) on victus "& '${PB_VICTUS//\'/\'\'}' $*" ;;
  esac
}

# ---------------------------------------------------------------- results
declare -a ROWS=()
FAILED=0
record() { # record ID STATUS SECONDS DETAIL
  local id=$1 status=$2 secs=$3 detail=${4:-} failure_code=${5:-}
  ROWS+=("$id|$status|$secs|${detail//|//}|$failure_code")
  [ "$status" = PASS ] || [ "$status" = SKIP ] || FAILED=1
  printf '%-28s %-5s %6ss  %s\n' "$id" "$status" "$secs" "${detail:0:150}"
}
# Preserve the actual failure without exposing command output in shared receipts.
record_command_failure() {
  local id=$1 seconds=$2 output=$3 diagnostic code detail
  mkdir -p "$RESULTS_DIR/diagnostics"
  chmod 0700 "$RESULTS_DIR/diagnostics"
  diagnostic=$(umask 077; mktemp "$RESULTS_DIR/diagnostics/$RUN_ID.XXXXXX") || return 1
  printf '%s' "$output" > "$diagnostic"
  code=$(python3 "$HERE/failure_triage.py" "$diagnostic")
  detail="command failed; private diagnostics: $diagnostic"
  [ "$code" != network_dns ] || detail="NETWORK network_dns; $detail"
  record "$id" FAIL "$seconds" "$detail" "$code"
}

skipped() {
  if [ -n "$ONLY_LIST" ] && [[ "$ONLY_IDS" != *"|$1|"* && "$ONLY_GROUPS" != *"|$1|"* ]]; then
    return 0
  fi
  if [ -n "$ONLY_CHECK" ] && [ "$1" != "$ONLY_CHECK" ] && [ "$1" != "${ONLY_CHECK%%:*}" ]; then
    return 0
  fi
  [[ ",$SKIP," == *",$1,"* ]]
}
now() { date +%s; }
# Only the final receipt belongs to the remote wrapper; command stdout may
# contain a misleading rc=0. The SSH transport must also have succeeded.
command_ok() {
  [ "$1" = 0 ] && [ "$(tr -d '\r' <<<"$2" | tail -n 1)" = rc=0 ]
}
marker_present() { grep -Fxq -- "$2" <<<"$(tr -d '\r' <<<"$1")"; }

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
  local t=$1 prefix=${2:-exec} marker="ex-$RUN_ID" t0 out status
  t0=$(now); out=$(on "$CLIENT" "timeout 120 $PB_HP exec $t -- $(remote_echo_cmd "$t" "$marker"); echo rc=\$?" 2>&1); status=$?
  if command_ok "$status" "$out" && marker_present "$out" "$marker"; then record "$prefix:$t" PASS $(( $(now)-t0 ))
  else record_command_failure "$prefix:$t" $(( $(now)-t0 )) "$out"; fi
}

# Long work runs on HP in the background as uploaded job scripts, so remote
# commands never need nested shell quoting. Results are collected later.
device_job() { # device_job DEVICE TAG (job body on stdin)
  local device=$1 tag=$2
  on "$device" "mkdir -p /tmp/$RUN_ID && cat > /tmp/$RUN_ID/$tag.sh" >/dev/null
  on "$device" "cd /tmp/$RUN_ID && (nohup sh -c 'date +%s > $tag.start; sh $tag.sh > $tag.out 2>&1; echo rc=\$? >> $tag.out; date +%s > $tag.end' >/dev/null 2>&1 &)" >/dev/null
}
start_long_exec() { # start_long_exec TARGET TAG SECONDS
  local t=$1 tag=$2 secs=$3
  if [ "$t" = victus ]; then
    printf '%s\n' "timeout $((secs+300)) $PB_HP exec $t -- powershell -NoProfile -Command \"Start-Sleep $secs; 'FINAL'\"" | device_job "$CLIENT" "$tag"
  else
    printf '%s\n' "timeout $((secs+300)) $PB_HP exec $t -- sh -c 'sleep $secs; echo FINAL'" | device_job "$CLIENT" "$tag"
  fi
}
start_long_ssh() {
  local t=$1 tag=$2 secs=$3
  if [ "$t" = victus ]; then
    printf '%s\n' "timeout $((secs+300)) $PB_HP ssh $t -- powershell -NoProfile -Command \"Start-Sleep $secs; 'SSHFINAL'\" < /dev/null" | device_job "$CLIENT" "$tag"
  else
    printf '%s\n' "timeout $((secs+300)) $PB_HP ssh $t -- \"sleep $secs; echo SSHFINAL\" < /dev/null" | device_job "$CLIENT" "$tag"
  fi
}
collect() { # collect ID TAG EXPECT-MARKER : waits for the background job
  local id=$1 tag=$2 want=$3 minimum=${4:-0} out status completed=0 deadline
  deadline=$(( $(now)+minimum+300 ))
  while [ "$(now)" -lt "$deadline" ]; do
    if on "$CLIENT" "test -f /tmp/$RUN_ID/$tag.end"; then completed=1; break; fi
    sleep 15
  done
  [ "$completed" = 1 ] || { record "$id" FAIL "$((minimum+300))" "job did not finish before deadline"; return; }
  out=$(on "$CLIENT" "cat /tmp/$RUN_ID/$tag.out; echo; echo dur=\$(( \$(cat /tmp/$RUN_ID/$tag.end 2>/dev/null || date +%s) - \$(cat /tmp/$RUN_ID/$tag.start) ))" 2>&1)
  status=$?
  local dur; dur=$(grep -oE 'dur=[0-9]+' <<<"$out" | cut -d= -f2)
  if [ "$status" = 0 ] && marker_present "$out" "$want" && [ "$(grep -v '^dur=' <<<"$out" | sed '/^$/d' | tail -1)" = rc=0 ] && [[ "$dur" =~ ^[0-9]+$ ]] \
      && [ "$dur" -ge "$minimum" ]; then record "$id" PASS "${dur:-?}"
  else record_command_failure "$id" "${dur:-?}" "$out"; fi
}

test_ssh_short() {
  local t=$1 marker="ssh-$RUN_ID" t0 out status
  t0=$(now); out=$(on "$CLIENT" "timeout 120 $PB_HP ssh $t -- $(remote_echo_cmd "$t" "$marker") < /dev/null; echo rc=\$?" 2>&1); status=$?
  if command_ok "$status" "$out" && marker_present "$out" "$marker"; then record "ssh:$t" PASS $(( $(now)-t0 ))
  else record_command_failure "ssh:$t" $(( $(now)-t0 )) "$out"; fi
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
if ok and len(sys.argv) == 10:
    ready, resume, next_line, next_token = sys.argv[6:10]
    with open(ready, "w") as receipt:
        receipt.write("ready")
    deadline = time.monotonic() + 600
    while not os.path.exists(resume) and time.monotonic() < deadline:
        time.sleep(0.1)
    ok = os.path.exists(resume)
    if ok:
        buf = b""
        os.write(fd, (next_line + "\r").encode())
        ok = read_until(next_token.encode(), time.time() + 90)
os.write(fd, b"exit\r")
time.sleep(3)
try:
    os.kill(pid, signal.SIGTERM)
except OSError:
    pass
# Reap the owned child on success and failure; do not expose terminal contents.
try:
    deadline = time.monotonic() + 3
    while os.waitpid(pid, os.WNOHANG)[0] == 0:
        if time.monotonic() >= deadline:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
            break
        time.sleep(0.05)
finally:
    os.close(fd)
print("TERMINAL_OK" if ok else "TERMINAL_FAIL")
sys.exit(0 if ok else 1)
PY
)
DRIVER_UPLOADED=0
cleanup_terminal() { # cleanup_terminal TARGET EXACT-NAME CHECK-ID
  local t=$1 name=$2 id=$3 t0 out status
  t0=$(now)
  if ! on "$CLIENT" "mkdir -p /tmp/$RUN_ID && cat > /tmp/$RUN_ID/session_cleanup.py" < "$HERE/session_cleanup.py"; then
    record "$id:$t" FAIL 0 "could not upload exact session cleanup helper"
    return
  fi
  out=$(on "$CLIENT" "python3 /tmp/$RUN_ID/session_cleanup.py $PB_HP $t $name" 2>&1); status=$?
  if [ "$status" = 0 ] && marker_present "$out" SESSION_CLEANUP_OK; then
    record "$id:$t" PASS $(( $(now)-t0 ))
  else
    record "$id:$t" FAIL $(( $(now)-t0 )) "exact terminal session cleanup was not acknowledged; output withheld"
  fi
}

test_terminal() { # test_terminal TARGET [ID-PREFIX]
  local t=$1 prefix=${2:-terminal} t0 out status marker="tm$RUN_ID$RANDOM" line
  if [ $DRIVER_UPLOADED = 0 ]; then
    printf '%s\n' "$TERM_DRIVER" | on "$CLIENT" "mkdir -p /tmp/$RUN_ID && cat > /tmp/$RUN_ID/term.py" && DRIVER_UPLOADED=1
  fi
  # POSIX shells and PowerShell both print MARKER-42 for these lines.
  if [ "$t" = victus ]; then line="echo $marker-\$(6*7)"; else line="echo $marker-\$((6*7))"; fi
  t0=$(now)
  out=$(on "$CLIENT" "python3 /tmp/$RUN_ID/term.py $PB_HP $t smoke-$marker '$line' $marker-42" 2>&1); status=$?
  if [ "$status" = 0 ] && marker_present "$out" TERMINAL_OK; then record "$prefix:$t" PASS $(( $(now)-t0 ))
  else record "$prefix:$t" FAIL $(( $(now)-t0 )) "terminal or transport failed; output withheld"; fi
  cleanup_terminal "$t" "smoke-$marker" "cleanup-$prefix"
}

test_transfer() {
  local t=$1
  local name="smoke-$RUN_ID-$t.bin" t0 src_sha out dst_sha status
  t0=$(now)
  src_sha=$(on "$CLIENT" "mkdir -p /tmp/$RUN_ID && head -c 52428800 /dev/urandom > /tmp/$RUN_ID/$name && sha256sum /tmp/$RUN_ID/$name | cut -d' ' -f1")
  out=$(on "$CLIENT" "timeout 900 $PB_HP send /tmp/$RUN_ID/$name --to $t; echo rc=\$?" 2>&1); status=$?
  if ! command_ok "$status" "$out"; then record_command_failure "transfer:$t" $(( $(now)-t0 )) "$out"; return; fi
  case "$t" in
    victus) dst_sha=$(on victus "\$f = Get-ChildItem -Path \$HOME -Recurse -Filter '$name' -ErrorAction SilentlyContinue -Depth 3 | Select-Object -First 1; if (\$f) { (Get-FileHash \$f.FullName -Algorithm SHA256).Hash.ToLower(); Remove-Item \$f.FullName }" | tr -d '\r') ;;
    apple) dst_sha=$(on apple "f=\$(find ~ -maxdepth 3 -name '$name' 2>/dev/null | head -1); [ -n \"\$f\" ] && shasum -a 256 \"\$f\" | cut -d' ' -f1 && rm -f \"\$f\"") ;;
    *) dst_sha=$(on "$t" "f=\$(find ~ -maxdepth 3 -name '$name' -not -path '/tmp/*' 2>/dev/null | grep -v '^/tmp' | head -1); [ -n \"\$f\" ] && sha256sum \"\$f\" | cut -d' ' -f1 && rm -f \"\$f\"") ;;
  esac
  if [ -n "$dst_sha" ] && [ "$dst_sha" = "$src_sha" ]; then record "transfer:$t" PASS $(( $(now)-t0 ))
  else record "transfer:$t" FAIL $(( $(now)-t0 )) "received sha '${dst_sha:-missing}' != source"; fi
}

test_preview() { # on a Unix device: serve a marker on loopback, publish, fetch from the workspace
  local d=$1 port=$(( 18000 + RANDOM % 1000 )) marker="pv-$RUN_ID" t0 out url id status stopped
  t0=$(now)
  out=$(on "$d" "mkdir -p /tmp/$RUN_ID-pv && cd /tmp/$RUN_ID-pv && echo $marker > index.html && (nohup python3 -m http.server $port --bind 127.0.0.1 >/dev/null 2>&1 & echo \$! > srv.pid) && sleep 1 && \
    $( [ "$d" = apple ] && echo "$PB_APPLE" || echo "$PB_HP" ) preview $port --background --ttl 10m --json" 2>&1)
  status=$?
  id=$(python3 "$HERE/preview_receipt.py" id <<<"$out" 2>/dev/null)
  url=$(python3 "$HERE/preview_receipt.py" endpoint <<<"$out" 2>/dev/null)
  if [ "$status" != 0 ] || [ -z "$id" ] || [ -z "$url" ]; then record "preview:$d" FAIL $(( $(now)-t0 )) "preview creation failed or returned no canonical resource; output withheld"
  else
    local body=""
    for _ in $(seq 1 12); do body=$(curl -fsS --max-time 10 "$url" 2>/dev/null) && grep -q "$marker" <<<"$body" && break; sleep 5; done
    if grep -q "$marker" <<<"$body"; then record "preview:$d" PASS $(( $(now)-t0 ))
    else record "preview:$d" FAIL $(( $(now)-t0 )) "URL did not serve marker"; fi
  fi
  if [ -n "$id" ]; then
    stopped=$(pb_on "$d" preview stop "$id" --json 2>/dev/null)
    status=$?
    if [ "$status" = 0 ] && python3 "$HERE/preview_receipt.py" stopped "$id" <<<"$stopped" >/dev/null 2>&1; then
      record "cleanup-preview:$d" PASS $(( $(now)-t0 ))
    else
      record "cleanup-preview:$d" FAIL $(( $(now)-t0 )) "exact preview stop was not acknowledged; output withheld"
    fi
  else
    record "cleanup-preview:$d" FAIL $(( $(now)-t0 )) "could not identify the created preview for cleanup"
  fi
  on "$d" "kill \$(cat /tmp/$RUN_ID-pv/srv.pid) 2>/dev/null; rm -rf /tmp/$RUN_ID-pv" >/dev/null 2>&1
}

start_update_terminal() {
  local t=$1
  local before="before-$RUN_ID-$t" after="after-$RUN_ID-$t" line next
  if [ "$DRIVER_UPLOADED" = 0 ]; then
    printf '%s\n' "$TERM_DRIVER" | on "$CLIENT" "mkdir -p /tmp/$RUN_ID && cat > /tmp/$RUN_ID/term.py" || return 1
    DRIVER_UPLOADED=1
  fi
  if [ "$t" = victus ]; then
    line="\$pbSmoke = \$PID; echo $before-\$(6*7)"
    next="if (\$pbSmoke -eq \$PID) { echo $after-\$(6*7) }"
  else
    line="pb_smoke=\$\$; echo $before-\$((6*7))"
    next="[ \"\$pb_smoke\" = \"\$\$\" ] && echo $after-\$((6*7))"
  fi
  printf '%s\n' "python3 /tmp/$RUN_ID/term.py $PB_HP $t smoke-update-$RUN_ID '$line' $before-42 /tmp/$RUN_ID/upd-term-$t.ready /tmp/$RUN_ID/upd-term-$t.resume '$next' $after-42" | device_job "$CLIENT" "upd-term-$t"
  local deadline=$(( $(now) + 120 ))
  while [ "$(now)" -lt "$deadline" ]; do
    on "$CLIENT" "test -f /tmp/$RUN_ID/upd-term-$t.ready" && return 0
    sleep 1
  done
  return 1
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
  # Verify shell identity before activation, then query that same PTY after it.
  if ! start_update_terminal "$t"; then
    record "during-update-terminal:$t" FAIL 120 "terminal did not become ready before update"
    on "$CLIENT" "touch /tmp/$RUN_ID/upd-term-$t.resume" >/dev/null
    collect "reap-update-terminal:$t" "upd-term-$t" TERMINAL_OK
    cleanup_terminal "$t" "smoke-update-$RUN_ID" cleanup-update-terminal
    return
  fi
  start_long_ssh "$t" "upd-ssh-$t" 240
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
  on "$CLIENT" "touch /tmp/$RUN_ID/upd-term-$t.resume" >/dev/null
  collect "during-update-terminal:$t" "upd-term-$t" TERMINAL_OK
  cleanup_terminal "$t" "smoke-update-$RUN_ID" cleanup-update-terminal
  collect "during-update-ssh:$t" "upd-ssh-$t" SSHFINAL 240
  test_terminal "$t" after-update-terminal
  test_exec "$t" after-update-exec
  collect "during-update-exec:$t" "upd-exec-$t" FINAL 240
}

# Each source gets one compressed fixture upload, shared by terminal checks.
TERMINAL_FIXTURES_READY=","
stage_terminal_fixtures() {
  local source=$1
  [[ "$TERMINAL_FIXTURES_READY" != *",$source,"* ]] || return 0
  if ! on "$source" "umask 077; mkdir -p /tmp/$RUN_ID; tar -xzf - -C /tmp/$RUN_ID" < "$PAPERBOAT_SMOKE_FIXTURES"; then
    return 1
  fi
  TERMINAL_FIXTURES_READY+="$source,"
}

# Main-path protocol checks run from each real client, without the long/stress
# checks when selected through --skip. Transfers require a different recipient.
test_application_pairs() {
  local kind=$1 script=$2 check=${3:-} source target id out status t0 arguments
  skipped "$kind" && return
  for source in "${TLIST[@]}"; do
    for target in "${TLIST[@]}"; do
      case "$kind" in
        transfer-multi|exec-repeat|ssh-tools|transfer-manage|exec-cancel|ssh-doctor|inbox-manage|transfer-resume|transfer-errors)
          [ "$source" != "$target" ] || continue ;;
        preview-manage|preview-inspect|send-self|preview-access) [ "$source" = "$target" ] || continue ;;
      esac
      id="$kind:$source-$target"
      skipped "$id" && continue
      case "$kind" in transfer-manage|transfer-resume)
        if same_ct_host "$source" "$target"; then
          record "$id" SKIP 0 "two containers on one Docker host: the transfer ends before the check can interrupt it; judged on two real hosts"
          continue
        fi ;;
      esac
      t0=$(now)
      if ! stage_terminal_fixtures "$source"; then
        record "$id" FAIL 0 "could not stage compressed application fixture"
        continue
      fi
      arguments="$target"
      [ -z "$check" ] || arguments+=" --check $check"
      out=$(on "$source" "python3 /tmp/$RUN_ID/$script $arguments" 2>&1); status=$?
      if [ "$status" = 0 ] && python3 -c '
import json,sys
r=json.loads(sys.argv[1]); assert r["status"]=="PASS" and r["target"]==sys.argv[2]
assert r["check"]==sys.argv[3] and r["enrollment_unchanged"] is True
' "$out" "$target" "$kind" 2>/dev/null; then
        record "$id" PASS $(( $(now)-t0 ))
      else
        record_command_failure "$id" $(( $(now)-t0 )) "$out"
      fi
    done
  done
}

# Normal behavior from each selected client to each target. Each run owns and
# cleans only its random non-default terminal, including assertion failures.
test_terminal_main_pairs() {
  local source target out status t0 monitor id staged
  for source in "${TLIST[@]}"; do
    staged=1
    stage_terminal_fixtures "$source" || staged=0
    for target in "${TLIST[@]}"; do
      id="terminal-main:$source-$target"
      skipped "$id" && continue
      if [ "$staged" = 0 ]; then
        record "$id" FAIL 0 "could not stage the compressed terminal fixture"
        continue
      fi
      monitor=htop
      [ "$target" != apple ] || monitor="${APPLE_HTOP:-/opt/homebrew/bin/htop}"
      monitor=$(python3 -c 'import shlex,sys; print(shlex.quote(sys.argv[1]))' "$monitor")
      t0=$(now)
      out=$(on "$source" "python3 /tmp/$RUN_ID/terminal_lifecycle.py $target --check all --monitor-path $monitor" 2>&1); status=$?
      if [ "$status" = 0 ] && python3 -c '
import json,sys
receipt=json.loads(sys.argv[1])
assert receipt["status"]=="PASS" and receipt["check"]=="terminal-all" and receipt["target"]==sys.argv[2]
assert set(receipt["phases"])==set("input_output unicode resize colour ctrl_c ctrl_z large_paste vim less htop ctrl_d exact_session_cleanup".split())
' "$out" "$target" 2>/dev/null; then
        record "$id" PASS $(( $(now)-t0 ))
      else
        record_command_failure "$id" $(( $(now)-t0 )) "$out"
      fi
    done
  done
}

# Public management commands and the local project-directory entry point.
test_terminal_commands() {
  local source target id out status t0 staged
  for source in "${TLIST[@]}"; do
    staged=1
    stage_terminal_fixtures "$source" || staged=0
    for target in "${TLIST[@]}"; do
      id="session-manage:$source-$target"
      skipped session-manage && continue
      skipped "$id" && continue
      t0=$(now)
      if [ "$staged" = 0 ]; then
        record "$id" FAIL 0 "could not stage the terminal fixture"
        continue
      fi
      out=$(on "$source" "python3 /tmp/$RUN_ID/session_lifecycle.py $target --check manage" 2>&1); status=$?
      if [ "$status" = 0 ] && python3 -c '
import json,sys
receipt=json.loads(sys.argv[1])
assert receipt["status"]=="PASS" and receipt["check"]=="session-manage" and receipt["target"]==sys.argv[2]
' "$out" "$target" 2>/dev/null; then
        record "$id" PASS $(( $(now)-t0 ))
      else
        record_command_failure "$id" $(( $(now)-t0 )) "$out"
      fi
    done
    id="terminal-new:$source"
    skipped terminal-new && continue
    skipped "$id" && continue
    t0=$(now)
    if [ "$staged" = 0 ]; then
      record "$id" FAIL 0 "could not stage the terminal fixture"
      continue
    fi
    out=$(on "$source" "python3 /tmp/$RUN_ID/terminal_lifecycle.py $source --check main --entry new" 2>&1); status=$?
    if [ "$status" = 0 ] && python3 -c '
import json,sys
receipt=json.loads(sys.argv[1])
assert receipt["status"]=="PASS" and receipt["check"]=="terminal-main"
assert receipt["entry"]=="new" and receipt["target"]==sys.argv[2]
' "$out" "$source" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 ))
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

# Menus, persistent status-bar settings, and owner-side grant lifecycle.
test_terminal_features() {
  local source target kind script id out status t0
  for source in "${TLIST[@]}"; do
    for kind in terminal-entry status-bar session-sharing-owner; do
      id="$kind:$source"
      skipped "$kind" && continue
      skipped "$id" && continue
      target=$source
      case "$kind" in
        terminal-entry) script=terminal_entry.py ;;
        status-bar) script=status_bar_lifecycle.py ;;
        session-sharing-owner) script=session_sharing_lifecycle.py ;;
      esac
      if [ "$kind" != session-sharing-owner ] && [[ ",$TARGETS," == *,$CLIENT,* && ",$TARGETS," == *,apple,* ]]; then
        [ "$source" != "$CLIENT" ] && target=$CLIENT || target=apple
      fi
      t0=$(now)
      if ! stage_terminal_fixtures "$source"; then
        record "$id" FAIL 0 "could not stage the terminal fixture"
        continue
      fi
      extra=""; [ "$kind" != session-sharing-owner ] || extra=" --team $SMOKE_TEAM"
      out=$(on "$source" "python3 /tmp/$RUN_ID/$script $target$extra" 2>&1); status=$?
      if [ "$status" = 0 ] && python3 -c '
import json,sys
receipt=json.loads(sys.argv[1].splitlines()[-1])
assert receipt["status"]=="PASS" and receipt["check"]==sys.argv[2] and receipt["target"]==sys.argv[3]
' "$out" "$kind" "$target" 2>/dev/null; then
        record "$id" PASS $(( $(now)-t0 ))
      else
        record_command_failure "$id" $(( $(now)-t0 )) "$out"
      fi
    done
  done
}

# The creator first reattaches locally; a separate enrolled device then proves
# replay and the same shell variable through its own authenticated CLI session.
test_terminal_replay() {
  local target out status t0 staged=1 id tag phase command controller_ok parent parent_status
  # The second enrolled device is apple when selected, else dadape (Linux pair).
  local controller=""
  for target in apple dadape "$PEER"; do [[ ",$TARGETS," == *,$target,* ]] && controller=$target; done
  [[ ",$TARGETS," == *,$CLIENT,* && -n "$controller" ]] || return 0
  stage_terminal_fixtures "$CLIENT" || staged=0
  stage_terminal_fixtures "$controller" || staged=0
  for target in "${TLIST[@]}"; do
    id="terminal-replay:$target"
    skipped "$id" && continue
    if [ "$staged" = 0 ]; then
      record "$id" FAIL 0 "could not stage the cross-device fixture"
      continue
    fi
    t0=$(now); tag="terminal-replay-$target"; controller_ok=0
    printf '%s\n' "python3 /tmp/$RUN_ID/session_lifecycle.py $target --check cross && echo TERMINAL_REPLAY_OK" | device_job "$CLIENT" "$tag"
    phase=$(on "$CLIENT" "python3 /tmp/$RUN_ID/session_controller.py wait --job /tmp/$RUN_ID/$tag" 2>&1); status=$?
    out="$phase"
    if [ "$status" = 0 ]; then
      command=$(python3 "$HERE/session_controller.py" command --phase "$phase" --script "/tmp/$RUN_ID/session_lifecycle.py")
      out=$(on "$controller" "$command" 2>&1); status=$?
      if [ "$status" = 0 ] && python3 -c '
import json,sys
receipt=json.loads(sys.argv[1])
assert receipt["check"]=="session-resume" and receipt["status"]=="PASS" and receipt["target"]==sys.argv[2]
assert receipt["same_shell"] is True and receipt["replayed_output"] is True
' "$out" "$target" 2>/dev/null; then
        controller_ok=1
      fi
      command=$(python3 "$HERE/session_controller.py" signal --phase "$phase" --success "$controller_ok")
      on "$CLIENT" "$command" >/dev/null 2>&1 || controller_ok=0
    fi
    parent=$(on "$CLIENT" "python3 /tmp/$RUN_ID/session_controller.py finish --job /tmp/$RUN_ID/$tag" 2>&1); parent_status=$?
    if [ "$controller_ok" = 1 ] && command_ok "$parent_status" "$parent" && marker_present "$parent" TERMINAL_REPLAY_OK; then
      record "$id" PASS $(( $(now)-t0 ))
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out
$parent"
    fi
  done
}


test_private_access() {
  local kind source target id tag phase output parent command success status t0
  for kind in access-machine private-address private-https; do
    skipped "$kind" && continue
    for source in "${TLIST[@]}"; do
      for target in "${TLIST[@]}"; do
        id="$kind:$source-$target"
        skipped "$id" && continue
        t0=$(now); tag="$kind-$source-$target"; success=0
        if ! stage_terminal_fixtures "$source" || ! stage_terminal_fixtures "$target"; then
          record "$id" FAIL 0 "could not stage compressed private TCP fixture"
          continue
        fi
        printf '%s\n' "python3 /tmp/$RUN_ID/private_access.py origin --target $target --check $kind && echo PRIVATE_ORIGIN_OK" | device_job "$target" "$tag"
        phase=$(on "$target" "python3 /tmp/$RUN_ID/session_controller.py wait --job /tmp/$RUN_ID/$tag --expected-state private_access_required" 2>&1); status=$?
        output="$phase"
        if [ "$status" = 0 ]; then
          command=$(python3 -c 'import shlex,sys;print(shlex.join(["python3",sys.argv[2],"probe","--phase",sys.argv[1],"--check",sys.argv[3]]))' "$phase" "/tmp/$RUN_ID/private_access.py" "$kind")
          output=$(on "$source" "$command" 2>&1); status=$?
          if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1]);assert r["status"]=="PASS" and r["check"]==sys.argv[2] and r["target"]==sys.argv[3] and r["exact_bytes"] and r["enrollment_unchanged"];assert r["trusted_https"] if sys.argv[2]=="private-https" else r["half_close"]' "$output" "$kind" "$target" 2>/dev/null; then success=1; fi
          command=$(python3 "$HERE/session_controller.py" signal --phase "$phase" --success "$success" --expected-state private_access_required)
          on "$target" "$command" >/dev/null 2>&1 || success=0
        fi
        parent=$(on "$target" "python3 /tmp/$RUN_ID/session_controller.py finish --job /tmp/$RUN_ID/$tag" 2>&1); status=$?
        if [ "$success" = 1 ] && command_ok "$status" "$parent" && marker_present "$parent" PRIVATE_ORIGIN_OK; then
          if [ "$kind" = private-https ]; then
            record "$id" PASS $(( $(now)-t0 )) "both private URL forms; OS-trusted TLS and normal DNS; exact response"
          else
            record "$id" PASS $(( $(now)-t0 )) "exact binary bytes; TCP half-close; origin and forward cleaned"
          fi
        else
          record_command_failure "$id" $(( $(now)-t0 )) "$output
$parent"
        fi
      done
    done
  done
}

test_inbox_approval() {
  skipped inbox-approval && return
  local device id tag source_id phase output parent command success status t0
  if ! stage_terminal_fixtures ubuntu24; then
    for device in "${TLIST[@]}"; do record "inbox-approval:$device" FAIL 0 "could not stage teammate source"; done
    return
  fi
  if ! source_id=$(on ubuntu24 "python3 -c 'import sys;sys.path.insert(0,\"/tmp/$RUN_ID\");from install_lifecycle import registration;print(registration()[\"machine_id\"])'") || ! [[ "$source_id" =~ ^[A-Za-z0-9_-]{3,128}$ ]]; then
    for device in "${TLIST[@]}"; do record "inbox-approval:$device" FAIL 0 "teammate source identity unavailable"; done
    return
  fi
  for device in "${TLIST[@]}"; do
    id="inbox-approval:$device"
    skipped "$id" && continue
    t0=$(now); tag="inbox-approval-$device"; success=0
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage recipient fixture"
      continue
    fi
    printf '%s\n' "python3 /tmp/$RUN_ID/inbox_approval.py --source-machine-id $source_id && echo INBOX_APPROVAL_OK" | device_job "$device" "$tag"
    phase=$(on "$device" "python3 /tmp/$RUN_ID/session_controller.py wait --job /tmp/$RUN_ID/$tag --expected-state inbox_sender_required" 2>&1); status=$?
    output="$phase"
    if [ "$status" = 0 ]; then
      command=$(python3 -c 'import shlex,sys;print(shlex.join(["python3",sys.argv[2],"--phase",sys.argv[1]]))' "$phase" "/tmp/$RUN_ID/inbox_sender.py")
      output=$(on ubuntu24 "timeout 400 $command" 2>&1); status=$?
      if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1]);assert r["check"]=="inbox-sender" and r["status"]=="PASS" and r["approval_acknowledged"] and r["decline_acknowledged"]' "$output" 2>/dev/null; then success=1; fi
      command=$(python3 "$HERE/session_controller.py" signal --phase "$phase" --success "$success" --expected-state inbox_sender_required)
      on "$device" "$command" >/dev/null 2>&1 || success=0
    fi
    parent=$(on "$device" "python3 /tmp/$RUN_ID/session_controller.py finish --job /tmp/$RUN_ID/$tag" 2>&1); status=$?
    if [ "$success" = 1 ] && command_ok "$status" "$parent" && marker_present "$parent" INBOX_APPROVAL_OK; then
      record "$id" PASS $(( $(now)-t0 )) "independent teammate; approve delivers exact bytes; decline publishes nothing"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$output
$parent"
    fi
  done
}

test_local_access_settings() {
  skipped local-access-settings && return
  local device id out status t0 password
  for device in "${TLIST[@]}"; do
    id="local-access-settings:$device"
    skipped "$id" && continue
    t0=$(now)
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage local settings fixture"
      continue
    fi
    password="${HP_PASS:-}"
    [ "$device" != apple ] || password="$APPLE_PASS"
    out=$(printf '%s\n' "$password" | on "$device" "python3 /tmp/$RUN_ID/local_access_settings.py" 2>&1); status=$?
    if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1]);assert r["check"]=="local-access-settings" and r["status"]=="PASS" and r["alias_proxy_apply_reset"] and r["enrollment_preserved"]' "$out" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 )) "alias, proxy, apply, domain set/reset; original settings restored"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

test_reset_lifecycle() {
  skipped reset-lifecycle && return
  local device id out status t0 password
  for device in "${TLIST[@]}"; do
    id="reset-lifecycle:$device"
    skipped "$id" && continue
    t0=$(now)
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage the reset fixture"
      continue
    fi
    password=""
    [ "$device" != apple ] || password="$APPLE_PASS"
    out=$(printf '%s\n' "$password" | on "$device" "python3 /tmp/$RUN_ID/reset_lifecycle.py" 2>&1); status=$?
    if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1]);assert r["check"]=="reset-lifecycle" and r["status"]=="PASS" and r["old_record_removed"] and r["local_address_kept"] and r["new_record_same_alias"] and r["inbox_preserved"]' "$out" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 )) "reset then pair: same name and local address, old record removed, Inbox kept"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

# Tunnel domains (C089, C100-C105, C123) need a DNS zone and its Cloudflare token: runs only when
# SMOKE_CF_TOKEN_FILE (a local file, never printed) and SMOKE_CF_ZONE are set. The token is copied to
# the device for the run and removed with it.
test_tunnel_domain() {
  skipped tunnel-domain && return
  [ -n "${SMOKE_CF_TOKEN_FILE:-}" ] && [ -n "${SMOKE_CF_ZONE:-}" ] || return 0
  local device id out status t0
  for device in "${TLIST[@]}"; do
    id="tunnel-domain:$device"
    skipped "$id" && continue
    t0=$(now)
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage the device fixture"
      continue
    fi
    on "$device" "umask 077; cat > /tmp/$RUN_ID/cf-token" < "$SMOKE_CF_TOKEN_FILE"
    out=$(on "$device" "python3 /tmp/$RUN_ID/tunnel_domain.py --cloudflare-token-file /tmp/$RUN_ID/cf-token --zone $SMOKE_CF_ZONE" 2>&1); status=$?
    on "$device" "rm -f /tmp/$RUN_ID/cf-token" >/dev/null 2>&1
    if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1].strip().splitlines()[-1]);assert r["check"]=="tunnel-domain" and r["status"]=="PASS" and r["domain_add_list_instructions_verify_remove"] and r["ephemeral_start_stop"] and r["served_over_https"] and r["enrollment_preserved"]' "$out" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 )) "C089 C100-C105 C123: domain add/list/instructions/verify/serve/remove, ephemeral start/stop"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

# Release loop: update N-1 -> N and N -> N-1 on one device, both ending ready with a matched guard version.
# Runs only when SMOKE_XBUILD_PREV / SMOKE_XBUILD_CUR (local pb files) and their reported versions
# SMOKE_XBUILD_PREV_VERSION / SMOKE_XBUILD_CUR_VERSION are set; it ends on the previous build, so run it last
# on a device that is allowed to move builds (a disposable container).
test_update_crossbuild() {
  skipped update-crossbuild && return
  [ -n "${SMOKE_XBUILD_PREV:-}" ] && [ -n "${SMOKE_XBUILD_CUR:-}" ] || return 0
  local device id out status t0
  for device in "${TLIST[@]}"; do
    id="update-crossbuild:$device"
    skipped "$id" && continue
    t0=$(now)
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage the device fixture"
      continue
    fi
    on "$device" "umask 077; cat > /tmp/$RUN_ID/xbuild-prev; chmod 755 /tmp/$RUN_ID/xbuild-prev" < "$SMOKE_XBUILD_PREV"
    on "$device" "umask 077; cat > /tmp/$RUN_ID/xbuild-cur; chmod 755 /tmp/$RUN_ID/xbuild-cur" < "$SMOKE_XBUILD_CUR"
    out=$(on "$device" "cd /tmp/$RUN_ID && python3 update_crossbuild.py --previous /tmp/$RUN_ID/xbuild-prev --previous-version ${SMOKE_XBUILD_PREV_VERSION:?} --current /tmp/$RUN_ID/xbuild-cur --current-version ${SMOKE_XBUILD_CUR_VERSION:?} ${SMOKE_XBUILD_PING:+--ping-target $SMOKE_XBUILD_PING}" 2>&1); status=$?
    if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1].strip().splitlines()[-1]);assert r["check"]=="update-crossbuild" and r["status"]=="PASS" and r["guard_version_matched"] and r["enrollment_preserved"]' "$out" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 )) "update $SMOKE_XBUILD_PREV_VERSION -> $SMOKE_XBUILD_CUR_VERSION -> $SMOKE_XBUILD_PREV_VERSION: ready both ways, guard version matched"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

# F10-01: set/list/unset of personal values and per-machine overrides reach new execs and terminals, never logged.
# Runs only when SMOKE_ENV_VAULT_FILE (an owner-only file with the ENV vault password) is set; the target is the
# next device of the run (a second machine of the same account), so a run needs two devices.
test_env_scopes() {
  skipped env-scopes && return
  [ -n "${SMOKE_ENV_VAULT_FILE:-}" ] || return 0
  local device id out status t0 target index=0
  for device in "${TLIST[@]}"; do
    id="env-scopes:$device"
    index=$((index+1))
    skipped "$id" && continue
    target=${TLIST[$((index % ${#TLIST[@]}))]}
    t0=$(now)
    if [ "$target" = "$device" ]; then
      record "$id" FAIL 0 "needs a second machine of the account as the target; run with two devices"
      continue
    fi
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage the device fixture"
      continue
    fi
    on "$device" "umask 077; cat > /tmp/$RUN_ID/env-vault.pw" < "$SMOKE_ENV_VAULT_FILE"
    out=$(on "$device" "cd /tmp/$RUN_ID && python3 env_scopes.py $target --password-file /tmp/$RUN_ID/env-vault.pw; rc=\$?; rm -f /tmp/$RUN_ID/env-vault.pw; exit \$rc" 2>&1); status=$?
    if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1].strip().splitlines()[-1]);assert r["check"]=="env-scopes" and r["status"]=="PASS" and r["personal_in_exec_and_terminal"] and r["override_isolated_to_machine"] and r["unset_restores_and_removes"] and r["never_in_output_or_diagnostics"]' "$out" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 )) "personal and per-machine values in new exec and terminal on $target, override not leaked, unset removes, never in output or diagnostics"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

# F14-06: an official install updates itself at the configured time. Runs only when SMOKE_SCHEDULED_UPDATE=1 and
# the device is a paired official install older than the newest official release (it takes about 10 minutes).
test_update_scheduled() {
  skipped update-scheduled && return
  [ "${SMOKE_SCHEDULED_UPDATE:-}" = 1 ] || return 0
  local device id out status t0
  for device in "${TLIST[@]}"; do
    id="update-scheduled:$device"
    skipped "$id" && continue
    t0=$(now)
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage the device fixture"
      continue
    fi
    out=$(on "$device" "cd /tmp/$RUN_ID && python3 update_scheduled.py" 2>&1); status=$?
    if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1].strip().splitlines()[-1]);assert r["check"]=="update-scheduled" and r["status"]=="PASS" and r["disabled_stayed_put"] and r["settings_restored"]' "$out" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 )) "disabled stayed put; at the configured time the machine reached $(python3 -c 'import json,sys;print(json.loads(sys.argv[1].strip().splitlines()[-1])["to"])' "$out") by itself; doctor healthy; settings restored"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

# F14-07: a minimum supported version blocks an old official build, never a custom build. Runs against STAGING only (never
# production) and only when SMOKE_STAGING_FLOOR_CRED (the staging operator credential file), SMOKE_STAGING_OFFICIAL and
# SMOKE_STAGING_CUSTOM (two staging machines: an official build and a dev build, containers on the staging docker host)
# are set. It runs on this host, not on a device; the floor is cleared on every exit path.
test_update_floor() {
  skipped update-floor && return
  [ -n "${SMOKE_STAGING_FLOOR_CRED:-}" ] && [ -n "${SMOKE_STAGING_OFFICIAL:-}" ] && [ -n "${SMOKE_STAGING_CUSTOM:-}" ] || return 0
  local id=update-floor t0 out status
  t0=$(now)
  out=$(${SMOKE_UPDATE_FLOOR_CMD:-python3 "$HERE/update_floor.py"} --cred "$SMOKE_STAGING_FLOOR_CRED" --official "$SMOKE_STAGING_OFFICIAL" --custom "$SMOKE_STAGING_CUSTOM" ${SMOKE_STAGING_FLOOR:+--floor $SMOKE_STAGING_FLOOR} 2>&1); status=$?
  if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1].strip().splitlines()[-1]);assert r["check"]=="update-floor" and r["status"]=="PASS" and r["custom_unaffected"] and r["floor_cleared"] and len(r["refused"])==3' "$out" 2>/dev/null; then
    record "$id" PASS $(( $(now)-t0 )) "staging floor: old official build refused (machine list, session list, exec) with the update message, custom build unaffected, floor cleared and the official build works again"
  else
    record_command_failure "$id" $(( $(now)-t0 )) "$out"
  fi
}

# Runs only when SMOKE_FRESH_HOST (a Linux VM with Docker, image laneh-ubuntu24-systemd, and a signed-in `pb` user)
# and SMOKE_FRESH_BINARY (the linux-amd64 build under test) are set: a FRESH pair, then `pb access machine` to it
# with no daemon restart (the private-access P0 of 2026-10-26 showed only on freshly paired machines).
test_fresh_pair_access() {
  skipped fresh-pair-access && return
  [ -n "${SMOKE_FRESH_HOST:-}" ] && [ -n "${SMOKE_FRESH_BINARY:-}" ] || return 0
  local out status t0
  t0=$(now)
  out=$("$HERE/fresh_pair_access.sh" "$SMOKE_FRESH_HOST" "$SMOKE_FRESH_BINARY" 2>&1); status=$?
  if [ "$status" = 0 ] && python3 -c 'import json,sys;r=json.loads(sys.argv[1].strip().splitlines()[-1]);assert r["check"]=="fresh-pair-access" and r["status"]=="PASS"' "$out" 2>/dev/null; then
    record fresh-pair-access PASS $(( $(now)-t0 )) "fresh pair on $SMOKE_FRESH_HOST, then pb access machine reached the origin"
  else
    record_command_failure fresh-pair-access $(( $(now)-t0 )) "$out"
  fi
}

# team-lifecycle / team-roles need a controlled teammate profile (a second signed-in account) on the device that
# runs them. A disposable container can never hold one (it would need that account's sign-in), so they never run
# there. In the lab ubuntu24 holds the profile and its own account is the fixture team's independent teammate: it
# always runs both checks in the member role (ids team-lifecycle:ubuntu24, team-roles:ubuntu24), and so does every
# non-container device in the run (owner role). A run therefore always covers them, without a hand judgement.
test_team_checks() {
  local spec group script flags device devices
  for spec in "team-lifecycle team_lifecycle.py" "team-roles team_lifecycle.py --roles"; do
    set -- $spec; group=$1; script=$2; flags=${3:-}
    devices=()
    for device in "${TLIST[@]}"; do ct_of "$device" >/dev/null || devices+=("$device"); done
    [[ " ${devices[*]:-} " == *" ubuntu24 "* ]] || devices+=(ubuntu24)
    test_device_application "$group" "$script" "$flags" "${devices[@]}"
  done
}

test_device_application() {
  local group=$1 script=$2 base_flags=${3:-} flags
  skipped "$group" && return
  shift 3 2>/dev/null || shift $#
  local devices=("$@") device id out status t0
  [ ${#devices[@]} -gt 0 ] || devices=("${TLIST[@]}")
  for device in "${devices[@]}"; do
    flags=$base_flags
    id="$group:$device"
    skipped "$id" && continue
    t0=$(now)
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage the device fixture"
      continue
    fi
    case "$group" in
      team-lifecycle|team-roles)
        flags+=" --team $SMOKE_TEAM"
        [ "$device" != ubuntu24 ] || flags+=" --device-role member" ;;
      doctor-lifecycle|session-limit) flags=$device ;;
    esac
    out=$(on "$device" "python3 /tmp/$RUN_ID/$script $flags" 2>&1); status=$?
    if [ "$status" = 0 ] && python3 -c '
import json,sys
r=json.loads(sys.argv[1])
assert r["check"]==sys.argv[2] and r["status"]=="PASS"
assert r.get("enrollment_preserved") or sys.argv[2] in ("doctor-lifecycle", "session-limit")
if sys.argv[2] in ("team-lifecycle", "team-roles"):
 assert all(r[k] for k in ("create_get_list_delete", "invite_accept_decline_cancel", "independent_verified_recipient"))
 assert sys.argv[2]!="team-roles" or r["roles_remove_leave_transfer"]
elif sys.argv[2]=="doctor-lifecycle":
 assert all(r[k] for k in ("healthy_requires_terminal_and_exec", "failure_explained", "repair_recovers"))
elif sys.argv[2]=="session-limit":
 assert r["opened"]==25 and r["closed_not_counted"] and r["reopened_after_close"]==5
elif sys.argv[2]=="tunnel-transfer":
 assert r["upload_16mib"] and r["download_16mib"]
elif sys.argv[2]=="tunnel-survival":
 assert all(r[k] for k in ("pause_resume", "daemon_restart", "update", "host_service_restart"))
elif sys.argv[2]=="preview-survival":
 assert all(r[k] for k in ("daemon_restart", "origin_unavailable", "origin_recovers", "stop_ends")) and r["host_restart"] in ("survived", "not_run")
elif sys.argv[2]=="tunnel-failures":
 assert r["origin_stopped"] and r["host_stopped"]
elif sys.argv[2]=="network-inventory":
 assert r["edges_and_verified_relays"]
elif sys.argv[2]=="tunnel-lifecycle":
 assert all(r[k] for k in ("status_doctor_logs", "capture_replay", "pause_resume", "port_policy"))
elif sys.argv[2]=="config-sync":
 assert all(r[k] for k in ("repository_connect_list", "validate_apply", "status_assignment", "force_pull", "unassign"))
elif sys.argv[2]=="config-survival":
 assert all(r[k] for k in ("config_set_survives_update", "customization_survives_update", "reset_restores"))
elif sys.argv[2]=="approve-refusals":
 assert r["refusals"] == ["machine_not_found", "approval_not_pending"]
elif sys.argv[2]=="tunnel-manage":
 assert all(r[k] for k in ("create_show_list_delete", "route_add_update_remove", "rename"))
elif sys.argv[2]=="tunnel-public-protocols":
 assert r["public_tcp_echo"] and r["tls_passthrough_origin_certificate"]
else:
 raise AssertionError("unknown device application")
' "$out" "$group" 2>/dev/null; then
      record "$id" PASS $(( $(now)-t0 ))
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$out"
    fi
  done
}

# The origin stays on its enrolled device; TLS/ALPN/body/WebSocket probes run
# here, independently of that device's network and client credentials.
test_preview_protocols() {
  local kind=${1:-preview} flag=""
  [ "$kind" = tunnel ] && flag=" --tunnel"
  skipped $kind-protocols && return
  local probe_root probe device id tag phase url marker output parent status success command t0
  probe_root=$(mktemp -d)
  probe="$probe_root/protocol-client"
  if ! (cd "$HERE/../.." && GOMAXPROCS=2 GOFLAGS=-p=2 go build -o "$probe" "$HERE/preview_protocol_client.go") >"$probe_root/build.log" 2>&1; then
    for device in "${TLIST[@]}"; do record_command_failure "preview-protocols:$device" 0 "$(cat "$probe_root/build.log")"; done
    rm -rf "$probe_root"
    return
  fi
  for device in "${TLIST[@]}"; do
    id="$kind-protocols:$device"
    skipped "$id" && continue
    t0=$(now); tag="$kind-protocols-$device"; success=0; output=""
    if ! stage_terminal_fixtures "$device"; then
      record "$id" FAIL 0 "could not stage compressed protocol fixture"
      continue
    fi
    printf '%s\n' "python3 /tmp/$RUN_ID/preview_protocols.py $device$flag && echo PREVIEW_PROTOCOLS_OK" | device_job "$device" "$tag"
    phase=$(on "$device" "python3 /tmp/$RUN_ID/session_controller.py wait --job /tmp/$RUN_ID/$tag --expected-state preview_protocol_required" 2>&1); status=$?
    output="$phase"
    if [ "$status" = 0 ]; then
      url=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["url"])' "$phase")
      marker=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["marker"])' "$phase")
      output=$("$probe" --url "$url" --marker "$marker" 2>&1); status=$?
      if [ "$status" = 0 ]; then success=1; fi
      command=$(python3 "$HERE/session_controller.py" signal --phase "$phase" --success "$success" --expected-state preview_protocol_required)
      on "$device" "$command" >/dev/null 2>&1 || success=0
    fi
    parent=$(on "$device" "python3 /tmp/$RUN_ID/session_controller.py finish --job /tmp/$RUN_ID/$tag" 2>&1); status=$?
    if [ "$success" = 1 ] && command_ok "$status" "$parent" && marker_present "$parent" PREVIEW_PROTOCOLS_OK; then
      record "$id" PASS $(( $(now)-t0 )) "HTTP1/2/3, WebSocket binary/Unicode,16MiB body; exact cleanup"
    else
      record_command_failure "$id" $(( $(now)-t0 )) "$output
$parent"
    fi
  done
  rm -rf "$probe_root"
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
test_terminal_commands
test_terminal_features
skipped terminal-main || test_terminal_main_pairs
skipped terminal-replay || test_terminal_replay
test_application_pairs exec-main exec_lifecycle.py exec
test_application_pairs ssh-main ssh_lifecycle.py
test_application_pairs ssh-tools ssh_tools_lifecycle.py
test_application_pairs transfer-multi transfer_lifecycle.py
test_application_pairs transfer-resume transfer_resume.py
test_application_pairs transfer-errors transfer_errors.py
test_application_pairs exec-repeat exec_lifecycle.py repeated
test_application_pairs readiness-main exec_lifecycle.py readiness
test_application_pairs exec-cancel exec_cancel.py
test_application_pairs ssh-doctor ssh_doctor_lifecycle.py
test_application_pairs ssh-alias ssh_alias_lifecycle.py
test_application_pairs inventory-freshness inventory_freshness.py
test_application_pairs inbox-manage inbox_manage.py
test_device_application update-observation update_observation.py
test_inbox_approval
test_private_access
test_fresh_pair_access
test_application_pairs transfer-manage transfer_manage.py
test_application_pairs preview-manage preview_manage.py
test_application_pairs preview-inspect preview_inspect.py
test_application_pairs send-self send_self.py
test_application_pairs preview-access preview_access.py
test_team_checks
test_device_application tunnel-manage tunnel_manage.py
test_device_application tunnel-public-protocols tunnel_public_protocols.py
test_device_application approve-refusals approve_refusals.py
test_tunnel_domain
test_device_application config-sync config_sync.py
test_device_application config-survival config_survival.py
test_reset_lifecycle
test_device_application tunnel-lifecycle tunnel_lifecycle.py
test_device_application tunnel-survival tunnel_survival.py
test_device_application tunnel-failures tunnel_failures.py
test_device_application preview-survival preview_survival.py
test_device_application tunnel-transfer tunnel_transfer.py
test_local_access_settings
test_device_application doctor-lifecycle doctor_lifecycle.py
test_device_application session-limit session_limit.py
test_device_application network-inventory network_inventory.py
test_preview_protocols
test_preview_protocols tunnel
for t in "${TLIST[@]}"; do
  skipped ssh-long || collect "ssh-long:$t" "long-ssh-$t" SSHFINAL "$LONG_SSH"
  skipped exec-long || collect "exec-long:$t" "long-exec-$t" FINAL "$LONG_EXEC"
done

if [ -n "$UPDATE_DIR" ]; then
  for t in "${TLIST[@]}"; do update_target "$t"; done
fi
test_update_crossbuild
test_env_scopes
test_update_scheduled
test_update_floor

# ---------------------------------------------------------------- report
if [ -n "$ONLY_CHECK" ]; then
  found=0
  for row in "${ROWS[@]}"; do [ "${row%%|*}" != "$ONLY_CHECK" ] || found=1; done
  if [ "$found" = 0 ]; then
    record "$ONLY_CHECK" FAIL 0 "requested check did not run; a retired check needs its original runner"
  fi
fi
if [ -n "$ONLY_LIST" ]; then
  for listed in ${ONLY_IDS//|/ }; do
    found=0
    for row in "${ROWS[@]}"; do [ "${row%%|*}" != "$listed" ] || found=1; done
    [ "$found" = 1 ] || record "$listed" FAIL 0 "listed green check did not run; fix the runner or the list"
  done
fi
for source in "${TLIST[@]}"; do on "$source" "rm -rf /tmp/$RUN_ID" >/dev/null 2>&1; done
{
  printf '{"run":"%s","at":"%s","expect_version":"%s","update_version":"%s","results":[' "$RUN_ID" "$STAMP" "$EXPECT_VERSION" "$UPDATE_VERSION"
  first=1
  for row in "${ROWS[@]}"; do
    IFS='|' read -r id status secs detail failure_code <<<"$row"
    [ $first = 1 ] || printf ','
    first=0
    printf '{"id":"%s","status":"%s","seconds":"%s","detail":%s' "$id" "$status" "$secs" "$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$detail")"
    [ "$failure_code" != network_dns ] || printf ',"triage":"NETWORK","failure_code":"network_dns"'
    printf '}'
  done
  printf ']}\n'
} > "$RESULTS_DIR/$STAMP.json"
echo
pass=$(printf '%s\n' "${ROWS[@]}" | grep -c '|PASS|'); fail=$(printf '%s\n' "${ROWS[@]}" | grep -c '|FAIL|')
echo "PASS $pass  FAIL $fail   results: $RESULTS_DIR/$STAMP.json"
if [ "$FAILED" != 0 ]; then
  python3 "$HERE/last_green.py" "$RESULTS_DIR/$STAMP.json" || \
    echo "Last-green report unavailable; smoke failures still block the gate." >&2
fi
exit $FAILED
