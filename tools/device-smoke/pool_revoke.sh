# shellcheck shell=bash
# Sourced by linuxpool.sh, macpool.sh and winpool.sh: revoke a runner's Paperboat machine records when the runner ends.
#
# A pool runner's throwaway machines (aliases `<pool>-<build>-<slot>`, the pool prefix and the slot) stay "online" on the
# server until their heartbeat times out after the runner is gone, which fails the deploy gate's "online before, offline
# after" check. So the records are revoked first, by exact machine id, through cleanup_test_account.py, which still only
# touches throwaway machines of the cleanup accounts that are not in cleanup_protected, and (with --pool and --slot)
# only those whose alias carries this pool prefix and one of these slots.
#
# The caller defines POOL (e.g. linuxpool) and CLAIMS (the claim directory) before sourcing.
# POOL_REVOKE_CMD overrides the cleanup command (tests); POOL_REVOKE=0 turns the revoking off.

pool_revoke_cmd() {
  if [ -n "${POOL_REVOKE_CMD:-}" ]; then
    local words; read -ra words <<<"$POOL_REVOKE_CMD"; "${words[@]}" "$@"
  else
    python3 "$(dirname "${BASH_SOURCE[0]}")/cleanup_test_account.py" "$@"
  fi
}

# pool_record SLOT MACHINE_ID: remember a machine id registered on this slot's runner, so its revoke is exact even if the
# alias is later renamed. Ids are also found by alias at revoke time, so recording is optional.
pool_record() {
  local n=$1 id=$2 f
  [[ $id =~ ^machine_[0-9a-f-]{36}$ ]] || { echo "$POOL: not a machine id: $id" >&2; return 1; }
  mkdir -p "$CLAIMS"; f=$CLAIMS/$n.machines
  grep -qxF "$id" "$f" 2>/dev/null || (umask 077; echo "$id" >> "$f")
}

# pool_revoke SLOT...: revoke every machine record of these slots' runners, by exact id. Returns 1 if that failed.
# Never touches anything but machines named <POOL>-...-<slot> for these slots (the cleanup script enforces it again).
pool_revoke() {
  [ "${POOL_REVOKE:-1}" != 0 ] || return 0
  local slots=("$@") scope=() ids=() found=() out id n f seen=' '
  [ ${#slots[@]} -gt 0 ] || return 0
  scope=(--pool "$POOL"); for n in "${slots[@]}"; do scope+=(--slot "$n"); done
  for n in "${slots[@]}"; do
    f=$CLAIMS/$n.machines
    [ -f "$f" ] || continue
    while read -r id; do if [[ $id =~ ^machine_[0-9a-f-]{36}$ ]]; then ids+=("$id"); fi; done <"$f"
  done
  out=$(pool_revoke_cmd "${scope[@]}" --all --stale '0 seconds') || { echo "$POOL: cannot list machine records to revoke" >&2; return 1; }
  for id in $out; do if [[ $id =~ ^machine_[0-9a-f-]{36}$ ]]; then ids+=("$id"); fi; done
  for id in "${ids[@]}"; do case "$seen" in *" $id "*) ;; *) seen+="$id "; found+=("$id") ;; esac; done
  if [ ${#found[@]} -gt 0 ]; then
    local idargs=(); for id in "${found[@]}"; do idargs+=(--id "$id"); done
    out=$(pool_revoke_cmd "${scope[@]}" "${idargs[@]}" --stale '0 seconds' --apply) || { echo "$POOL: revoking ${#found[@]} machine record(s) failed" >&2; return 1; }
    echo "$POOL: $out" >&2
  fi
  for n in "${slots[@]}"; do rm -f "$CLAIMS/$n.machines"; done
}

# Runners start in the second account's runner repository when this controller has one (owner/name and token files): its job
# limit is its own, so the Linux and macOS hosts leave the main account's slots to Windows and CI.
pool_gh() {
  local conf=$HOME/.config/paperboat-dev/linuxpool-gh
  if [ -s "$conf.repo" ] && [ -s "$conf.token" ]; then GH_REPO=$(cat "$conf.repo") GH_TOKEN=$(cat "$conf.token") gh "$@"; else gh "$@"; fi
}
