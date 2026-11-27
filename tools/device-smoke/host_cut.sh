#!/usr/bin/env bash
# SMOKE_PEER_CUT for a peer that is a real host (dadape): cuts only the Paperboat services of one user there, so the
# host's other services and the controller's SSH keep working. container_cut.sh is the same contract for a container.
#
#   host_cut.sh SSH_HOST UID check                nothing is changed: nft works and every pb of UID is in a unit to cut
#   host_cut.sh SSH_HOST UID add all              every packet of those units except loopback, both directions
#   host_cut.sh SSH_HOST UID add server IP...     their outgoing packets to those addresses
#   host_cut.sh SSH_HOST UID add addresses IP...  their packets to and from those addresses
#   host_cut.sh SSH_HOST UID remove
#
# The units are the system units paperboat-*uUID* and the user's paperboatd.service, matched by cgroup in one nftables
# table (`inet pbcut`), added and removed atomically; a root timer on the host removes it after PB_CUT_FAILSAFE
# seconds (900) whatever happens to the controller. `remove` prints `sent=N ... dropped=M`: sent counts packets of
# those units that passed the output drop. A unit that restarted during the cut got a new cgroup the rules do not
# match, and a pb of UID outside the units was never cut: both fail the removal.
#
# Where sudo wants a password, PB_CUT_SUDO_FILE names a private file on the controller with it on the first line. It goes
# over ssh's stdin to the host's sudo only: never an argument, never printed.
set -euo pipefail
host=${1:?ssh host} uid=${2:?uid} action=${3:?check, add or remove}; shift 3
[[ $uid =~ ^[0-9]+$ ]] || { echo "UID must be a number" >&2; exit 2; }
mode=""
case $action in
check | remove) [ $# = 0 ] || { echo "$action takes nothing more" >&2; exit 2; } ;;
add)
  mode=${1:?all, server or addresses}; shift
  case $mode in all) [ $# = 0 ] ;; server | addresses) [ $# -gt 0 ] ;; *) false ;; esac ||
    { echo "usage: add all | add server IP... | add addresses IP..." >&2; exit 2; }
  for ip; do [[ $ip =~ ^[0-9a-fA-F:.]+$ ]] || { echo "not an IP address: $ip" >&2; exit 2; }; done ;;
*) echo "usage: host_cut.sh SSH_HOST UID check|add ...|remove" >&2; exit 2 ;;
esac
failsafe=${PB_CUT_FAILSAFE:-900}
[[ $failsafe =~ ^[0-9]+$ ]] || { echo "PB_CUT_FAILSAFE must be seconds" >&2; exit 2; }
password=""
if [ -n "${PB_CUT_SUDO_FILE:-}" ]; then
  [ -r "$PB_CUT_SUDO_FILE" ] || { echo "PB_CUT_SUDO_FILE cannot be read" >&2; exit 2; }
  IFS= read -r password <"$PB_CUT_SUDO_FILE" || true
