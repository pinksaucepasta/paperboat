#!/usr/bin/env python3
"""usage: macos-check.py NAME SECONDS RESULTS_FILE CMD...
Run one device check with a timeout; append only its JSON result lines (tagged job_check) to RESULTS_FILE,
or one FAIL line when it printed none. Exit 1 on failure. Diagnostics are dropped, never uploaded."""
import json
import subprocess
import sys

name, secs, results, cmd = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4:]
try:
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=secs, stdin=subprocess.DEVNULL)
    code, out = str(r.returncode), r.stdout
except subprocess.TimeoutExpired as e:
    code, out = 'timeout', (e.stdout or b'').decode() if isinstance(e.stdout, bytes) else (e.stdout or '')
lines = []
for line in out.splitlines():
    try:
        item = json.loads(line)
    except ValueError:
        continue
    if isinstance(item, dict):
        lines.append({'job_check': name, **item})
failed = code != '0' or any(i.get('status') == 'FAIL' for i in lines)
if not lines or (code != '0' and not any(i.get('status') == 'FAIL' for i in lines)):
    lines.append({'job_check': name, 'status': 'FAIL', 'exit': code})
with open(results, 'a') as f:
    for i in lines:
        f.write(json.dumps(i) + '\n')
        print(json.dumps(i), flush=True)
sys.exit(1 if failed else 0)
