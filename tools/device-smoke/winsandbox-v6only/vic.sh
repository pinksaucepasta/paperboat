#!/bin/bash
# usage: vic.sh '<powershell or cmd>'  (password read from REMOTE_TESTING.md, never printed)
export SSHPASS="$(grep 'ssh pujan@victus' /home/ubuntu/paperboat/REMOTE_TESTING.md | head -1 | sed 's/.* pass //' | tr -d '\r ')"
[ "${1:-}" = --password ] && { echo "$SSHPASS"; exit 0; }
exec sshpass -e ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 pujan@victus "$@"
