#!/usr/bin/env bash
# Temporary Windows test machines on GitHub-hosted runners (.github/workflows/winpool.yml), see README.md.
#
#   winpool.sh start N [HOURS]   start N machines (HOURS 1-5, default 2), wait until SSH answers, print their targets
#   winpool.sh list              machines whose tunnel is up, and who started them
#   winpool.sh ssh N [COMMAND]   SSH into machine N
#   winpool.sh stop [N...]       stop machines you started (default: all of yours)
#
# Machine N is the SSH host winpool-N: administrator runneradmin, reached through the jump host's loopback port
# 22200+N with the key ~/.ssh/winpool_controller. Point the Windows checks at it with SMOKE_WINDOWS_SSH=winpool-N
# (and no VICTUS_PASS). "You" is your worktree (or WINPOOL_OWNER): every lane starts its machines from this one
# controller, which is where the start lock and the owner records live.
set -euo pipefail

JUMP=${WINPOOL_JUMP:-ubuntu@anvit}
SLOTS=(1 2 3 4 5 6 7 8 9 10 11 12 13 14)
CONF=$HOME/.ssh/winpool.conf
KNOWN=$HOME/.ssh/winpool_known_hosts
CLAIMS=$HOME/.ssh/winpool-claims # N: who started machine N
OWNER=${WINPOOL_OWNER:-$(git -C "$(dirname "$0")" rev-parse --show-toplevel)}

die() { echo "winpool: $*" >&2; exit 1; }

# Writes the winpool-N hosts and makes ~/.ssh/config include them: the checks pass only a host name to ssh.
configure() {
  [ -f "$HOME/.ssh/winpool_controller" ] || die "missing ~/.ssh/winpool_controller (its public half is the repository variable WINPOOL_CONTROLLER_PUBKEY)"
  {
    # Every runner is a new machine with a new host key: kept in its own file, forgotten at each start.
    printf 'Host winpool-*\n  HostName 127.0.0.1\n  User runneradmin\n  ProxyJump %s\n  IdentityFile ~/.ssh/winpool_controller\n' "$JUMP"
    printf '  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking accept-new\n' "$KNOWN"
    for n in "${SLOTS[@]}"; do printf 'Host winpool-%d\n  Port %d\n' "$n" $((22200 + n)); done
  } > "$CONF"
  touch "$HOME/.ssh/config" && chmod 600 "$HOME/.ssh/config" "$CONF"
  grep -qxF 'Include winpool.conf' "$HOME/.ssh/config" || sed -i '1i Include winpool.conf' "$HOME/.ssh/config"
}

up() { # machine numbers whose tunnel port listens on the jump host
  local listening
  listening=$(ssh -o BatchMode=yes -o ConnectTimeout=15 "$JUMP" 'ss -ltnH') || die "cannot reach the jump host $JUMP"
  grep -oE '127\.0\.0\.1:222(0[1-9]|1[0-4]) ' <<<"$listening" | sed -E 's/.*:222([0-9]{2}) /\1/; s/^0//' | sort -n || true
}

answers() { ssh -o BatchMode=yes -o ConnectTimeout=15 "winpool-$1" 'cmd /c exit 0' 2>/dev/null </dev/null; }

slot() { [[ ${1:-} =~ ^([1-9]|1[0-4])$ ]] || die "machine number must be 1-14"; }

start() {
  local count=${1:-} hours=${2:-2} busy free=() n deadline
  [[ $count =~ ^([1-9]|1[0-4])$ ]] || die "usage: winpool.sh start N [HOURS] (N 1-14)"
  [[ $hours =~ ^[1-5]$ ]] || die "HOURS must be 1-5"
  # One start at a time on this controller, and a machine that was just started is taken before its tunnel is up:
  # two lanes starting seconds apart otherwise pick the same numbers.
  mkdir -p "$CLAIMS"
  exec 9>"$CLAIMS/lock"; flock 9
  busy=$(up; find "$CLAIMS" -regextype posix-extended -regex '.*/([1-9]|1[0-4])' -mmin -15 -printf '%f\n')
  for n in "${SLOTS[@]}"; do grep -qx "$n" <<<"$busy" || free+=("$n"); done
  [ "${#free[@]}" -ge "$count" ] || die "only ${#free[@]} of ${#SLOTS[@]} machines are free (winpool.sh list)"
  free=("${free[@]:0:count}")
  for n in "${free[@]}"; do ssh-keygen -q -R "[127.0.0.1]:$((22200 + n))" -f "$KNOWN" >/dev/null 2>&1 || true; done
  gh workflow run winpool.yml -f slots="$(IFS=,; echo "${free[*]}")" -f hours="$hours" >/dev/null
  for n in "${free[@]}"; do echo "$OWNER" > "$CLAIMS/$n"; done
  flock -u 9
  echo "started machines ${free[*]} for ${hours} h; waiting for SSH (about 2 minutes)" >&2
  deadline=$((SECONDS + 900))
  for n in "${free[@]}"; do
    until answers "$n"; do
      [ $SECONDS -lt $deadline ] || die "winpool-$n did not answer within 15 minutes: gh run list --workflow winpool.yml"
      sleep 10
    done
    echo "winpool-$n"
  done
}

owner() { cat "$CLAIMS/$1" 2>/dev/null || true; }

stop() { # only machines this caller started: another lane's checks are running on the others
  local n targets=("$@")
  [ $# -gt 0 ] || for n in $(up); do [ "$(owner "$n")" != "$OWNER" ] || targets+=("$n"); done
  for n in "${targets[@]}"; do
    slot "$n"
    [ "$(owner "$n")" = "$OWNER" ] || die "winpool-$n was not started by you ($OWNER) but by: $(owner "$n")"
    ssh -o BatchMode=yes -o ConnectTimeout=15 "winpool-$n" 'cmd /c "echo stop>C:\winpool-stop"' </dev/null ||
      die "winpool-$n did not answer; it ends by itself after its hours"
    rm -f "$CLAIMS/$n"
    echo "winpool-$n stopping"
  done
}

command=${1:-}
[ $# -gt 0 ] && shift
case "$command" in
  start) configure; start "$@" ;;
  list) configure; for n in $(up); do echo "winpool-$n  $(owner "$n")"; done ;;
  ssh) configure; slot "${1:-}"; n=$1; shift; exec ssh "winpool-$n" "$@" ;;
  stop) configure; stop "$@" ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
