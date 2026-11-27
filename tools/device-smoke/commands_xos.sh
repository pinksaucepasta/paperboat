#!/usr/bin/env bash
# Per-command rows (CHECKLIST C-rows) on L, M and W, and the cross-OS X rows, on one fleet of three real machines of one
# account that already run the release under test. One part per call; every script of a part runs even when an earlier
# one failed; the exit status is the number of failed scripts (0 = all passed).
#
#   commands_xos.sh PART          PART = commands | net | term | ssh | send | tunnel | names
#
# Environment (xplat_fleet.py): XPLAT_L_SSH (a Linux runner with passwordless sudo; its signed-in pb is the controller),
# XPLAT_MAC_SSH, SMOKE_WINDOWS_SSH, XPLAT_L_NAME, XPLAT_M_NAME, XPLAT_W_NAME (their Paperboat machine names),
# CX_VERSION (the release on all three), CX_OUT (a directory for the logs), SMOKE_WINDOWS_PASSWORD (commands part).
# A part owns its three machines while it runs: net stops the Linux service for its offline check.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
part=${1:?part: commands|net|term|ssh|send|tunnel|names}
: "${XPLAT_L_SSH:?}" "${XPLAT_MAC_SSH:?}" "${SMOKE_WINDOWS_SSH:?}" "${XPLAT_L_NAME:?}" "${XPLAT_M_NAME:?}" "${XPLAT_W_NAME:?}" "${CX_VERSION:?}" "${CX_OUT:?}"
export XPLAT_L_HOST="$XPLAT_L_SSH" XPLAT_CONTROLLER=L XPLAT_PREFIX="${XPLAT_PREFIX:-cx}"
mkdir -p "$CX_OUT" && export SMOKE_RESULTS_DIR="$CX_OUT"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=30 -o ServerAliveInterval=30)
failed=0 names=()

step() { # NAME TIMEOUT command...
  local name=$1 limit=$2 rc=0; shift 2
  echo "--- $part/$name started $(date -u +%H:%M:%S)"
  (cd "$HERE" && timeout "$limit" "$@") >"$CX_OUT/$part-$name.log" 2>&1 || rc=$?
  if [ $rc = 0 ]; then echo "PASS $part/$name"; else
    echo "FAIL $part/$name (exit $rc) log $CX_OUT/$part-$name.log"; failed=$((failed + 1)); names+=("$name")
    grep -E '^FAIL|"status": "FAIL"|Traceback|Error:' "$CX_OUT/$part-$name.log" | cut -c1-300 | head -40
  fi
}

# GitHub's macOS runner images share one set of SSH host keys, which pb rightly refuses ("This machine was cloned with
# its SSH host keys"): such a runner gets keys of its own, and the controller forgets the old one for its tunnel port.
prepare_mac() {
  case $XPLAT_MAC_SSH in macpool-*) ;; *) return 0 ;; esac
  "${SSH[@]}" -n "$XPLAT_MAC_SSH" '$HOME/.local/bin/pb ssh doctor '"$XPLAT_M_NAME"' --json 2>&1; $HOME/.local/bin/pb status --json' |
    grep -q 'cloned with its SSH host keys' || return 0
  echo "$XPLAT_MAC_SSH shares its SSH host keys with another runner image: new keys"
  "${SSH[@]}" -n "$XPLAT_MAC_SSH" 'sudo -n sh -c "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A >/dev/null"' || return 1
  ssh-keygen -q -R "[127.0.0.1]:$((22300 + ${XPLAT_MAC_SSH##*-}))" -f "$HOME/.ssh/macpool_known_hosts" >/dev/null 2>&1
  sleep 75 # pb publishes the new keys within a minute
}

stage() { # the harness on a Unix device, in a private directory
  python3 "$HERE/stage_fixtures.py" "$HERE" | "${SSH[@]}" "$1" 'umask 077; rm -rf /tmp/pbcx && mkdir /tmp/pbcx && tar -xzf - -C /tmp/pbcx'
}

