#!/bin/bash
# PR-03 server half: an idle machine's API requests per second over an interval, from the staging server's log,
# checked against perf_budgets.BUDGETS['idle_server']. Staging only. The machine is told apart from the rest of a shared
# stack by its build: install a pb whose version string is unique to this machine (-X ...buildinfo.Version=dev-<sha>-idle),
# the access log's `client=paperboat/<version>` then counts exactly its requests (edge/node/trust calls and the edge's
# Go-http-client are not the machine's). Run it next to tools/device-smoke/perf_idle.py on the machine.
#   VERSION=dev-<sha>-idle LOG_CMD='ssh ubuntu@anvit "cd ~/pb-release-staging && docker compose logs --no-log-prefix --since $START --until $END server"' \
#     idle_api_requests.sh [seconds=600]
# LOG_CMD prints the server's log between the UTC times in $START and $END (RFC 3339).
set -euo pipefail
SECONDS_WINDOW=${1:-600}; : "${VERSION:?unique pb build version of the idle machine}" "${LOG_CMD:?command printing the server log between START and END}"
start=$(date -u +%Y-%m-%dT%H:%M:%SZ); sleep "$SECONDS_WINDOW"; end=$(date -u +%Y-%m-%dT%H:%M:%SZ)
log=$(START=$start END=$end bash -c "$LOG_CMD")
# an empty or failed log read must not look like an idle machine ($START and $END expand locally, not on the remote side)
[ "$(grep -c 'msg=http_request' <<<"$log")" -gt 0 ] || { echo "idle_api_requests: LOG_CMD printed no http_request lines between $start and $end" >&2; exit 1; }
count=$(grep 'msg=http_request' <<<"$log" | grep -c "client=paperboat/$VERSION\$" || true)
python3 - "$count" "$SECONDS_WINDOW" "$(cd "$(dirname "$0")/../device-smoke" && pwd)" <<'PY'
import json, sys
count, seconds, path = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
sys.path.insert(0, path)
from perf_budgets import BUDGETS
metrics = {'api_requests_per_s': round(count / seconds, 3), 'api_requests': count, 'seconds': seconds}
failures = [n for n, (kind, bound) in BUDGETS['idle_server'].items() if (metrics[n] > bound if kind == 'max' else metrics[n] < bound)]
print(json.dumps({'status': 'FAIL' if failures else 'PASS', 'check': 'perf-idle-server', 'metrics': metrics, 'budget_failures': failures}))
PY
