#!/usr/bin/env python3
"""Create Paperboat Sentry releases, or record an already-verified deployment."""

import argparse
import datetime
import json
import os
import re
import stat
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

SAFE = re.compile(r"^[A-Za-z0-9_.:-]{1,128}$")
REPOSITORY = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
SHA = re.compile(r"^[0-9a-f]{40}$")
MAX_RESPONSE = 65536


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def fail(message):
    raise SystemExit(message)


def token_value(args):
    environment_token = os.getenv("SENTRY_AUTH_TOKEN")
    if environment_token and args.token_file:
        fail("configure SENTRY_AUTH_TOKEN or SENTRY_AUTH_TOKEN_FILE, not both")
    if args.token_file:
        descriptor = None
        try:
            descriptor = os.open(args.token_file, os.O_RDONLY | os.O_NOFOLLOW)
            metadata = os.fstat(descriptor)
            if not stat.S_ISREG(metadata.st_mode):
                fail("SENTRY_AUTH_TOKEN_FILE must be a regular file")
            if stat.S_IMODE(metadata.st_mode) & 0o077:
                fail("SENTRY_AUTH_TOKEN_FILE must not be accessible by group or others")
            with os.fdopen(descriptor, "rb", closefd=True) as source:
                descriptor = None
                raw = source.read(16385)
            if len(raw) > 16384:
                fail("SENTRY_AUTH_TOKEN_FILE exceeds 16384 bytes")
            value = raw.decode("utf-8").strip()
        except (OSError, UnicodeDecodeError):
            fail("SENTRY_AUTH_TOKEN_FILE could not be read safely")
        finally:
            if descriptor is not None:
                os.close(descriptor)
    else:
        value = environment_token
    if not value or any(character.isspace() for character in value):
        fail("Sentry authentication token is missing or invalid")
    return value


def request(args, method, path, payload):
    url = args.api_url.rstrip("/") + path
    body = json.dumps(payload, separators=(",", ":")).encode()
    for attempt in range(1, args.attempts + 1):
        req = urllib.request.Request(
            url,
            data=body,
            method=method,
            headers={
                "Authorization": "Bearer " + args.auth_token,
                "Content-Type": "application/json",
                "User-Agent": "paperboat-sentry-release/1",
            },
        )
        try:
            with OPENER.open(req, timeout=args.timeout) as response:
                if response.status not in (200, 201, 208):
                    fail(f"Sentry API returned unexpected HTTP {response.status}")
                if len(response.read(MAX_RESPONSE + 1)) > MAX_RESPONSE:
                    fail("Sentry API response exceeded 65536 bytes")
                return
        except urllib.error.HTTPError as error:
            retryable = error.code == 429 or 500 <= error.code <= 599
            if not retryable or attempt == args.attempts:
                fail(f"Sentry API {method} failed with HTTP {error.code} after {attempt} attempt(s)")
        except (TimeoutError, urllib.error.URLError):
            if attempt == args.attempts:
                fail(f"Sentry API {method} failed after {attempt} attempt(s)")
        time.sleep(min(attempt, 2))


def verify_readiness(args):
    parsed = urllib.parse.urlsplit(args.readiness_url or "")
    local_test = parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "::1")
    if not ((parsed.scheme == "https" or local_test) and parsed.hostname and not parsed.username and not parsed.password):
        fail("deploy requires an HTTPS readiness URL without embedded credentials")
    try:
        request = urllib.request.Request(args.readiness_url, headers={"Accept": "application/json"})
        with OPENER.open(request, timeout=args.timeout) as response:
            if response.status != 200:
                fail(f"rollout readiness check returned HTTP {response.status}")
            body = response.read(MAX_RESPONSE + 1)
            if len(body) > MAX_RESPONSE:
                fail("rollout readiness response exceeded 65536 bytes")
            try:
                readiness = json.loads(body)
            except (UnicodeDecodeError, json.JSONDecodeError):
                fail("rollout readiness response must be JSON")
            if not isinstance(readiness, dict) or readiness.get("release") != args.release:
                fail("rollout readiness response did not report the expected release")
    except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError) as error:
        code = getattr(error, "code", None)
        fail(f"rollout readiness check failed{f' with HTTP {code}' if code else ''}")


def validate_common(args):
    for name in ("organization", "project", "release"):
        value = getattr(args, name)
        if not SAFE.fullmatch(value or ""):
            fail(f"Sentry {name} must match {SAFE.pattern}")
    args.auth_token = token_value(args)


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("release", "deploy"))
    parser.add_argument("--organization", default=os.getenv("SENTRY_ORG"))
    parser.add_argument("--project", default=os.getenv("SENTRY_PROJECT"))
    parser.add_argument("--release", default=os.getenv("SENTRY_RELEASE"))
    parser.add_argument("--token-file", default=os.getenv("SENTRY_AUTH_TOKEN_FILE"))
    parser.add_argument("--api-url", default=os.getenv("SENTRY_API_URL", "https://sentry.io/api/0"))
    parser.add_argument("--timeout", type=float, default=10)
    parser.add_argument("--attempts", type=int, default=3)
    parser.add_argument("--repository")
    parser.add_argument("--commit")
    parser.add_argument("--source-url")
    parser.add_argument("--environment")
    parser.add_argument("--name")
    parser.add_argument("--url")
    parser.add_argument("--readiness-url")
    args = parser.parse_args(argv)
    validate_common(args)
    if not 1 <= args.attempts <= 3 or not 0 < args.timeout <= 30:
        fail("attempts must be 1..3 and timeout must be greater than 0 and at most 30 seconds")
    org = urllib.parse.quote(args.organization, safe="")
    version = urllib.parse.quote(args.release, safe="")
    now = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
    if args.command == "release":
        if not REPOSITORY.fullmatch(args.repository or "") or not SHA.fullmatch(args.commit or ""):
            fail("release requires a full owner/repository and a 40-character lowercase commit SHA")
        if not args.source_url or not args.source_url.startswith("https://"):
            fail("release requires an HTTPS source URL")
        payload = {
            "version": args.release,
            "projects": [args.project],
            "ref": args.commit,
            "refs": [{"repository": args.repository, "commit": args.commit}],
            "url": args.source_url,
            "dateReleased": now,
        }
        request(args, "POST", f"/organizations/{org}/releases/", payload)
        # POST may return 208 for an earlier attempt. PUT makes the exact commit
        # association and final publication timestamp deterministic on reruns.
        update = {key: payload[key] for key in ("ref", "refs", "url", "dateReleased")}
        request(args, "PUT", f"/organizations/{org}/releases/{version}/", update)
    else:
        if not SAFE.fullmatch(args.environment or ""):
            fail("deploy requires a valid environment")
        verify_readiness(args)
        payload = {"environment": args.environment, "projects": [args.project], "dateFinished": now}
        if args.name:
            payload["name"] = args.name
        if args.url:
            payload["url"] = args.url
        request(args, "POST", f"/organizations/{org}/releases/{version}/deploys/", payload)


if __name__ == "__main__":
    main()