windows_pb() {
  local script='(Get-ChildItem "C:\Program Files\Paperboat\users\*\bin\pb.exe" | Select-Object -First 1).FullName'
  "${SSH[@]}" -n "$SMOKE_WINDOWS_SSH" "powershell -NoProfile -EncodedCommand $(printf '%s' "\$ProgressPreference='SilentlyContinue'; $script" | iconv -t UTF-16LE | base64 -w0)" 2>/dev/null | tr -d '\r' | grep 'pb\.exe$'
}

unix_command_rows() { # HOST MACHINE PEER
  stage "$1" || return 1
  "${SSH[@]}" "$1" "cd /tmp/pbcx && echo unused | XDG_RUNTIME_DIR=/run/user/\$(id -u) PATH=\$HOME/.local/bin:\$PATH python3 command_rows.py $2 --peer $3 --sudo-password-stdin"
}

case $part in
commands) # every C row on every OS (command_sweep.py), then the deeper main-use rows (command_rows.py)
  : "${SMOKE_WINDOWS_PASSWORD:?}"
  step sweep-L 900 python3 command_sweep.py --ssh "$XPLAT_L_SSH" --platform L --version "$CX_VERSION" --machine "$XPLAT_M_NAME" --out "$CX_OUT/sweep-L.json"
  step sweep-M 900 python3 command_sweep.py --ssh "$XPLAT_MAC_SSH" --platform M --version "$CX_VERSION" --machine "$XPLAT_L_NAME" --out "$CX_OUT/sweep-M.json"
  step sweep-W 900 python3 command_sweep.py --ssh "$SMOKE_WINDOWS_SSH" --platform W --version "$CX_VERSION" --machine "$XPLAT_L_NAME" --out "$CX_OUT/sweep-W.json"
  export -f stage unix_command_rows; export HERE; SSHQ="${SSH[*]}"; export SSHQ
  step rows-L 900 bash -c "SSH=(\$SSHQ); unix_command_rows $XPLAT_L_SSH $XPLAT_L_NAME $XPLAT_M_NAME"
  step rows-M 900 bash -c "SSH=(\$SSHQ); unix_command_rows $XPLAT_MAC_SSH $XPLAT_M_NAME $XPLAT_L_NAME"
  wpb=$(windows_pb)
  step rows-W 1500 env PB_SMOKE_WINDOWS_PB="$wpb" python3 command_rows.py "$XPLAT_W_NAME" --peer "$XPLAT_L_NAME"
  ;;
net) # X-19, X-17 (its offline check stops the Linux service, so it runs last), relay-data-path
  step X-19-private-access 1500 python3 xplat_access.py --devices L,M,W
  step relay-data-path 1200 python3 relay_data_path.py --devices L,M,W
  step X-17-exec 1500 python3 xplat_exec.py --devices L,M,W
  ;;
term) # X-16 (and the join leg of X-25), X-24 keys, closing a terminal
  step X-16-terminal 1500 python3 xplat_term.py --devices L,M,W --only shell,utf8,resize,ctrlc,ctrld,exit,replay,join
  step X-24-keys 1200 python3 xplat_keys.py --devices L,M,W
  step terminal-close 600 python3 xplat_term_close.py --devices L,M
  ;;
ssh) # X-18, X-29, X-28 (and the hook part of X-30)
  prepare_mac || echo "FAIL $part/prepare-mac"
  step X-18-ssh-family 1800 python3 xplat_ssh.py --devices L,M,W
  step X-29-forwarding 1500 python3 xpc_ssh.py --devices L,M,W
  step X-28-plain-ssh 1200 python3 xpc_plainssh.py --devices L,M,W --only config,ssh,hook,doctor
  ;;
send) # X-14, X-15 (and F6-07 folder sends)
  step X-14-15-send 2400 python3 xplat_send.py --devices L,M,W
  ;;
tunnel) # X-20 origins, C304 extend, C305 policy list
  step X-20-tunnel-origins 1500 python3 xplat_tunnel.py --devices L,M,W
  step C304-C305-extend-policy 1500 python3 xplat_tunnel_extend_policy.py --devices L,M,W
  ;;
names) # X-40
  step X-40-names 1500 python3 xplat_names.py --devices L,M,W
  ;;
*) echo "unknown part $part" >&2; exit 2 ;;
esac
if [ $failed = 0 ]; then echo "PASS commands-xos $part"; else echo "FAIL commands-xos $part: ${names[*]}"; fi
exit $failed
