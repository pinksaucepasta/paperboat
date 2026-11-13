#!/usr/bin/env bash
# Temporary macOS test machines on GitHub-hosted runners (.github/workflows/macpool.yml), see README.md.
# Same design as winpool.sh (and the same controller key and tunnel secrets), on its own ports 22301-22308.
#
#   macpool.sh start N [HOURS]   start N machines (HOURS 1-5, default 2), wait until SSH answers, print their targets
#   macpool.sh list              machines whose tunnel is up, and who started them
#   macpool.sh ssh N [COMMAND]   SSH into machine N
#   macpool.sh stop [N...]       stop machines you started (default: all of yours)
#   macpool.sh release N...      give up your claim but leave the machine running for the next lane
#   macpool.sh adopt [N]         claim a running machine nobody claims (default: the first one), print its target
#
# Machine N is the SSH host macpool-N: user runner (passwordless sudo), reached through the jump host's loopback port
# 22300+N with the key ~/.ssh/winpool_controller. "You" is your worktree (or MACPOOL_OWNER): every lane starts its
# machines from this one controller, which is where the start lock and the owner records live.
set -euo pipefail

JUMP=${WINPOOL_JUMP:-ubuntu@anvit}
MAX=8
CONF=$HOME/.ssh/macpool.conf
KNOWN=$HOME/.ssh/macpool_known_hosts
CLAIMS=$HOME/.ssh/macpool-claims # N: who started machine N
OWNER=${MACPOOL_OWNER:-$(git -C "$(dirname "$0")" rev-parse --show-toplevel)}

die() { echo "macpool: $*" >&2; exit 1; }

# Writes the macpool-N hosts and makes ~/.ssh/config include them: the checks pass only a host name to ssh.
configure() {
  [ -f "$HOME/.ssh/winpool_controller" ] || die "missing ~/.ssh/winpool_controller (its public half is the repository variable WINPOOL_CONTROLLER_PUBKEY)"
  {
    # Every runner is a new machine with a new host key: kept in its own file, forgotten at each start.
    printf 'Host macpool-*\n  HostName 127.0.0.1\n  User runner\n  ProxyJump %s\n  IdentityFile ~/.ssh/winpool_controller\n' "$JUMP"
    printf '  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking accept-new\n' "$KNOWN"
    for n in $(seq "$MAX"); do printf 'Host macpool-%d\n  Port %d\n' "$n" $((22300 + n)); done
  } > "$CONF"
  touch "$HOME/.ssh/config" && chmod 600 "$HOME/.ssh/config" "$CONF"
  grep -qxF 'Include macpool.conf' "$HOME/.ssh/config" || sed -i '1i Include macpool.conf' "$HOME/.ssh/config"
}

up() { # machine numbers whose tunnel port listens on the jump host
  local listening
  listening=$(ssh -o BatchMode=yes -o ConnectTimeout=15 "$JUMP" 'ss -ltnH') || die "cannot reach the jump host $JUMP"
  grep -oE '127\.0\.0\.1:2230[1-8] ' <<<"$listening" | sed -E 's/.*:2230([1-8]) /\1/' | sort -n || true
}

answers() { ssh -o BatchMode=yes -o ConnectTimeout=15 "macpool-$1" true 2>/dev/null </dev/null; }

slot() { [[ ${1:-} =~ ^[1-8]$ ]] || die "machine number must be 1-$MAX"; }

start() {
  local count=${1:-} hours=${2:-2} busy free=() n deadline
  [[ $count =~ ^[1-8]$ ]] || die "usage: macpool.sh start N [HOURS] (N 1-$MAX)"
  [[ $hours =~ ^[1-5]$ ]] || die "HOURS must be 1-5"
  # One start at a time on this controller, and a machine that was just started is taken before its tunnel is up:
  # two lanes starting seconds apart otherwise pick the same numbers.
  mkdir -p "$CLAIMS"
  exec 9>"$CLAIMS/lock"; flock 9
  busy=$(up; find "$CLAIMS" -regextype posix-extended -regex '.*/[1-8]' -mmin -15 -printf '%f\n')
  for n in $(seq "$MAX"); do grep -qx "$n" <<<"$busy" || free+=("$n"); done
  [ "${#free[@]}" -ge "$count" ] || die "only ${#free[@]} of $MAX machines are free (macpool.sh list)"
  free=("${free[@]:0:count}")
  for n in "${free[@]}"; do ssh-keygen -q -R "[127.0.0.1]:$((22300 + n))" -f "$KNOWN" >/dev/null 2>&1 || true; done
  gh workflow run macpool.yml -f slots="$(IFS=,; echo "${free[*]}")" -f hours="$hours" >/dev/null
  for n in "${free[@]}"; do echo "$OWNER" > "$CLAIMS/$n"; done
  flock -u 9
  echo "started machines ${free[*]} for ${hours} h; waiting for SSH (about 3 minutes)" >&2
  deadline=$((SECONDS + 900))
  for n in "${free[@]}"; do
    until answers "$n"; do
      [ $SECONDS -lt $deadline ] || die "macpool-$n did not answer within 15 minutes: gh run list --workflow macpool.yml"
      sleep 10
    done
    echo "macpool-$n"
  done
}

owner() { cat "$CLAIMS/$1" 2>/dev/null || true; }

stop() { # only machines this caller started: another lane's checks are running on the others
  local n targets=("$@")
  [ $# -gt 0 ] || for n in $(up); do [ "$(owner "$n")" != "$OWNER" ] || targets+=("$n"); done
  for n in "${targets[@]}"; do
    slot "$n"
    [ "$(owner "$n")" = "$OWNER" ] || die "macpool-$n was not started by you ($OWNER) but by: $(owner "$n")"
    ssh -o BatchMode=yes -o ConnectTimeout=15 "macpool-$n" 'touch /tmp/macpool-stop' </dev/null ||
      die "macpool-$n did not answer; it ends by itself after its hours"
    rm -f "$CLAIMS/$n"
    echo "macpool-$n stopping"
  done
}

release() { # the next lane adopts it: reuse takes seconds, a new runner minutes plus GitHub's queue
  local n
  for n in "$@"; do
    slot "$n"
    [ "$(owner "$n")" = "$OWNER" ] || die "macpool-$n was not started by you ($OWNER) but by: $(owner "$n")"
    rm -f "$CLAIMS/$n"; echo "macpool-$n released"
  done
}

adopt() {
  local n
  mkdir -p "$CLAIMS"
  exec 9>"$CLAIMS/lock"; flock 9
  for n in ${1:-$(up)}; do
    slot "$n"
    [ -z "$(owner "$n")" ] && answers "$n" || continue
    echo "$OWNER" > "$CLAIMS/$n"; echo "macpool-$n"; return
  done
  die "no running unclaimed machine (macpool.sh list); use macpool.sh start 1"
}

command=${1:-}
[ $# -gt 0 ] && shift
case "$command" in
  start) configure; start "$@" ;;
  list) configure; for n in $(up); do echo "macpool-$n  $(owner "$n")"; done ;;
  ssh) configure; slot "${1:-}"; n=$1; shift; exec ssh "macpool-$n" "$@" ;;
  stop) configure; stop "$@" ;;
  release) configure; release "$@" ;;
  adopt) configure; adopt "$@" ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
