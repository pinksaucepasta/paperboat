#!/usr/bin/env bash
# Temporary Linux test hosts on GitHub-hosted runners (.github/workflows/linpool.yml), see README.md.
# Same design as winpool.sh (and the same controller key and tunnel secrets), on its own ports 22401-22412.
#
#   linpool.sh start N [HOURS]   start N machines (HOURS 1-5, default 2), wait until SSH answers, print their targets
#   linpool.sh list              machines whose tunnel is up, and who started them
#   linpool.sh ssh N [COMMAND]   SSH into machine N
#   linpool.sh stop [N...]       stop machines you started (default: all of yours)
#   linpool.sh release N...      give up your claim but leave the machine running for the next lane
#   linpool.sh adopt [N]         claim a running machine nobody claims (default: the first one), print its target
#
# Machine N is the SSH host linpool-N: user runner (passwordless sudo, Go, gcc, postgres:17 on 127.0.0.1), reached through the jump host's loopback port
# 22400+N with the key ~/.ssh/winpool_controller. "You" is your worktree (or LINPOOL_OWNER): every lane starts its
# machines from this one controller, which is where the start lock and the owner records live.
set -euo pipefail

JUMP=${WINPOOL_JUMP:-ubuntu@anvit}
MAX=12
CONF=$HOME/.ssh/linpool.conf
KNOWN=$HOME/.ssh/linpool_known_hosts
CLAIMS=$HOME/.ssh/linpool-claims # N: who started machine N
OWNER=${LINPOOL_OWNER:-$(git -C "$(dirname "$0")" rev-parse --show-toplevel)}

die() { echo "linpool: $*" >&2; exit 1; }

# Writes the linpool-N hosts and makes ~/.ssh/config include them: the checks pass only a host name to ssh.
configure() {
  [ -f "$HOME/.ssh/winpool_controller" ] || die "missing ~/.ssh/winpool_controller (its public half is the repository variable WINPOOL_CONTROLLER_PUBKEY)"
  {
    # Every runner is a new machine with a new host key: kept in its own file, forgotten at each start.
    printf 'Host linpool-*\n  HostName 127.0.0.1\n  User runner\n  ProxyJump %s\n  IdentityFile ~/.ssh/winpool_controller\n' "$JUMP"
    printf '  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking accept-new\n' "$KNOWN"
    for n in $(seq "$MAX"); do printf 'Host linpool-%d\n  Port %d\n' "$n" $((22400 + n)); done
  } > "$CONF"
  touch "$HOME/.ssh/config" && chmod 600 "$HOME/.ssh/config" "$CONF"
  grep -qxF 'Include linpool.conf' "$HOME/.ssh/config" || sed -i '1i Include linpool.conf' "$HOME/.ssh/config"
}

up() { # machine numbers whose tunnel port listens on the jump host
  local listening
  listening=$(ssh -o BatchMode=yes -o ConnectTimeout=15 "$JUMP" 'ss -ltnH') || die "cannot reach the jump host $JUMP"
  grep -oE '127\.0\.0\.1:224(0[1-9]|1[0-2]) ' <<<"$listening" | sed -E 's/.*:224([0-9]{2}) /\1/; s/^0//' | sort -n || true
}

answers() { ssh -o BatchMode=yes -o ConnectTimeout=15 "linpool-$1" true 2>/dev/null </dev/null; }

slot() { [[ ${1:-} =~ ^([1-9]|1[0-2])$ ]] || die "machine number must be 1-$MAX"; }

start() {
  local count=${1:-} hours=${2:-2} busy free=() n deadline
  [[ $count =~ ^([1-9]|1[0-2])$ ]] || die "usage: linpool.sh start N [HOURS] (N 1-$MAX)"
  [[ $hours =~ ^[1-5]$ ]] || die "HOURS must be 1-5"
  # One start at a time on this controller, and a machine that was just started is taken before its tunnel is up:
  # two lanes starting seconds apart otherwise pick the same numbers.
  mkdir -p "$CLAIMS"
  exec 9>"$CLAIMS/lock"; flock 9
  busy=$(up; find "$CLAIMS" -regextype posix-extended -regex '.*/([1-9]|1[0-2])' -mmin -15 -printf '%f\n')
  for n in $(seq "$MAX"); do grep -qx "$n" <<<"$busy" || free+=("$n"); done
  [ "${#free[@]}" -ge "$count" ] || die "only ${#free[@]} of $MAX machines are free (linpool.sh list)"
  free=("${free[@]:0:count}")
  for n in "${free[@]}"; do ssh-keygen -q -R "[127.0.0.1]:$((22400 + n))" -f "$KNOWN" >/dev/null 2>&1 || true; done
  gh workflow run linpool.yml -f slots="$(IFS=,; echo "${free[*]}")" -f hours="$hours" >/dev/null
  for n in "${free[@]}"; do echo "$OWNER" > "$CLAIMS/$n"; done
  flock -u 9
  echo "started machines ${free[*]} for ${hours} h; waiting for SSH (about 2 minutes)" >&2
  deadline=$((SECONDS + 900))
  for n in "${free[@]}"; do
    until answers "$n"; do
      [ $SECONDS -lt $deadline ] || die "linpool-$n did not answer within 15 minutes: gh run list --workflow linpool.yml"
      sleep 10
    done
    echo "linpool-$n"
  done
}

owner() { cat "$CLAIMS/$1" 2>/dev/null || true; }

stop() { # only machines this caller started: another lane's checks are running on the others
  local n targets=("$@")
  [ $# -gt 0 ] || for n in $(up); do [ "$(owner "$n")" != "$OWNER" ] || targets+=("$n"); done
  for n in "${targets[@]}"; do
    slot "$n"
    [ "$(owner "$n")" = "$OWNER" ] || die "linpool-$n was not started by you ($OWNER) but by: $(owner "$n")"
    ssh -o BatchMode=yes -o ConnectTimeout=15 "linpool-$n" 'touch /tmp/linpool-stop' </dev/null ||
      die "linpool-$n did not answer; it ends by itself after its hours"
    rm -f "$CLAIMS/$n"
    echo "linpool-$n stopping"
  done
}

release() { # the next lane adopts it: reuse takes seconds, a new runner minutes plus GitHub's queue
  local n
  for n in "$@"; do
    slot "$n"
    [ "$(owner "$n")" = "$OWNER" ] || die "linpool-$n was not started by you ($OWNER) but by: $(owner "$n")"
    rm -f "$CLAIMS/$n"; echo "linpool-$n released"
  done
}

adopt() {
  local n
  mkdir -p "$CLAIMS"
  exec 9>"$CLAIMS/lock"; flock 9
  for n in ${1:-$(up)}; do
    slot "$n"
    [ -z "$(owner "$n")" ] && answers "$n" || continue
    echo "$OWNER" > "$CLAIMS/$n"; echo "linpool-$n"; return
  done
  die "no running unclaimed machine (linpool.sh list); use linpool.sh start 1"
}

command=${1:-}
[ $# -gt 0 ] && shift
case "$command" in
  start) configure; start "$@" ;;
  list) configure; for n in $(up); do echo "linpool-$n  $(owner "$n")"; done ;;
  ssh) configure; slot "${1:-}"; n=$1; shift; exec ssh "linpool-$n" "$@" ;;
  stop) configure; stop "$@" ;;
  release) configure; release "$@" ;;
  adopt) configure; adopt "$@" ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
