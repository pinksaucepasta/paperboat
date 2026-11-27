#!/usr/bin/env bash
# Per-command rows (CHECKLIST C-rows) on L, M and W, and the cross-OS X rows, on one fleet of three real machines of one
# account that already run the release under test. One part per call; every script of a part runs even when an earlier
# one failed; the exit status is the number of failed scripts (0 = all passed).
#
#   commands_xos.sh PART          PART = commands | net | term | ssh | send | tunnel | names | config | env | removal
# config, env and removal need a fleet of a throwaway account (xos_fleet.py run BUILD_DIR -- commands_xos.sh PART, which
# also sets every variable below except CX_OUT and SMOKE_WINDOWS_PASSWORD); env resets the account's vault and removal
# revokes, resets and uninstalls its machines.
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

unix_command_rows() { # HOST MACHINE PEER [command_rows.py arguments]
  stage "$1" || return 1
  "${SSH[@]}" "$1" "cd /tmp/pbcx && echo unused | XDG_RUNTIME_DIR=/run/user/\$(id -u) PATH=\$HOME/.local/bin:\$PATH python3 command_rows.py $2 --peer $3 --sudo-password-stdin ${*:4}"
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
config) # X-01..X-10: one Git repository served by the Linux runner, reached by M and W through ssh -R
  port=$((20000 + RANDOM % 9000))
  # the controller dials the repository through its own forward to the Linux runner, which the Windows forward uses
  ssh -o BatchMode=yes -o ExitOnForwardFailure=yes -N -L "127.0.0.1:$port:127.0.0.1:$port" "$XPLAT_L_SSH" &
  forward=$!
  trap 'kill $forward 2>/dev/null' EXIT
  export XPLAT_REPO_HOST="$XPLAT_L_SSH" XPLAT_REPO_ADDR=127.0.0.1 XPLAT_REPO_BRIDGE=127.0.0.1 XPLAT_REPO_PORT=$port \
    XPLAT_REPO_ROOT=/tmp/cx-cfg-repo XPLAT_APP_DIR=xplat-cx XPLAT_REPO_NAME=xplat-config-cx
  step X-01-10-config-sync 3000 python3 xplat_config.py --devices L,M,W
  step config-first-sync 900 python3 xplat_config_firstsync.py --devices L,W
  ;;
env) # the ENV command rows with an unlocked vault, X-11, X-30, X-12, and last X-13 (it resets the vault)
  : "${XPLAT_VAULT_PW:?env runs on the fleet of a throwaway account, see xos_fleet.py}" "${SMOKE_WINDOWS_PASSWORD:?}"
  export -f stage unix_command_rows; export HERE; SSHQ="${SSH[*]}"; export SSHQ
  env_rows=C143,C144,C146,C150,C286
  step env-command-rows-L 600 bash -c "SSH=(\$SSHQ); unix_command_rows $XPLAT_L_SSH $XPLAT_L_NAME $XPLAT_M_NAME --only $env_rows --require-vault"
  step env-command-rows-M 600 bash -c "SSH=(\$SSHQ); unix_command_rows $XPLAT_MAC_SSH $XPLAT_M_NAME $XPLAT_L_NAME --only $env_rows --require-vault"
  step env-command-rows-W 900 env PB_SMOKE_WINDOWS_PB="$(windows_pb)" python3 command_rows.py "$XPLAT_W_NAME" --peer "$XPLAT_L_NAME" --only $env_rows --require-vault
  step X-11-env-delivery 1500 python3 xplat_env.py --devices L,M,W
  step X-11-env-in-connect 900 python3 xplat_term.py --devices L,M,W --only connect
  step X-30-env-plain-ssh 900 python3 xpc_plainssh.py --devices L,M,W --only env,hook
  step X-12-env-scopes 2400 python3 xplat_envscopes.py --devices L,M,W
  step X-13-env-vault 2400 python3 xplat_vault.py --devices L,M,W
  ;;
removal) # X-41; every script enrolls the machines it removes again
  step X-41-removal 3000 python3 xplat_removal.py
  step X-41-tunnel-preview-exec 1800 python3 xplat_removal_extras.py L,M,W
  step X-41-local-names 1800 python3 xplat_removal_names.py
  step X-41-env 2400 python3 xplat_removal_env.py
  step X-41-config-sync 2400 env XPLAT_REPO_HOST="$XPLAT_L_SSH" python3 xplat_removal_config.py
  ;;
*) echo "unknown part $part" >&2; exit 2 ;;
esac
if [ $failed = 0 ]; then echo "PASS commands-xos $part"; else echo "FAIL commands-xos $part: ${names[*]}"; fi
exit $failed