fi
script=$(cat <<'REMOTE'
set -euo pipefail
PATH=/usr/sbin:/usr/bin:/sbin:/bin
uid=$1 action=$2 mode=${3:-}; shift $(($# < 3 ? $# : 3))
state=/run/pbcut.units

units() { # one line a unit: cgroup path (no leading slash) and the cgroup directory's inode
  local group
  {
    for unit in $(systemctl list-units --no-legend --plain --state=active "paperboat-*u$uid.service" "paperboat-*u$uid-*.service" | awk '{print $1}'); do
      systemctl show -p ControlGroup --value "$unit"
    done
    { find "/sys/fs/cgroup/user.slice/user-$uid.slice/user@$uid.service" -type d -name paperboatd.service 2>/dev/null || true; } |
      sed 's#^/sys/fs/cgroup##'
  } | while read -r group; do
    [ -z "$group" ] || echo "${group#/} $(stat -c %i "/sys/fs/cgroup$group")"
  done | sort
}

outside() { # pb processes of the user on the host itself whose cgroup is not one of $1's (a container is another machine)
  local pid group
  for pid in $(pgrep -x pb -u "$uid" || true); do
    group=$(sed -n 's#^0::/##p' "/proc/$pid/cgroup" 2>/dev/null) || continue
    case $group in *docker-*.scope | *docker-*.scope/* | docker/* | *lxc*) continue ;; esac
    [ -z "$group" ] || awk -v g="$group" '$1 == g || index(g, $1 "/") == 1 {f = 1} END {exit !f}' <<<"$1" || echo "$pid:$group"
  done
}

ready() {
  command -v nft >/dev/null || { echo "nft is not installed" >&2; exit 1; }
  now=$(units)
  [ -n "$now" ] || { echo "no active Paperboat unit of uid $uid on this host" >&2; exit 1; }
  stray=$(outside "$now")
  [ -z "$stray" ] || { echo "pb runs outside the units to cut: $stray" >&2; exit 1; }
}

case $action in
check)
  ready
  echo "ready: $(wc -l <<<"$now") units"
  ;;
add)
  ready
  ! nft list table inet pbcut >/dev/null 2>&1 || { echo "a cut is already in place (table inet pbcut)" >&2; exit 1; }
  rules() { # rules CHAIN-DIRECTION VERDICT
    local dir=$1 verdict=$2 group inode level match ip family side
    while read -r group inode; do
      level=$(awk -F/ '{print NF}' <<<"$group")
      match="socket cgroupv2 level $level \"$group\""
      if [ "$mode" = all ]; then
        [ "$dir" = in ] && echo "iifname != \"lo\" $match counter $verdict" || echo "oifname != \"lo\" $match counter $verdict"
      else
        for ip in "${addresses[@]}"; do
          case $ip in *:*) family=ip6 ;; *) family=ip ;; esac
          [ "$dir" = in ] && side=saddr || side=daddr
          echo "$family $side $ip $match counter $verdict"
        done
      fi
    done <<<"$now"
  }
  addresses=("$@")
  systemctl stop pbcut-failsafe.timer >/dev/null 2>&1 || true
  systemd-run --quiet --collect --unit=pbcut-failsafe --on-active="$PB_CUT_FAILSAFE" /usr/sbin/nft delete table inet pbcut ||
    { echo "the failsafe timer could not be set: not cut" >&2; exit 1; }
  {
    echo "table inet pbcut {"
    echo "chain out { type filter hook output priority 0;"; rules out drop; echo "}"
    if [ "$mode" != server ]; then echo "chain in { type filter hook input priority 0;"; rules in drop; echo "}"; fi
    echo "chain sent { type filter hook output priority 300;"; rules sent accept; echo "}"
    echo "}"
  } | nft -f -
  echo "$now" >"$state"
  ;;
remove)
  listing=$(nft list table inet pbcut) || { echo "not cut: no table inet pbcut (the failsafe may have removed it)" >&2; exit 1; }
  nft delete table inet pbcut # first: nothing below may leave the host cut
  now=$(units)
  stray=$(outside "$now")
  systemctl stop pbcut-failsafe.timer >/dev/null 2>&1 || true
  total() { awk -v chains=" $1 " '$1 == "chain" {on = index(chains, " " $2 " ")} on && match($0, /counter packets [0-9]+/) {n += substr($0, RSTART + 16, RLENGTH - 16)} END {print n + 0}' <<<"$listing"; }
  before=$(cat "$state"); rm -f "$state"
  [ "$now" = "$before" ] || { echo "not cut: a Paperboat unit started or restarted during the cut, outside the rules" >&2; exit 1; }
  [ -z "$stray" ] || { echo "not cut: pb ran outside the cut units: $stray" >&2; exit 1; }
  echo "cut of uid $uid's Paperboat units: sent=$(total sent) dropped=$(total 'out in')"
  ;;
esac
REMOTE
)
# line 1 of stdin is the password (or empty), the rest the script: the wrapper takes the line before sudo sees anything,
# so a sudo that asks for nothing can never hand it to bash as a command
run="env PB_CUT_FAILSAFE=$failsafe bash -c \"\$script\" host_cut $uid $action $mode $*"
wrapper="IFS= read -r pw; script=\$(cat); if sudo -n true 2>/dev/null; then exec sudo -n $run; fi; printf '%s\\n' \"\$pw\" | sudo -S -k -p '' $run"
{ printf '%s\n' "$password"; printf '%s\n' "$script"; } | ssh -o BatchMode=yes -o ConnectTimeout=15 "$host" "bash -c $(printf '%q' "$wrapper")"
