#!/usr/bin/env bash
# Run one command on the configured Windows target, or copy a file to it.
set -eu
exec python3 "$(dirname "$0")/../dev_targets.py" run bash -c '
  : "${SMOKE_WINDOWS_SSH:?configure the Windows SSH login}" "${SMOKE_WINDOWS_PASSWORD:?configure the Windows password}"
  export SSHPASS=$SMOKE_WINDOWS_PASSWORD
  if [ "${1:-}" = --scp ]; then
    exec sshpass -e scp -q -o StrictHostKeyChecking=accept-new -- "$2" "$SMOKE_WINDOWS_SSH:$3"
  fi
  exec sshpass -e ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -- "$SMOKE_WINDOWS_SSH" "$@"
' bash "$@"
