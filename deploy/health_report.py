"""Push one operator-side host-health report (hosts, canary, deploy) to the admin panel through the operator API.

PAPERBOAT_HEALTH_REPORT_URL (the server's origin) and PAPERBOAT_HEALTH_REPORT_OPERATOR_FILE are optional; without them
nothing is sent. The file (mode 0600) holds an operations-role operator: email on line 1, password on line 2, the
authenticator key (base32) on line 3. There is no separate reporting credential. Edges never report here: each edge's
certificate renewal rides in its own heartbeat. Best effort: a failed push never fails the caller.
"""
import base64
import hashlib
import hmac
import http.cookiejar
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path


def totp(secret, offset=0, now=None):
    """The RFC 6238 code (30 s steps, 6 digits, SHA-1) an authenticator shows for a base32 secret."""
    secret = secret.replace(" ", "").upper()
    step = int((time.time() if now is None else now) // 30) + offset
    key = base64.b32decode(secret + "=" * (-len(secret) % 8))
    mac = hmac.new(key, step.to_bytes(8, "big"), hashlib.sha1).digest()
    at = mac[-1] & 15
    return "%06d" % ((int.from_bytes(mac[at:at + 4], "big") & 0x7fffffff) % 1000000)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Operator:
    def __init__(self, url, credential_file):
        if not url.startswith("https://"):
            raise RuntimeError("PAPERBOAT_HEALTH_REPORT_URL must be an https:// origin: the operator password is sent to it")
        self.url, self.credential_file, self.csrf = url.rstrip("/"), Path(credential_file), ""
        # No redirects: the password and session never leave the configured HTTPS origin.
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), NoRedirect())

    def call(self, method, path, body, headers=None):
        all_headers = {"content-type": "application/json", **(headers or {})}
        if self.csrf:
            all_headers["x-csrf-token"] = self.csrf
        request = urllib.request.Request(self.url + path, method=method, headers=all_headers, data=json.dumps(body).encode())
        try:
            with self.opener.open(request, timeout=10) as response:
                return response.status, json.loads(response.read() or b"null")
        except urllib.error.HTTPError as error:
            return error.code, None

    def login(self):
        lines = self.credential_file.read_text().split("\n")
        email, password, key = lines[0].strip(), lines[1].strip(), (lines[2].strip() if len(lines) > 2 else "")
        status, data = self.call("POST", "/v1/operator/login", {"email": email, "password": password})
        if status != 200:
            raise RuntimeError(f"operator login failed (HTTP {status})")
        self.csrf = data["data"]["csrf_token"]
        if data["data"].get("mfa") == "verified":
            return
        if data["data"].get("mfa") != "verify" or not key:
            raise RuntimeError("the reporting operator needs its authenticator key on line 3 of the credential file")
        for offset in (0, 1):  # a code is accepted once per 30 s step: the next step's code is accepted too
            status, _ = self.call("POST", "/v1/operator/mfa/verify", {"code": totp(key, offset)})
            if status == 200:
                return
        raise RuntimeError(f"the reporting operator's second factor was refused (HTTP {status})")

    def report(self, source, payload):
        status, _ = self.call("POST", "/v1/admin/host-health/reports", {"source": source, "payload": payload}, {
            "operator-target": source, "operator-reason": "host-health-report", "idempotency-key": secrets.token_hex(16)})
        if status != 200:
            raise RuntimeError(f"host health report refused (HTTP {status})")


def push(source, payload):
    url, credential_file = os.environ.get("PAPERBOAT_HEALTH_REPORT_URL"), os.environ.get("PAPERBOAT_HEALTH_REPORT_OPERATOR_FILE")
    if not url or not credential_file:
        return
    try:
        operator = Operator(url, credential_file)
        operator.login()
        operator.report(source, payload)
    except Exception as error:  # never mask the caller's own result
        print(f"health report failed: {error}", file=sys.stderr)
