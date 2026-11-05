#!/bin/sh
# Run one compile/test command on oracle inside one of N shared slots (default 4), under nice -n 15.
# Usage: oracle-slot.sh go build ./...   Waits for a free slot; slots are flock'd files, freed on any exit.
set -eu
n=${PB_ORACLE_SLOTS:-4}
dir=/tmp/pb-oracle-slots
mkdir -p "$dir"
i=0
while :; do
  slot="$dir/$((i % n)).lock"
  exec 9>"$slot"
  if flock -n 9; then break; fi
  i=$((i + 1))
  if [ $((i % n)) -eq 0 ]; then sleep 2; fi
done
exec nice -n 15 "$@"
