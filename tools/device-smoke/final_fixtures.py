#!/usr/bin/env python3
"""Server-side fixtures of the final re-check for one throwaway account, through the control plane's scoped endpoint.

  final_fixtures.py --email E [--member M ...] [--wait SECONDS]   create, print {"env": {...}}
  final_fixtures.py --email E --teardown                          remove, so the account can be deleted

The account must have signed in once. Create gives it a team it owns with the paid entitlement granted as test data and
each --member (an account that has signed in once) as an active member; idempotent. It prints one JSON line the runner
merges into the environment of every check: PAPERBOAT_SMOKE_TEAM is the team. Teardown makes every other member
inactive; a team left with its owner alone is deleted by the product itself when the account is deleted.

The only credential is FINAL_FIXTURES_TOKEN (the one CI secret, never printed); FINAL_FIXTURES_URL is the endpoint base
(https://<api host>/v1/test-fixtures). The same endpoint creates and removes the custom-domain test DNS records
(see dns_fixture below); the server writes them with its own DNS token.
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request


def call(method, path, body=None, timeout=60):
    """One authenticated request; returns (status, decoded body or None). Never echoes the token."""
    base, token = os.environ.get("FINAL_FIXTURES_URL", "").rstrip("/"), os.environ.get("FINAL_FIXTURES_TOKEN", "")
    if not base.startswith("https://") or not token:
        raise SystemExit("set FINAL_FIXTURES_URL (https://<api host>/v1/test-fixtures) and FINAL_FIXTURES_TOKEN")
    request = urllib.request.Request(
        base + path, method=method, data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            return response.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            return error.code, json.loads(raw)
        except ValueError:
            return error.code, None


def fail(status, answer, what):
    code = ((answer or {}).get("error") or {}).get("code", "")
    raise SystemExit("%s failed: HTTP %d %s" % (what, status, code))


def dns_fixture(kind, name, content):
    """Create one test DNS record; returns its id (delete with dns_remove). Same endpoint as the team calls."""
    status, answer = call("POST", "/dns", {"type": kind, "name": name, "content": content})
    if status != 200:
        fail(status, answer, "create DNS record")
    return answer["data"]["id"]


def dns_remove(record_id):
    status, answer = call("DELETE", "/dns/" + record_id)
    if status != 204:
        fail(status, answer, "delete DNS record")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--email", required=True)
    parser.add_argument("--member", action="append", default=[], help="another signed-in account to add as a team member")
    parser.add_argument("--teardown", action="store_true")
    parser.add_argument("--wait", type=int, default=60, help="seconds to wait for the account's first sign-in")
    args = parser.parse_args(argv)
    if args.teardown:
        status, answer = call("DELETE", "/team", {"email": args.email})
        if status != 200:
            fail(status, answer, "teardown")
        print(json.dumps({"teardown": answer["data"]}))
        return 0
    deadline = time.monotonic() + args.wait
    while True:
        status, answer = call("POST", "/team", {"email": args.email, "members": args.member})
        code = ((answer or {}).get("error") or {}).get("code", "")
        if status == 404 and code == "account_not_found" and time.monotonic() < deadline:
            time.sleep(2)
            continue
        break
    if status != 200:
        fail(status, answer, "create team")
    print(json.dumps({"env": answer["data"]["env"]}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
