#!/bin/sh
# Run one narrow compile/test command on oracle in a fair queue of N slots (default 4), under nice -n 15.
#   oracle-slot.sh go vet ./internal/x/
#   PB_SLOT_PRIORITY=1 oracle-slot.sh go build ./cmd/pb   # pushes and integration: served before normal jobs
# FIFO: each caller takes a numbered ticket and runs once it is among the first N live tickets (priority
# tickets sort first). Tickets of exited callers are pruned, so a killed job never blocks the queue.
# Broad work (./..., several modules, -race, full packages) belongs on GitHub: tools/ci-test.sh.
set -eu
n=${PB_ORACLE_SLOTS:-4}
dir=/tmp/pb-oracle-queue
mkdir -p "$dir"
class=1
[ "${PB_SLOT_PRIORITY:-}" = 1 ] && class=0
exec 8>"$dir/.lock"
flock 8
seq=$(( $(cat "$dir/.seq" 2>/dev/null || echo 0) + 1 ))
echo "$seq" > "$dir/.seq"
ticket=$(printf '%s-%012d' "$class" "$seq")
echo $$ > "$dir/$ticket"
flock -u 8
exec 8>&-
trap 'rm -f "$dir/$ticket"' EXIT
trap 'exit 143' INT TERM HUP
while :; do
  pos=0
  for t in $(ls "$dir" | sort); do
    pid=$(cat "$dir/$t" 2>/dev/null || true)
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then rm -f "$dir/$t"; continue; fi
    [ "$t" = "$ticket" ] && break
    pos=$((pos + 1))
  done
  [ "$pos" -lt "$n" ] && break
  sleep 1
done
nice -n 15 "$@"
