#!/usr/bin/env python3
"""Revoke stale throwaway test machines on the shared test accounts.

A test account's every machine and signed-in CLI session is a peer of every other, so throwaway
containers that are never removed grow its peer-network configuration until it hits the size
bound and native access stops for the whole account. Run after each device run.

A lane passes its own throwaway prefix (--prefix laner-) and only machines whose alias starts
with it are considered; --all (every throwaway pattern of every lane) is for Lane B only. Only
machines matching AND not seen for --stale (default 2 h) are revoked, together with the CLI sessions `pb pair` created for them. Protected
devices (ubuntu24, pbvm*, dadape, hp, apple, victus) are never touched. Dry-run by default: it
prints the ids it would revoke; --apply revokes them in one transaction and prints them again.
It runs SQL on the production database through `ssh <host> docker exec psql` (operator access).
"""
import argparse
import re
import subprocess
import sys

DEFAULT_ACCOUNTS = ["user_5ae46801-d921-4983-be1c-db50ad886304"]
THROWAWAY = r"^(lane[a-z0-9]*-|luf14)"
PROTECTED = r"(ubuntu24|pbvm|dadape|^hp|apple|victus)"

SELECT = """
CREATE TEMP TABLE doomed ON COMMIT DROP AS
SELECT m.id FROM paperboat.user_machines m
WHERE m.user_id = ANY(%(accounts)s) AND m.revoked_at IS NULL AND m.deleted_at IS NULL
  AND (m.last_seen_at IS NULL OR m.last_seen_at < now() - %(stale)s::interval)
  AND m.alias ~ %(throwaway)s AND m.alias !~* %(protected)s;
"""

# The same fence as migration 118: end the machines' pair-time CLI sessions with their tokens.
REVOKE = """
UPDATE paperboat.user_machines m SET state='revoked', revoked_at=now(), online=false, updated_at=now(), version=version+1
FROM doomed d WHERE m.id=d.id;
UPDATE paperboat.cli_access_tokens t SET revoked_at=coalesce(t.revoked_at, now())
FROM paperboat.cli_client_sessions s JOIN doomed d ON d.id=s.user_machine_id
WHERE t.cli_client_session_id=s.id AND t.revoked_at IS NULL AND s.state='active';
UPDATE paperboat.cli_refresh_tokens t SET state='revoked', revoked_at=coalesce(t.revoked_at, now())
FROM paperboat.cli_client_sessions s JOIN doomed d ON d.id=s.user_machine_id
WHERE t.cli_client_session_id=s.id AND t.state<>'revoked' AND s.state='active';
UPDATE paperboat.cli_client_sessions s SET state='revoked', revoked_at=coalesce(s.revoked_at, now()),
  revocation_reason=coalesce(s.revocation_reason, 'machine_removed'), version=s.version+1
FROM doomed d WHERE d.id=s.user_machine_id AND s.state='active';
"""


def quote(value):
    return "'" + value.replace("'", "''") + "'"


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--host", default="root@coolify", help="ssh target running the database container")
    parser.add_argument("--account", action="append", help="test account id (repeatable); default: the shared test account")
    parser.add_argument("--stale", default="2 hours", help="revoke only machines unseen this long (PostgreSQL interval)")
    parser.add_argument("--apply", action="store_true", help="revoke; without it only list")
    scope = parser.add_mutually_exclusive_group(required=True)
    scope.add_argument("--prefix", help="this lane's throwaway alias prefix, e.g. laner- (required for lanes)")
    scope.add_argument("--all", action="store_true", help="every lane's throwaway machines (Lane B only)")
    args = parser.parse_args()
    throwaway = THROWAWAY
    if args.prefix:
        if not re.fullmatch(r"[a-z][a-z0-9]*-", args.prefix) or re.search(PROTECTED, args.prefix):
            parser.error("--prefix must be a lane prefix like laner- (lowercase, ending in -)")
        throwaway = "^" + re.escape(args.prefix)
    accounts = args.account or DEFAULT_ACCOUNTS
    params = {
        "accounts": "ARRAY[" + ",".join(quote(a) for a in accounts) + "]::text[]",
        "stale": quote(args.stale), "throwaway": quote(throwaway), "protected": quote(PROTECTED),
    }
    sql = "\\set ON_ERROR_STOP 1\nBEGIN;\nSET LOCAL search_path TO paperboat, public;\n" + SELECT % params
    if args.apply:
        sql += REVOKE
    sql += "\\copy (SELECT id FROM doomed ORDER BY id) TO STDOUT\n" + ("COMMIT;\n" if args.apply else "ROLLBACK;\n")
    done = subprocess.run(["ssh", args.host, "docker", "exec", "-i", "paperboat-postgres-1",
                           "psql", "-U", "postgres", "-d", "paperboat", "-qAt"],
                          input=sql, capture_output=True, text=True, timeout=120)
    if done.returncode != 0 or "ERROR" in done.stderr:
        print("cleanup failed: " + done.stderr.strip()[:300], file=sys.stderr)
        return 1
    ids = [line for line in done.stdout.split() if line.startswith("machine_")]
    verb = "revoked" if args.apply else "would revoke"
    print(f"{verb} {len(ids)} machine(s)" + (": " + " ".join(ids) if ids else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
