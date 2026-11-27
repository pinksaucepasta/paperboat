# Sourced by remote-test.sh and win-remote-test.sh. Where a shared runner board tool exists (PB_RUNNER, default
# ~/bin/runner), a test run holds the host's board lock and refuses a host another lane holds. No tool: no-op.
board_tool=${PB_RUNNER:-$HOME/bin/runner}
board_name='' board_renew=''

board_guard() { # PURPOSE HOST... : the spellings of one host (alias, ssh destination); the first the board knows counts
  [ -x "$board_tool" ] || return 0
  local purpose=$1 name state=none
  shift
  for name in "$@"; do
    name=${name#*@}
    state=$("$board_tool" guard "$name" "$purpose" $$) || exit $?
    [ "$state" = none ] || break
  done
  [ "$state" != none ] || return 0
  board_name=$name
  # Redirected fully: the child must not hold the caller's pipes or lock descriptors open. It takes its sleep with it.
  ( trap 'kill "$!" 2>/dev/null; exit 0' TERM
    while kill -0 $$; do sleep 600 & wait "$!"; "$board_tool" keep "$name"; done ) </dev/null >/dev/null 2>&1 8>&- &
  board_renew=$!
}

board_release() {
  [ -z "$board_renew" ] || kill "$board_renew" 2>/dev/null || true
  [ -z "$board_name" ] || "$board_tool" unguard "$board_name" $$ >/dev/null 2>&1 || true
  board_renew='' board_name=''
}
