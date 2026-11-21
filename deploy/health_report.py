"""Push one host-health report to the server for the admin panel's alerts (same endpoint and credential as the canary).

PAPERBOAT_HEALTH_REPORT_URL (the server's origin) and PAPERBOAT_HEALTH_REPORT_TOKEN_FILE (a file holding the server's
PAPERBOAT_HOST_HEALTH_TOKEN) are optional; without them nothing is sent. Best effort: a failed push never fails the caller.
"""
import json
import os
import sys
import urllib.request
from pathlib import Path


def push(source, payload):
    url, token_file = os.environ.get("PAPERBOAT_HEALTH_REPORT_URL"), os.environ.get("PAPERBOAT_HEALTH_REPORT_TOKEN_FILE")
    if not url or not token_file:
        return
    try:
        request = urllib.request.Request(
            url.rstrip("/") + "/v1/operations/host-health/reports", method="POST",
            data=json.dumps({"source": source, "payload": payload}).encode(),
            headers={"content-type": "application/json", "authorization": "Bearer " + Path(token_file).read_text().strip()})
        urllib.request.urlopen(request, timeout=10).close()
    except (OSError, ValueError) as error:
        print(f"health report failed: {error}", file=sys.stderr)
