#!/usr/bin/env python3
"""Close/delete only the exact non-default terminal created by a smoke check."""
import json
import re
import subprocess
import sys
import tempfile
import time


class CleanupError(Exception):
    pass


def command(pb, *args):
    with tempfile.TemporaryFile() as output:
        result = subprocess.run([pb, "session", *args, "--json"], stdout=output,
                                stderr=subprocess.DEVNULL, timeout=30, check=False)
        if output.tell() > 2 * 1024 * 1024:
            raise CleanupError('pb session output over 2 MiB')
        output.seek(0)
        receipt = json.load(output)
    if not isinstance(receipt, dict):
        raise CleanupError('pb session output is not a JSON object')
    return result.returncode, receipt


def inventory(pb, target, environment=None):
    code, receipt = command(pb, "list", target)
    actual = receipt.get("environment", {}).get("id")
    sessions = receipt.get("sessions")
    if code or receipt.get("version") != "1" or not isinstance(actual, str) or not actual:
        raise CleanupError('pb session list failed or gave an invalid receipt')
    if environment is not None and actual != environment:
        raise CleanupError('pb session list answered for another environment')
    if not isinstance(sessions, list) or any(not isinstance(s, dict) for s in sessions):
        raise CleanupError('pb session list gave an invalid sessions array')
    return actual, sessions


def mutate(pb, action, target, environment, session):
    code, challenge = command(pb, action, target, session)
    data = challenge.get("data", {})
    token = data.get("confirmation_token", "")
    if (code != 2 or challenge.get("ok") is not False
            or challenge.get("error", {}).get("code") != "confirmation_required"
            or data.get("outcome") != "confirmation_required"
            or data.get("scope") != f"session-{action}:{environment}:{session}"
            or not isinstance(token, str) or not re.fullmatch(r"[A-Z0-9]{6}", token)):
        raise CleanupError('the confirmation challenge did not match')
    code, receipt = command(pb, action, target, session, "--confirm", token)
    if (code or receipt.get("version") != "1" or receipt.get("session_id") != session
            or receipt.get("environment", {}).get("id") != environment):
        raise CleanupError('the confirmed operation gave a wrong receipt')
    if action == "delete":
        if receipt.get("deleted") is not True:
            raise CleanupError('delete receipt did not say deleted')
    elif receipt.get("state") not in ("closed", "closing"):
        raise CleanupError('close receipt is not closed or closing')


def cleanup(pb, target, name):
    if not name.startswith("smoke-") or not re.fullmatch(r"[A-Za-z0-9_-]+", name):
        raise CleanupError('refusing to touch a session not named smoke-*')
    environment, sessions = inventory(pb, target)
    owned = [s for s in sessions if s.get("name") == name]
    if not owned:
        return
    if len(owned) != 1 or owned[0].get("is_default") is not False:
        raise CleanupError('the session is missing, duplicated or the default one')
    session = owned[0].get("id", "")
    if not isinstance(session, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", session):
        raise CleanupError('the session id is invalid')
    if owned[0].get("state") != "closed":
        mutate(pb, "close", target, environment, session)
        deadline = time.monotonic() + 30
        while True:
            _, sessions = inventory(pb, target, environment)
            current = [s for s in sessions if s.get("id") == session]
            if len(current) != 1 or current[0].get("name") != name or current[0].get("is_default") is not False:
                raise CleanupError('the session changed while waiting for it to close')
            if current[0].get("state") == "closed":
                break
            if time.monotonic() >= deadline:
                raise CleanupError('the session did not show closed within 30 s')
            time.sleep(1)
    mutate(pb, "delete", target, environment, session)
    _, remaining = inventory(pb, target, environment)
    if any(s.get("id") == session or s.get("name") == name for s in remaining):
        raise CleanupError('the session is still listed after delete')


if __name__ == "__main__":
    try:
        cleanup(*sys.argv[1:])
    except (CleanupError, OSError, ValueError, TypeError, subprocess.TimeoutExpired):
        print("SESSION_CLEANUP_FAIL")
        sys.exit(1)
    print("SESSION_CLEANUP_OK")
