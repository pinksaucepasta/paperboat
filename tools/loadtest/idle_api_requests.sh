#!/bin/bash
# PR-03 server half: an idle machine's API requests per second over an interval, from the staging server's log,
# checked against perf_budgets.BUDGETS['idle_server']. Staging only, with exactly one active machine (the host
# runtime's requests carry no machine id, so every machine-side request is counted; edge, node and trust calls,
# and the edge's Go-http-client/1.1, are not). Run it next to tools/device-smoke/perf_idle.py on the machine.
#   idle_api_requests.sh [seconds=600] [server unit=pb-staging-server] [sql runner=~/lanem/sq.sh]
set -euo pipefail
SECONDS_WINDOW=${1:-600}; UNIT=${2:-pb-staging-server}; SQL=${3:-$HOME/lanem/sq.sh}
active=$(echo "SELECT count(*) FROM paperboat.user_machines WHERE revoked_at IS NULL AND deleted_at IS NULL AND seat_state='occupied' AND online" | "$SQL" -At)
[ "$active" = 1 ] || { echo "{\"status\":\"FAIL\",\"check\":\"perf-idle-server\",\"error\":\"$active online machines on staging, want exactly 1\"}"; exit 1; }
start=$(date -u "+%Y-%m-%d %H:%M:%S"); sleep "$SECONDS_WINDOW"; end=$(date -u "+%Y-%m-%d %H:%M:%S")
count=$(sudo journalctl -u "$UNIT" --since "$start" --until "$end" --no-pager -o cat | grep 'msg=http_request' \
  | grep -vE 'path=/v1/(edge|nodes|trust)/|client=Go-http-client/1\.1' | wc -l)
python3 - "$count" "$SECONDS_WINDOW" "$(cd "$(dirname "$0")/../device-smoke" && pwd)" <<'PY'
import json, sys
count, seconds, path = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
sys.path.insert(0, path)
from perf_budgets import BUDGETS
metrics = {'api_requests_per_s': round(count / seconds, 3), 'api_requests': count, 'seconds': seconds}
failures = [n for n, (kind, bound) in BUDGETS['idle_server'].items() if (metrics[n] > bound if kind == 'max' else metrics[n] < bound)]
print(json.dumps({'status': 'FAIL' if failures else 'PASS', 'check': 'perf-idle-server', 'metrics': metrics, 'budget_failures': failures}))
PY
